package main

import (
	"net"
	"reflect"
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

func TestCleanupStaleChassisDisabledWhenZero(t *testing.T) {
	a := &Agent{
		cfg:            Config{StaleChassisGracePeriod: 0},
		missingChassis: make(map[string]time.Time),
	}

	// Should return immediately without touching missingChassis.
	a.cleanupStaleChassis(map[string]bool{"node-1": true})

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
