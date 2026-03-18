package main

import (
	"fmt"
	"log/slog"
	"net"

	"github.com/vishvananda/netlink"
)

// CheckBridgeDevice verifies that the bridge device exists and is up.
func (rm *RouteManager) CheckBridgeDevice() error {
	if rm.dryRun {
		slog.Info("[dry-run] skipping bridge device check", "dev", rm.bridgeDev)
		return nil
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

// =============================================================================
// Kernel routes via netlink (Linux only)
// =============================================================================

func (rm *RouteManager) AddKernelRoute(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would add kernel route", "ip", ip, "dev", rm.bridgeDev)
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

	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("add kernel route %s/32 dev %s: %w", ip, rm.bridgeDev, err)
	}

	slog.Info("kernel route ensured", "ip", ip, "dev", rm.bridgeDev)
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

	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dst,
		Scope:     netlink.SCOPE_LINK,
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
func (rm *RouteManager) ListKernelRoutes() ([]string, error) {
	link, err := netlink.LinkByName(rm.bridgeDev)
	if err != nil {
		return nil, fmt.Errorf("find bridge %s: %w", rm.bridgeDev, err)
	}

	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
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
