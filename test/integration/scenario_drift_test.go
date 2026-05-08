//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// All scenarios in this file cover #55 — the agent's drift-recovery layer.
//
// Two mechanisms are under test:
//   - the periodic reconcile ticker (`reconcile_interval`) that re-derives
//     desired vs. actual every cycle and reinstalls anything missing, and
//   - `verifyRoutes`, the post-mutation safety net that catches routes that
//     vanished between the add and the verify (historically a vtysh/FRR race).
//
// Without coverage here, a refactor that drops either layer would not be
// caught: the unit tests in agent_test.go exercise the wiring but do not
// observe drift on a real kernel + FRR.
//
// Each scenario uses testenv.FastDefaults() so the periodic tick is 2s rather
// than the production 5s, keeping the suite well under the parent #42 budget.

// TestScenario_DriftKernelRouteHealed (#55 scenario 1):
//
// With one FIP installed, deleting its /32 kernel route out from under the
// agent must be detected by the periodic reconcile and reinstalled within at
// most a couple of ticks. The agent re-adds the kernel route via
// `ensureRoutes` (it sees `currentKernel` no longer contains the IP, so
// `needsKernel` is true) — independent of FRR, which is unchanged.
func TestScenario_DriftKernelRouteHealed(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftk",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.42"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.42")

	// Sanity: agent installs the route first time round.
	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 10*time.Second)

	// Drift: delete the kernel route directly. The agent did not initiate
	// this change, so no event-triggered reconcile fires — recovery has to
	// come from the periodic ticker.
	if out, err := exec.Command("ip", "route", "del", fip+"/32", "dev", testenv.DefaultBridgeDev).CombinedOutput(); err != nil {
		t.Fatalf("ip route del %s/32 dev %s: %v (%s)", fip, testenv.DefaultBridgeDev, err, strings.TrimSpace(string(out)))
	}

	// Within reconcile_interval (2s) + processing slack the agent must have
	// noticed the missing kernel route and reinstalled it. Allow up to 3
	// ticks before failing — drift detection is a best-effort property and
	// asserting on a single tick would be flaky.
	testenv.AssertKernelRoute(t, fip, 8*time.Second)

	// FRR was untouched, so the static route should never have left.
	testenv.AssertFRRRoute(t, fip, 1*time.Second)
}

// TestScenario_DriftFRRRouteHealed (#55 scenario 2):
//
// With one FIP installed, deleting its FRR static route out of the VRF must
// be detected by the periodic reconcile and re-added within a couple of
// ticks. The agent reissues the route via `ensureRoutes`'s `addFRR` batch
// (it sees `currentFRR` no longer contains the IP, so `needsFRR` is true).
//
// Because `addFRR` is non-empty for that cycle, this also exercises the
// `verifyRoutes` safety-net call at the end of the reconcile — but the
// primary recovery mechanism here is the periodic tick itself.
func TestScenario_DriftFRRRouteHealed(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftf",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.43"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.43")

	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 10*time.Second)

	// Drift: rip the static route out of FRR via vtysh. The agent's
	// AddFRRRoutes uses `ip route <ip>/32 <nexthop>`; FRR's staticd
	// requires the same `<prefix> <nexthop>` arguments on the `no` form
	// (a bare `no ip route <ip>/32` is rejected as incomplete). This
	// mirrors what a buggy FRR config-replay or a careful operator would
	// do during an emergency.
	const nexthop = "169.254.0.1"
	args := []string{
		"-c", "conf t",
		"-c", "vrf " + testenv.DefaultVRFName,
		"-c", "no ip route " + fip + "/32 " + nexthop,
		"-c", "exit-vrf",
		"-c", "end",
	}
	if out, err := exec.Command("vtysh", args...).CombinedOutput(); err != nil {
		t.Fatalf("vtysh no ip route %s/32 %s: %v (%s)", fip, nexthop, err, strings.TrimSpace(string(out)))
	}

	// Allow a generous 2× tick window — vtysh is noticeably slower than
	// netlink, and we don't want flakes on a busy CI runner.
	testenv.AssertFRRRoute(t, fip, 8*time.Second)

	// Kernel was untouched, so the /32 should never have left.
	testenv.AssertKernelRoute(t, fip, 1*time.Second)
}

// TestScenario_DriftSameCycleReAdd (#55 scenario 3):
//
// Inject a kernel-route disappearance immediately after an event-triggered
// reconcile to exercise the same-cycle re-add path:
//   - install FIP A, wait for routes
//   - delete the kernel route for FIP A
//   - install FIP B (this kicks an NB-event reconcile)
//   - both FIPs must end up present in kernel and FRR
//
// During the FIP-B reconcile the agent will:
//   - see FIP B is missing in both kernel and FRR → add both
//   - see FIP A is missing in kernel (FRR is still present) → re-add via
//     `ensureRoutes` (`needsKernel=true, needsFRR=false`)
//   - because `addFRR` is non-empty (for B), call `verifyRoutes` at the end,
//     which is the actual safety-net hook this scenario covers
//
// If a future refactor drops `verifyRoutes` and reorders the
// add-then-verify into a single non-checked add, the test still passes
// because `ensureRoutes` already re-adds A. That is intentional: this test
// asserts the *outcome* (both routes present after a same-cycle drift), not
// the specific code path that got us there. The scenario fails if the agent
// loses track of A entirely while processing B.
func TestScenario_DriftSameCycleReAdd(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "drifts",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fipA = "198.51.100.51"
		fipB = "198.51.100.52"
	)
	testenv.AddFIP(t, ctx, nb, router, fipA, "10.0.0.51")
	testenv.AssertKernelRoute(t, fipA, 10*time.Second)
	testenv.AssertFRRRoute(t, fipA, 10*time.Second)

	// Drift: delete the kernel route for FIP A. We do NOT wait for the
	// periodic ticker here — the next step injects an event-triggered
	// reconcile via NB, and we want it to observe A as missing.
	if out, err := exec.Command("ip", "route", "del", fipA+"/32", "dev", testenv.DefaultBridgeDev).CombinedOutput(); err != nil {
		t.Fatalf("ip route del %s/32 dev %s: %v (%s)", fipA, testenv.DefaultBridgeDev, err, strings.TrimSpace(string(out)))
	}

	// Trigger an NB event so the agent reconciles immediately.
	testenv.AddFIP(t, ctx, nb, router, fipB, "10.0.0.52")

	// Both FIPs must end up present. The agent has to handle "new FIP B
	// arrived" *and* "FIP A drifted" within the same reconcile cycle.
	testenv.AssertKernelRoute(t, fipA, 10*time.Second)
	testenv.AssertKernelRoute(t, fipB, 10*time.Second)
	testenv.AssertFRRRoute(t, fipA, 10*time.Second)
	testenv.AssertFRRRoute(t, fipB, 10*time.Second)
}
