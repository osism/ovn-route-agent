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

// CheckBridgeDevice verifies that the bridge device exists, is up, and that the
// agent has sufficient privileges (root or CAP_NET_ADMIN) for route management.
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
		return fmt.Errorf("bridge device %s exists but is not up", rm.bridgeDev)
	}
	slog.Info("bridge device is up", "dev", rm.bridgeDev)
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
