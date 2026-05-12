//go:build integration

package testenv

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// PortForwardLoopbackDev is the device name the agent's port_forward_dev
// defaults to, and which setup.sh provisions inside vrf-provider.
const PortForwardLoopbackDev = "loopback1"

// Sysctl paths the agent's port-forward feature mutates on a global basis
// (per-device sysctls go away with the veth pair so we don't track them
// here). Tests that flip these must save/restore via SaveSysctl.
const (
	SysctlUDPL3mdevAccept = "/proc/sys/net/ipv4/udp_l3mdev_accept"
	SysctlTCPL3mdevAccept = "/proc/sys/net/ipv4/tcp_l3mdev_accept"
)

// Port-forward fwmark / priority / table constants mirrored from the agent's
// nftables.go (dnatFwmark, dnatReplyFwmark, dnatFwmarkPriority,
// dnatReplyPriority) and config.go (PortForwardTableID default of 201).
//
// The duplication is deliberate: the integration package builds with
// `//go:build integration` and cannot import from `main`, so any test that
// wants to assert on the agent's policy-route plumbing has to know these
// values out-of-band. Keep them in lock step with the agent's source — a
// change to either side without the other will silently break scenario
// tests' policy-route / fwmark assertions.
const (
	// DefaultDnatFwmark is the fwmark the agent stamps on original-direction
	// DNAT'd packets. Steered to the main table via the ip rule at
	// DefaultDnatFwmarkPriority.
	DefaultDnatFwmark = 0x100

	// DefaultDnatReplyFwmark is the fwmark the agent stamps on
	// reply-direction DNAT'd packets. Steered to the port-forward routing
	// table (DefaultPortForwardTableID) via the ip rule at
	// DefaultDnatReplyPriority.
	DefaultDnatReplyFwmark = 0x200

	// DefaultDnatFwmarkPriority is the priority of the ip rule that matches
	// DefaultDnatFwmark and looks up the main table. Must be < 1000 to win
	// against the l3mdev VRF rule.
	DefaultDnatFwmarkPriority = 150

	// DefaultDnatReplyPriority is the priority of the ip rule that matches
	// DefaultDnatReplyFwmark and looks up the port-forward routing table.
	DefaultDnatReplyPriority = 151

	// DefaultPortForwardTableID is the routing table the agent uses for
	// DNAT reply traffic by default. Matches config.PortForwardTableID's
	// default of 201.
	DefaultPortForwardTableID = 201
)

// BridgeProxyARPPath returns the per-device proxy_arp sysctl path the agent
// flips on startup via EnableProxyARP. Used by bridge-IP lifecycle scenarios
// (#63) that wrap the path in SaveSysctl so the host is left as it was
// regardless of how the agent currently treats restore-on-shutdown.
func BridgeProxyARPPath(bridge string) string {
	return "/proc/sys/net/ipv4/conf/" + bridge + "/proxy_arp"
}

// EnsureLoopback1 checks that the loopback1 dummy device exists and is
// enslaved to vrf-provider. It does *not* create the device — that is
// setup.sh's job and a missing device means the host was not bootstrapped.
// Tests skip rather than fail in that case, mirroring how Setup() handles
// missing prerequisites.
func EnsureLoopback1(t *testing.T) {
	t.Helper()
	if _, err := net.InterfaceByName(PortForwardLoopbackDev); err != nil {
		t.Skipf("device %s not present (run test/integration/setup.sh first): %v",
			PortForwardLoopbackDev, err)
	}
	// Best-effort verify it is enslaved to vrf-provider; not all kernels
	// expose this in /proc, so a failure here is informational only.
	out, err := exec.Command("ip", "-d", "link", "show", PortForwardLoopbackDev).CombinedOutput()
	if err == nil && !strings.Contains(string(out), "vrf-provider") {
		t.Logf("warning: %s does not appear to be in vrf-provider (output: %s)",
			PortForwardLoopbackDev, strings.TrimSpace(string(out)))
	}
}

