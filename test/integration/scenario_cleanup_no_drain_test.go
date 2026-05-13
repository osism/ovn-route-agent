//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_FailureInjection_RemoveManagedNBEntriesNoDrain (#88 item 3):
//
// Drive the agent's cleanup_on_shutdown path with drain_on_shutdown=false and
// verify two contracts at once:
//
//   - RemoveManagedNBEntries removes the agent-tagged Logical_Router_Static_
//     Route and Static_MAC_Binding rows. We use the gatewayless-virtual-
//     gateway path (no pre-existing default route in NB) because that is the
//     only feature today that plants entries with the "ovn-network-agent" =
//     "managed" external_ids tag.
//   - No "drain:" log lines appear in the agent's stderr. The drain branch
//     in Agent.Run() is gated entirely on cfg.DrainOnShutdown, so a regression
//     that started running drain unconditionally (or that emitted a drain
//     log line from a stray code path) would surface as a spurious match here.
func TestScenario_FailureInjection_RemoveManagedNBEntriesNoDrain(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "nodrain",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	on := true
	off := false
	cfg.CleanupOnShutdown = &on
	cfg.DrainOnShutdown = &off
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)

	// Wait for the agent to plant its gatewayless managed entries: a
	// default-route on the router with the managed external_ids tag plus
	// a static MAC binding for the synthesized virtual-gateway IP.
	const vgw = "198.51.100.254"
	testenv.Eventually(t, func() bool {
		r, ok := testenv.FindStaticRoute(t, ctx, nb, router.RouterUUID, "0.0.0.0/0")
		return ok && r.ExternalIDs["ovn-network-agent"] == "managed"
	}, 15*time.Second, 200*time.Millisecond, "agent must plant managed default route")
	testenv.EventuallyValue(t, func() (testenv.NBStaticMACBinding, bool) {
		return testenv.FindMACBinding(t, ctx, nb, router.LRPName, vgw)
	}, 15*time.Second, 200*time.Millisecond, "agent must plant managed MAC binding")

	if err := a.Stop(20 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}

	// NB cleanup ran: managed route + MAC binding are gone.
	if _, ok := testenv.FindStaticRoute(t, ctx, nb, router.RouterUUID, "0.0.0.0/0"); ok {
		t.Errorf("managed default route still present after SIGTERM with cleanup_on_shutdown=true")
	}
	if _, ok := testenv.FindMACBinding(t, ctx, nb, router.LRPName, vgw); ok {
		t.Errorf("managed MAC binding still present after SIGTERM with cleanup_on_shutdown=true")
	}

	logs := a.LogTail(100000)

	// Cleanup branch ran (positive marker so a misconfiguration that
	// silently skipped both branches surfaces here, not as a missing
	// negative assertion below).
	if !strings.Contains(logs, "shutting down, cleaning up routes") {
		t.Errorf("expected cleanup log line not found; tail:\n%s", a.LogTail(40))
	}

	// Drain branch did NOT run. We match on the "drain:" prefix the
	// drain helpers in ovn_gateway.go use ("drain: no gateway chassis ...",
	// "drain: gateway chassis priority lowered", "drain: complete, ...",
	// "drain: timeout exceeded ...") plus the top-level "drain mode active"
	// log line in agent.go.
	for _, marker := range []string{
		"drain mode active",
		"drain:",
		"drain failed",
	} {
		if strings.Contains(logs, marker) {
			t.Errorf("spurious drain log line %q present; tail:\n%s", marker, a.LogTail(60))
		}
	}
}
