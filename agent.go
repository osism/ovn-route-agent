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

	// Combine FIPs and SNAT IPs, deduplicate
	desiredIPs := uniqueIPs(append(state.FIPs, state.SNATIPs...))

	slog.Info("reconciling",
		"has_local_routers", state.HasLocalRouters,
		"local_routers", len(state.LocalRouters),
		"local_host", state.LocalChassisName,
		"desired_ips", len(desiredIPs),
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

		a.ensureRoutes(desiredIPs)
	} else {
		a.removeAllRoutes("no locally active routers")
	}
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

// cleanup removes all managed routes and OVS flows on shutdown.
func (a *Agent) cleanup() {
	a.removeAllRoutes("shutdown cleanup")

	if err := a.routing.RemoveOVSFlows(); err != nil {
		slog.Error("failed to remove OVS flows", "error", err)
	}
	if err := a.routing.CleanupRoutingTable(); err != nil {
		slog.Error("failed to flush routing table", "error", err)
	}
}

// isManaged returns true if the IP is within any of the managed network CIDRs.
// If no CIDRs are configured, all /32 routes on the bridge are considered managed.
func (a *Agent) isManaged(ip string) bool {
	if len(a.cfg.NetworkFilters) == 0 {
		return true
	}
	parsedIP := net.ParseIP(ip)
	return parsedIP != nil && containedInAny(parsedIP, a.cfg.NetworkFilters)
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
