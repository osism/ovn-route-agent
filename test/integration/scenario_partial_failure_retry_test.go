//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_FailureInjection_PartialFailureFRR (#88 item 6):
//
// Steady-state contract under test: when the FRR write plane fails but the
// kernel plane is healthy, the agent retries only the failing plane on
// subsequent reconciles. The kernel route stays a single entry on br-ex
// (no spurious re-add), and the Prometheus counters separate the planes
// correctly:
//
//   - route_readds_total{plane="frr"}   increments at least once
//   - route_readds_total{plane="kernel"} stays at zero
//
// We realise the partial failure with the vtysh shim filtered to "conf t"
// — that substring is present in AddFRRRoutes / DelFRRRoutes but not in
// ListFRRRoutes (which uses `show ip route ...`). So the agent's
// pre-reconcile reads succeed and accurately report "FRR route absent",
// the subsequent add fails, and verifyRoutes hits its missing-FRR branch
// (which is the branch that increments route_readds_total{plane="frr"}).
//
// The kernel-side delete operation isn't shimmed — netlink does not pay
// the FRR plane any attention — so the kernel route is never withdrawn,
// never re-added, and the kernel counter stays at zero.
func TestScenario_FailureInjection_PartialFailureFRR(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fipartial",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	shim := testenv.WithFailingTool(t, "vtysh", 10).MatchArg("conf t")

	cfg := testenv.FastDefaults()
	cfg.ExtraEnv = append(cfg.ExtraEnv, shim.Env())
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)

	const fip = "198.51.100.42"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.42")

	// Baseline: agent installs both planes cleanly while the shim is
	// still disarmed.
	testenv.AssertKernelRoute(t, fip, 15*time.Second)
	testenv.AssertFRRRoute(t, fip, 15*time.Second)
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_route_readds_total",
		map[string]string{"plane": "frr"},
		func(v float64, present bool) bool { return present && v == 0 },
		5*time.Second)
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_route_readds_total",
		map[string]string{"plane": "kernel"},
		func(v float64, present bool) bool { return present && v == 0 },
		5*time.Second)

	// Arm the shim, then rip the FRR route out of band so the agent
	// detects drift on the next reconcile. We bypass the shim by
	// invoking vtysh from the test process — the shim sits on PATH for
	// the agent subprocess only.
	shim.Arm()
	if err := deleteFRRRouteRaw(t, fip); err != nil {
		t.Fatalf("delete FRR route out of band: %v", err)
	}

	// Failure window: route_readds_total{plane="frr"} increments because
	// verifyRoutes hits the missing-FRR branch while AddFRRRoutes is still
	// being rejected. consecutive_readds settles at >= 1 in the same cycle.
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_route_readds_total",
		map[string]string{"plane": "frr"},
		func(v float64, present bool) bool { return present && v >= 1 },
		20*time.Second)
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_consecutive_readds", nil,
		func(v float64, present bool) bool { return present && v >= 1 },
		5*time.Second)

	// Disarm and let the next reconcile push FRR back. We Stop the agent
	// before exit (deferred below) so the shutdown path also goes through
	// real vtysh.
	shim.Disarm()
	testenv.AssertFRRRoute(t, fip, 20*time.Second)
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_consecutive_readds", nil,
		func(v float64, present bool) bool { return present && v == 0 },
		20*time.Second)

	// Kernel plane was never re-added: route_readds_total{plane="kernel"}
	// is still 0, and there is exactly one /32 route for the FIP on br-ex
	// (an over-eager AddKernelRoute on a healthy entry would surface as a
	// duplicate line in `ip route show`).
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_route_readds_total",
		map[string]string{"plane": "kernel"},
		func(v float64, present bool) bool { return present && v == 0 },
		5*time.Second)
	if dup := countKernelRouteEntries(t, fip); dup != 1 {
		t.Errorf("kernel route for %s appeared %d times after recovery, want 1", fip, dup)
	}

	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
}

// deleteFRRRouteRaw removes a /32 static route from the agent's default VRF
// using a fresh vtysh invocation. It is the test-side mirror of
// RouteManager.DelFRRRoutes and is used to rip routes out of band so the
// agent's reconcile observes drift it cannot have introduced itself.
func deleteFRRRouteRaw(t *testing.T, ip string) error {
	t.Helper()
	args := []string{
		"-c", "conf t",
		"-c", "vrf " + testenv.DefaultVRFName,
		"-c", "no ip route " + ip + "/32 " + testenv.DefaultVethNexthop,
		"-c", "exit-vrf",
		"-c", "end",
	}
	out, err := exec.Command("vtysh", args...).CombinedOutput()
	if err != nil {
		return &deleteFRRError{err: err, out: strings.TrimSpace(string(out))}
	}
	return nil
}

type deleteFRRError struct {
	err error
	out string
}

func (e *deleteFRRError) Error() string {
	if e.out == "" {
		return e.err.Error()
	}
	return e.err.Error() + " (output: " + e.out + ")"
}

// countKernelRouteEntries returns the number of `<ip>/32` entries on the
// default bridge device. A healthy installation has exactly one; values
// other than one indicate either a missing or a duplicated kernel route.
func countKernelRouteEntries(t *testing.T, ip string) int {
	t.Helper()
	out, err := exec.Command("ip", "-4", "route", "show", ip+"/32", "dev", testenv.DefaultBridgeDev).CombinedOutput()
	if err != nil {
		t.Fatalf("countKernelRouteEntries: ip route show: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
