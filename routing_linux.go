package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/vishvananda/netlink"
)

// rtProtoOVNNetworkAgent is a custom route protocol number used to tag kernel
// routes created by ReconcileVethLeakNetworks. This distinguishes them from
// routes installed by FRR/zebra (which use RTPROT_ZEBRA or RTPROT_STATIC).
const rtProtoOVNNetworkAgent = 44

// CheckBridgeDevice verifies that the bridge device exists and that the agent
// has sufficient privileges (root or CAP_NET_ADMIN) for route management.
// If the device exists but is not up, it will be brought up automatically.
func (rm *RouteManager) CheckBridgeDevice() error {
	if rm.dryRun {
		slog.Info("[dry-run] skipping bridge device check", "dev", rm.bridgeDev)
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("agent must run as root (current euid: %d)", os.Geteuid())
	}
	link, err := netlink.LinkByName(rm.bridgeDev)
	if err != nil {
		return fmt.Errorf("bridge device %s not found: %w", rm.bridgeDev, err)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		slog.Info("bridge device is not up, bringing it up", "dev", rm.bridgeDev)
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to bring up bridge device %s: %w", rm.bridgeDev, err)
		}
	}
	slog.Info("bridge device is up", "dev", rm.bridgeDev)
	return nil
}

// EnsureBridgeIP adds a /32 IP address to the bridge device if not already present.
// This gives the kernel a source IP for ARP resolution on the bridge.
func (rm *RouteManager) EnsureBridgeIP(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would add bridge IP", "ip", ip, "dev", rm.bridgeDev)
		return nil
	}
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return fmt.Errorf("invalid IP: %s", ip)
	}

	link, err := netlink.LinkByName(rm.bridgeDev)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", rm.bridgeDev, err)
	}

	addr := &netlink.Addr{
		IPNet: &net.IPNet{IP: parsedIP, Mask: net.CIDRMask(32, 32)},
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list addrs on %s: %w", rm.bridgeDev, err)
	}
	for _, a := range addrs {
		if a.IP.Equal(parsedIP) {
			slog.Debug("bridge IP already present", "ip", ip, "dev", rm.bridgeDev)
			return nil
		}
	}

	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("add IP %s/32 to %s: %w", ip, rm.bridgeDev, err)
	}
	slog.Info("bridge IP added", "ip", ip, "dev", rm.bridgeDev)
	return nil
}

// RemoveBridgeIP removes the /32 IP address from the bridge device.
func (rm *RouteManager) RemoveBridgeIP(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove bridge IP", "ip", ip, "dev", rm.bridgeDev)
		return nil
	}
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return fmt.Errorf("invalid IP: %s", ip)
	}

	link, err := netlink.LinkByName(rm.bridgeDev)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", rm.bridgeDev, err)
	}

	addr := &netlink.Addr{
		IPNet: &net.IPNet{IP: parsedIP, Mask: net.CIDRMask(32, 32)},
	}

	if err := netlink.AddrDel(link, addr); err != nil {
		return fmt.Errorf("remove IP %s/32 from %s: %w", ip, rm.bridgeDev, err)
	}
	slog.Info("bridge IP removed", "ip", ip, "dev", rm.bridgeDev)
	return nil
}

// EnableProxyARP enables proxy ARP on the bridge device so the kernel responds
// to ARP requests for any IP it has a route for on that interface.
func (rm *RouteManager) EnableProxyARP() error {
	if rm.dryRun {
		slog.Info("[dry-run] would enable proxy ARP", "dev", rm.bridgeDev)
		return nil
	}
	path := filepath.Join("/proc/sys/net/ipv4/conf", rm.bridgeDev, "proxy_arp")
	if err := os.WriteFile(path, []byte("1\n"), 0644); err != nil {
		return fmt.Errorf("enable proxy ARP on %s: %w", rm.bridgeDev, err)
	}
	slog.Info("proxy ARP enabled", "dev", rm.bridgeDev)
	return nil
}

