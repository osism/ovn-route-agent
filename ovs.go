package main

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

const ovsCookieMACTweak = "0x999"

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
	delCmd := rm.ovsCmd("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieMACTweak))
	if out, err := delCmd.CombinedOutput(); err != nil {
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

// RemoveOVSFlows removes all agent-managed OVS flows from the bridge device.
func (rm *RouteManager) RemoveOVSFlows() error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove OVS MAC-tweak flows", "dev", rm.bridgeDev)
		return nil
	}
	cmd := rm.ovsCmd("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieMACTweak))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("del OVS flows on %s: %w (output: %s)", rm.bridgeDev, err, strings.TrimSpace(string(out)))
	}
	slog.Info("OVS MAC-tweak flows removed", "dev", rm.bridgeDev)
	return nil
}

// discoverPatchPort finds the patch-type port on the bridge device that
// connects to OVN's integration bridge.
func (rm *RouteManager) discoverPatchPort() (string, error) {
	listCmd := rm.ovsCmd("ovs-vsctl", "list-ports", rm.bridgeDev)
	out, err := listCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("list-ports %s: %w (output: %s)", rm.bridgeDev, err, strings.TrimSpace(string(out)))
	}

	ports := strings.Fields(strings.TrimSpace(string(out)))
	for _, port := range ports {
		typeCmd := rm.ovsCmd("ovs-vsctl", "--if-exists", "get", "Interface", port, "type")
		typeOut, err := typeCmd.CombinedOutput()
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
	cmd := rm.ovsCmd("ovs-vsctl", "get", "Interface", port, "ofport")
	out, err := cmd.CombinedOutput()
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
	cmd := rm.ovsCmd("ovs-ofctl", "add-flow", rm.bridgeDev, flow)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ovs-ofctl add-flow %s %q: %w (output: %s)", rm.bridgeDev, flow, err, strings.TrimSpace(string(out)))
	}
	return nil
}
