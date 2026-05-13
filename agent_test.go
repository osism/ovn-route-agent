package main

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUniqueIPs(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{"nil input", nil, nil},
		{"empty input", []string{}, nil},
		{"single IP", []string{"10.0.0.1"}, []string{"10.0.0.1"}},
		{"duplicates removed", []string{"10.0.0.2", "10.0.0.1", "10.0.0.2"}, []string{"10.0.0.1", "10.0.0.2"}},
		{"sorted output", []string{"10.0.0.3", "10.0.0.1", "10.0.0.2"}, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}},
		{"whitespace trimmed", []string{" 10.0.0.1 ", "10.0.0.2\t"}, []string{"10.0.0.1", "10.0.0.2"}},
		{"empty strings filtered", []string{"", "10.0.0.1", "", "10.0.0.2", " "}, []string{"10.0.0.1", "10.0.0.2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uniqueIPs(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("uniqueIPs(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsManaged(t *testing.T) {
	tests := []struct {
		name  string
		cidrs []string
		ip    string
		want  bool
	}{
		{"no filter matches all", nil, "192.168.1.1", true},
		{"matching CIDR /24", []string{"10.0.0.0/24"}, "10.0.0.5", true},
		{"non-matching CIDR", []string{"10.0.0.0/24"}, "192.168.1.1", false},
		{"broader CIDR /16 matches", []string{"10.0.0.0/16"}, "10.0.1.5", true},
		{"narrower CIDR /24 excludes", []string{"10.0.0.0/24"}, "10.0.1.5", false},
		{"boundary IP included", []string{"10.0.0.0/24"}, "10.0.0.255", true},
		{"invalid IP returns false", []string{"10.0.0.0/24"}, "not-an-ip", false},
		{"multiple CIDRs first matches", []string{"10.0.0.0/24", "172.16.0.0/12"}, "10.0.0.5", true},
		{"multiple CIDRs second matches", []string{"10.0.0.0/24", "172.16.0.0/12"}, "172.20.0.1", true},
		{"multiple CIDRs none matches", []string{"10.0.0.0/24", "172.16.0.0/12"}, "192.168.1.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var filters []*net.IPNet
			for _, cidrStr := range tt.cidrs {
				_, cidr, err := net.ParseCIDR(cidrStr)
				if err != nil {
					t.Fatalf("ParseCIDR(%q) error: %v", cidrStr, err)
				}
				filters = append(filters, cidr)
			}
			a := &Agent{effectiveFilters: filters}
			got := a.isManaged(tt.ip)
			if got != tt.want {
				t.Errorf("isManaged(%q) with cidrs %v = %v, want %v", tt.ip, tt.cidrs, got, tt.want)
			}
		})
	}
}

func TestComputeEffectiveNetworks(t *testing.T) {
	_, manual, _ := net.ParseCIDR("10.0.0.0/24")
	_, discovered, _ := net.ParseCIDR("198.51.100.0/24")

	t.Run("manual config takes precedence", func(t *testing.T) {
		a := &Agent{cfg: Config{NetworkFilters: []*net.IPNet{manual}}}
		eff := a.computeEffectiveNetworks([]*net.IPNet{discovered})
		if len(eff) != 1 || eff[0].String() != "10.0.0.0/24" {
			t.Errorf("expected manual config, got %v", eff)
		}
	})

	t.Run("auto-discovery when no manual config", func(t *testing.T) {
		a := &Agent{cfg: Config{}}
		eff := a.computeEffectiveNetworks([]*net.IPNet{discovered})
		if len(eff) != 1 || eff[0].String() != "198.51.100.0/24" {
			t.Errorf("expected discovered network, got %v", eff)
		}
	})

	t.Run("nil when nothing configured or discovered", func(t *testing.T) {
		a := &Agent{cfg: Config{}}
		eff := a.computeEffectiveNetworks(nil)
		if len(eff) != 0 {
			t.Errorf("expected empty, got %v", eff)
		}
	})
}

func TestIsManagedWithEffectiveFilters(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{effectiveFilters: []*net.IPNet{cidr}}

	if !a.isManaged("198.51.100.10") {
		t.Error("198.51.100.10 should be managed within 198.51.100.0/24")
	}
	if a.isManaged("10.0.0.1") {
		t.Error("10.0.0.1 should not be managed outside 198.51.100.0/24")
	}

	// With nil effectiveFilters, all IPs are managed.
	a.effectiveFilters = nil
	if !a.isManaged("10.0.0.1") {
		t.Error("all IPs should be managed when effectiveFilters is nil")
	}
}

func TestTriggerReconcile(t *testing.T) {
	a := &Agent{
		reconcileCh: make(chan struct{}, 1),
	}

	// First trigger should succeed.
	a.triggerReconcile()
	select {
	case <-a.reconcileCh:
		// ok
	default:
		t.Error("expected reconcile signal, got none")
	}

	// Second trigger without draining should not block.
	a.triggerReconcile()
	a.triggerReconcile() // Should not block even with full channel.
}

func TestVerifyRoutesDryRun(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	a := &Agent{
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
	}

	// In dry-run mode, ListFRRRoutes and ListKernelRoutes return empty
	// lists (nil, nil). This means verifyRoutes sees every desired IP as
	// missing and attempts re-adds — but those are also dry-run no-ops.
	// This is by design: we exercise the full code path without side effects.
	n := a.verifyRoutes([]string{"10.0.0.1", "10.0.0.2", "10.0.0.3"})
	if n != 6 { // 3 FRR + 3 kernel
		t.Errorf("expected 6 re-adds in dry-run, got %d", n)
	}
}

func TestVerifyRoutesSkipsUnmanagedIPs(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	a := &Agent{
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
	}

	// IPs outside the managed CIDR should be skipped — zero re-adds.
	n := a.verifyRoutes([]string{"192.168.1.1", "172.16.0.1"})
	if n != 0 {
		t.Errorf("expected 0 re-adds for unmanaged IPs, got %d", n)
	}
}

func TestVerifyRoutesEmptyDesired(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	a := &Agent{routing: rm}

	// Empty desired list should be a no-op.
	if n := a.verifyRoutes(nil); n != 0 {
		t.Errorf("expected 0 re-adds for nil desired, got %d", n)
	}
	if n := a.verifyRoutes([]string{}); n != 0 {
		t.Errorf("expected 0 re-adds for empty desired, got %d", n)
	}
}

func TestVerifyRoutesConsecutiveReAddCounter(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	a := &Agent{
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
	}

	// Simulate multiple consecutive cycles with missing routes (dry-run
	// always reports all routes as missing since list calls return nil).
	desired := []string{"10.0.0.1"}
	for i := 1; i <= 5; i++ {
		a.verifyRoutes(desired)
		if a.consecutiveReAdds != i {
			t.Errorf("after cycle %d: expected consecutiveReAdds=%d, got %d", i, i, a.consecutiveReAdds)
		}
	}

	// A cycle with no managed IPs (nothing to re-add) resets the counter.
	a.verifyRoutes([]string{"192.168.1.1"}) // unmanaged → 0 re-adds
	if a.consecutiveReAdds != 0 {
		t.Errorf("expected consecutiveReAdds=0 after clean cycle, got %d", a.consecutiveReAdds)
	}
}

// TestReconcileNoLocalRoutersInvokesRemoveAllRoutes drives reconcile down
// the inactive-chassis branch: with no local routers and no port forwards,
// the agent calls removeAllRoutes("no locally active routers …") and
// cleans up veth-leak, prefix-list, and hairpin-flow state.
func TestReconcileNoLocalRoutersInvokesRemoveAllRoutes(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	// state.HasLocalRouters defaults to false; DiscoveredNetworks empty.
	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}
	a.reconcile(context.Background(), "test")
	// Reconcile must complete and leave effectiveFilters in a clean state.
	if len(a.effectiveFilters) != 0 {
		t.Errorf("effectiveFilters should be empty when no networks discovered, got %v", a.effectiveFilters)
	}
}

// TestRemoveAllRoutesDryRun exercises removeAllRoutes end-to-end. In dry-run
// mode List* helpers return (nil, nil) so the function walks every branch
// (FRR list, kernel list, BGP refresh skipped because no routes to remove).
func TestRemoveAllRoutesDryRun(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	a := &Agent{routing: rm}
	a.removeAllRoutes("test reason")
}

// TestEnsureRoutesAddsMissingAndRemovesStale drives ensureRoutes with a
// vtysh hook so the FRR add/delete paths and the BGP-refresh-on-delete
// branch all execute. Kernel route helpers fail (bridge does not exist or
// platform is non-Linux), which is expected and survives as a logged
// warning — that path is itself exercised.
func TestEnsureRoutesAddsMissingAndRemovesStale(t *testing.T) {
	rec := newVtyshRecorder()
	// FRR currently has B (already desired) and stale-X (managed but not desired).
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		`S>* 198.51.100.20/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
S>* 198.51.100.99/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
`,
		nil,
	)

	rm := &RouteManager{
		bridgeDev:     "ovnagent-nonexistent-br",
		vrfName:       "vrf-provider",
		vethNexthop:   "169.254.0.1",
		execVtyshHook: rec.hook(),
	}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	// Desired: 198.51.100.10 (new) and 198.51.100.20 (already in FRR).
	a.ensureRoutes([]string{"198.51.100.10", "198.51.100.20"})

	var sawAdd, sawDel, sawRefresh bool
	for _, c := range rec.calls {
		joined := strings.Join(c, " ")
		switch {
		case strings.Contains(joined, "show ip route vrf"):
			continue
		case strings.Contains(joined, "ip route 198.51.100.10/32 169.254.0.1") &&
			!strings.Contains(joined, "no ip route"):
			sawAdd = true
		case strings.Contains(joined, "no ip route 198.51.100.99/32"):
			sawDel = true
		case strings.Contains(joined, "clear ip bgp vrf vrf-provider"):
			sawRefresh = true
		}
	}
	if !sawAdd {
		t.Errorf("expected add of 198.51.100.10, got calls: %v", rec.calls)
	}
	if !sawDel {
		t.Errorf("expected del of stale 198.51.100.99, got calls: %v", rec.calls)
	}
	if !sawRefresh {
		t.Errorf("expected BGP refresh after deletes, got calls: %v", rec.calls)
	}
}

