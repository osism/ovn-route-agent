//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_FailureInjection_RouterMasqueradeStartupBeforeSNAT (#88
// item 4):
//
// router_masquerade rules are emitted only when both halves of the source
// match are known: at least one VIP carries router_masquerade=true (from
// agent config) AND at least one SNAT external IP is present on a locally-
// active router (from OVN NB).
//
// Start the agent with router_masquerade configured but NO snat-type NAT
// entry on the router. The postrouting_snat chain must not be emitted: the
// ruleset would carry zero rules and emitting an empty chain serves no
// purpose. After adding the SNAT NAT entry to NB the next reconcile picks
// it up and the chain materialises with the matching rule.
//
// This is the inverse of TestScenario_PortForwardRouterMasquerade in
// scenario_port_forward_test.go — that test asserts steady-state when
// both inputs are present from the start. Here we pin the ordering edge
// case: agent ready before NB has the data.
func TestScenario_FailureInjection_RouterMasqueradeStartupBeforeSNAT(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()
	testenv.EnsureLoopback1(t)

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "rmasqord",
		LRPMAC:      "fa:16:3e:7a:00:02",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const (
		vip     = "198.51.100.41"
		snatIP  = "198.51.100.50"
		snatNet = "10.0.0.0/24"
	)

	cfg := testenv.FastDefaults()
	cfg.PortForwardDev = testenv.PortForwardLoopbackDev
	cfg.PortForwardCTZone = intPtr(64003)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:              vip,
		RouterMasquerade: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)
	t.Cleanup(func() { testenv.ScrubPortForwardResidue(t) })

	// Sanity: the rest of the port-forward ruleset is up (prerouting_dnat
	// is emitted regardless of router_masquerade state). Asserting on it
	// proves the agent reconciled the new VIP before we test for the
	// absence of postrouting_snat.
	testenv.AssertNftChainExists(t, testenv.DefaultNftTable, "prerouting_dnat", 15*time.Second)

	// router_masquerade alone (no SNAT IP) must not emit the chain.
	// 3s is two reconcile ticks at FastDefaults's 2s interval — plenty
	// to surface a regression that emits the chain prematurely without
	// stalling the suite.
	testenv.AssertNftChainAbsent(t, testenv.DefaultNftTable, "postrouting_snat", 3*time.Second)

	// Add a snat-type NAT entry on the same router. The next reconcile
	// must see it (OVN.UpdateState extracts SNAT IPs from NB NAT rows of
	// type=snat on locally-active routers) and emit the chain.
	testenv.AddSNATEntry(t, ctx, nb, router, snatIP, snatNet)

	// The rule shape mirrors TestScenario_PortForwardRouterMasquerade:
	// saddr = snatIP, masquerade statement. Asserting on the rule (not
	// just the chain existence) guards against a regression where the
	// chain is emitted but the rule body is wrong.
	testenv.AssertNftRuleInChain(t, testenv.DefaultNftTable, "postrouting_snat",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "saddr", snatIP) && r.HasMasquerade()
		},
		30*time.Second, "router masquerade rule for VIP "+vip+" with SNAT IP "+snatIP)

	// Final sanity: the agent didn't log a hard failure on the SNAT
	// transition. We're not asserting on a specific log marker — just
	// that the per-reconcile log doesn't carry a fatal "failed to
	// reconcile port forwarding" line for the new state.
	logs := a.LogTail(100000)
	if strings.Contains(logs, "failed to reconcile port forwarding") {
		t.Errorf("agent logged a port-forward reconcile failure during the SNAT transition; tail:\n%s",
			a.LogTail(40))
	}
}
