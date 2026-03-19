package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// RouteManager handles kernel routes on the provider bridge and FRR static routes.
type RouteManager struct {
	bridgeDev    string
	vrfName      string
	vethNexthop  string
	routeTableID int
	ovsWrapper   []string // prefix args for ovs-vsctl/ovs-ofctl (e.g. ["docker", "exec", "openvswitch_vswitchd"])
	dryRun       bool

	// Veth VRF leak settings
	vethLeakEnabled      bool
	vethProviderIP       string
	vethLeakTableID      int
	vethLeakRulePriority int
	networkFilters       []*net.IPNet // from manual config (may be empty for auto-discovery)

	// FRR prefix-list management
	frrPrefixList string

	// Cached OVS discovery results (populated on first use).
	cachedPatchPort string
	cachedOfport    string
	cachedBridgeMAC string
}

func NewRouteManager(cfg Config) *RouteManager {
	rm := &RouteManager{
		bridgeDev:            cfg.BridgeDev,
		vrfName:              cfg.VRFName,
		vethNexthop:          cfg.VethNexthop,
		routeTableID:         cfg.RouteTableID,
		dryRun:               cfg.DryRun,
		vethLeakEnabled:      cfg.VethLeakEnabled,
		vethProviderIP:       cfg.VethProviderIP,
		vethLeakTableID:      cfg.VethLeakTableID,
		vethLeakRulePriority: cfg.VethLeakRulePriority,
		networkFilters:       cfg.NetworkFilters,
		frrPrefixList:        cfg.FRRPrefixList,
	}
	if cfg.OVSWrapper != "" {
		rm.ovsWrapper = strings.Fields(cfg.OVSWrapper)
	}
	return rm
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
// FRR prefix-list management
// =============================================================================

// prefixListEntry represents a single entry in an FRR ip prefix-list.
type prefixListEntry struct {
	Seq     int
	Network string // e.g. "198.51.100.0/24"
}

// ListFRRPrefixListEntries returns the current "permit ... ge 32 le 32" entries
// in the configured FRR prefix-list. Returns nil if no prefix-list is configured.
//
// Safety: frrPrefixList is validated by isValidIdentifier (alphanumeric, hyphen,
// underscore, dot) in config validation. Network strings come from net.IPNet.String().
func (rm *RouteManager) ListFRRPrefixListEntries() ([]prefixListEntry, error) {
	if rm.frrPrefixList == "" {
		return nil, nil
	}
	cmd := exec.Command("vtysh",
		"-c", fmt.Sprintf("show ip prefix-list %s", rm.frrPrefixList),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("vtysh show prefix-list %s: %w (output: %s)", rm.frrPrefixList, err, strings.TrimSpace(string(output)))
	}

	outStr := string(output)
	if strings.Contains(outStr, "Can't find") || strings.TrimSpace(outStr) == "" {
		return nil, nil
	}

	var entries []prefixListEntry
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		// Match: seq <N> permit <network> ge 32 le 32
		if len(fields) >= 8 && fields[0] == "seq" && fields[2] == "permit" &&
			fields[4] == "ge" && fields[5] == "32" && fields[6] == "le" && fields[7] == "32" {
			seq, serr := strconv.Atoi(fields[1])
			if serr != nil {
				continue
			}
			entries = append(entries, prefixListEntry{Seq: seq, Network: fields[3]})
		}
	}
	return entries, nil
}

// ReconcileFRRPrefixList ensures the managed prefix-list contains exactly one
// "permit <network> ge 32 le 32" entry per desired network.
// Pass nil to remove all managed entries (cleanup).
func (rm *RouteManager) ReconcileFRRPrefixList(networks []*net.IPNet) error {
	if rm.frrPrefixList == "" {
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would reconcile FRR prefix-list", "name", rm.frrPrefixList, "networks", len(networks))
		return nil
	}

	current, err := rm.ListFRRPrefixListEntries()
	if err != nil {
		return fmt.Errorf("list prefix-list entries: %w", err)
	}

	// Build current and desired maps.
	currentByNetwork := make(map[string]int, len(current)) // network → seq
	maxSeq := 0
	for _, e := range current {
		currentByNetwork[e.Network] = e.Seq
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}

	desired := make(map[string]bool, len(networks))
	for _, n := range networks {
		desired[n.String()] = true
	}

	// Add missing entries (before removing stale ones, to avoid a window with no entries).
	for network := range desired {
		if _, exists := currentByNetwork[network]; !exists {
			maxSeq += 5
			cmd := exec.Command("vtysh",
				"-c", "conf t",
				"-c", fmt.Sprintf("ip prefix-list %s seq %d permit %s ge 32 le 32", rm.frrPrefixList, maxSeq, network),
				"-c", "end",
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("add prefix-list entry %s seq %d: %w (output: %s)", network, maxSeq, err, strings.TrimSpace(string(output)))
			}
			slog.Info("FRR prefix-list entry added", "name", rm.frrPrefixList, "network", network, "seq", maxSeq)
		}
	}

	// Remove stale entries.
	for network, seq := range currentByNetwork {
		if !desired[network] {
			cmd := exec.Command("vtysh",
				"-c", "conf t",
				"-c", fmt.Sprintf("no ip prefix-list %s seq %d permit %s ge 32 le 32", rm.frrPrefixList, seq, network),
				"-c", "end",
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("remove prefix-list entry %s seq %d: %w (output: %s)", network, seq, err, strings.TrimSpace(string(output)))
			}
			slog.Info("FRR prefix-list entry removed", "name", rm.frrPrefixList, "network", network, "seq", seq)
		}
	}

	return nil
}

// =============================================================================
// Helpers
// =============================================================================

func isNoSuchRoute(err error) bool {
	return strings.Contains(err.Error(), "no such process")
}

func isNoSuchRule(err error) bool {
	return strings.Contains(err.Error(), "no such file or directory")
}