// TestRemoveAllRoutesWithStubbedFRRList exercises the FRR-driven removal
// path: a stub vtysh hook reports two managed routes, the agent batches
// the deletion, and a BGP soft-refresh follows because routes were removed.
func TestRemoveAllRoutesWithStubbedFRRList(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		`S>* 198.51.100.10/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
S>* 198.51.100.11/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
`,
		nil,
	)
	rm := &RouteManager{
		// Use a synthetic bridge name that does not exist on either macOS
		// or Linux CI hosts so ListKernelRoutes errors out (netlink) or
		// returns "only supported on Linux" (stub) instead of touching real
		// kernel state. dryRun is intentionally false here because the
		// FRR-list short-circuits to nil in dry-run mode and would skip
		// the code path we want to exercise.
		bridgeDev:     "ovnagent-nonexistent-br",
		vrfName:       "vrf-provider",
		vethNexthop:   "169.254.0.1",
		execVtyshHook: rec.hook(),
	}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	a.removeAllRoutes("test")

	// Expect: list FRR (1), batch delete (1), BGP soft-refresh (1).
	var sawDel, sawRefresh bool
	for _, c := range rec.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "no ip route 198.51.100.10/32") &&
			strings.Contains(joined, "no ip route 198.51.100.11/32") {
			sawDel = true
		}
		if strings.Contains(joined, "clear ip bgp vrf vrf-provider") {
			sawRefresh = true
		}
	}
	if !sawDel {
		t.Errorf("expected batched delete of both managed IPs, got calls: %v", rec.calls)
	}
	if !sawRefresh {
		t.Errorf("expected BGP soft-refresh after deletes, got calls: %v", rec.calls)
	}
}

