//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// NetworkFilters / manual network_cidrs scenarios for issue #57.
//
// The agent decides which external IPs are "managed" two ways:
//   - auto-discovery from Logical_Router_Port.networks (default; covered by #42)
//   - manual override via the `network_cidr` config key, which takes precedence
//     over discovery (config.go's effectiveNetworkFilters)
//
// The manual override is the operator's only knob for opting some networks out
// of BGP announcement. An external IP that falls outside the configured CIDRs
// MUST NOT install kernel/FRR routes — otherwise the agent silently announces
// networks the operator explicitly excluded.
//
// Path 2 was previously untested at the integration level; these scenarios pin
// it down so a precedence flip or a typo in CIDR parsing is caught.
//
// Scenario 4 from the issue ("invalid CIDR fails fast") is unit-covered:
// TestValidateConfig in config_test.go has "invalid CIDR" and "one valid
// one invalid CIDR" subtests asserting validateConfig rejects malformed
// entries before the agent reaches Run().

// TestScenario_NetworkCIDRsManualOverridesDiscovery (#57 scenario 1):
//
// With a router whose LRP carries 198.51.100.11/24 but the agent configured
// with network_cidr=["10.99.0.0/16"], the manual filter takes precedence over
// auto-discovery. A NAT for 198.51.100.42 (inside the discovered network but
// OUTSIDE the manual filter) must NOT install routes — that is the contract
// operators rely on to keep selected networks off the BGP fabric. A NAT for
// 10.99.0.42 (inside the manual filter, outside the discovered network) must
// install kernel + FRR routes.
func TestScenario_NetworkCIDRsManualOverridesDiscovery(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "ncidr1",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.NetworkCIDRs = []string{"10.99.0.0/16"}
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fipInScope    = "10.99.0.42"
		fipOutOfScope = "198.51.100.42"
	)
	testenv.AddFIP(t, ctx, nb, router, fipInScope, "10.0.0.42")
	testenv.AddFIP(t, ctx, nb, router, fipOutOfScope, "10.0.0.43")

	// The in-scope FIP is the contract: routes must appear within a normal
	// reconcile window.
	testenv.AssertKernelRoute(t, fipInScope, 10*time.Second)
	testenv.AssertFRRRoute(t, fipInScope, 10*time.Second)

	// The out-of-scope FIP is the *whole point* of the manual filter — it
	// must NOT be announced. We assert the negative after the in-scope FIP
	// has converged so the agent has demonstrably finished a reconcile that
	// could have installed it.
	testenv.AssertNoKernelRoute(t, fipOutOfScope, 1*time.Second)
	testenv.AssertNoFRRRoute(t, fipOutOfScope, 1*time.Second)
}

