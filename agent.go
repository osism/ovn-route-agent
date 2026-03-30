package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
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

	// consecutiveReAdds tracks how many reconcile cycles in a row the
	// post-change verification had to re-add missing routes. A sustained
	// non-zero value indicates persistent route instability (e.g. FRR
	// misconfiguration) and triggers escalated logging.
	consecutiveReAdds int

	// missingChassis tracks when each chassis was first observed as absent
	// from the OVN SB Chassis table. Used for stale entry cleanup with a
	// configurable grace period.
	missingChassis map[string]time.Time

	// staleCleanupJitter is a random duration (0-30s) added to the grace
	// period to prevent multiple agents from cleaning up simultaneously.
	staleCleanupJitter time.Duration
}

// maxStaleCleanupJitter is the maximum random jitter added to the stale
// chassis grace period to prevent thundering-herd cleanup across agents.
const maxStaleCleanupJitter = 30 * time.Second

func NewAgent(cfg Config) (*Agent, error) {
	a := &Agent{
		cfg:                cfg,
		routing:            NewRouteManager(cfg),
		reconcileCh:        make(chan struct{}, 1),
		missingChassis:     make(map[string]time.Time),
		staleCleanupJitter: time.Duration(rand.Int63n(int64(maxStaleCleanupJitter))),
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

	// Set up port forwarding (DNAT) rules (requires veth pair).
	if err := a.routing.SetupPortForward(); err != nil {
		return fmt.Errorf("port forward setup: %w", err)
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

	// Restore any gateway chassis priorities that were drained by a previous run.
	if a.cfg.DrainOnShutdown {
		a.ovn.RestoreDrainedGateways(ctx, a.ovn.GetState().LocalChassisName)
	}

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
			// Drain must happen BEFORE cleanup and BEFORE OVN connection close.
			// Use a fresh context since the parent ctx is already cancelled.
			if a.cfg.DrainOnShutdown {
				drainCtx, drainCancel := context.WithTimeout(context.Background(), a.cfg.DrainTimeout)
				slog.Info("drain mode active, migrating gateways away", "timeout", a.cfg.DrainTimeout)
				if err := a.ovn.DrainGateways(drainCtx, a.ovn.GetState().LocalChassisName); err != nil {
					slog.Error("drain failed", "error", err)
				}
				drainCancel()
			}
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

	// hairpinMACMap maps each IP that needs a hairpin flow to the MAC of
	// the router port that owns it. This includes FIPs, SNAT IPs, and
	// router gateway IPs (LRP networks). Port-forward VIPs are
	// intentionally excluded because their DNAT is handled by nftables.
	//
	// The MAC is used as mod_dl_dst in the hairpin flow so that OVN's
	// L2 lookup delivers the reflected packet to the correct router.
	hairpinMACMap := make(map[string]string, len(state.NATIPToRouterMAC))
	for ip, mac := range state.NATIPToRouterMAC {
		hairpinMACMap[ip] = mac
	}
	// Router gateway IPs (LRP networks) are included so that VMs on a
	// same-chassis router can reach other routers' gateway addresses,
	// matching the behaviour seen from outside.
	for _, lr := range state.LocalRouters {
		for _, cidr := range lr.LRPNetworks {
			ip, _, err := net.ParseCIDR(cidr)
			if err != nil {
				continue
			}
			if lr.LRPMAC != "" {
				hairpinMACMap[ip.String()] = lr.LRPMAC
			}
		}
	}

	// hairpinIPs is the flat list of IPs for kernel routes and FRR.
	// Map keys are already unique; just sort for deterministic ordering.
	hairpinIPs := make([]string, 0, len(hairpinMACMap))
	for ip := range hairpinMACMap {
		hairpinIPs = append(hairpinIPs, ip)
	}
	sort.Strings(hairpinIPs)

	// desiredIPs extends hairpinIPs with port-forward VIPs — these need
	// kernel routes on br-ex and FRR static routes for BGP announcement,
	// independent of whether any OVN routers are locally active.
	desiredIPs := hairpinIPs
	for _, pf := range a.cfg.PortForwards {
		desiredIPs = append(desiredIPs, pf.VIP)
	}
	desiredIPs = uniqueIPs(desiredIPs)

	slog.Info("reconciling",
		"has_local_routers", state.HasLocalRouters,
		"local_routers", len(state.LocalRouters),
		"local_host", state.LocalChassisName,
		"desired_ips", len(desiredIPs),
		"effective_networks", len(a.effectiveFilters),
	)
	if len(desiredIPs) > 0 {
		slog.Debug("desired IP list", "ips", desiredIPs)
	}

	if state.HasLocalRouters {
		// Ensure OVS MAC-tweak flows are in place (only when active).
		if err := a.routing.EnsureOVSFlows(); err != nil {
			slog.Error("failed to ensure OVS flows", "error", err)
		}

		// Reconcile per-IP hairpin flows for same-chassis cross-router
		// communication. These reflect FIP/SNAT-IP traffic from OVN back
		// into OVN's external pipeline instead of sending it to the kernel,
		// fixing the case where two routers share the same gateway chassis.
		if err := a.routing.ReconcileOVSHairpinFlows(hairpinMACMap); err != nil {
			slog.Error("failed to reconcile OVS hairpin flows", "error", err)
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

		// Ensure the active chassis has a strictly higher priority than
		// standby peers, preventing reverse failover after drain/restore.
		if err := a.ovn.EnsureActivePriorityLead(a.ovn.ctx, state.LocalRouters, state.LocalChassisName); err != nil {
			slog.Error("failed to ensure active priority lead", "error", err)
		}

		// Reconcile per-network veth leak routes and policy rules.
		if err := a.routing.ReconcileVethLeakNetworks(a.effectiveFilters); err != nil {
			slog.Error("failed to reconcile veth leak networks", "error", err)
		}

		// Reconcile FRR prefix-list entries for discovered networks.
		if err := a.routing.ReconcileFRRPrefixList(a.effectiveFilters); err != nil {
			slog.Error("failed to reconcile FRR prefix-list", "error", err)
		}
	} else {
		// No locally active routers — remove per-network veth leak and prefix-list entries.
		if err := a.routing.ReconcileVethLeakNetworks(nil); err != nil {
			slog.Error("failed to clean veth leak networks", "error", err)
		}
		if err := a.routing.ReconcileFRRPrefixList(nil); err != nil {
			slog.Error("failed to clean FRR prefix-list", "error", err)
		}
		if err := a.routing.ReconcileOVSHairpinFlows(nil); err != nil {
			slog.Error("failed to clean OVS hairpin flows", "error", err)
		}
	}

	// Port forwarding reconciliation runs regardless of local router
	// presence — DNAT VIPs are managed independently of OVN gateway state.
	if err := a.routing.ReconcilePortForward(a.effectiveFilters); err != nil {
		slog.Error("failed to reconcile port forwarding", "error", err)
	}

	// Ensure routes for all desired IPs (FIPs, SNATs, and port forward VIPs).
	// When no local routers are present but port forwards are configured,
	// this still installs VIP routes on br-ex and in FRR.
	if len(desiredIPs) > 0 || state.HasLocalRouters {
		a.ensureRoutes(desiredIPs)
	} else {
		a.removeAllRoutes("no locally active routers and no port forward VIPs")
	}

	// Check for stale chassis entries from dead nodes (runs on every agent).
	// This runs after gateway routing reconciliation so that a surviving agent
	// creates its own routes before removing entries from dead chassis.
	a.cleanupStaleChassis(state.AllChassisNames)
}

// cleanupStaleChassis detects chassis that have disappeared from the SB Chassis
// table and, after a configurable grace period (plus random jitter), removes
// their managed NB entries. Any surviving agent can perform this cleanup.
func (a *Agent) cleanupStaleChassis(allChassis map[string]bool) {
	if a.cfg.StaleChassisGracePeriod <= 0 {
		return
	}

	referencedChassis := a.ovn.ListManagedRouteChassis(a.ovn.ctx)
	if referencedChassis == nil {
		return
	}

	now := time.Now()
	effectiveGrace := a.cfg.StaleChassisGracePeriod + a.staleCleanupJitter

	// Update missingChassis map: add newly missing, remove returned.
	for chassisName := range referencedChassis {
		if allChassis[chassisName] {
			// Chassis is back (or still alive) — remove from missing tracker.
			if _, tracked := a.missingChassis[chassisName]; tracked {
				slog.Info("previously missing chassis has returned", "chassis", chassisName)
				delete(a.missingChassis, chassisName)
			}
		} else {
			// Chassis is missing.
			if _, tracked := a.missingChassis[chassisName]; !tracked {
				a.missingChassis[chassisName] = now
				slog.Warn("chassis referenced by managed routes is missing from SB",
					"chassis", chassisName)
			}
		}
	}

	// Prune entries for chassis no longer referenced by any managed route.
	for chassisName := range a.missingChassis {
		if !referencedChassis[chassisName] {
			delete(a.missingChassis, chassisName)
		}
	}

	// Find chassis that have exceeded the grace period.
	staleChassis := make(map[string]bool)
	for chassisName, firstSeen := range a.missingChassis {
		if now.Sub(firstSeen) >= effectiveGrace {
			staleChassis[chassisName] = true
			slog.Warn("chassis exceeded stale grace period, cleaning up managed entries",
				"chassis", chassisName,
				"missing_since", firstSeen,
				"grace_period", effectiveGrace)
		}
	}

	if len(staleChassis) == 0 {
		return
	}

	if err := a.ovn.CleanupStaleChassisManagedEntries(a.ovn.ctx, staleChassis); err != nil {
		slog.Error("failed to clean up stale chassis entries", "error", err)
		return
	}

	for chassisName := range staleChassis {
		delete(a.missingChassis, chassisName)
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

	// Collect missing and stale routes, then apply in batches.
	var addFRR []string
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
			addFRR = append(addFRR, ip)
		}
	}

	// Batch-add all missing FRR routes in one vtysh call.
	if len(addFRR) > 0 {
		if err := a.routing.AddFRRRoutes(addFRR); err != nil {
			slog.Error("failed to batch-add FRR routes", "count", len(addFRR), "ips", addFRR, "error", err)
		}
	}

	// Collect stale routes for batch removal.
	var delFRR []string
	removedSet := make(map[string]bool)
	for _, ip := range currentKernel {
		if !desiredSet[ip] && a.isManaged(ip) {
			slog.Info("removing stale route", "ip", ip)
			// Remove FRR first to stop attracting traffic before tearing down the data plane.
			delFRR = append(delFRR, ip)
			removedSet[ip] = true
		}
	}

	// Collect orphaned FRR routes that have no corresponding kernel route.
	for _, ip := range currentFRR {
		if !desiredSet[ip] && a.isManaged(ip) && !removedSet[ip] {
			slog.Info("removing orphaned FRR route", "ip", ip)
			delFRR = append(delFRR, ip)
		}
	}

	// Batch-remove all stale/orphaned FRR routes in one vtysh call.
	if len(delFRR) > 0 {
		if err := a.routing.DelFRRRoutes(delFRR); err != nil {
			slog.Error("failed to batch-del FRR routes", "count", len(delFRR), "ips", delFRR, "error", err)
		}
	}

	// Remove stale kernel routes (after FRR withdrawal).
	for _, ip := range currentKernel {
		if removedSet[ip] {
			if err := a.routing.DelKernelRoute(ip); err != nil {
				slog.Error("failed to remove kernel route", "ip", ip, "error", err)
			}
		}
	}

	// Only trigger a BGP soft-refresh when routes were removed. For
	// additions FRR's normal route redistribution announces the new
	// static routes automatically. A blanket "clear ip bgp * soft out"
	// re-evaluates outbound policy for ALL routes; doing this on every
	// addition risks disrupting existing BGP announcements.
	if len(delFRR) > 0 {
		if err := a.routing.RefreshBGP(); err != nil {
			slog.Warn("BGP soft-refresh failed, peers may wait for MRAI timer", "error", err)
		}
	}

	// Safety net: after any route changes, verify that all desired routes
	// are still present. A BGP soft-refresh re-evaluates outbound policy
	// and could (in edge cases) interact with FRR in unexpected ways.
	if len(addFRR) > 0 || len(delFRR) > 0 {
		a.verifyRoutes(desiredIPs)
	}
}

// removeAllRoutes removes all managed FIP routes.
// The reason parameter is used in log messages to indicate why routes are being removed.
func (a *Agent) removeAllRoutes(reason string) {
	// Collect all managed FRR routes for batch removal (FRR first to stop attracting traffic).
	var delFRR []string
	currentFRR, err := a.routing.ListFRRRoutes()
	if err != nil {
		slog.Error("failed to list FRR routes", "error", err)
	} else {
		for _, ip := range currentFRR {
			if a.isManaged(ip) {
				delFRR = append(delFRR, ip)
			}
		}
	}

	if len(delFRR) > 0 {
		slog.Info("batch-removing FRR routes", "count", len(delFRR), "reason", reason)
		if err := a.routing.DelFRRRoutes(delFRR); err != nil {
			slog.Error("failed to batch-del FRR routes", "count", len(delFRR), "ips", delFRR, "error", err)
		}
	}

	// Remove kernel routes.
	currentKernel, err := a.routing.ListKernelRoutes()
	if err != nil {
		slog.Error("failed to list kernel routes", "error", err)
	} else {
		for _, ip := range currentKernel {
			if a.isManaged(ip) {
				slog.Info("removing kernel route", "ip", ip, "reason", reason)
				if err := a.routing.DelKernelRoute(ip); err != nil {
					slog.Error("failed to remove kernel route", "ip", ip, "error", err)
				}
			}
		}
	}

	if len(delFRR) > 0 {
		if err := a.routing.RefreshBGP(); err != nil {
			slog.Warn("BGP soft-refresh failed, peers may wait for MRAI timer", "error", err)
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
	// Tear down port forwarding before veth leak (DNAT return route uses veth).
	if err := a.routing.TeardownPortForward(); err != nil {
		slog.Error("failed to tear down port forwarding", "error", err)
	}
	if err := a.routing.TeardownVethLeak(); err != nil {
		slog.Error("failed to tear down veth VRF leak", "error", err)
	}
	if err := a.routing.RemoveBridgeIP(a.cfg.BridgeIP); err != nil {
		slog.Error("failed to remove bridge IP", "error", err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	if err := a.ovn.RemoveManagedNBEntries(cleanupCtx); err != nil {
		slog.Error("failed to remove managed OVN NB entries", "error", err)
	}
}

// consecutiveReAddThreshold is the number of consecutive reconcile cycles
// with route re-adds before logging escalates from Warn to Error.
const consecutiveReAddThreshold = 3

// verifyRoutes checks that all desired IPs still have both a kernel route and
// an FRR static route after route mutations. Any route that disappeared
// (e.g. due to a vtysh race or unexpected FRR behaviour) is re-added
// immediately so that existing connections are not disrupted.
//
// Returns the number of routes that had to be re-added (0 means all routes
// were present). The agent tracks consecutive non-zero results and escalates
// logging to help operators detect persistent route instability.
func (a *Agent) verifyRoutes(desiredIPs []string) int {
	// Re-read current FRR routes.
	currentFRR, err := a.routing.ListFRRRoutes()
	if err != nil {
		slog.Error("post-change FRR route verification failed", "error", err)
		return 0
	}
	frrSet := make(map[string]bool, len(currentFRR))
	for _, ip := range currentFRR {
		frrSet[ip] = true
	}

	// Re-read current kernel routes.
	currentKernel, err := a.routing.ListKernelRoutes()
	if err != nil {
		slog.Error("post-change kernel route verification failed", "error", err)
		return 0
	}
	kernelSet := make(map[string]bool, len(currentKernel))
	for _, ip := range currentKernel {
		kernelSet[ip] = true
	}

	var reAddFRR []string
	reAddKernel := 0
	for _, ip := range desiredIPs {
		if !a.isManaged(ip) {
			continue
		}
		if !frrSet[ip] {
			slog.Warn("FRR route missing after route change, re-adding", "ip", ip)
			reAddFRR = append(reAddFRR, ip)
		}
		if !kernelSet[ip] {
			slog.Warn("kernel route missing after route change, re-adding", "ip", ip)
			if err := a.routing.AddKernelRoute(ip); err != nil {
				slog.Error("failed to re-add kernel route", "ip", ip, "error", err)
			}
			reAddKernel++
		}
	}

	if len(reAddFRR) > 0 {
		if err := a.routing.AddFRRRoutes(reAddFRR); err != nil {
			slog.Error("failed to re-add FRR routes", "count", len(reAddFRR), "error", err)
		}
	}

	totalReAdds := len(reAddFRR) + reAddKernel
	if totalReAdds > 0 {
		a.consecutiveReAdds++
		if a.consecutiveReAdds >= consecutiveReAddThreshold {
			slog.Error("persistent route instability detected: routes required re-adding for multiple consecutive cycles",
				"consecutive_cycles", a.consecutiveReAdds,
				"re_added_this_cycle", totalReAdds,
			)
		}
	} else {
		a.consecutiveReAdds = 0
	}

	return totalReAdds
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
