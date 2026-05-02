package main

import (
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strings"
)

const (
	ovsCookieMACTweak = "0x999"
	ovsCookieHairpin  = "0x998"
)

// MACTweakFlow returns the OpenFlow rule string for a MAC-tweak flow.
func MACTweakFlow(cookie, ofport, mac string, ipv6 bool) string {
	proto := "ip"
	if ipv6 {
		proto = "ipv6"
	}
	return fmt.Sprintf("cookie=%s,priority=900,%s,in_port=%s,actions=mod_dl_dst:%s,NORMAL",
		cookie, proto, ofport, mac)
}

// ovsCmd builds an exec.Cmd for an OVS command, prepending the configured
// wrapper (e.g. "docker exec openvswitch_vswitchd") if set.
func (rm *RouteManager) ovsCmd(binary string, args ...string) *exec.Cmd {
	if len(rm.ovsWrapper) > 0 {
		fullArgs := append(rm.ovsWrapper[1:], binary)
		fullArgs = append(fullArgs, args...)
		return exec.Command(rm.ovsWrapper[0], fullArgs...)
	}
	return exec.Command(binary, args...)
}

// runOVS builds and runs an OVS command. When execOVSHook is set (tests) the
// command is dispatched through it instead of being executed.
func (rm *RouteManager) runOVS(binary string, args ...string) ([]byte, error) {
	cmd := rm.ovsCmd(binary, args...)
	if rm.execOVSHook != nil {
		return rm.execOVSHook(cmd)
	}
	return cmd.CombinedOutput()
}

// EnsureOVSFlows installs MAC-tweak flows on the bridge device.
// These flows rewrite the destination MAC of packets arriving from OVN's
// integration bridge (via the patch-provnet port) to the bridge device's own
// MAC, so the kernel can properly receive and route them.
//
// Discovery results are cached after the first successful call.
func (rm *RouteManager) EnsureOVSFlows() error {
	if rm.dryRun {
		slog.Info("[dry-run] would ensure OVS MAC-tweak flows", "dev", rm.bridgeDev)
		return nil
	}

	// Use cached values if available; discover on first call.
	if rm.cachedPatchPort == "" {
		patchPort, err := rm.discoverPatchPort()
		if err != nil {
			return fmt.Errorf("discover patch port on %s: %w", rm.bridgeDev, err)
		}
		ofport, err := rm.getOFPort(patchPort)
		if err != nil {
			return fmt.Errorf("get ofport for %s: %w", patchPort, err)
		}
		mac, err := rm.GetBridgeMAC()
		if err != nil {
			return fmt.Errorf("get bridge MAC: %w", err)
		}
		rm.cachedPatchPort = patchPort
		rm.cachedOfport = ofport
		rm.cachedBridgeMAC = mac.String()
		slog.Info("OVS discovery complete", "patch_port", patchPort, "ofport", ofport, "mac", rm.cachedBridgeMAC)
	}

	// Delete existing agent-managed flows (idempotent replace).
	if out, err := rm.runOVS("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieMACTweak)); err != nil {
		slog.Warn("failed to delete old OVS flows", "error", err, "output", strings.TrimSpace(string(out)))
	}

	// Add IPv4 and IPv6 MAC-tweak flows.
	ipv4Flow := MACTweakFlow(ovsCookieMACTweak, rm.cachedOfport, rm.cachedBridgeMAC, false)
	if err := rm.addOVSFlow(ipv4Flow); err != nil {
		return fmt.Errorf("add IPv4 MAC-tweak flow: %w", err)
	}

	ipv6Flow := MACTweakFlow(ovsCookieMACTweak, rm.cachedOfport, rm.cachedBridgeMAC, true)
	if err := rm.addOVSFlow(ipv6Flow); err != nil {
		return fmt.Errorf("add IPv6 MAC-tweak flow: %w", err)
	}

	slog.Debug("OVS MAC-tweak flows ensured", "dev", rm.bridgeDev)
	return nil
}

// HairpinFlow returns the OpenFlow rule string for a same-chassis hairpin flow.
// The flow intercepts packets from OVN (via the patch port) destined for a
// locally-managed IP and sends them back through the same patch port using
// output:in_port. OVN then processes the packet as incoming on the external
// logical switch, allowing correct DNAT/ICMP handling without leaving the host.
//
// Both source and destination MACs are rewritten:
//   - dl_src is set to the bridge device's own MAC (bridgeMAC) so the reflected
//     packet appears as external traffic to OVN, avoiding loop detection.
//   - dl_dst is set to the owning router port's MAC (routerMAC) so OVN's L2
//     lookup on the external logical switch delivers the packet to the correct
//     router. Without this, the original dl_dst may be unresolved (e.g.
//     00:00:00:00:00:00) when OVN's ARP resolution between co-located routers
//     has not completed.
//
// Priority 910 ensures hairpin fires before the MAC-tweak flow (priority 900),
// so locally-managed IPs are reflected into OVN while all other traffic
// (destined for remote IPs) still falls through to MAC-tweak and exits to the
// physical network normally.
func HairpinFlow(cookie, ofport, ip, bridgeMAC, routerMAC string, ipv6 bool) string {
	if ipv6 {
		return fmt.Sprintf("cookie=%s,priority=910,ipv6,in_port=%s,ipv6_dst=%s/128,actions=mod_dl_src:%s,mod_dl_dst:%s,output:in_port",
			cookie, ofport, ip, bridgeMAC, routerMAC)
	}
	return fmt.Sprintf("cookie=%s,priority=910,ip,in_port=%s,ip_dst=%s/32,actions=mod_dl_src:%s,mod_dl_dst:%s,output:in_port",
		cookie, ofport, ip, bridgeMAC, routerMAC)
}

