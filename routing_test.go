package main

import (
	"errors"
	"net"
	"testing"
)

func TestIsNoSuchRoute(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no such process", errors.New("no such process"), true},
		{"wrapped no such process", errors.New("netlink: del route: no such process"), true},
		{"other error", errors.New("permission denied"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNoSuchRoute(tt.err)
			if got != tt.want {
				t.Errorf("isNoSuchRoute(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNewRouteManager(t *testing.T) {
	cfg := Config{
		BridgeDev:   "br-ex",
		VRFName:     "vrf-provider",
		VethNexthop: "169.254.0.1",
	}

	rm := NewRouteManager(cfg)

	if rm.bridgeDev != "br-ex" {
		t.Errorf("bridgeDev = %q, want %q", rm.bridgeDev, "br-ex")
	}
	if rm.vrfName != "vrf-provider" {
		t.Errorf("vrfName = %q, want %q", rm.vrfName, "vrf-provider")
	}
	if rm.vethNexthop != "169.254.0.1" {
		t.Errorf("vethNexthop = %q, want %q", rm.vethNexthop, "169.254.0.1")
	}
	if rm.routeTableID != 0 {
		t.Errorf("routeTableID = %d, want 0", rm.routeTableID)
	}
	if rm.dryRun {
		t.Error("dryRun should be false by default")
	}
}

func TestNewRouteManagerWithTableID(t *testing.T) {
	cfg := Config{
		BridgeDev:    "br-ex",
		VRFName:      "vrf-provider",
		VethNexthop:  "169.254.0.1",
		RouteTableID: 100,
	}

	rm := NewRouteManager(cfg)

	if rm.routeTableID != 100 {
		t.Errorf("routeTableID = %d, want 100", rm.routeTableID)
	}
}

func TestDryRunBridgeIP(t *testing.T) {
	rm := &RouteManager{
		bridgeDev: "br-ex",
		dryRun:    true,
	}

	if err := rm.EnsureBridgeIP("169.254.169.254"); err != nil {
		t.Errorf("EnsureBridgeIP() in dry-run should not error, got: %v", err)
	}
	if err := rm.RemoveBridgeIP("169.254.169.254"); err != nil {
		t.Errorf("RemoveBridgeIP() in dry-run should not error, got: %v", err)
	}
}

func TestDryRunOVSFlows(t *testing.T) {
	rm := &RouteManager{
		bridgeDev: "br-ex",
		dryRun:    true,
	}

	if err := rm.EnsureOVSFlows(); err != nil {
		t.Errorf("EnsureOVSFlows() in dry-run should not error, got: %v", err)
	}
	if err := rm.RemoveOVSFlows(); err != nil {
		t.Errorf("RemoveOVSFlows() in dry-run should not error, got: %v", err)
	}
}

func TestNewRouteManagerDryRun(t *testing.T) {
	cfg := Config{
		BridgeDev:   "br-ex",
		VRFName:     "vrf-provider",
		VethNexthop: "169.254.0.1",
		DryRun:      true,
	}

	rm := NewRouteManager(cfg)

	if !rm.dryRun {
		t.Error("dryRun should be true when config has DryRun=true")
	}
}

func TestDryRunFRRRoutes(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}

	if err := rm.AddFRRRoute("10.0.0.1"); err != nil {
		t.Errorf("AddFRRRoute() in dry-run should not error, got: %v", err)
	}
	if err := rm.DelFRRRoute("10.0.0.1"); err != nil {
		t.Errorf("DelFRRRoute() in dry-run should not error, got: %v", err)
	}
}

func TestDryRunFRRRoutesBatch(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if err := rm.AddFRRRoutes(ips); err != nil {
		t.Errorf("AddFRRRoutes() in dry-run should not error, got: %v", err)
	}
	if err := rm.DelFRRRoutes(ips); err != nil {
		t.Errorf("DelFRRRoutes() in dry-run should not error, got: %v", err)
	}
}

func TestFRRRoutesBatchEmpty(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
	}

	if err := rm.AddFRRRoutes(nil); err != nil {
		t.Errorf("AddFRRRoutes(nil) should be no-op, got: %v", err)
	}
	if err := rm.DelFRRRoutes(nil); err != nil {
		t.Errorf("DelFRRRoutes(nil) should be no-op, got: %v", err)
	}
	if err := rm.AddFRRRoutes([]string{}); err != nil {
		t.Errorf("AddFRRRoutes([]) should be no-op, got: %v", err)
	}
	if err := rm.DelFRRRoutes([]string{}); err != nil {
		t.Errorf("DelFRRRoutes([]) should be no-op, got: %v", err)
	}
}

func TestDryRunRefreshBGP(t *testing.T) {
	rm := &RouteManager{
		vrfName: "vrf-provider",
		dryRun:  true,
	}
	if err := rm.RefreshBGP(); err != nil {
		t.Errorf("RefreshBGP() in dry-run should not error, got: %v", err)
	}
}

func TestFRRRoutesBatchValidation(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}

	invalid := []string{"10.0.0.1", "not-an-ip", "10.0.0.2"}
	if err := rm.AddFRRRoutes(invalid); err == nil {
		t.Error("AddFRRRoutes() with invalid IP should return error")
	}
	if err := rm.DelFRRRoutes(invalid); err == nil {
		t.Error("DelFRRRoutes() with invalid IP should return error")
	}

	// CIDR notation is not a valid bare IP.
	cidr := []string{"10.0.0.1/32"}
	if err := rm.AddFRRRoutes(cidr); err == nil {
		t.Error("AddFRRRoutes() with CIDR notation should return error")
	}
}

func TestNewRouteManagerVethLeak(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	cfg := Config{
		BridgeDev:            "br-ex",
		VRFName:              "vrf-provider",
		VethNexthop:          "169.254.0.1",
		VethLeakEnabled:      true,
		VethProviderIP:       "169.254.0.2",
		VethLeakTableID:      200,
		VethLeakRulePriority: 2000,
		NetworkFilters:       []*net.IPNet{cidr},
	}

	rm := NewRouteManager(cfg)

	if !rm.vethLeakEnabled {
		t.Error("vethLeakEnabled should be true")
	}
	if rm.vethProviderIP != "169.254.0.2" {
		t.Errorf("vethProviderIP = %q, want %q", rm.vethProviderIP, "169.254.0.2")
	}
	if rm.vethLeakTableID != 200 {
		t.Errorf("vethLeakTableID = %d, want 200", rm.vethLeakTableID)
	}
	if rm.vethLeakRulePriority != 2000 {
		t.Errorf("vethLeakRulePriority = %d, want 2000", rm.vethLeakRulePriority)
	}
	if len(rm.networkFilters) != 1 {
		t.Errorf("networkFilters length = %d, want 1", len(rm.networkFilters))
	}
}

func TestDryRunVethLeak(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		vrfName:         "vrf-provider",
		vethNexthop:     "169.254.0.1",
		vethLeakEnabled: true,
		vethProviderIP:  "169.254.0.2",
		vethLeakTableID: 200,
		dryRun:          true,
	}

	if err := rm.SetupVethLeak(); err != nil {
		t.Errorf("SetupVethLeak() in dry-run should not error, got: %v", err)
	}
	if err := rm.TeardownVethLeak(); err != nil {
		t.Errorf("TeardownVethLeak() in dry-run should not error, got: %v", err)
	}
}

