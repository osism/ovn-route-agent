//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// NAT-type variant scenarios for issue #62.
//
// The agent's NB read path treats `dnat_and_snat` and `snat` rows uniformly:
// both end up in NATIPToRouterMAC, so both produce kernel /32 routes, FRR
// static routes, and per-IP hairpin flows on br-ex. The earlier #42 scenarios
// only exercised `dnat_and_snat`; these tests pin down the `snat` path and the
// `external_mac` override that distributed FIPs set on `dnat_and_snat`.

// TestScenario_PureSNATEntry (#62 scenario 1):
//
// An `snat`-typed NAT row (logical_ip is a CIDR, not a single host) still has
// its external_ip treated as a host that must be reachable from outside: the
// agent installs a /32 kernel route, an FRR static route, and a hairpin flow
// pointing back at the owning LRP MAC. Pin this behaviour so a future change
// can't silently filter `snat` rows out of the desired set.
func TestScenario_PureSNATEntry(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "snatr1",
		LRPMAC:      "fa:16:3e:5a:00:01",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const snatIP = "198.51.100.10"
	testenv.AddSNATEntry(t, ctx, nb, router, snatIP, "10.0.0.0/24")

	// Kernel and FRR routes mirror the FIP path.
	testenv.AssertKernelRoute(t, snatIP, 15*time.Second)
	testenv.AssertFRRRoute(t, snatIP, 15*time.Second)

	// Per-IP hairpin flow with mod_dl_dst pointing at the owning LRP MAC.
	// Reuses the same predicate shape as the FIP scenarios.
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+snatIP) &&
				strings.Contains(line, "mod_dl_dst:"+router.LRPMAC)
		}, 15*time.Second, "hairpin flow for SNAT IP → router MAC")
}

// TestScenario_FIPWithExternalMAC (#62 scenario 2):
//
// When a `dnat_and_snat` NAT row carries an `external_mac` override (Neutron
// sets this for distributed FIPs), the agent's NB write path is unchanged:
// it does NOT create a Static_MAC_Binding for the FIP IP itself — bindings
// are only installed for the virtual gateway IP (.254 of the LRP network),
// and that one still uses the local br-ex MAC, not the FIP's external_mac.
//
// This is intentional: the agent is a host-side BGP/route adapter, not an
// OVN tunnel-MAC manager. external_mac is for OVN's internal pipeline, the
// agent leaves it untouched. The test pins the negative: no FIP-level
// Static_MAC_Binding appears regardless of external_mac, and the existing
// .254 binding still points at the bridge MAC.
func TestScenario_FIPWithExternalMAC(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fipmacr1",
		LRPMAC:      "fa:16:3e:ee:00:01",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fip         = "198.51.100.77"
		externalMAC = "fa:16:3e:de:ad:be"
	)
	testenv.AddFIPWithExternalMAC(t, ctx, nb, router, fip, "10.0.0.77", externalMAC)

	// Sanity: the agent still installs the FIP route (kernel + FRR) — the
	// presence of external_mac must not change the host-side announcement.
	testenv.AssertKernelRoute(t, fip, 15*time.Second)
	testenv.AssertFRRRoute(t, fip, 15*time.Second)

	// The agent's only Static_MAC_Binding is for the virtual gateway IP
	// (.254 of the LRP network), pointing at the bridge MAC. Wait for it
	// before asserting the negative on the FIP IP — otherwise the test
	// races the reconcile loop.
	const vgw = "198.51.100.254"
	binding := testenv.EventuallyValue(t, func() (testenv.NBStaticMACBinding, bool) {
		return testenv.FindMACBinding(t, ctx, nb, router.LRPName, vgw)
	}, 15*time.Second, 200*time.Millisecond, "virtual gateway MAC binding must exist")
	if strings.EqualFold(binding.MAC, externalMAC) {
		t.Errorf("virtual gateway binding MAC = %q, must not be the FIP external_mac %q",
			binding.MAC, externalMAC)
	}
	bridgeMAC := readBridgeMAC(t)
	if !strings.EqualFold(binding.MAC, bridgeMAC) {
		t.Errorf("virtual gateway binding MAC = %q, want bridge MAC %q",
			binding.MAC, bridgeMAC)
	}

	// Negative: the agent must NOT have created a Static_MAC_Binding for
	// the FIP IP itself, regardless of external_mac. This is the load
	// bearing assertion for this scenario — any change that starts
	// mirroring external_mac into NB would surface here.
	if b, ok := testenv.FindMACBinding(t, ctx, nb, router.LRPName, fip); ok {
		t.Errorf("unexpected Static_MAC_Binding for FIP %s: %+v", fip, b)
	}
}

// TestScenario_MixedNATTypesOnOneRouter (#62 scenario 3):
//
// A router carrying both a `dnat_and_snat` row (FIP) and a `snat` row at the
// same time must yield both external IPs as hairpin/kernel/FRR routes with no
// duplication or loss. Without this scenario, a regression that filtered one
// type out of NATIPToRouterMAC would only fail in tests that exercise that
// type in isolation.
func TestScenario_MixedNATTypesOnOneRouter(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "mixnat",
		LRPMAC:      "fa:16:3e:3e:00:01",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fip     = "198.51.100.88"
		snatIP  = "198.51.100.12"
		logical = "10.0.0.88"
		snatNet = "10.0.0.0/24"
	)
	testenv.AddFIP(t, ctx, nb, router, fip, logical)
	testenv.AddSNATEntry(t, ctx, nb, router, snatIP, snatNet)

	// Both external IPs reachable as kernel + FRR routes.
	testenv.AssertKernelRoute(t, fip, 15*time.Second)
	testenv.AssertKernelRoute(t, snatIP, 15*time.Second)
	testenv.AssertFRRRoute(t, fip, 15*time.Second)
	testenv.AssertFRRRoute(t, snatIP, 15*time.Second)

	// And both have hairpin flows with the same owning router's MAC.
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fip) &&
				strings.Contains(line, "mod_dl_dst:"+router.LRPMAC)
		}, 15*time.Second, "hairpin flow for FIP → router MAC")
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+snatIP) &&
				strings.Contains(line, "mod_dl_dst:"+router.LRPMAC)
		}, 15*time.Second, "hairpin flow for SNAT IP → router MAC")
}