// ReconcileOVSHairpinFlows installs per-IP hairpin flows on the bridge device.
//
// Without these flows, same-chassis traffic between FIPs on different OVN
// routers is mishandled: OVN sends it via the localnet port to br-ex, the
// MAC-tweak flow delivers it to the kernel, but the kernel has no "local"
// address for the destination FIP and either drops or loops the packet.
// From a different chassis the same traffic arrives via the physical network
// and OVN processes it correctly — explaining the asymmetric failure.
//
// ipToRouterMAC maps each IP to the MAC of the router port that owns it.
// The MAC is written into the flow as mod_dl_dst so that OVN's L2 lookup
// delivers the reflected packet to the correct router port.
//
// EnsureOVSFlows must be called before this method so that cachedOfport is
// populated. If cachedOfport is empty this method is a no-op.
//
// Pass nil or an empty map to remove all hairpin flows (e.g. when no
// locally-active routers remain).
func (rm *RouteManager) ReconcileOVSHairpinFlows(ipToRouterMAC map[string]string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would reconcile OVS hairpin flows", "count", len(ipToRouterMAC))
		return nil
	}
	if rm.cachedOfport == "" {
		// Patch port not yet discovered; EnsureOVSFlows must run first.
		slog.Warn("skipping OVS hairpin flow reconcile: patch port ofport not yet cached")
		return nil
	}

	// Full replace: delete all current hairpin flows then reinstall.
	// The replacement window is sub-millisecond and tolerable.
	if out, err := rm.runOVS("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieHairpin)); err != nil {
		return fmt.Errorf("del hairpin OVS flows on %s: %w (output: %s)", rm.bridgeDev, err, strings.TrimSpace(string(out)))
	}

	for ip, routerMAC := range ipToRouterMAC {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return fmt.Errorf("invalid IP %q", ip)
		}
		isIPv6 := parsed.To4() == nil
		flow := HairpinFlow(ovsCookieHairpin, rm.cachedOfport, ip, rm.cachedBridgeMAC, routerMAC, isIPv6)
		if err := rm.addOVSFlow(flow); err != nil {
			return fmt.Errorf("add hairpin flow for %s: %w", ip, err)
		}
	}

	slog.Debug("OVS hairpin flows reconciled", "count", len(ipToRouterMAC))
	return nil
}

// RemoveOVSFlows removes all agent-managed OVS flows from the bridge device.
func (rm *RouteManager) RemoveOVSFlows() error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove OVS MAC-tweak flows", "dev", rm.bridgeDev)
		return nil
	}
	out, err := rm.runOVS("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieMACTweak))
	if err != nil {
		return fmt.Errorf("del OVS flows on %s: %w (output: %s)", rm.bridgeDev, err, strings.TrimSpace(string(out)))
	}
	slog.Info("OVS MAC-tweak flows removed", "dev", rm.bridgeDev)

	hout, herr := rm.runOVS("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieHairpin))
	if herr != nil {
		return fmt.Errorf("del hairpin OVS flows on %s: %w (output: %s)", rm.bridgeDev, herr, strings.TrimSpace(string(hout)))
	}
	slog.Info("OVS hairpin flows removed", "dev", rm.bridgeDev)

	return nil
}

// discoverPatchPort finds the patch-type port on the bridge device that
// connects to OVN's integration bridge.
func (rm *RouteManager) discoverPatchPort() (string, error) {
	out, err := rm.runOVS("ovs-vsctl", "list-ports", rm.bridgeDev)
	if err != nil {
		return "", fmt.Errorf("list-ports %s: %w (output: %s)", rm.bridgeDev, err, strings.TrimSpace(string(out)))
	}

	ports := strings.Fields(strings.TrimSpace(string(out)))
	for _, port := range ports {
		typeOut, err := rm.runOVS("ovs-vsctl", "--if-exists", "get", "Interface", port, "type")
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(typeOut)) == "patch" {
			return port, nil
		}
	}

	return "", fmt.Errorf("no patch port found on bridge %s", rm.bridgeDev)
}

// getOFPort returns the OpenFlow port number for an OVS port.
func (rm *RouteManager) getOFPort(port string) (string, error) {
	out, err := rm.runOVS("ovs-vsctl", "get", "Interface", port, "ofport")
	if err != nil {
		return "", fmt.Errorf("get ofport for %s: %w (output: %s)", port, err, strings.TrimSpace(string(out)))
	}
	ofport := strings.TrimSpace(string(out))
	if ofport == "" || ofport == "-1" {
		return "", fmt.Errorf("invalid ofport %q for %s", ofport, port)
	}
	return ofport, nil
}

func (rm *RouteManager) addOVSFlow(flow string) error {
	out, err := rm.runOVS("ovs-ofctl", "add-flow", rm.bridgeDev, flow)
	if err != nil {
		return fmt.Errorf("ovs-ofctl add-flow %s %q: %w (output: %s)", rm.bridgeDev, flow, err, strings.TrimSpace(string(out)))
	}
	return nil
}
