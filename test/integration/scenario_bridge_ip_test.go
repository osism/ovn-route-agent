//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// All scenarios in this file cover #63 — the cold-start bridge-IP and
// proxy-ARP housekeeping that happens before any reconcile cycle. The
// agent's Run() adds BridgeIP (default 169.254.169.254/32) to br-ex via
// EnsureBridgeIP so the kernel has a source for ARP, and flips
// /proc/sys/net/ipv4/conf/br-ex/proxy_arp to 1 via EnableProxyARP so the
// kernel responds to ARP requests for FIPs. On SIGTERM with
// cleanup_on_shutdown=true (the default) RemoveBridgeIP undoes the address
// add; proxy_arp is currently NOT restored — TestScenario_ProxyARPNotRestored
// pins that down so the operator-facing contract is explicit.
//
// State the harness's general Teardown does not scrub:
//   - bridge addresses on br-ex (only added by these scenarios and the agent)
//   - the per-device proxy_arp sysctl
//
// Each scenario therefore wraps proxy_arp in SaveSysctl and registers a
// best-effort `ip addr del` for both the default and custom bridge IPs so
// one failed test does not poison the next.

const (
	defaultBridgeIPCIDR = "169.254.169.254/32"
	customBridgeIPCIDR  = "169.254.42.42/32"
)

// startBridgeIPScenario is the bridge-IP analogue of startScenario /
// startPFScenario. It performs the host preconditions (Setup + failure
// dump), saves the per-device proxy_arp sysctl so the agent's unconditional
// flip to 1 does not leak into subsequent tests, and defensively scrubs any
// bridge addresses left behind by a previous crash. The Setup-registered
// Teardown does not touch bridge addresses, so the trailing-edge scrub here
// is the only thing keeping scenarios in this file from interfering with
// the rest of the suite.
//
// Returns a Defaults() AgentConfig. The caller mutates BridgeIP /
// CleanupOnShutdown to drive the specific scenario.
func startBridgeIPScenario(t *testing.T) testenv.AgentConfig {
	t.Helper()
	testenv.Setup(t)
	testenv.RegisterFailureDump(t)
	testenv.SaveSysctl(t, testenv.BridgeProxyARPPath(testenv.DefaultBridgeDev))

	scrubBridgeIPs(t)
	t.Cleanup(func() { scrubBridgeIPs(t) })

	return testenv.Defaults()
}

// scrubBridgeIPs removes the default and custom bridge IPs from br-ex,
// ignoring errors (the addresses may not be present). Used at both the
// leading and trailing edges of every bridge-IP scenario.
func scrubBridgeIPs(t *testing.T) {
	t.Helper()
	for _, cidr := range []string{defaultBridgeIPCIDR, customBridgeIPCIDR} {
		_ = exec.Command("ip", "addr", "del", cidr, "dev", testenv.DefaultBridgeDev).Run()
	}
}

// TestScenario_BridgeIPOnStartup (#63 scenario 1):
//
// Cold start the agent with default config. The agent must add the default
// link-local /32 to br-ex during Run() before "agent running" is logged,
// so AssertBridgeAddress lands as soon as WaitReady returns.
func TestScenario_BridgeIPOnStartup(t *testing.T) {
	cfg := startBridgeIPScenario(t)
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertBridgeAddress(t, testenv.DefaultBridgeDev, defaultBridgeIPCIDR, 10*time.Second)
}

// TestScenario_BridgeIPNotDuplicatedOnRestart (#63 scenario 2):
//
// Stop and restart the agent without cleanup in between. The second Run()
// must observe the existing address via EnsureBridgeIP's idempotency check
// and short-circuit — never producing a duplicate entry. We assert exactly
// one /32 (AssertBridgeAddress fails fast on count != 1) and that the
// agent logged no "add IP" error line.
//
// cleanup_on_shutdown is disabled on the first run so the leftover address
// is what the second run encounters; this is the load-bearing path because
// a cleaned-up first run would leave nothing for the second run to
// duplicate.
func TestScenario_BridgeIPNotDuplicatedOnRestart(t *testing.T) {
	cfg := startBridgeIPScenario(t)
	off := false
	cfg.CleanupOnShutdown = &off

	a1 := readyAgent(t, cfg)
	testenv.AssertBridgeAddress(t, testenv.DefaultBridgeDev, defaultBridgeIPCIDR, 10*time.Second)
	if err := a1.Stop(15 * time.Second); err != nil {
		t.Fatalf("first agent stop: %v", err)
	}
	// Still present — first agent had cleanup_on_shutdown=false.
	testenv.AssertBridgeAddress(t, testenv.DefaultBridgeDev, defaultBridgeIPCIDR, 5*time.Second)

	a2 := readyAgent(t, cfg)
	defer a2.Stop(15 * time.Second)

	// Exactly one entry after the second start — AssertBridgeAddress fails
	// the test if the agent double-added the address.
	testenv.AssertBridgeAddress(t, testenv.DefaultBridgeDev, defaultBridgeIPCIDR, 10*time.Second)

	// EnsureBridgeIP's idempotency branch emits "bridge IP already present"
	// at debug level (the harness sets log_level=debug). Asserting on that
	// line is a stronger pin than just "no error" — it proves the agent
	// took the fast path instead of issuing a redundant AddrAdd that would
	// have come back as EEXIST.
	logs := a2.LogTail(100000)
	if !strings.Contains(logs, "bridge IP already present") {
		t.Errorf("expected 'bridge IP already present' on restart; last logs:\n%s", a2.LogTail(40))
	}
}