// GetBridgeMAC returns the hardware MAC address of the bridge device.
func (rm *RouteManager) GetBridgeMAC() (net.HardwareAddr, error) {
	link, err := netlink.LinkByName(rm.bridgeDev)
	if err != nil {
		return nil, fmt.Errorf("find bridge %s: %w", rm.bridgeDev, err)
	}
	return link.Attrs().HardwareAddr, nil
}

// =============================================================================
// Kernel routes via netlink (Linux only)
// =============================================================================

func (rm *RouteManager) AddKernelRoute(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would add kernel route", "ip", ip, "dev", rm.bridgeDev, "table", rm.routeTableID)
		return nil
	}
	link, err := netlink.LinkByName(rm.bridgeDev)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", rm.bridgeDev, err)
	}

	dst := &net.IPNet{
		IP:   net.ParseIP(ip),
		Mask: net.CIDRMask(32, 32),
	}
	if dst.IP == nil {
		return fmt.Errorf("invalid IP: %s", ip)
	}

	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dst,
		Scope:     netlink.SCOPE_LINK,
	}
	if rm.routeTableID > 0 {
		route.Table = rm.routeTableID
	}

	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("add kernel route %s/32 dev %s: %w", ip, rm.bridgeDev, err)
	}

	// Add ip rule when using a dedicated routing table.
	// If the rule fails, remove the route to avoid an orphaned route without a matching rule.
	if rm.routeTableID > 0 {
		if err := rm.ensureIPRule(dst); err != nil {
			_ = netlink.RouteDel(route)
			return fmt.Errorf("add ip rule for %s (route rolled back): %w", ip, err)
		}
	}

	slog.Info("kernel route ensured", "ip", ip, "dev", rm.bridgeDev, "table", rm.routeTableID)
	return nil
}

func (rm *RouteManager) DelKernelRoute(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove kernel route", "ip", ip, "dev", rm.bridgeDev)
		return nil
	}
	link, err := netlink.LinkByName(rm.bridgeDev)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", rm.bridgeDev, err)
	}

	dst := &net.IPNet{
		IP:   net.ParseIP(ip),
		Mask: net.CIDRMask(32, 32),
	}
	if dst.IP == nil {
		return fmt.Errorf("invalid IP: %s", ip)
	}

	// Remove ip rule first (stop steering traffic before removing route).
	if rm.routeTableID > 0 {
		rm.removeIPRule(dst)
	}

	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dst,
		Scope:     netlink.SCOPE_LINK,
	}
	if rm.routeTableID > 0 {
		route.Table = rm.routeTableID
	}

	if err := netlink.RouteDel(route); err != nil {
		if isNoSuchRoute(err) {
			slog.Debug("kernel route already absent", "ip", ip, "dev", rm.bridgeDev)
			return nil
		}
		return fmt.Errorf("del kernel route %s/32 dev %s: %w", ip, rm.bridgeDev, err)
	}

	slog.Info("kernel route removed", "ip", ip, "dev", rm.bridgeDev)
	return nil
}

// ListKernelRoutes returns all /32 routes on the bridge device.
// When a dedicated routing table is configured, only routes from that table are returned.
func (rm *RouteManager) ListKernelRoutes() ([]string, error) {
	if rm.dryRun {
		return nil, nil
	}
	link, err := netlink.LinkByName(rm.bridgeDev)
	if err != nil {
		return nil, fmt.Errorf("find bridge %s: %w", rm.bridgeDev, err)
	}

	var routes []netlink.Route
	if rm.routeTableID > 0 {
		filter := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Table:     rm.routeTableID,
		}
		routes, err = netlink.RouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
	} else {
		routes, err = netlink.RouteList(link, netlink.FAMILY_V4)
	}
	if err != nil {
		return nil, fmt.Errorf("list routes on %s: %w", rm.bridgeDev, err)
	}

	var ips []string
	for _, r := range routes {
		if r.Dst != nil {
			ones, _ := r.Dst.Mask.Size()
			if ones == 32 {
				ips = append(ips, r.Dst.IP.String())
			}
		}
	}
	return ips, nil
}

