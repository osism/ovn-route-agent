//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// All scenarios in this file cover #43 — port-forward (DNAT, conntrack zones,
// hairpin masquerade) integration tests. Each scenario configures the agent
// with a specific port_forwards block, asserts the resulting nft ruleset and
// kernel state via JSON-parsed `nft -j list ruleset` (not regex on text), and
// relies on the harness's scrubLocalState to delete leaked state between runs.
//
// The agent's port-forward feature requires veth_leak_enabled=true (handled
// by the harness defaults) and a loopback1 device in vrf-provider (provisioned
// by setup.sh — EnsureLoopback1 short-circuits the test if it's missing).

const (
	pfTable = testenv.DefaultNftTable
)

// boolPtr returns a pointer to b — used for *bool fields in fixtures.
func boolPtr(b bool) *bool { return &b }

// intPtr returns a pointer to i — used for *int fields in AgentConfig.
func intPtr(i int) *int { return &i }

// pfDefaults returns a Defaults() AgentConfig augmented with the port-forward
// fields tests need. Caller appends to PortForwards.
func pfDefaults(t *testing.T) testenv.AgentConfig {
	t.Helper()
	cfg := testenv.Defaults()
	cfg.PortForwardDev = testenv.PortForwardLoopbackDev
	// Use a non-default ct zone so collisions with anything OVN might use
	// surface as test failures rather than silent crosstalk.
	cfg.PortForwardCTZone = intPtr(64000)
	return cfg
}

// startPFScenario is the port-forward analogue of startScenario. It performs
// the same OVN bring-up plus EnsureLoopback1, so scenarios that don't drive
// any NB/SB state still have the device they need. It also issues a defensive
// scrub of port-forward residue at the start so a previous failed test cannot
// pollute the current one (the registered Cleanup also scrubs at the end via
// Teardown).
func startPFScenario(t *testing.T) testenv.AgentConfig {
	t.Helper()
	testenv.Setup(t)
	testenv.EnsureLoopback1(t)
	// We don't need ovn-northd paused for port-forward scenarios that don't
	// inject SB Port_Bindings, but we do need a clean slate.
	testenv.PauseOVNNorthd(t)
	testenv.PauseOVNController(t)
	// Defensive scrub: drop any nft table / loopback addresses / fwmark
	// rules left behind by a prior failing scenario before we bring the
	// agent up. The Setup-registered Teardown handles the trailing edge.
	testenv.ScrubPortForwardResidue(t)

	return pfDefaults(t)
}

// TestScenario_PortForwardSingleBackendDNAT (#43 scenario 1).
//
// A VIP with a single rule and one backend should produce:
//   - prerouting_dnat:    ip daddr <VIP> tcp dport 80 dnat to <backend>:80
//   - prerouting_ctzone:  ip daddr <VIP> tcp dport 80 ct zone set <zone>
//     ip saddr <backend> tcp sport 80 ct zone set <zone>
//   - prerouting_fwmark:  meta mark set 0x100 / 0x200 (for original/reply)
func TestScenario_PortForwardSingleBackendDNAT(t *testing.T) {
	cfg := startPFScenario(t)

	const (
		vip     = "198.51.100.10"
		backend = "10.0.0.100"
	)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP: vip,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: backend,
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// DNAT rule.
	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "daddr", vip) &&
				r.HasMatch("tcp", "dport", 80) &&
				r.HasDNATTo(backend, 80)
		},
		15*time.Second, "single-backend DNAT rule for "+vip+":80")

	// Conntrack zone: original direction (VIP daddr).
	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_ctzone",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "daddr", vip) &&
				r.HasMatch("tcp", "dport", 80) &&
				r.HasCTZoneSet(*cfg.PortForwardCTZone)
		},
		10*time.Second, "ctzone original-direction rule for "+vip)

	// Conntrack zone: reply direction (backend saddr).
	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_ctzone",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "saddr", backend) &&
				r.HasMatch("tcp", "sport", 80) &&
				r.HasCTZoneSet(*cfg.PortForwardCTZone)
		},
		10*time.Second, "ctzone reply-direction rule for backend "+backend)

	// Fwmark: original (0x100) and reply (0x200).
	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_fwmark",
		func(r testenv.NftRule) bool { return r.HasMarkSet(0x100) },
		10*time.Second, "prerouting_fwmark sets 0x100 on original direction")
	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_fwmark",
		func(r testenv.NftRule) bool { return r.HasMarkSet(0x200) },
		10*time.Second, "prerouting_fwmark sets 0x200 on reply direction")
}