// TestCleanupRunsShutdownPipeline drives the agent's cleanup() in dry-run
// mode so each step (FRR routes, prefix-list, OVS flows, routing table,
// port forwards, veth leak, bridge IP, OVN managed entries) executes without
// touching real system state. The OVN nb client is a fake so the final
// RemoveManagedNBEntries call uses the in-memory rows.
func TestCleanupRunsShutdownPipeline(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	a := &Agent{
		cfg:     Config{BridgeIP: "169.254.169.254"},
		ovn:     c,
		routing: rm,
	}
	// Must not panic; all sub-calls are dry-run no-ops or interact with the
	// fake OVN client (no in-memory routers → RemoveManagedNBEntries early returns).
	a.cleanup()
}

// TestCleanupStaleChassis_TracksAndPrunes verifies the full tracking flow:
// (1) a chassis referenced by a managed route but missing from allChassis is
// added to missingChassis; (2) a chassis that returns is removed; (3) a
// chassis no longer referenced is pruned without waiting for grace.
func TestCleanupStaleChassis_TracksAndPrunes(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Logical_Router_Static_Route",
		&NBLogicalRouterStaticRoute{
			UUID: "r-dead",
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-gone",
			},
		},
		&NBLogicalRouterStaticRoute{
			UUID: "r-alive",
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-a",
			},
		},
	)

	a := &Agent{
		cfg:            Config{StaleChassisGracePeriod: 5 * time.Minute},
		ovn:            c,
		missingChassis: make(map[string]time.Time),
	}

	// First call: host-gone is missing, host-a is alive.
	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true})
	if _, tracked := a.missingChassis["host-gone"]; !tracked {
		t.Errorf("expected host-gone to be tracked as missing, got %v", a.missingChassis)
	}
	if _, tracked := a.missingChassis["host-a"]; tracked {
		t.Errorf("host-a is alive and must not be tracked as missing")
	}

	// Second call: host-gone returns; it must be removed from tracking.
	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true, "host-gone": true})
	if _, tracked := a.missingChassis["host-gone"]; tracked {
		t.Error("host-gone returned and must be removed from missingChassis")
	}

	// Third call: nb has only routes for host-a (host-gone route was deleted
	// elsewhere). missingChassis must be pruned even though grace would still apply.
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID: "r-alive",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-a",
		},
	})
	a.missingChassis["stale-record"] = time.Now() // synthetic stale entry
	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true})
	if _, tracked := a.missingChassis["stale-record"]; tracked {
		t.Error("stale-record is unreferenced and must be pruned")
	}
}

