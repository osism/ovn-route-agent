//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_OVNDatabasePauseResume (#64 scenario 1):
//
// SIGSTOP every ovsdb-server process for ~10s, then SIGCONT. The contract
// being pinned here is the agent's resilience against a transient OVN
// disconnect:
//
//   - the agent does NOT exit during the stall
//   - the kernel route installed before the pause is still present after
//     resume, and the agent process is still alive
//
// On the Ubuntu harness, ovn-ctl runs the NB ovsdb-server, the SB
// ovsdb-server, and ovn-northd under the single ovn-central systemd unit
// (see TestScenario_DrainStuckNBWrite for the long-form discussion). There
// is no clean way to pause only the NB server, so PauseOVNDatabases pauses
// both — a strictly harder version of the scenario described in the issue,
// since the agent now has zero OVN visibility instead of one DB.
//
// The pause window (10s) is intentionally shorter than libovsdb's 30s
// inactivity probe: this scenario covers the "stall and recover" path,
// not the "TCP reconnect" path. Exercising reconnection through a longer
// outage is the natural follow-up once the ovn_connection_state metric
// can be asserted to drop and recover (the issue calls this out
// conditionally on "once metrics is wired").
func TestScenario_OVNDatabasePauseResume(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "reconnect",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	const fip = "198.51.100.66"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.66")

	cfg := testenv.Defaults()
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Pre-condition: the agent has installed the kernel route, so the
	// post-resume "still there" check has signal (rather than racing the
	// initial install against the pause window).
	testenv.AssertKernelRoute(t, fip, 15*time.Second)

	if !a.Alive() {
		t.Fatalf("agent exited before pause; setup is broken (last logs:\n%s)", a.LogTail(40))
	}

	// Pause both ovsdb-server processes for 10s. The helper sleeps
	// synchronously and SIGCONTs on return; a t.Cleanup inside the helper
	// guarantees resume even if the test goroutine fails mid-pause.
	testenv.PauseOVNDatabases(t, 10*time.Second)

	if !a.Alive() {
		t.Fatalf("agent exited during DB pause — resilience contract violated (last logs:\n%s)",
			a.LogTail(60))
	}

	// After resume, the kernel route must still be present. The agent
	// reads desired state from its libovsdb cache, which is unchanged
	// across the pause, and never tore the route down. AssertKernelRoute
	// polls so any reconcile tick that started mid-pause and only
	// completed after SIGCONT does not race the assertion.
	testenv.AssertKernelRoute(t, fip, 15*time.Second)

	if !a.Alive() {
		t.Fatalf("agent exited after DB resume — clean reconnect contract violated (last logs:\n%s)",
			a.LogTail(60))
	}
}
