//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// All scenarios in this file cover #58 — the agent's FRR prefix-list reconcile
// surface (`ReconcileFRRPrefixList`). The harness's Defaults() leaves
// frr_prefix_list empty, so until this file existed the entire lifecycle was
// untested on a real FRR. A regression here is operationally severe: a stale
// or missing entry directly governs which routes get re-announced over BGP.
//
// Each test uses a unique prefix-list name (rather than the production
// ANNOUNCED-NETWORKS that setup.sh seeds) so the agent's reconcile starts
// from a known-empty state and t.Cleanup can drop the entire list without
// disturbing the FRR baseline.

// prefixListCleanup registers a t.Cleanup that drops the named prefix-list.
// FRR auto-creates the list on first entry, so dropping it on the way out
// keeps a failed test from poisoning subsequent runs through residual
// "permit ... ge 32 le 32" entries.
func prefixListCleanup(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			testenv.DumpFRRPrefixList(t, name)
		}
		testenv.RemoveFRRPrefixList(t, name)
	})
}

// TestScenario_PrefixListEntryAdded (#58 scenario 1):
//
// Configure a fresh prefix-list and a router with LRP network 198.51.100.11/24.
// The agent's first reconcile must observe the discovered network
// (198.51.100.0/24) and install a `permit 198.51.100.0/24 ge 32 le 32` entry.
// Without this assertion, a regression that broke the LRP→DiscoveredNetworks
// pipeline or the prefix-list write path would only surface as silent BGP
// announcement gaps in production.
func TestScenario_PrefixListEntryAdded(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const listName = "OVN-AGENT-TEST-58-S1"
	prefixListCleanup(t, listName)

	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "plistadd",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.FRRPrefixList = listName
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertFRRPrefixListContains(t, listName, "198.51.100.0/24", 15*time.Second)
}

// TestScenario_PrefixListStaleEntryPruned (#58 scenario 2):
//
// With two locally-active routers on disjoint LRP networks, both networks
// must show up in the prefix-list. After deleting the second router the
// agent's reconcile must reap its entry within a couple of ticks, leaving
// the first router's entry alone. Exercises the "remove stale entries" loop
// in ReconcileFRRPrefixList (routing.go:351).
func TestScenario_PrefixListStaleEntryPruned(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const (
		listName = "OVN-AGENT-TEST-58-S2"
		net1     = "198.51.100.0/24"
		net2     = "203.0.113.0/24"
	)
	prefixListCleanup(t, listName)

	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "plist1",
		LRPMAC:      "fa:16:3e:11:22:33",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	r2 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "plist2",
		LRPMAC:      "fa:16:3e:11:22:44",
		LRPNetworks: []string{"203.0.113.11/24"},
	})

	cfg := testenv.FastDefaults()
	cfg.FRRPrefixList = listName
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Both networks land on the first reconcile.
	testenv.AssertFRRPrefixListContains(t, listName, net1, 15*time.Second)
	testenv.AssertFRRPrefixListContains(t, listName, net2, 15*time.Second)

	// Drop router 2 — the agent loses 203.0.113.0/24 from DiscoveredNetworks
	// and must remove the corresponding entry on the next tick.
	deleteRouter(t, ctx, nb, r2.RouterUUID)

	testenv.AssertNoFRRPrefixListEntry(t, listName, net2, 15*time.Second)

	// net1 must remain — guards against an over-zealous reconciler that
	// re-emits "remove all" when any single router disappears.
	testenv.AssertFRRPrefixListContains(t, listName, net1, 1*time.Second)
}