func TestDisabledVethLeak(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		vrfName:         "vrf-provider",
		vethNexthop:     "169.254.0.1",
		vethLeakEnabled: false,
	}

	if err := rm.SetupVethLeak(); err != nil {
		t.Errorf("SetupVethLeak() when disabled should not error, got: %v", err)
	}
	if err := rm.TeardownVethLeak(); err != nil {
		t.Errorf("TeardownVethLeak() when disabled should not error, got: %v", err)
	}
}

func TestNewRouteManagerFRRPrefixList(t *testing.T) {
	cfg := Config{
		BridgeDev:     "br-ex",
		VRFName:       "vrf-provider",
		VethNexthop:   "169.254.0.1",
		FRRPrefixList: "ANNOUNCED-NETWORKS",
	}
	rm := NewRouteManager(cfg)
	if rm.frrPrefixList != "ANNOUNCED-NETWORKS" {
		t.Errorf("frrPrefixList = %q, want %q", rm.frrPrefixList, "ANNOUNCED-NETWORKS")
	}
}

func TestReconcileFRRPrefixListDisabled(t *testing.T) {
	rm := &RouteManager{frrPrefixList: ""}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcileFRRPrefixList() with empty name should be no-op, got: %v", err)
	}
}

func TestReconcileFRRPrefixListDryRun(t *testing.T) {
	rm := &RouteManager{frrPrefixList: "ANNOUNCED-NETWORKS", dryRun: true}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcileFRRPrefixList() in dry-run should not error, got: %v", err)
	}
}

