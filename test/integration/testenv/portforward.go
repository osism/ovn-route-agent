//go:build integration

package testenv

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
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
// priorities 150/151, the entire port-forward routing table (default 201),
// and the agent's nft table. Best-effort — used between scenarios so a
// failed test does not poison the next.
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
	for _, prio := range []string{"150", "151"} {
		_ = exec.Command("ip", "rule", "del", "priority", prio).Run()
	}

	// Flush the port-forward reply table (default 201). If the test used a
	// non-default table we don't know it here — but the default-table flush
	// is the safe path for the harness's typical Defaults() config.
	_ = exec.Command("ip", "route", "flush", "table", "201").Run()

	// Drop the agent's nft table. Already covered by scrubLocalState, but
	// repeating it here makes scrubPortForwardState self-contained for
	// callers that need just port-forward cleanup.
	if out, err := exec.Command("nft", "list", "table", "ip", DefaultNftTable).CombinedOutput(); err == nil && len(out) > 0 {
		_ = exec.Command("nft", "delete", "table", "ip", DefaultNftTable).Run()
	}
}