// AssertVIPOnLoopback fails the test if `ip -j addr show dev loopback1` does
// not list vip/32 within timeout.
func AssertVIPOnLoopback(t *testing.T, vip string, timeout time.Duration) {
	t.Helper()
	if net.ParseIP(vip) == nil {
		t.Fatalf("AssertVIPOnLoopback: invalid IP %q", vip)
	}
	deadline := time.Now().Add(timeout)
	for {
		if vipPresentOnLoopback(t, vip) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VIP %s/32 not present on %s after %s",
				vip, PortForwardLoopbackDev, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertVIPNotOnLoopback fails the test if vip persists on loopback1 past
// timeout. Used to verify manage_vip:false leaves the device alone and that
// teardown removes managed VIPs.
func AssertVIPNotOnLoopback(t *testing.T, vip string, timeout time.Duration) {
	t.Helper()
	if net.ParseIP(vip) == nil {
		t.Fatalf("AssertVIPNotOnLoopback: invalid IP %q", vip)
	}
	deadline := time.Now().Add(timeout)
	for {
		if !vipPresentOnLoopback(t, vip) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VIP %s/32 still present on %s after %s",
				vip, PortForwardLoopbackDev, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func vipPresentOnLoopback(t *testing.T, vip string) bool {
	t.Helper()
	out, err := exec.Command("ip", "-j", "-4", "addr", "show", "dev", PortForwardLoopbackDev).CombinedOutput()
	if err != nil {
		return false
	}
	type addrInfo struct {
		Local     string `json:"local"`
		Prefixlen int    `json:"prefixlen"`
	}
	var entries []struct {
		AddrInfo []addrInfo `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		return false
	}
	for _, e := range entries {
		for _, a := range e.AddrInfo {
			if a.Local == vip && a.Prefixlen == 32 {
				return true
			}
		}
	}
	return false
}

// SaveSysctl reads the current value of path and registers a t.Cleanup that
// writes it back. Returns the saved value. Used for global sysctls like
// udp_l3mdev_accept where a leak would silently change kernel behaviour for
// every subsequent test on the host.
func SaveSysctl(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sysctl %s: %v", path, err)
	}
	original := strings.TrimSpace(string(data))
	t.Cleanup(func() {
		if err := os.WriteFile(path, []byte(original+"\n"), 0o644); err != nil {
			t.Logf("restore sysctl %s=%s: %v", path, original, err)
		}
	})
	return original
}

// AssertSysctl reads path and fails the test if the trimmed contents do not
// equal want within timeout.
func AssertSysctl(t *testing.T, path, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			last = strings.TrimSpace(string(data))
			if last == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("sysctl %s = %q, want %q (after %s)", path, last, want, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ScrubPortForwardResidue is the exported entry point for tests that want to
// scrub port-forward state without going through the full Setup/Teardown
// pair. Internally it just delegates to scrubPortForwardState.
func ScrubPortForwardResidue(t *testing.T) {
	t.Helper()
	scrubPortForwardState(t)
}

// scrubPortForwardState removes all per-test residue of the agent's
// port-forward feature: managed addresses on loopback1, fwmark ip rules at
// the default DNAT priorities, the entire port-forward routing table
// (default DefaultPortForwardTableID), and the agent's nft table. Best-effort
// — used between scenarios so a failed test does not poison the next.
func scrubPortForwardState(t *testing.T) {
	t.Helper()

	// Flush all addresses on loopback1. The device itself stays in place
	// (setup.sh created it and tests share it).
	if _, err := net.InterfaceByName(PortForwardLoopbackDev); err == nil {
		_ = exec.Command("ip", "addr", "flush", "dev", PortForwardLoopbackDev).Run()
	}

	// Remove DNAT fwmark policy rules. RuleDel via netlink would require
	// importing the agent's package; the `ip rule del` form by priority is
	// idempotent enough for cleanup.
	for _, prio := range []int{DefaultDnatFwmarkPriority, DefaultDnatReplyPriority} {
		_ = exec.Command("ip", "rule", "del", "priority", strconv.Itoa(prio)).Run()
	}

	// Flush the port-forward reply table (default DefaultPortForwardTableID).
	// If the test used a non-default table we don't know it here — but the
	// default-table flush is the safe path for the harness's typical
	// Defaults() config.
	_ = exec.Command("ip", "route", "flush", "table", strconv.Itoa(DefaultPortForwardTableID)).Run()

	// Drop the agent's nft table. Already covered by scrubLocalState, but
	// repeating it here makes scrubPortForwardState self-contained for
	// callers that need just port-forward cleanup.
	if out, err := exec.Command("nft", "list", "table", "ip", DefaultNftTable).CombinedOutput(); err == nil && len(out) > 0 {
		_ = exec.Command("nft", "delete", "table", "ip", DefaultNftTable).Run()
	}
}