// TestCleanupStaleChassis_TriggersCleanupAfterGrace verifies that a chassis
// missing for longer than the configured grace period causes the agent to
// call CleanupStaleChassisManagedEntries against the OVN client.
func TestCleanupStaleChassis_TriggersCleanupAfterGrace(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-1", Name: "router1", StaticRoutes: []string{"r-stale"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID: "r-stale",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-gone",
		},
	})

	a := &Agent{
		cfg:            Config{StaleChassisGracePeriod: time.Millisecond},
		ovn:            c,
		missingChassis: make(map[string]time.Time),
	}
	// Pre-seed the tracker so the grace period has already elapsed.
	a.missingChassis["host-gone"] = time.Now().Add(-time.Hour)

	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true})

	tx := nb.recordedTransacts()
	if len(tx) == 0 {
		t.Fatal("expected CleanupStaleChassisManagedEntries to issue at least one transact")
	}
	// host-gone should be removed from the tracker after successful cleanup.
	if _, tracked := a.missingChassis["host-gone"]; tracked {
		t.Error("host-gone should be removed from missingChassis after grace-period cleanup")
	}
}

// TestCleanupStaleChassis_BailsOnListError verifies that an OVN list error
// short-circuits cleanupStaleChassis: it returns early and does not mutate
// missingChassis state.
func TestCleanupStaleChassis_BailsOnListError(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.listErr = errors.New("connection refused")

	a := &Agent{
		cfg:            Config{StaleChassisGracePeriod: time.Minute},
		ovn:            c,
		missingChassis: make(map[string]time.Time),
	}
	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true})
	if len(a.missingChassis) != 0 {
		t.Errorf("missingChassis should not be mutated on list error, got %v", a.missingChassis)
	}
}

