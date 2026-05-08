//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// IPv6 reconciliation scenarios for issue #54.
//
// Coverage scope: OVS flow plumbing only.
//
// The agent's OVS layer is family-aware (MACTweakFlow / HairpinFlow accept
// an `ipv6 bool`, ovs.go), but the kernel-route and FRR-route paths are v4
// only today: AddKernelRoute hardcodes net.CIDRMask(32, 32), AddFRRRoutes
// emits "ip route .../32" via vtysh, and the veth nexthop is v4. The /128
// kernel-route and "ipv6 route" FRR assertions called for in #54 cannot
// pass without first extending those code paths in routing_linux.go and
// routing.go (and giving the agent a v6 nexthop). Those changes are out of
// scope for this PR — the integration suite landing first lets us catch
// regressions on the v6 OVS path that does work, and the routing extension
// can land in a follow-up with these scenarios growing kernel/FRR asserts
// at that point.

// TestScenario_IPv6FIPAddRemove (#54 scenario 1):
//
// Inserting a NAT entry with a v6 external IP on a router whose LRP carries
// a v6 network installs:
//   - the per-bridge ipv6 MAC-tweak flow (cookie 0x999, ipv6 protocol)
//   - a per-IP ipv6 hairpin flow (cookie 0x998, ipv6_dst=<fip>) with
//     mod_dl_dst pointing at the owning router's LRP MAC
//
// Removing the NAT entry should remove the hairpin flow for that FIP. The
// MAC-tweak flow stays while any local router is active (per-bridge, not
// per-IP) — so the removal half only verifies the per-IP flow disappears.
func TestScenario_IPv6FIPAddRemove(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fipv6r1",
		LRPMAC:      "fa:16:3e:66:00:01",
		LRPNetworks: []string{"2001:db8:cafe::11/64"},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)

	const fipExternal = "2001:db8:cafe::42"
	natUUID := testenv.AddFIP(t, ctx, nb, router, fipExternal, "fd00::42")

	// Per-bridge MAC-tweak flow exists in both v4 and v6 forms. dump-flows
	// renders the L2 selector in its protocol-keyword form: ",ip," for v4
	// and ",ipv6," for v6. Match on the surrounding commas to disambiguate
	// (a bare "ip" substring matches "ipv6" too).
	testenv.AssertOVSFlowMatches(t, "0x999",
		func(line string) bool {
			return strings.Contains(line, ",ipv6,") &&
				strings.Contains(line, "mod_dl_dst:")
		}, 15*time.Second, "v6 MAC-tweak flow on br-ex")

	// Per-IP hairpin flow for the v6 FIP. dump-flows reports the v6
	// destination as ipv6_dst=<ip> (no /128 suffix when the host mask
	// covers everything, mirroring the /32 elision on the v4 form).
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "ipv6_dst="+fipExternal) &&
				strings.Contains(line, "mod_dl_dst:"+router.LRPMAC)
		}, 15*time.Second, "v6 hairpin flow for FIP "+fipExternal)

	// Remove the NAT entry → the per-IP hairpin flow goes away. Use
	// AssertNoOVSFlowMatches with the same predicate so we don't disturb
	// the still-needed v4 cookie=0x998 flows on this bridge.
	testenv.RemoveFIP(t, ctx, nb, router, natUUID)
	testenv.AssertNoOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "ipv6_dst="+fipExternal)
		}, 15*time.Second, "v6 hairpin flow must disappear after NAT removal")

	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
}

// TestScenario_IPv6MultiRouterHairpin (#54 scenario 2):
//
// Two routers active locally on the same chassis, each with a v6 FIP. The
// agent installs cookie=0x998 hairpin flows for both FIPs with mod_dl_dst
// pointing at the *owning* router's LRP MAC. Mirrors
// TestScenario_MultiRouterOnOneChassis but on the v6 path.
func TestScenario_IPv6MultiRouterHairpin(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	r1 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "v6multia",
		LRPMAC:      "fa:16:3e:cc:00:01",
		LRPNetworks: []string{"2001:db8:cafe::11/64"},
	})
	r2 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "v6multib",
		LRPMAC:      "fa:16:3e:dd:00:02",
		LRPNetworks: []string{"2001:db8:beef::11/64"},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fipA = "2001:db8:cafe::55"
		fipB = "2001:db8:beef::66"
	)
	testenv.AddFIP(t, ctx, nb, r1, fipA, "fd00::55")
	testenv.AddFIP(t, ctx, nb, r2, fipB, "fd00::66")

	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "ipv6_dst="+fipA) &&
				strings.Contains(line, "mod_dl_dst:"+r1.LRPMAC)
		}, 15*time.Second, "v6 hairpin flow for FIP A → router A MAC")
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "ipv6_dst="+fipB) &&
				strings.Contains(line, "mod_dl_dst:"+r2.LRPMAC)
		}, 15*time.Second, "v6 hairpin flow for FIP B → router B MAC")
}

// TestScenario_IPv6DualStackRouter (#54 scenario 3):
//
// Single router with a dual-stack LRP (one v4 network, one v6 network).
// The agent's hairpinMACMap collects every LRP gateway IP, so on br-ex we
// expect one v4 hairpin flow (nw_dst=<lrp-v4-ip>) and one v6 hairpin flow
// (ipv6_dst=<lrp-v6-ip>) under cookie=0x998, both pointing at the same LRP
// MAC.
func TestScenario_IPv6DualStackRouter(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:   "ds1",
		LRPMAC: "fa:16:3e:ee:00:01",
		LRPNetworks: []string{
			"198.51.100.11/24",
			"2001:db8:cafe::11/64",
		},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst=198.51.100.11") &&
				strings.Contains(line, "mod_dl_dst:"+router.LRPMAC)
		}, 15*time.Second, "v4 hairpin flow for LRP gateway (dual-stack)")
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "ipv6_dst=2001:db8:cafe::11") &&
				strings.Contains(line, "mod_dl_dst:"+router.LRPMAC)
		}, 15*time.Second, "v6 hairpin flow for LRP gateway (dual-stack)")
}
