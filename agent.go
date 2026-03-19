package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"
)

// Agent is the main OVN route synchronization agent.
type Agent struct {
	cfg     Config
	ovn     *OVNClient
	routing *RouteManager

	// Channel to trigger reconciliation
	reconcileCh chan struct{}

	// effectiveFilters holds the network filters in effect for the current
	// reconciliation cycle — either from manual config or auto-discovered
	// from OVN Logical_Router_Port.Networks.
	effectiveFilters []*net.IPNet
}

func NewAgent(cfg Config) (*Agent, error) {
	a := &Agent{
		cfg:         cfg,
		routing:     NewRouteManager(cfg),
		reconcileCh: make(chan struct{}, 1),
	}

	a.ovn = NewOVNClient(cfg, a.triggerReconcile)

	return a, nil
}

// triggerReconcile requests an asynchronous reconciliation (non-blocking).
func (a *Agent) triggerReconcile() {
	select {
	case a.reconcileCh <- struct{}{}:
	default:
		// Already pending
	}
}

// Run starts the agent: connects to OVN, runs initial reconciliation,
// then loops on events and periodic reconciliation.
func (a *Agent) Run(ctx context.Context) error {
	// Verify that the bridge device exists and is up before proceeding.
	if err := a.routing.CheckBridgeDevice(); err != nil {
		return fmt.Errorf("bridge device check failed: %w", err)
	}

	// Add a link-local IP to br-ex so the kernel can ARP on the interface.
	if err := a.routing.EnsureBridgeIP(a.cfg.BridgeIP); err != nil {
		return fmt.Errorf("ensure bridge IP: %w", err)
	}

	// Enable proxy ARP so the kernel responds to ARP requests for FIP addresses.
	if err := a.routing.EnableProxyARP(); err != nil {
		return fmt.Errorf("enable proxy ARP: %w", err)
	}

	// Set up veth VRF leak for route leaking between default VRF and provider VRF.
	if err := a.routing.SetupVethLeak(); err != nil {
		return fmt.Errorf("veth VRF leak setup: %w", err)
	}

	if a.cfg.GatewayPort == "" {
		slog.Info("tracking all chassisredirect ports (multi-router mode)")
	} else {
		slog.Info("tracking single chassisredirect port", "gateway_port", a.cfg.GatewayPort)
	}

	// Connect to OVN with retry
	for {
		err := a.ovn.Connect(ctx)
		if err == nil {
			break
		}
		slog.Error("failed to connect to OVN, retrying in 5s", "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	defer a.ovn.Close()

	// Initial reconciliation
	a.reconcile()

	// Drain any reconcile signals queued during startup — the initial
	// reconcile already handled the current state.
	select {
	case <-a.reconcileCh:
	default:
	}

	// Main loop
	ticker := time.NewTicker(a.cfg.ReconcileInterval)
	defer ticker.Stop()

	slog.Info("agent running", "reconcile_interval", a.cfg.ReconcileInterval)

	for {
		select {
		case <-ctx.Done():
			if a.cfg.CleanupOnShutdown {
				slog.Info("shutting down, cleaning up routes")
				a.cleanup()
			} else {
				slog.Info("shutting down, keeping routes in place")
			}
			return nil

		case <-a.reconcileCh:
			slog.Debug("event-triggered reconciliation")
			a.reconcile()

		case <-ticker.C:
			slog.Debug("periodic reconciliation")
			a.reconcile()
		}
	}
}

// reconcile ensures the local routing state matches the desired state from OVN.
func (a *Agent) reconcile() {
	state := a.ovn.GetState()

	// Compute effective network filters for this cycle.
	a.effectiveFilters = a.computeEffectiveNetworks(state.DiscoveredNetworks)

	// Combine FIPs and SNAT IPs, deduplicate
	desiredIPs := uniqueIPs(append(state.FIPs, state.SNATIPs...))

	slog.Info("reconciling",
		"has_local_routers", state.HasLocalRouters,
		"local_routers", len(state.LocalRouters),
		"local_host", state.LocalChassisName,
		"desired_ips", len(desiredIPs),
		"effective_networks", len(a.effectiveFilters),
	)

	if state.HasLocalRouters {
		// Ensure OVS MAC-tweak flows are in place (only when active).
		if err := a.routing.EnsureOVSFlows(); err != nil {
			slog.Error("failed to ensure OVS flows", "error", err)
		}

		// Ensure OVN default routes and static MAC bindings for local routers.
		bridgeMAC := a.routing.cachedBridgeMAC
		if bridgeMAC == "" {
			if mac, err := a.routing.GetBridgeMAC(); err == nil {
				bridgeMAC = mac.String()
			}
		}
		if bridgeMAC != "" {
			if err := a.ovn.EnsureGatewayRouting(a.ovn.ctx, state.LocalRouters, bridgeMAC); err != nil {
				slog.Error("failed to ensure gateway routing", "error", err)
			}
		}

		// Reconcile per-network veth leak routes and policy rules.
		if err := a.routing.ReconcileVethLeakNetworks(a.effectiveFilters); err != nil {
			slog.Error("failed to reconcile veth leak networks", "error", err)
		}

		// Reconcile FRR prefix-list entries for discovered networks.
		if err := a.routing.ReconcileFRRPrefixList(a.effectiveFilters); err != nil {
			slog.Error("failed to reconcile FRR prefix-list", "error", err)
		}

		a.ensureRoutes(desiredIPs)
	} else {
		// No locally active routers — remove per-network veth leak and prefix-list entries.
		if err := a.routing.ReconcileVethLeakNetworks(nil); err != nil {
			slog.Error("failed to clean veth leak networks", "error", err)
		}
		if err := a.routing.ReconcileFRRPrefixList(nil); err != nil {
			slog.Error("failed to clean FRR prefix-list", "error", err)
		}
		a.removeAllRoutes("no locally active routers")
	}
}

// computeEffectiveNetworks returns the network filters to use: manual config if set,
// otherwise auto-discovered networks from OVN Logical_Router_Port.
func (a *Agent) computeEffectiveNetworks(discovered []*net.IPNet) []*net.IPNet {
	return effectiveNetworkFilters(a.cfg.NetworkFilters, discovered)
}

// ensureRoutes adds routes for all desired IPs and removes stale ones.
func (a *Agent) ensureRoutes(desiredIPs []string) {
	desiredSet := make(map[string]bool, len(desiredIPs))
	for _, ip := range desiredIPs {
		desiredSet[ip] = true
	}

	// Collect current state so we only add what is actually missing.
	currentKernelSet := make(map[string]bool)
	currentKernel, err := a.routing.ListKernelRoutes()
	if err != nil {
		slog.Error("failed to list kernel routes", "error", err)
	} else {
		for _, ip := range currentKernel {
			currentKernelSet[ip] = true
		}
	}

	currentFRRSet := make(map[string]bool)
	currentFRR, err := a.routing.ListFRRRoutes()
	if err != nil {
		slog.Error("failed to list FRR routes", "error", err)
	} else {
		for _, ip := range currentFRR {
			currentFRRSet[ip] = true
		}
	}

	// Add missing routes
	for _, ip := range desiredIPs {
		needsKernel := !currentKernelSet[ip]
		needsFRR := !currentFRRSet[ip]

		if !needsKernel && !needsFRR {
			slog.Debug("route already exists", "ip", ip)
			continue
		}

		slog.Info("ensuring route", "ip", ip, "needs_kernel", needsKernel, "needs_frr", needsFRR)

		if needsKernel {
			if err := a.routing.AddKernelRoute(ip); err != nil {
				slog.Error("failed to add kernel route", "ip", ip, "error", err)
			}
		}
		if needsFRR {
			if err := a.routing.AddFRRRoute(ip); err != nil {
				slog.Error("failed to add FRR route", "ip", ip, "error", err)
			}
		}
	}

	// Remove stale kernel routes (also removes the corresponding FRR route)
	removedSet := make(map[string]bool)
	for _, ip := range currentKernel {
		if !desiredSet[ip] && a.isManaged(ip) {
			slog.Info("removing stale route", "ip", ip)
			if err := a.routing.RemoveRoute(ip); err != nil {
				slog.Error("failed to remove stale route", "ip", ip, "error", err)
			}
			removedSet[ip] = true
		}
	}

	// Remove orphaned FRR routes that have no corresponding kernel route
	// (skip IPs already handled in the stale route loop above)
	for _, ip := range currentFRR {
		if !desiredSet[ip] && a.isManaged(ip) && !removedSet[ip] {
			slog.Info("removing orphaned FRR route", "ip", ip)
			if err := a.routing.DelFRRRoute(ip); err != nil {
				slog.Error("failed to remove orphaned FRR route", "ip", ip, "error", err)
			}
		}
	}
}

// removeAllRoutes removes all managed FIP routes.
// The reason parameter is used in log messages to indicate why routes are being removed.
func (a *Agent) removeAllRoutes(reason string) {
	currentKernel, err := a.routing.ListKernelRoutes()
	if err != nil {
		slog.Error("failed to list kernel routes", "error", err)
	} else {
		for _, ip := range currentKernel {
			if a.isManaged(ip) {
				slog.Info("removing route", "ip", ip, "reason", reason)
				if err := a.routing.RemoveRoute(ip); err != nil {
					slog.Error("failed to remove route", "ip", ip, "error", err)
				}
			}
		}
	}

	// Remove any orphaned FRR routes that exist without corresponding kernel routes
	currentFRR, err := a.routing.ListFRRRoutes()
	if err != nil {
		slog.Error("failed to list FRR routes", "error", err)
		return
	}
	for _, ip := range currentFRR {
		if a.isManaged(ip) {
			slog.Info("removing orphaned FRR route", "ip", ip, "reason", reason)
			if err := a.routing.DelFRRRoute(ip); err != nil {
				slog.Error("failed to remove FRR route", "ip", ip, "error", err)
			}
		}
	}
}

// cleanup removes all managed routes, OVS flows, and OVN NB entries on shutdown.
func (a *Agent) cleanup() {
	a.removeAllRoutes("shutdown cleanup")

	// Clean up FRR prefix-list entries.
	if err := a.routing.ReconcileFRRPrefixList(nil); err != nil {
		slog.Error("failed to cleanup FRR prefix-list", "error", err)
	}

	if err := a.routing.RemoveOVSFlows(); err != nil {
		slog.Error("failed to remove OVS flows", "error", err)
	}
	if err := a.routing.CleanupRoutingTable(); err != nil {
		slog.Error("failed to flush routing table", "error", err)
	}
	if err := a.routing.TeardownVethLeak(); err != nil {
		slog.Error("failed to tear down veth VRF leak", "error", err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	if err := a.ovn.RemoveManagedNBEntries(cleanupCtx); err != nil {
		slog.Error("failed to remove managed OVN NB entries", "error", err)
	}
}

// isManaged returns true if the IP is within any of the effective network CIDRs.
// If no CIDRs are configured or discovered, all /32 routes on the bridge are considered managed.
func (a *Agent) isManaged(ip string) bool {
	if len(a.effectiveFilters) == 0 {
		return true
	}
	parsedIP := net.ParseIP(ip)
	return parsedIP != nil && containedInAny(parsedIP, a.effectiveFilters)
}

// uniqueIPs deduplicates and sorts a list of IP strings.
func uniqueIPs(ips []string) []string {
	seen := make(map[string]bool, len(ips))
	var result []string
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip != "" && !seen[ip] {
			seen[ip] = true
			result = append(result, ip)
		}
	}
	sort.Strings(result)
	return result
}