// =============================================================================
// IP rule helpers (policy routing)
// =============================================================================

// ensureIPRule adds an ip rule: "to <dst> lookup <table>" if not already present.
func (rm *RouteManager) ensureIPRule(dst *net.IPNet) error {
	rule := netlink.NewRule()
	rule.Dst = dst
	rule.Table = rm.routeTableID
	rule.Priority = 1000

	if err := netlink.RuleAdd(rule); err != nil {
		// Ignore "already exists".
		if !isFileExists(err) {
			return err
		}
	}
	return nil
}

// removeIPRule removes the ip rule for <dst>.
func (rm *RouteManager) removeIPRule(dst *net.IPNet) {
	rule := netlink.NewRule()
	rule.Dst = dst
	rule.Table = rm.routeTableID
	rule.Priority = 1000

	if err := netlink.RuleDel(rule); err != nil {
		slog.Debug("ip rule already absent or failed to remove", "dst", dst, "error", err)
	}
}

// CleanupRoutingTable removes all routes and ip rules from the dedicated routing table.
func (rm *RouteManager) CleanupRoutingTable() error {
	if rm.routeTableID == 0 {
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would flush routing table", "table", rm.routeTableID)
		return nil
	}

	// Remove all ip rules pointing to this table.
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list ip rules: %w", err)
	}
	for _, r := range rules {
		if r.Table == rm.routeTableID {
			if err := netlink.RuleDel(&r); err != nil {
				slog.Warn("failed to remove ip rule", "rule", r, "error", err)
			}
		}
	}

	// Remove all routes in the table.
	filter := &netlink.Route{Table: rm.routeTableID}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_TABLE)
	if err != nil {
		return fmt.Errorf("list routes in table %d: %w", rm.routeTableID, err)
	}
	for _, r := range routes {
		if err := netlink.RouteDel(&r); err != nil {
			slog.Warn("failed to remove route from table", "route", r, "error", err)
		}
	}

	slog.Info("routing table flushed", "table", rm.routeTableID)
	return nil
}

func isFileExists(err error) bool {
	return errors.Is(err, syscall.EEXIST)
}

// =============================================================================
// Veth VRF leak (replaces contrib/veth-vrf-leak.sh)
// =============================================================================

// vethPrefixLen is the prefix length for the veth link-local /30 subnet.
const vethPrefixLen = 30