// TestScenario_PrefixListClearedOnNoLocalRouters (#58 scenario 3):
//
// While the agent owns prefix-list entries, simulate ovn-northd rebinding
// the chassisredirect Port_Binding to a peer chassis (failover-style). The
// agent observes HasLocalRouters→false and must hit the
// `ReconcileFRRPrefixList(nil)` branch in agent.reconcile (agent.go:300),
// clearing every managed entry. Reuses MakeChassis + SetCRPortChassis from
// the failover scenario.
func TestScenario_PrefixListClearedOnNoLocalRouters(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const listName = "OVN-AGENT-TEST-58-S3"
	prefixListCleanup(t, listName)

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "plistnolr",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	cfg.FRRPrefixList = listName
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Sanity: confirm the entry is in place before we trigger failover so
	// the post-failover assertion really proves removal.
	testenv.AssertFRRPrefixListContains(t, listName, "198.51.100.0/24", 15*time.Second)

	// Insert a peer chassis and rebind the CR Port_Binding to it. After this
	// commits, the agent's local-router detection sees no locally-active
	// routers and must clear the prefix-list.
	peerChassis := testenv.MakeChassis(t, ctx, sb, "plist-peer")
	testenv.SetCRPortChassis(t, ctx, sb, router.CRPortUUID, &peerChassis)

	testenv.AssertFRRPrefixListEmpty(t, listName, 15*time.Second)
}

// TestScenario_PrefixListClearedOnShutdown (#58 scenario 4):
//
// SIGTERM with cleanup_on_shutdown=true must invoke the
// `ReconcileFRRPrefixList(nil)` call in agent.cleanup (agent.go:570), which
// removes every managed entry before the process exits. Mirrors the
// equivalent veth-leak teardown scenario.
func TestScenario_PrefixListClearedOnShutdown(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const listName = "OVN-AGENT-TEST-58-S4"
	prefixListCleanup(t, listName)

	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "plistshut",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cleanup := true
	cfg.CleanupOnShutdown = &cleanup
	cfg.FRRPrefixList = listName

	a := readyAgent(t, cfg)
	testenv.AssertFRRPrefixListContains(t, listName, "198.51.100.0/24", 15*time.Second)

	// Use manual Stop (rather than WithAgent) so the post-SIGTERM assertion
	// runs while we are still inside the test, not at t.Cleanup time.
	if err := a.Stop(20 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}

	testenv.AssertFRRPrefixListEmpty(t, listName, 5*time.Second)
}

// TestScenario_PrefixListLeavesUnrelatedEntriesAlone (#58 scenario 5, edge):
//
// ListFRRPrefixListEntries only matches lines with the trailing `ge 32 le 32`
// clause. Anything else — for example a hand-written `permit 10.0.0.0/8`
// without `ge`/`le` qualifiers — is invisible to the reconciler and must not
// be touched. We pre-seed such an entry, run the agent through a full
// reconcile cycle, and verify both: the agent's managed entry shows up AND
// the seeded entry survives.
//
// The seq for the seeded entry deliberately sits below the agent's allocated
// range (it always picks maxSeq+5 starting from 0), so a reconciler that
// went rogue and rewrote everything by sequence would be detected.
func TestScenario_PrefixListLeavesUnrelatedEntriesAlone(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const listName = "OVN-AGENT-TEST-58-S5"
	prefixListCleanup(t, listName)

	// Seed a non-managed entry: `permit 10.0.0.0/8` without `ge`/`le` is
	// outside the agent's reconcile surface (ListFRRPrefixListEntries skips
	// it) and must survive the agent's lifetime untouched.
	testenv.SeedFRRPrefixListEntry(t, listName, 100, "permit 10.0.0.0/8")

	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "plistedge",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	cfg.FRRPrefixList = listName
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Managed entry lands.
	testenv.AssertFRRPrefixListContains(t, listName, "198.51.100.0/24", 15*time.Second)

	// Wait at least one reconcile tick (FastDefaults => 2s) to give a buggy
	// reconciler the chance to wipe the seeded entry. The assertion below
	// then proves the entry survived through that cycle.
	time.Sleep(3 * time.Second)

	// Match the raw line because the seeded entry has no `ge`/`le` qualifiers
	// and is not surfaced as a structured FRRPrefixListEntry by the helpers.
	testenv.AssertFRRPrefixListLineContains(t, listName, "seq 100 permit 10.0.0.0/8", 1*time.Second)
}
