package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strings"
)

// RouteManager handles kernel routes on the provider bridge and FRR static routes.
type RouteManager struct {
	bridgeDev   string
	vrfName     string
	vethNexthop string
	dryRun      bool
}

func NewRouteManager(cfg Config) *RouteManager {
	return &RouteManager{
		bridgeDev:   cfg.BridgeDev,
		vrfName:     cfg.VRFName,
		vethNexthop: cfg.VethNexthop,
		dryRun:      cfg.DryRun,
	}
}

// validateIP checks that the given string is a valid IPv4 address.
func validateIP(ip string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid IP address: %q", ip)
	}
	return nil
}

// =============================================================================
// FRR routes via vtysh
// =============================================================================

func (rm *RouteManager) AddFRRRoute(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would add FRR route", "ip", ip, "vrf", rm.vrfName, "nexthop", rm.vethNexthop)
		return nil
	}
	cmd := exec.Command("vtysh",
		"-c", "conf t",
		"-c", fmt.Sprintf("vrf %s", rm.vrfName),
		"-c", fmt.Sprintf("ip route %s/32 %s", ip, rm.vethNexthop),
		"-c", "exit-vrf",
		"-c", "end",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("vtysh add route %s: %w (output: %s)", ip, err, strings.TrimSpace(string(output)))
	}
	slog.Info("FRR route ensured", "ip", ip, "vrf", rm.vrfName, "nexthop", rm.vethNexthop, "output", strings.TrimSpace(string(output)))
	return nil
}

func (rm *RouteManager) DelFRRRoute(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove FRR route", "ip", ip, "vrf", rm.vrfName, "nexthop", rm.vethNexthop)
		return nil
	}
	cmd := exec.Command("vtysh",
		"-c", "conf t",
		"-c", fmt.Sprintf("vrf %s", rm.vrfName),
		"-c", fmt.Sprintf("no ip route %s/32 %s", ip, rm.vethNexthop),
		"-c", "exit-vrf",
		"-c", "end",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("vtysh del route %s: %w (output: %s)", ip, err, strings.TrimSpace(string(output)))
	}
	slog.Info("FRR route removed", "ip", ip, "vrf", rm.vrfName, "output", strings.TrimSpace(string(output)))
	return nil
}

// HasFRRRoute checks if a static route for the IP exists in the VRF.
func (rm *RouteManager) HasFRRRoute(ip string) bool {
	if err := validateIP(ip); err != nil {
		return false
	}
	cmd := exec.Command("vtysh",
		"-c", fmt.Sprintf("show ip route vrf %s %s/32", rm.vrfName, ip),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "static")
}

// ListFRRRoutes returns all static /32 routes in the VRF.
func (rm *RouteManager) ListFRRRoutes() ([]string, error) {
	cmd := exec.Command("vtysh",
		"-c", fmt.Sprintf("show ip route vrf %s static", rm.vrfName),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("vtysh list routes: %w", err)
	}

	var ips []string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		// Lines like: S>* 198.51.100.10/32 [1/0] via 169.254.0.1, ...
		if strings.HasPrefix(line, "S") && strings.Contains(line, "/32") {
			parts := strings.Fields(line)
			for _, p := range parts {
				if strings.Contains(p, "/32") {
					ip, _, _ := net.ParseCIDR(p)
					if ip != nil {
						ips = append(ips, ip.String())
					}
					break
				}
			}
		}
	}
	return ips, nil
}

// =============================================================================
// Combined operations
// =============================================================================

// EnsureRoute adds both kernel and FRR routes for an IP.
func (rm *RouteManager) EnsureRoute(ip string) error {
	if err := validateIP(ip); err != nil {
		return err
	}
	if err := rm.AddKernelRoute(ip); err != nil {
		return fmt.Errorf("kernel route: %w", err)
	}
	if err := rm.AddFRRRoute(ip); err != nil {
		return fmt.Errorf("FRR route: %w", err)
	}
	return nil
}

// RemoveRoute removes both FRR and kernel routes for an IP.
// FRR is removed first to stop attracting traffic before tearing down the data plane.
func (rm *RouteManager) RemoveRoute(ip string) error {
	if err := validateIP(ip); err != nil {
		return err
	}
	ferr := rm.DelFRRRoute(ip)
	kerr := rm.DelKernelRoute(ip)
	return errors.Join(ferr, kerr)
}

// =============================================================================
// Helpers
// =============================================================================

func isNoSuchRoute(err error) bool {
	return strings.Contains(err.Error(), "no such process")
}