// SetupVethLeak creates a veth pair for selective route leaking between the
// default VRF and the provider VRF.  The method is idempotent.
func (rm *RouteManager) SetupVethLeak() error {
	if !rm.vethLeakEnabled {
		slog.Debug("veth VRF leak disabled, skipping setup")
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would set up veth VRF leak",
			"veth_default", vethDefaultName,
			"veth_provider", vethProviderName,
			"nexthop", rm.vethNexthop,
			"provider_ip", rm.vethProviderIP,
			"table", rm.vethLeakTableID,
			"priority", rm.vethLeakRulePriority,
			"networks", rm.networkFilters,
		)
		return nil
	}

	nexthopIP := net.ParseIP(rm.vethNexthop)
	providerIP := net.ParseIP(rm.vethProviderIP)

	// 1. Create veth pair (or reuse existing)
	var vethDefault, vethProvider netlink.Link
	vethDefault, err := netlink.LinkByName(vethDefaultName)
	if err != nil {
		// Create new veth pair
		veth := &netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{Name: vethDefaultName},
			PeerName:  vethProviderName,
		}
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("create veth pair: %w", err)
		}
		vethDefault, err = netlink.LinkByName(vethDefaultName)
		if err != nil {
			return fmt.Errorf("find %s after creation: %w", vethDefaultName, err)
		}
		slog.Info("veth pair created", "default", vethDefaultName, "provider", vethProviderName)
	} else {
		slog.Debug("veth pair already exists, reusing")
	}

	vethProvider, err = netlink.LinkByName(vethProviderName)
	if err != nil {
		return fmt.Errorf("find %s: %w", vethProviderName, err)
	}

	// 2. Place veth-provider into VRF
	vrfLink, err := netlink.LinkByName(rm.vrfName)
	if err != nil {
		return fmt.Errorf("find VRF %s: %w", rm.vrfName, err)
	}
	if err := netlink.LinkSetMaster(vethProvider, vrfLink); err != nil {
		return fmt.Errorf("set %s master to %s: %w", vethProviderName, rm.vrfName, err)
	}

	// 3. Assign IPs (AddrReplace for idempotency)
	defaultAddr := &netlink.Addr{
		IPNet: &net.IPNet{IP: nexthopIP, Mask: net.CIDRMask(vethPrefixLen, 32)},
	}
	if err := netlink.AddrReplace(vethDefault, defaultAddr); err != nil {
		return fmt.Errorf("assign IP to %s: %w", vethDefaultName, err)
	}

	providerAddr := &netlink.Addr{
		IPNet: &net.IPNet{IP: providerIP, Mask: net.CIDRMask(vethPrefixLen, 32)},
	}
	if err := netlink.AddrReplace(vethProvider, providerAddr); err != nil {
		return fmt.Errorf("assign IP to %s: %w", vethProviderName, err)
	}

	// 4. Bring interfaces up
	if err := netlink.LinkSetUp(vethDefault); err != nil {
		return fmt.Errorf("bring up %s: %w", vethDefaultName, err)
	}
	if err := netlink.LinkSetUp(vethProvider); err != nil {
		return fmt.Errorf("bring up %s: %w", vethProviderName, err)
	}

	// 5. Static neighbor entries (ip neigh replace ... nud permanent)
	// Refetch links to get up-to-date hardware addresses after LinkSetUp.
	vethDefault, err = netlink.LinkByName(vethDefaultName)
	if err != nil {
		return fmt.Errorf("refetch %s: %w", vethDefaultName, err)
	}
	vethProvider, err = netlink.LinkByName(vethProviderName)
	if err != nil {
		return fmt.Errorf("refetch %s: %w", vethProviderName, err)
	}
	defaultMAC := vethDefault.Attrs().HardwareAddr
	providerMAC := vethProvider.Attrs().HardwareAddr

	if err := netlink.NeighSet(&netlink.Neigh{
		LinkIndex:    vethDefault.Attrs().Index,
		IP:           providerIP,
		HardwareAddr: providerMAC,
		State:        netlink.NUD_PERMANENT,
	}); err != nil {
		return fmt.Errorf("set neighbor on %s: %w", vethDefaultName, err)
	}

	if err := netlink.NeighSet(&netlink.Neigh{
		LinkIndex:    vethProvider.Attrs().Index,
		IP:           nexthopIP,
		HardwareAddr: defaultMAC,
		State:        netlink.NUD_PERMANENT,
	}); err != nil {
		return fmt.Errorf("set neighbor on %s: %w", vethProviderName, err)
	}

	// 6. Default route in leak table: default via <provider_ip> dev veth-default table <leak_table>
	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: vethDefault.Attrs().Index,
		Gw:        providerIP,
		Table:     rm.vethLeakTableID,
	}); err != nil {
		return fmt.Errorf("add default route in table %d: %w", rm.vethLeakTableID, err)
	}

	// Per-network routes and policy rules are managed dynamically by
	// ReconcileVethLeakNetworks() during each reconciliation cycle.
	// If static network_cidr is configured, set up initial per-network
	// routes now for backwards compatibility.
	if len(rm.networkFilters) > 0 {
		if err := rm.ReconcileVethLeakNetworks(rm.networkFilters); err != nil {
			return fmt.Errorf("initial veth leak network setup: %w", err)
		}
	}

	slog.Info("veth VRF leak setup complete",
		"nexthop", rm.vethNexthop,
		"provider_ip", rm.vethProviderIP,
		"table", rm.vethLeakTableID,
	)
	return nil
}