func TestReconcileVethLeakNetworksDisabled(t *testing.T) {
	rm := &RouteManager{vethLeakEnabled: false}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileVethLeakNetworks([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcileVethLeakNetworks() when disabled should be no-op, got: %v", err)
	}
}

func TestReconcileVethLeakNetworksDryRun(t *testing.T) {
	rm := &RouteManager{vethLeakEnabled: true, dryRun: true}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileVethLeakNetworks([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcileVethLeakNetworks() in dry-run should not error, got: %v", err)
	}
}

func TestNewRouteManagerPortForward(t *testing.T) {
	cfg := Config{
		BridgeDev:          "br-ex",
		VRFName:            "vrf-provider",
		VethNexthop:        "169.254.0.1",
		PortForwardEnabled: true,
		PortForwardDev:     "loopback1",
		PortForwardTableID: 202,
		PortForwards: []PortForwardVIP{
			{
				VIP:       "198.51.100.10",
				ManageVIP: true,
				Rules: []PortForwardRule{
					{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
				},
			},
		},
	}
	rm := NewRouteManager(cfg)

	if !rm.portForwardEnabled {
		t.Error("portForwardEnabled should be true")
	}
	if rm.portForwardDev != "loopback1" {
		t.Errorf("portForwardDev = %q, want %q", rm.portForwardDev, "loopback1")
	}
	if rm.portForwardTableID != 202 {
		t.Errorf("portForwardTableID = %d, want %d", rm.portForwardTableID, 202)
	}
	if len(rm.portForwards) != 1 {
		t.Errorf("len(portForwards) = %d, want 1", len(rm.portForwards))
	}
	if rm.portForwardL3mdevAccept {
		t.Error("portForwardL3mdevAccept should default to false")
	}
}

func TestNewRouteManagerPortForwardL3mdevAccept(t *testing.T) {
	cfg := Config{
		BridgeDev:               "br-ex",
		VRFName:                 "vrf-provider",
		VethNexthop:             "169.254.0.1",
		PortForwardEnabled:      true,
		PortForwardDev:          "loopback1",
		PortForwardTableID:      202,
		PortForwardL3mdevAccept: true,
		PortForwards: []PortForwardVIP{
			{
				VIP:       "198.51.100.10",
				ManageVIP: true,
				Rules: []PortForwardRule{
					{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
				},
			},
		},
	}
	rm := NewRouteManager(cfg)

	if !rm.portForwardL3mdevAccept {
		t.Error("portForwardL3mdevAccept should be true when explicitly set")
	}
}

func TestDryRunPortForward(t *testing.T) {
	cfg := Config{
		BridgeDev:          "br-ex",
		VRFName:            "vrf-provider",
		VethNexthop:        "169.254.0.1",
		DryRun:             true,
		PortForwardEnabled: true,
		PortForwardDev:     "loopback1",
		PortForwardTableID: 201,
		PortForwards: []PortForwardVIP{
			{VIP: "198.51.100.10", Rules: []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"}}},
		},
	}
	rm := NewRouteManager(cfg)

	if err := rm.SetupPortForward(); err != nil {
		t.Errorf("SetupPortForward() dry-run error: %v", err)
	}

	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	if err := rm.ReconcilePortForward([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcilePortForward() dry-run error: %v", err)
	}

	if err := rm.TeardownPortForward(); err != nil {
		t.Errorf("TeardownPortForward() dry-run error: %v", err)
	}
}

func TestDisabledPortForward(t *testing.T) {
	cfg := Config{
		BridgeDev:          "br-ex",
		VRFName:            "vrf-provider",
		VethNexthop:        "169.254.0.1",
		PortForwardEnabled: false,
	}
	rm := NewRouteManager(cfg)

	if err := rm.SetupPortForward(); err != nil {
		t.Errorf("SetupPortForward() disabled error: %v", err)
	}
	if err := rm.ReconcilePortForward(nil); err != nil {
		t.Errorf("ReconcilePortForward() disabled error: %v", err)
	}
	if err := rm.TeardownPortForward(); err != nil {
		t.Errorf("TeardownPortForward() disabled error: %v", err)
	}
}

func TestValidateIP(t *testing.T) {
	tests := []struct {
		ip      string
		wantErr bool
	}{
		{"10.0.0.1", false},
		{"192.168.1.1", false},
		{"255.255.255.255", false},
		{"::1", false},
		{"", true},
		{"not-an-ip", true},
		{"10.0.0.1/32", true},
		{"10.0.0", true},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			err := validateIP(tt.ip)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateIP(%q) error = %v, wantErr %v", tt.ip, err, tt.wantErr)
			}
		})
	}
}
