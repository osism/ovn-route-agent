//go:build integration

package integration

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_Failover (#42 scenario 4):
//
// While the agent is running with a locally-active router, simulate ovn-northd
// rebinding the chassisredirect Port_Binding to a peer chassis (as would
// happen if the peer's Gateway_Chassis priority were raised). The agent must
// notice HasLocalRouters→false and remove the per-IP routes/flows it
// installed.
func TestScenario_Failover(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fover",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	const fip = "198.51.100.77"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.77")

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Confirm the route is in place before we trigger failover, so the
	// after-failover assertion really proves removal (not just "never
	// installed").
	testenv.AssertKernelRoute(t, fip, 15*time.Second)
	testenv.AssertFRRRoute(t, fip, 15*time.Second)

	// Insert a peer chassis and rebind the CR Port_Binding to it. This
	// matches what ovn-northd would do once the peer became higher-priority.
	peerChassis := testenv.MakeChassis(t, ctx, sb, "peer-host")
	testenv.SetCRPortChassis(t, ctx, sb, router.CRPortUUID, &peerChassis)

	testenv.AssertNoKernelRoute(t, fip, 20*time.Second)
	testenv.AssertNoFRRRoute(t, fip, 20*time.Second)
	// Hairpin flows should also be gone (no local routers => empty map).
	testenv.AssertNoOVSFlow(t, "0x998", 20*time.Second)
}

// TestScenario_StaleChassisCleanup (#42 scenario 5):
//
// A managed NB static route tagged with the chassis name of a peer that
// disappears from SB Chassis must be cleaned up by any surviving agent after
// stale_chassis_grace_period elapses. The grace period is forced down to 2s
// for this test; jitter is bounded by maxStaleCleanupJitter (≤30s in
// production) but the test gives a generous 60s deadline to avoid flakes.
func TestScenario_StaleChassisCleanup(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	// Local router so the agent enters the productive branch every reconcile.
	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "stale",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	// Insert the peer chassis and seed a managed route tagged for it. The
	// route is on a *local* router because CleanupStaleChassisManagedEntries
	// only walks routes that the local agent can prove ownership of via the
	// chassis tag, not by router locality.
	peerName := "ghost-host"
	peerUUID := testenv.MakeChassis(t, ctx, sb, peerName)
	staleRouteUUID := testenv.SeedManagedRoute(t, ctx, nb, router,
		"203.0.113.99/32", "169.254.0.1", peerName)

	// Sanity: route is present before we delete the chassis.
	if testenv.CountManagedRoutes(t, ctx, nb, peerName) != 1 {
		t.Fatalf("seeded route not present (uuid=%s)", staleRouteUUID)
	}

	cfg := testenv.Defaults()
	staleGrace := "2s"
	cfg.StaleChassisGracePeriod = staleGrace
	cfg.ReconcileInterval = "2s" // tighten so the stale check runs quickly
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// While the chassis is alive, the agent must NOT remove the route — even
	// if a reconcile cycle ran in between.
	time.Sleep(3 * time.Second)
	if got := testenv.CountManagedRoutes(t, ctx, nb, peerName); got != 1 {
		t.Fatalf("agent removed route while chassis is alive (count=%d)", got)
	}

	// Delete the chassis; after grace + jitter the route must be gone.
	// Worst-case: 2s grace + 30s jitter + reconcile interval + safety = ~60s.
	testenv.DeleteChassis(t, ctx, sb, peerUUID)
	testenv.Eventually(t, func() bool {
		return testenv.CountManagedRoutes(t, ctx, nb, peerName) == 0
	}, 60*time.Second, 500*time.Millisecond,
		"surviving agent must delete managed route tagged for missing chassis")
}