// TeardownVethLeak removes all veth leak resources. Errors on missing resources
// are silently ignored so the method is safe to call even if setup was partial.
func (rm *RouteManager) TeardownVethLeak() error {
	if !rm.vethLeakEnabled {
		slog.Debug("veth VRF leak disabled, skipping teardown")
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would tear down veth VRF leak")
		return nil
	}

	providerIP := net.ParseIP(rm.vethProviderIP)

	// Steps 1-3 explicitly remove rules/routes before step 4 deletes the veth pair.
	// The kernel would garbage-collect connected routes on link deletion, but explicit
	// cleanup ensures policy rules are removed and gives clear log output on errors.

	// 1. Remove ALL policy rules pointing to our leak table with our priority
	// (covers both statically configured and dynamically discovered networks).
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err == nil {
		for _, r := range rules {
			if r.Table == rm.vethLeakTableID && r.Priority == rm.vethLeakRulePriority {
				if err := netlink.RuleDel(&r); err != nil {
					slog.Warn("failed to remove veth leak policy rule", "src", r.Src, "error", err)
				} else {
					slog.Debug("removed veth leak policy rule", "src", r.Src)
				}
			}
		}
	}

	// 2. Remove default route from leak table
	vethDefault, err := netlink.LinkByName(vethDefaultName)
	if err == nil {
		if err := netlink.RouteDel(&netlink.Route{
			LinkIndex: vethDefault.Attrs().Index,
			Gw:        providerIP,
			Table:     rm.vethLeakTableID,
		}); err != nil {
			if isNoSuchRoute(err) {
				slog.Debug("veth leak default route already absent")
			} else {
				slog.Warn("failed to remove veth leak default route", "error", err)
			}
		}
	}

	// 3. Remove ALL per-network routes from VRF via veth-provider
	// (query the system instead of relying on tracked state).
	vethProvider, vrfErr := netlink.LinkByName(vethProviderName)
	if vrfErr == nil {
		vrfTableID, err := rm.getVRFTableID()
		if err == nil {
			filter := &netlink.Route{
				LinkIndex: vethProvider.Attrs().Index,
				Table:     vrfTableID,
			}
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
			if err == nil {
				for _, r := range routes {
					if err := netlink.RouteDel(&r); err != nil {
						if isNoSuchRoute(err) {
							slog.Debug("veth leak VRF route already absent", "dst", r.Dst)
						} else {
							slog.Warn("failed to remove veth leak VRF route", "dst", r.Dst, "error", err)
						}
					} else {
						slog.Debug("removed veth leak VRF route", "dst", r.Dst)
					}
				}
			}
		}
	}

	// 4. Delete veth pair (deleting one end removes both)
	if vethDefault, err := netlink.LinkByName(vethDefaultName); err == nil {
		if err := netlink.LinkDel(vethDefault); err != nil {
			return fmt.Errorf("delete %s: %w", vethDefaultName, err)
		}
	}

	slog.Info("veth VRF leak teardown complete")
	return nil
}