// TestScenario_BridgeIPCustom (#63 scenario 3):
//
// Configure bridge_ip to a non-default address. The agent must add that
// address and MUST NOT also add the 169.254.169.254 default — the cold-
// start path uses cfg.BridgeIP as the single source of truth.
func TestScenario_BridgeIPCustom(t *testing.T) {
	cfg := startBridgeIPScenario(t)
	cfg.BridgeIP = "169.254.42.42"

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertBridgeAddress(t, testenv.DefaultBridgeDev, customBridgeIPCIDR, 10*time.Second)
	testenv.AssertNoBridgeAddress(t, testenv.DefaultBridgeDev, defaultBridgeIPCIDR, 5*time.Second)
}

// TestScenario_ProxyARPEnabled (#63 scenario 4):
//
// After the agent reaches ready the per-device proxy_arp sysctl must be 1.
// EnableProxyARP runs in Run() before the "agent running" log line, so the
// sysctl is already flipped by the time WaitReady returns.
//
// SaveSysctl in startBridgeIPScenario captures the pre-test value (typically
// 0 on a fresh host) and restores it at test exit so subsequent tests do
// not inherit the flipped value.
func TestScenario_ProxyARPEnabled(t *testing.T) {
	cfg := startBridgeIPScenario(t)
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertSysctl(t, testenv.BridgeProxyARPPath(testenv.DefaultBridgeDev), "1", 5*time.Second)
}

// TestScenario_BridgeIPRemovedOnCleanup (#63 scenario 5):
//
// With cleanup_on_shutdown=true (the agent's default), SIGTERM must run
// cleanup() which calls RemoveBridgeIP. The bridge address must be gone
// once Stop returns.
func TestScenario_BridgeIPRemovedOnCleanup(t *testing.T) {
	cfg := startBridgeIPScenario(t)
	on := true
	cfg.CleanupOnShutdown = &on

	a := readyAgent(t, cfg)
	testenv.AssertBridgeAddress(t, testenv.DefaultBridgeDev, defaultBridgeIPCIDR, 10*time.Second)

	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
	testenv.AssertNoBridgeAddress(t, testenv.DefaultBridgeDev, defaultBridgeIPCIDR, 5*time.Second)
}

// TestScenario_BridgeIPRetainedWhenCleanupDisabled (#63 scenario 6):
//
// With cleanup_on_shutdown=false, the agent's shutdown branch logs
// "shutting down, keeping routes in place" and skips cleanup() entirely.
// The bridge IP must therefore remain on br-ex after Stop.
//
// The trailing-edge scrub registered by startBridgeIPScenario removes the
// leftover address — the harness's general Teardown does not touch bridge
// addresses, so without that scrub the next scenario would inherit a
// 169.254.169.254/32 already on br-ex.
func TestScenario_BridgeIPRetainedWhenCleanupDisabled(t *testing.T) {
	cfg := startBridgeIPScenario(t)
	off := false
	cfg.CleanupOnShutdown = &off

	a := readyAgent(t, cfg)
	testenv.AssertBridgeAddress(t, testenv.DefaultBridgeDev, defaultBridgeIPCIDR, 10*time.Second)

	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}

	// Bridge IP must still be present. AssertBridgeAddress with a short
	// timeout is the right tool — it polls and fails fast if the address
	// has disappeared (which would mean cleanup ran despite the flag).
	testenv.AssertBridgeAddress(t, testenv.DefaultBridgeDev, defaultBridgeIPCIDR, 2*time.Second)

	logs := a.LogTail(100000)
	if !strings.Contains(logs, "shutting down, keeping routes in place") {
		t.Errorf("expected keep-routes log line not found; last logs:\n%s", a.LogTail(40))
	}
}

// TestScenario_ProxyARPNotRestoredOnShutdown (#63 scenario 7, optional):
//
// Pins the operator-facing contract: the agent enables proxy_arp on br-ex
// at startup but does NOT track or restore its pre-startup value on
// shutdown. EnableProxyARP unconditionally writes "1" to the sysctl, and
// cleanup() does not call any "DisableProxyARP" counterpart — so after
// SIGTERM the sysctl remains at 1 regardless of cleanup_on_shutdown.
//
// This test exists as a behavioural pin: if a future change adds a restore
// path, this test will start failing and the change author can update both
// the test and the documented contract together. SaveSysctl in
// startBridgeIPScenario takes care of resetting the value for subsequent
// tests on the host.
func TestScenario_ProxyARPNotRestoredOnShutdown(t *testing.T) {
	cfg := startBridgeIPScenario(t)
	path := testenv.BridgeProxyARPPath(testenv.DefaultBridgeDev)

	// Start from a known-off baseline so a "still 1 after shutdown" result
	// cannot be confused with "was already 1 before the agent started".
	if err := exec.Command("sh", "-c", "echo 0 > "+path).Run(); err != nil {
		t.Fatalf("seed proxy_arp=0: %v", err)
	}

	a := readyAgent(t, cfg)
	testenv.AssertSysctl(t, path, "1", 5*time.Second)

	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}

	// Contract: proxy_arp stays at 1 after shutdown. Update this test (and
	// the corresponding docs) only when adding a deliberate restore path.
	testenv.AssertSysctl(t, path, "1", 2*time.Second)
}