// TestScenario_NetworkCIDRsRestartPrunesOutOfScopeFIP (#57 scenario 2):
//
// Operators narrow the manual filter to drop a network from announcement.
// A restart with the new config must converge on the new desired set: the
// previously-installed FIP that now falls outside the filter must be gone,
// and the FIP still in scope must remain.
//
// Phase 1 runs with a wide filter covering both FIPs and default
// cleanup_on_shutdown=true, so its SIGTERM cleanup removes both routes via
// removeAllRoutes -> isManaged. Phase 2 then starts with the narrow filter
// and only the in-scope FIP gets re-installed.
//
// We assert the *outcome* (only the in-scope FIP present after Phase 2
// converges) rather than the specific cleanup path. A future refactor that
// moves the pruning from agent A's shutdown into agent B's reconcile still
// satisfies the contract.
func TestScenario_NetworkCIDRsRestartPrunesOutOfScopeFIP(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "ncidr2",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const (
		fipKept    = "10.99.0.42"
		fipDropped = "198.51.100.42"
	)
	testenv.AddFIP(t, ctx, nb, router, fipKept, "10.0.0.44")
	testenv.AddFIP(t, ctx, nb, router, fipDropped, "10.0.0.45")

	// Phase 1: wide filter covers both. Both FIPs land in kernel + FRR.
	wideCfg := testenv.Defaults()
	wideCfg.NetworkCIDRs = []string{"10.99.0.0/16", "198.51.100.0/24"}
	a1 := readyAgent(t, wideCfg)
	testenv.AssertKernelRoute(t, fipKept, 10*time.Second)
	testenv.AssertKernelRoute(t, fipDropped, 10*time.Second)
	testenv.AssertFRRRoute(t, fipKept, 10*time.Second)
	testenv.AssertFRRRoute(t, fipDropped, 10*time.Second)

	if err := a1.Stop(20 * time.Second); err != nil {
		t.Fatalf("phase 1 agent stop: %v", err)
	}

	// Phase 2: narrow filter excludes fipDropped.
	narrowCfg := testenv.Defaults()
	narrowCfg.NetworkCIDRs = []string{"10.99.0.0/16"}
	a2 := readyAgent(t, narrowCfg)
	defer a2.Stop(15 * time.Second)

	testenv.AssertKernelRoute(t, fipKept, 10*time.Second)
	testenv.AssertFRRRoute(t, fipKept, 10*time.Second)
	testenv.AssertNoKernelRoute(t, fipDropped, 15*time.Second)
	testenv.AssertNoFRRRoute(t, fipDropped, 15*time.Second)
}

// TestScenario_NetworkCIDRsEmptyFiltersClaimAllBridge32s (#57 scenario 3):
//
// With no manual filter AND no locally-active routers (so no auto-discovery),
// effectiveFilters is empty. The agent must treat every /32 on the bridge
// as managed — that is the implicit "no filters means manage everything"
// contract isManaged returning true on empty filters relies on. Without
// explicit coverage, a refactor that flips that default to "manage nothing"
// would silently leak orphan routes on chassis idle between gateway moves.
//
// We pin the contract down by planting a stray /32 on br-ex with the agent's
// own protocol number (rtproto 44, the value used for veth-leak per-network
// routes — the issue calls it out specifically) and verifying the agent
// reaps it. Path-wise: with no local routers and no port-forward VIPs,
// reconcile takes the removeAllRoutes branch (agent.go's "no locally active
// routers and no port forward VIPs"). removeAllRoutes calls isManaged on
// every kernel /32 on the bridge — empty filters means it returns true,
// so the stray gets removed.
func TestScenario_NetworkCIDRsEmptyFiltersClaimAllBridge32s(t *testing.T) {
	_, cancel, _, _ := startScenario(t)
	defer cancel()

	const strayIP = "198.51.100.99"
	addStrayBridgeRoute(t, strayIP)

	// Sanity: kernel actually has the route before the agent starts.
	testenv.AssertKernelRoute(t, strayIP, 1*time.Second)

	cfg := testenv.FastDefaults()
	// Intentionally no MakeLocalRouter and no NetworkCIDRs: this drives the
	// reconcile into the empty-filters branch. The state under test is
	// precisely "operator has not configured a manual filter and there are
	// no local routers right now" — a routine condition on chassis between
	// gateway moves, not an exotic edge case.
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Within a couple of reconcile ticks the agent must observe the stray
	// /32 as managed-but-not-desired and remove it. FastDefaults ticks at
	// 2s; allow ~3 ticks before failing.
	testenv.AssertNoKernelRoute(t, strayIP, 8*time.Second)
}

// addStrayBridgeRoute plants a /32 on the bridge with the agent's own protocol
// number (rtproto 44). The agent's ListKernelRoutes ignores protocol — it
// returns every /32 on the bridge — so the planted route is observable to the
// agent's reconcile loop the same way a leftover from a previous run would be.
func addStrayBridgeRoute(t *testing.T, ip string) {
	t.Helper()
	args := []string{"route", "add", ip + "/32", "dev", testenv.DefaultBridgeDev, "proto", "44"}
	if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatalf("ip %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}