// ReconcileVethLeakNetworks ensures per-network VRF routes and policy rules
// match the desired set of networks. Pass nil to remove all per-network state.
func (rm *RouteManager) ReconcileVethLeakNetworks(desired []*net.IPNet) error {
	if !rm.vethLeakEnabled {
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would reconcile veth leak networks", "desired", len(desired))
		return nil
	}

	nexthopIP := net.ParseIP(rm.vethNexthop)

	vethProvider, err := netlink.LinkByName(vethProviderName)
	if err != nil {
		return fmt.Errorf("find %s: %w", vethProviderName, err)
	}

	vrfTableID, err := rm.getVRFTableID()
	if err != nil {
		return fmt.Errorf("get VRF table ID: %w", err)
	}

	// Discover current per-network VRF routes via veth-provider.
	// We filter by our custom protocol (rtProtoOVNNetworkAgent) to only find
	// routes we created, ignoring routes installed by FRR (which use
	// RTPROT_ZEBRA or RTPROT_STATIC) that share the same interface and gateway.
	filter := &netlink.Route{
		LinkIndex: vethProvider.Attrs().Index,
		Table:     vrfTableID,
		Protocol:  rtProtoOVNNetworkAgent,
	}
	currentRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE|netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return fmt.Errorf("list VRF routes: %w", err)
	}

	currentNets := make(map[string]bool, len(currentRoutes))
	for _, r := range currentRoutes {
		// Defense-in-depth: also verify protocol in Go code in case the
		// netlink RT_FILTER_PROTOCOL flag does not filter correctly on
		// all kernel versions. Without this check, FRR-installed /32
		// routes (RTPROT_ZEBRA) sharing the same interface and gateway
		// could leak into currentNets and be treated as stale.
		if r.Dst != nil && r.Gw != nil && r.Gw.Equal(nexthopIP) && r.Protocol == rtProtoOVNNetworkAgent {
			currentNets[r.Dst.String()] = true
		}
	}

	desiredSet := make(map[string]*net.IPNet, len(desired))
	for _, n := range desired {
		desiredSet[n.String()] = n
	}

	// Add missing per-network routes and rules.
	for key, network := range desiredSet {
		if currentNets[key] {
			continue
		}
		if err := netlink.RouteReplace(&netlink.Route{
			LinkIndex: vethProvider.Attrs().Index,
			Dst:       network,
			Gw:        nexthopIP,
			Table:     vrfTableID,
			Protocol:  rtProtoOVNNetworkAgent,
		}); err != nil {
			return fmt.Errorf("add VRF route for %s: %w", network, err)
		}

		rule := netlink.NewRule()
		rule.Src = network
		rule.Table = rm.vethLeakTableID
		rule.Priority = rm.vethLeakRulePriority
		if err := netlink.RuleAdd(rule); err != nil {
			if !isFileExists(err) {
				return fmt.Errorf("add policy rule for %s: %w", network, err)
			}
		}
		slog.Info("veth leak network added", "network", network)
	}

	// Remove stale per-network routes and rules.
	for key := range currentNets {
		if _, wanted := desiredSet[key]; wanted {
			continue
		}
		_, staleNet, err := net.ParseCIDR(key)
		if err != nil {
			continue
		}
		if err := netlink.RouteDel(&netlink.Route{
			LinkIndex: vethProvider.Attrs().Index,
			Dst:       staleNet,
			Gw:        nexthopIP,
			Table:     vrfTableID,
			Protocol:  rtProtoOVNNetworkAgent,
		}); err != nil {
			if !isNoSuchRoute(err) {
				slog.Warn("failed to remove stale VRF route", "network", key, "error", err)
			}
		}
		slog.Info("veth leak network removed", "network", key)
	}

	// Reconcile policy rules independently — catches orphaned rules where
	// the route was removed externally but the rule remained.
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		slog.Warn("failed to list policy rules for reconciliation", "error", err)
	} else {
		for _, r := range rules {
			if r.Table != rm.vethLeakTableID || r.Priority != rm.vethLeakRulePriority || r.Src == nil {
				continue
			}
			if _, wanted := desiredSet[r.Src.String()]; !wanted {
				if err := netlink.RuleDel(&r); err != nil {
					if !isNoSuchRule(err) {
						slog.Warn("failed to remove orphaned policy rule", "src", r.Src, "error", err)
					}
				} else {
					slog.Info("orphaned veth leak policy rule removed", "src", r.Src)
				}
			}
		}
	}

	return nil
}

// getVRFTableID returns the routing table ID associated with the VRF.
func (rm *RouteManager) getVRFTableID() (int, error) {
	link, err := netlink.LinkByName(rm.vrfName)
	if err != nil {
		return 0, fmt.Errorf("find VRF %s: %w", rm.vrfName, err)
	}
	vrf, ok := link.(*netlink.Vrf)
	if !ok {
		return 0, fmt.Errorf("%s is not a VRF device", rm.vrfName)
	}
	return int(vrf.Table), nil
}