// TestScenario_PortForwardMultiBackendStickyHashing (#43 scenario 2).
//
// A 3-backend rule should generate a single DNAT statement using nft's jhash
// expression: `dnat to jhash ip saddr mod 3 map { 0:..., 1:..., 2:... }`.
// The map keys are 0-indexed; values pair (addr, port).
func TestScenario_PortForwardMultiBackendStickyHashing(t *testing.T) {
	cfg := startPFScenario(t)

	const vip = "198.51.100.20"
	backends := []string{"10.0.0.100", "10.0.0.101", "10.0.0.102"}
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP: vip,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 8080, DestAddrs: backends,
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	expected := []testenv.NftBackend{
		{Addr: backends[0], Port: 8080},
		{Addr: backends[1], Port: 8080},
		{Addr: backends[2], Port: 8080},
	}
	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "daddr", vip) &&
				r.HasMatch("tcp", "dport", 8080) &&
				r.HasDNATMap(expected)
		},
		15*time.Second, "jhash sticky DNAT map across 3 backends")
}

// TestScenario_PortForwardManageVIP (#43 scenario 3).
//
// manage_vip:true should add the VIP/32 to loopback1; manage_vip:false (the
// default) should leave it absent. Verified via `ip -j addr show dev loopback1`.
func TestScenario_PortForwardManageVIP(t *testing.T) {
	cfg := startPFScenario(t)

	const (
		managedVIP   = "198.51.100.10"
		unmanagedVIP = "198.51.100.11"
	)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{
		{
			VIP:       managedVIP,
			ManageVIP: true,
			Rules: []testenv.PortForwardRuleFixture{{
				Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
			}},
		},
		{
			VIP:       unmanagedVIP,
			ManageVIP: false,
			Rules: []testenv.PortForwardRuleFixture{{
				Proto: "tcp", Port: 81, DestAddr: "10.0.0.101",
			}},
		},
	}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertVIPOnLoopback(t, managedVIP, 15*time.Second)
	// The unmanaged VIP must stay off the device. Use a short timeout —
	// the agent never adds it, so a wait of a few seconds is enough to
	// rule out a race with the initial reconcile.
	testenv.AssertVIPNotOnLoopback(t, unmanagedVIP, 3*time.Second)
}

// TestScenario_PortForwardPerRuleMasquerade (#43 scenario 4).
//
// VIP-level masquerade:true with one rule overridden to masquerade:false
// should produce a postrouting_snat chain with exactly the inheriting rule's
// backend (and no entry for the overridden rule).
func TestScenario_PortForwardPerRuleMasquerade(t *testing.T) {
	cfg := startPFScenario(t)

	const (
		vip             = "198.51.100.30"
		inheritBackend  = "10.1.0.50"  // remote — should be masqueraded
		overrideBackend = "10.0.0.100" // local — masquerade:false override
	)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:        vip,
		Masquerade: true,
		Rules: []testenv.PortForwardRuleFixture{
			{
				Proto: "tcp", Port: 443, DestAddr: inheritBackend,
				// Inherits VIP-level masquerade=true.
			},
			{
				Proto: "udp", Port: 53, DestAddr: overrideBackend,
				Masquerade: boolPtr(false),
			},
		},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Inheriting rule appears in postrouting_snat.
	testenv.AssertNftRuleInChain(t, pfTable, "postrouting_snat",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "daddr", inheritBackend) &&
				r.HasMatch("tcp", "dport", 443) &&
				r.HasMasquerade()
		},
		15*time.Second, "inheriting rule must appear in postrouting_snat")

	// Overridden rule must NOT appear. Read the dump once and assert
	// directly — Eventually-style waits on absence are slow and we already
	// know the chain converged from the previous assertion.
	dump := testenv.DumpNftRuleset(t)
	for _, r := range dump.RulesIn(pfTable, "postrouting_snat") {
		if r.HasMatch("ip", "daddr", overrideBackend) {
			t.Errorf("overridden backend %s should not be masqueraded but appears in postrouting_snat: %s",
				overrideBackend, string(r.Raw))
		}
	}
}

// TestScenario_PortForwardHairpinMasquerade (#43 scenario 5).
//
// hairpin_masquerade:true plus a discovered provider network should produce
// a postrouting_snat rule of the form:
//
//	ip saddr <provider-net> ct original daddr <vip> ct status dnat masquerade
//
// To make the agent discover a provider network we insert a local router
// whose LRP networks fall in 198.51.100.0/24 — the agent's reconcile picks
// these up and feeds them to ReconcilePortForward as the providerNetworks
// arg, which buildNftRuleset uses to materialise the hairpin rule.
func TestScenario_PortForwardHairpinMasquerade(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()
	testenv.EnsureLoopback1(t)

	// Provider network 198.51.100.0/24 — the LRP network the agent will
	// discover and pass into the hairpin rule.
	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "hairpinr",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	_ = router

	const vip = "198.51.100.40"
	cfg := pfDefaults(t)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:               vip,
		HairpinMasquerade: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Wait for the reconcile that populates providerNetworks. The agent
	// emits the hairpin rule only on Reconcile (not Setup), so a longer
	// timeout is appropriate.
	testenv.AssertNftRuleInChain(t, pfTable, "postrouting_snat",
		func(r testenv.NftRule) bool {
			// Hairpin rule matches `ip saddr <provider-cidr>` (a
			// prefix expression in nft JSON) plus a masquerade
			// statement. The accompanying ct-expression matches
			// (`ct original daddr <vip>`, `ct status dnat`) are
			// not asserted here — saddr-prefix + masquerade
			// uniquely identify the hairpin rule because no other
			// rule in postrouting_snat carries a CIDR match.
			return r.HasMatchPrefix("ip", "saddr", "198.51.100.0/24") &&
				r.HasMasquerade()
		},
		30*time.Second, "hairpin masquerade rule for VIP "+vip+" with provider 198.51.100.0/24")
}