func TestCleanupStaleChassisDisabledWhenZero(t *testing.T) {
	a := &Agent{
		cfg:            Config{StaleChassisGracePeriod: 0},
		missingChassis: make(map[string]time.Time),
	}

	// Should return immediately without touching missingChassis.
	a.cleanupStaleChassis(context.Background(), map[string]bool{"node-1": true})

	if len(a.missingChassis) != 0 {
		t.Errorf("expected empty missingChassis when grace period is 0, got %v", a.missingChassis)
	}
}

func TestNewAgentInitializesMissingChassis(t *testing.T) {
	cfg := Config{
		VethNexthop: "169.254.0.1",
		VRFName:     "vrf-provider",
	}
	a, err := NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent() error: %v", err)
	}
	if a.missingChassis == nil {
		t.Error("missingChassis should be initialized")
	}
	if len(a.missingChassis) != 0 {
		t.Errorf("missingChassis should be empty, got %v", a.missingChassis)
	}
	if a.staleCleanupJitter < 0 || a.staleCleanupJitter > maxStaleCleanupJitter {
		t.Errorf("staleCleanupJitter = %v, should be in [0, %v]", a.staleCleanupJitter, maxStaleCleanupJitter)
	}
}

// TestConnectWithRetry_SucceedsFirstTry verifies the happy path: when connect
// returns nil on the first call, the helper returns nil immediately without
// sleeping for retryInterval.
func TestConnectWithRetry_SucceedsFirstTry(t *testing.T) {
	var calls atomic.Int32
	connect := func(context.Context) error {
		calls.Add(1)
		return nil
	}

	start := time.Now()
	if err := connectWithRetry(context.Background(), connect, time.Hour); err != nil {
		t.Fatalf("connectWithRetry: unexpected error: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("connect calls = %d, want 1", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("returned in %s, expected near-instant on first-try success", elapsed)
	}
}

// TestConnectWithRetry_RetriesOnFailure verifies the retry path: when connect
// fails several times, the helper keeps calling it without returning the
// error, until it eventually succeeds. The retry interval is set short so
// the test stays cheap.
func TestConnectWithRetry_RetriesOnFailure(t *testing.T) {
	var calls atomic.Int32
	const wantCalls = 3
	connect := func(context.Context) error {
		if calls.Add(1) < wantCalls {
			return errors.New("connection refused")
		}
		return nil
	}

	if err := connectWithRetry(context.Background(), connect, 10*time.Millisecond); err != nil {
		t.Fatalf("connectWithRetry: unexpected error: %v", err)
	}
	if got := calls.Load(); got != wantCalls {
		t.Errorf("connect calls = %d, want %d", got, wantCalls)
	}
}

// TestConnectWithRetry_ReturnsCtxErrOnCancel verifies that a context cancelled
// while the helper is in the retry-wait branch yields ctx.Err() (not a
// generic timeout, not a panic, not a swallowed error). This is the
// production contract that lets a SIGTERM during cold-start retry exit
// cleanly.
func TestConnectWithRetry_ReturnsCtxErrOnCancel(t *testing.T) {
	var calls atomic.Int32
	connect := func(context.Context) error {
		calls.Add(1)
		return errors.New("always fails")
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after entering the retry-wait branch; the retry
	// interval is long enough that the helper is definitely waiting on
	// either ctx.Done or the timer when the cancel fires.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := connectWithRetry(ctx, connect, 10*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("connectWithRetry err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("helper took %s to honour cancel — should return promptly", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("connect calls = %d, want 1 (one failed attempt before cancel)", got)
	}
}

func TestMissingChassisGracePeriodTracking(t *testing.T) {
	// Directly test the missingChassis map tracking logic.
	a := &Agent{
		cfg:                Config{StaleChassisGracePeriod: 5 * time.Minute},
		missingChassis:     make(map[string]time.Time),
		staleCleanupJitter: 0, // No jitter for deterministic testing.
	}

	now := time.Now()

	// Simulate: chassis "dead-node" was first seen missing 6 minutes ago.
	a.missingChassis["dead-node"] = now.Add(-6 * time.Minute)

	// Simulate: chassis "rebooting-node" was first seen missing 2 minutes ago.
	a.missingChassis["rebooting-node"] = now.Add(-2 * time.Minute)

	effectiveGrace := a.cfg.StaleChassisGracePeriod + a.staleCleanupJitter

	// Check which chassis have exceeded the grace period.
	var stale []string
	for name, firstSeen := range a.missingChassis {
		if now.Sub(firstSeen) >= effectiveGrace {
			stale = append(stale, name)
		}
	}

	if len(stale) != 1 || stale[0] != "dead-node" {
		t.Errorf("expected only dead-node to be stale, got %v", stale)
	}

	// Simulate: rebooting-node comes back.
	allChassis := map[string]bool{"rebooting-node": true}
	for name := range a.missingChassis {
		if allChassis[name] {
			delete(a.missingChassis, name)
		}
	}
	if _, tracked := a.missingChassis["rebooting-node"]; tracked {
		t.Error("rebooting-node should have been removed from missingChassis after returning")
	}
	if _, tracked := a.missingChassis["dead-node"]; !tracked {
		t.Error("dead-node should still be in missingChassis")
	}
}

// reconcile takes ctx and passes it to OVN methods that issue OVSDB
// transactions, but it does not loop or block, so a pre-cancelled context
// should never make it hang or panic — it should simply complete promptly
// while the ctx-aware OVN calls observe the cancel and return early. With
// a dry-run RouteManager and a fake OVN client carrying populated state,
// the test exercises the full reconcile body to lock in that "context
// cancellation while a reconcile is in flight" stays a no-side-effects
// fast-return, not a partial-write hang.
func TestReconcileCompletesPromptlyOnCancelledContext(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	// Populate state so reconcile takes the HasLocalRouters branch and
	// therefore reaches the ctx-aware EnsureGatewayRouting /
	// EnsureActivePriorityLead calls.
	c.state.LocalRouters = []LocalRouterInfo{
		{
			RouterName:  "router1",
			RouterUUID:  "lr-1",
			LRPName:     "lrp-abc",
			LRPMAC:      "aa:aa:aa:aa:aa:aa",
			LRPNetworks: []string{"198.51.100.0/24"},
		},
	}
	c.state.HasLocalRouters = true
	c.state.DiscoveredNetworks = []*net.IPNet{cidr}

	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE reconcile starts — the strict "mid-cycle" case

	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		// reconcile must not panic even with a cancelled ctx.
		a.reconcile(ctx, "test")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("reconcile did not return within 2s on cancelled ctx — possible hang")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("reconcile returned in %s, expected near-instant on cancelled ctx", elapsed)
	}

	// effectiveFilters is recomputed each cycle and is the only piece of
	// agent state mutated by reconcile. With HasLocalRouters=true and a
	// discovered /24, the slice must be set — i.e. reconcile completed its
	// state-derivation phase rather than aborting mid-flight in a way that
	// would leave the agent half-initialised on the next tick.
	if len(a.effectiveFilters) != 1 || a.effectiveFilters[0].String() != "198.51.100.0/24" {
		t.Errorf("effectiveFilters = %v, want [198.51.100.0/24]", a.effectiveFilters)
	}
}
