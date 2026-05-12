//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// gateway_port single-port scenarios for issue #62.
//
// `gateway_port` opts the agent into legacy single-router mode: of all the
// chassisredirect ports active on this chassis, only the named one counts as
// "local". Multi-router scenarios already cover the default (empty) value;
// these tests pin the single-port filter so a future change can't silently
// drop the gating or turn a missing port into a hard error.

// TestScenario_GatewayPortFiltersOtherRouters (#62 scenario 4):
//
// Two routers active locally, agent configured to track only routerA's CR
// port. The agent must install routes/flows for routerA's FIP and *only* for
// routerA's FIP — routerB's FIP must stay unannounced.
func TestScenario_GatewayPortFiltersOtherRouters(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	routerA := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "gpa",
		LRPMAC:      "fa:16:3e:a1:00:01",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	routerB := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "gpb",
		LRPMAC:      "fa:16:3e:b2:00:02",
		LRPNetworks: []string{"203.0.113.11/24"},
	})

	const (
		fipA = "198.51.100.55"
		fipB = "203.0.113.66"
	)
	testenv.AddFIP(t, ctx, nb, routerA, fipA, "10.0.0.55")
	testenv.AddFIP(t, ctx, nb, routerB, fipB, "10.0.1.66")

	cfg := testenv.Defaults()
	// Per #62 implementation hint: derive the CR-port name from the LRP
	// name MakeLocalRouter returns. This stays in lock step with the
	// fixture's naming convention.
	cfg.GatewayPort = "cr-" + routerA.LRPName
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// routerA's FIP must be present.
	testenv.AssertKernelRoute(t, fipA, 15*time.Second)
	testenv.AssertFRRRoute(t, fipA, 15*time.Second)
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fipA) &&
				strings.Contains(line, "mod_dl_dst:"+routerA.LRPMAC)
		}, 15*time.Second, "hairpin flow for routerA's FIP")

	// routerB's FIP must NOT appear in any of the agent's outputs. Use a
	// shorter timeout — we expect the agent to *not* install these, so
	// polling for the full 15s on each negative assertion just slows the
	// suite. The 5s ceiling is still well above the worst-case reconcile
	// debounce + tick.
	testenv.AssertNoKernelRoute(t, fipB, 5*time.Second)
	testenv.AssertNoFRRRoute(t, fipB, 5*time.Second)
	testenv.AssertNoOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fipB)
		}, 5*time.Second, "hairpin flow for routerB's FIP must not appear")
}

// TestScenario_GatewayPortMissingComesUpClean (#62 scenario 5):
//
// `gateway_port` pointing at a CR-port name that does not exist (typo,
// not-yet-created router, etc.) must be treated as "no local routers": the
// agent comes up cleanly (WaitReady succeeds), no per-IP state is installed,
// and the cookie=0x998 hairpin set on br-ex is empty. A future change that
// turned a missing gateway_port into a hard error would surface here as
// either a WaitReady failure or a presence assertion.
func TestScenario_GatewayPortMissingComesUpClean(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	// Seed a real router with an active CR port and a FIP. Without the
	// gateway_port filter the agent would treat this router as local and
	// install the FIP route — so its absence below proves the filter
	// rejected every CR port, not just "there were none to find".
	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "gpmiss",
		LRPMAC:      "fa:16:3e:c3:00:03",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	const fip = "198.51.100.99"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.99")

	cfg := testenv.Defaults()
	cfg.GatewayPort = "cr-lrp-doesnotexist"

	// readyAgent waits for "agent running" — if the missing port were a
	// hard error the agent would exit before logging it and WaitReady
	// would fail. This is the load-bearing positive assertion.
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Negative side: nothing the seeded router would have caused must
	// appear. Cap each polling assertion at 5s — see above.
	testenv.AssertNoKernelRoute(t, fip, 5*time.Second)
	testenv.AssertNoFRRRoute(t, fip, 5*time.Second)
	testenv.AssertNoOVSFlow(t, "0x998", 5*time.Second)
}