// TestScenario_PortForwardL3mdevSysctls (#43 scenario 6).
//
// port_forward_l3mdev_accept:true must set udp_l3mdev_accept=1 and
// tcp_l3mdev_accept=1 in /proc/sys/net/ipv4/. These are GLOBAL sysctls — the
// test saves and restores them via SaveSysctl so the host is left as it was
// found.
func TestScenario_PortForwardL3mdevSysctls(t *testing.T) {
	cfg := startPFScenario(t)

	// Capture original values so the t.Cleanup restores them after the
	// test, regardless of pass/fail.
	testenv.SaveSysctl(t, testenv.SysctlUDPL3mdevAccept)
	testenv.SaveSysctl(t, testenv.SysctlTCPL3mdevAccept)

	cfg.PortForwardL3mdevAccept = boolPtr(true)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP: "198.51.100.50",
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertSysctl(t, testenv.SysctlUDPL3mdevAccept, "1", 10*time.Second)
	testenv.AssertSysctl(t, testenv.SysctlTCPL3mdevAccept, "1", 10*time.Second)
}

// TestScenario_PortForwardOutputChains (#43 scenario 7).
//
// A local-backend rule with masquerade:false should leave both output_ctzone
// (zone assignment for locally-generated reply traffic) and output_fwmark
// (mark 0x200 with type=route to trigger reroute) chains in place, with rules
// mirroring their prerouting counterparts. The agent always emits these
// chains, but the rules inside them must include the local-backend's port —
// otherwise the same-host reply path is broken.
func TestScenario_PortForwardOutputChains(t *testing.T) {
	cfg := startPFScenario(t)

	const (
		vip          = "198.51.100.60"
		localBackend = "127.0.0.10"
	)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:        vip,
		Masquerade: false, // local backend, no SNAT
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 8443, DestAddr: localBackend,
			Masquerade: boolPtr(false),
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// output_ctzone exists and mirrors the original-direction zone rule.
	testenv.AssertNftChainExists(t, pfTable, "output_ctzone", 15*time.Second)
	testenv.AssertNftRuleInChain(t, pfTable, "output_ctzone",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "daddr", vip) &&
				r.HasMatch("tcp", "dport", 8443) &&
				r.HasCTZoneSet(*cfg.PortForwardCTZone)
		},
		10*time.Second, "output_ctzone mirrors VIP daddr rule")
	testenv.AssertNftRuleInChain(t, pfTable, "output_ctzone",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "saddr", localBackend) &&
				r.HasMatch("tcp", "sport", 8443) &&
				r.HasCTZoneSet(*cfg.PortForwardCTZone)
		},
		10*time.Second, "output_ctzone mirrors backend saddr rule")

	// output_fwmark exists and sets 0x200 on the reply direction.
	testenv.AssertNftChainExists(t, pfTable, "output_fwmark", 15*time.Second)
	testenv.AssertNftRuleInChain(t, pfTable, "output_fwmark",
		func(r testenv.NftRule) bool { return r.HasMarkSet(0x200) },
		10*time.Second, "output_fwmark sets 0x200 on locally-generated reply")

	// Verify the chain header is `type route` (not `type filter`) — the
	// route hook is required to trigger routing re-evaluation when the
	// mark changes. Without it the reply never re-enters the provider VRF.
	chain, ok := testenv.DumpNftRuleset(t).Chain(pfTable, "output_fwmark")
	if !ok {
		t.Fatal("output_fwmark chain missing in second dump")
	}
	if chain.Type != "route" {
		t.Errorf("output_fwmark chain type = %q, want %q", chain.Type, "route")
	}
}

// TestScenario_PortForwardCleanupOnSIGTERM (#43 scenario 8).
//
// SIGTERM with cleanup_on_shutdown=true (the harness default) must remove
// the agent's nft table and all VIP addresses managed by manage_vip:true.
// We start the agent, verify the desired state landed, send SIGTERM via
// AgentProc.Stop, and assert both the table and the VIP are gone.
func TestScenario_PortForwardCleanupOnSIGTERM(t *testing.T) {
	cfg := startPFScenario(t)

	const vip = "198.51.100.99"
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:       vip,
		ManageVIP: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.99",
		}},
	}}

	a := readyAgent(t, cfg)

	// Confirm we have something to clean up before signalling SIGTERM.
	testenv.AssertNftChainExists(t, pfTable, "prerouting_dnat", 15*time.Second)
	testenv.AssertVIPOnLoopback(t, vip, 15*time.Second)

	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}

	testenv.AssertNftTableAbsent(t, pfTable, 10*time.Second)
	testenv.AssertVIPNotOnLoopback(t, vip, 10*time.Second)
}