// TestScenario_DrainOnShutdown (#42 scenario 6):
//
// SIGTERM with drain_on_shutdown=true must lower this chassis's
// Gateway_Chassis priority to 0 BEFORE kernel routes are torn down. The
// drain loop blocks until SB shows no local CR ports — a goroutine
// simulates ovn-northd by rebinding the CR Port_Binding once it observes
// priority=0 in NB, so the agent's drain unblocks promptly.
//
// We verify ordering by recording timestamps for both transitions and
// asserting priority_zero < route_removed.
func TestScenario_DrainOnShutdown(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "drain",
		LRPNetworks: []string{"198.51.100.11/24"},
		GatewayChassis: []testenv.GatewayChassisEntry{
			{ChassisName: testenv.LocalHostname(t), Priority: 5},
		},
	})

	cfg := testenv.Defaults()
	on := true
	cfg.DrainOnShutdown = &on
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)

	// Wait for at least the LRP-network /32 route to land before draining.
	testenv.AssertKernelRoute(t, "198.51.100.11", 15*time.Second)

	// Pre-stage a peer chassis so ovn-controller does not complain when we
	// rebind the Port_Binding mid-drain.
	peerUUID := testenv.MakeChassis(t, ctx, sb, "drain-peer")
	gcName := "lrp-" + router.Name + "_" + testenv.LocalHostname(t)

	// Goroutine: poll NB for priority==0, record its timestamp, then
	// rebind the CR port_binding to the peer so countLocalCRPorts returns 0.
	// We do not call t.Fatalf from this goroutine — testing.T methods are
	// only safe on the test's own goroutine. Errors are surfaced via errCh.
	var (
		priorityZeroAt = make(chan time.Time, 1)
		errCh          = make(chan error, 1)
	)
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-tick.C:
				var entries []testenv.NBGatewayChassis
				if err := nb.List(ctx, &entries); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				for _, gc := range entries {
					if gc.Name != gcName || gc.Priority != 0 {
						continue
					}
					select {
					case priorityZeroAt <- time.Now():
					default:
					}
					rebind := &testenv.SBPortBinding{UUID: router.CRPortUUID, Chassis: &peerUUID}
					ops, opErr := sb.Where(rebind).Update(rebind, &rebind.Chassis)
					if opErr != nil {
						select {
						case errCh <- opErr:
						default:
						}
						return
					}
					if _, opErr := sb.Transact(ctx, ops...); opErr != nil {
						select {
						case errCh <- opErr:
						default:
						}
					}
					return
				}
			}
		}
	}()

	if err := a.Stop(45 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
	pcancel()
	select {
	case err := <-errCh:
		t.Fatalf("drain helper goroutine error: %v", err)
	default:
	}

	// Capture the moment the kernel route finally disappeared.
	routeGoneAt := testenv.EventuallyValue(t, func() (time.Time, bool) {
		out, err := exec.Command("ip", "-4", "route", "show", "198.51.100.11/32", "dev", testenv.DefaultBridgeDev).CombinedOutput()
		if err == nil && len(out) == 0 {
			return time.Now(), true
		}
		return time.Time{}, false
	}, 30*time.Second, 50*time.Millisecond, "kernel route for LRP IP must eventually be removed")

	select {
	case prioAt := <-priorityZeroAt:
		if !prioAt.Before(routeGoneAt) {
			t.Fatalf("priority=0 must be observed before route removal: prio=%s route_gone=%s",
				prioAt.Format(time.StampMilli), routeGoneAt.Format(time.StampMilli))
		}
	default:
		t.Fatal("never observed priority=0 in NB during drain — drain logic regressed")
	}

	// Final state: NB priority is 0 (drained, not yet restored — agent stopped
	// without restart).
	gc, ok := testenv.FindGatewayChassis(t, ctx, nb, gcName)
	if !ok {
		t.Fatal("Gateway_Chassis disappeared after drain")
	}
	if gc.Priority != 0 {
		t.Errorf("post-drain Gateway_Chassis priority = %d, want 0", gc.Priority)
	}
}

// TestScenario_RestoreDrainedOnStartup (#42 scenario 7):
//
// The agent starts with NB Gateway_Chassis already at priority 0 for this
// chassis (as if a previous shutdown had drained but not restored). On
// startup the agent must:
//   - restore the priority to 1 (RestoreDrainedGateways), and then
//   - boost it to ≥minActivePriority (=2) via EnsureActivePriorityLead so
//     it strictly outranks any peer that is also at priority 1 after
//     restore.
//
// The test seeds a peer entry at priority 0 too — so once the agent
// restores and boosts, the local entry sits at 2 against a peer at 0.
func TestScenario_RestoreDrainedOnStartup(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "restore",
		LRPNetworks: []string{"198.51.100.11/24"},
		GatewayChassis: []testenv.GatewayChassisEntry{
			{ChassisName: testenv.LocalHostname(t), Priority: 0},
			{ChassisName: "restore-peer", Priority: 0},
		},
	})

	gcLocal := "lrp-" + router.Name + "_" + testenv.LocalHostname(t)
	gcPeer := "lrp-" + router.Name + "_restore-peer"

	cfg := testenv.Defaults()
	on := true
	cfg.DrainOnShutdown = &on
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// EnsureActivePriorityLead boosts to maxPeer+1 with a floor of
	// minActivePriority (=2). With peer at 0, expect local priority 2.
	testenv.Eventually(t, func() bool {
		gc, ok := testenv.FindGatewayChassis(t, ctx, nb, gcLocal)
		return ok && gc.Priority >= 2
	}, 20*time.Second, 200*time.Millisecond,
		"local Gateway_Chassis must be restored from 0 → ≥2 (1 by RestoreDrainedGateways, then boosted by EnsureActivePriorityLead)")

	// Peer entry must be untouched by RestoreDrainedGateways (different chassis).
	if peer, ok := testenv.FindGatewayChassis(t, ctx, nb, gcPeer); ok {
		if peer.Priority != 0 {
			t.Errorf("peer Gateway_Chassis priority changed: got %d, want 0", peer.Priority)
		}
	} else {
		t.Errorf("peer Gateway_Chassis %s disappeared", gcPeer)
	}
}
