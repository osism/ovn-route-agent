package main

import (
	"net"
	"reflect"
	"testing"
)

func TestOvsdbEndpoints(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		want   []string
	}{
		{"single unix", "unix:/var/run/ovn/ovnsb_db.sock", []string{"unix:/var/run/ovn/ovnsb_db.sock"}},
		{"single tcp", "tcp:10.0.0.1:6642", []string{"tcp:10.0.0.1:6642"}},
		{"multiple endpoints", "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642", []string{"tcp:10.0.0.1:6642", "tcp:10.0.0.2:6642"}},
		{"with whitespace", " tcp:10.0.0.1:6642 , tcp:10.0.0.2:6642 ", []string{"tcp:10.0.0.1:6642", "tcp:10.0.0.2:6642"}},
		{"trailing comma", "tcp:10.0.0.1:6642,", []string{"tcp:10.0.0.1:6642"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ovsdbEndpoints(tt.remote)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ovsdbEndpoints(%q) = %v, want %v", tt.remote, got, tt.want)
			}
		})
	}
}

func TestGetHostname(t *testing.T) {
	h, err := getHostname()
	if err != nil {
		t.Fatalf("getHostname() error: %v", err)
	}
	if h == "" {
		t.Error("getHostname() returned empty string")
	}
	// Should not contain dots (FQDN stripped).
	for _, c := range h {
		if c == '.' {
			t.Errorf("getHostname() = %q, should not contain dots", h)
			break
		}
	}
}

func TestNewOVNClient(t *testing.T) {
	called := false
	cb := func() { called = true }

	cfg := Config{
		OVNSBRemote: "unix:/tmp/sb.sock",
		OVNNBRemote: "unix:/tmp/nb.sock",
	}

	c := NewOVNClient(cfg, cb)

	if c.state == nil {
		t.Fatal("state should not be nil")
	}
	if c.cfg.OVNSBRemote != cfg.OVNSBRemote {
		t.Errorf("cfg.OVNSBRemote = %q, want %q", c.cfg.OVNSBRemote, cfg.OVNSBRemote)
	}

	c.onChange()
	if !called {
		t.Error("onChange callback was not invoked")
	}
}

func TestGetStateSnapshot(t *testing.T) {
	c := NewOVNClient(Config{}, nil)
	c.state.LocalChassisName = "node1"
	c.state.LocalRouters = []LocalRouterInfo{
		{RouterName: "router1", RouterUUID: "uuid1", LRPName: "lrp-abc", LRPUUID: "lrp-uuid1", LRPNetworks: []string{"10.0.0.1/24"}, CRPort: "cr-lrp-abc"},
		{RouterName: "router2", RouterUUID: "uuid2", LRPName: "lrp-def", LRPUUID: "lrp-uuid2", LRPNetworks: []string{"172.16.0.1/16"}, CRPort: "cr-lrp-def"},
	}
	c.state.HasLocalRouters = true
	c.state.FIPs = []string{"10.0.0.1", "10.0.0.2"}
	c.state.SNATIPs = []string{"10.0.0.100"}

	snap := c.GetState()

	if snap.LocalChassisName != "node1" {
		t.Errorf("LocalChassisName = %q, want %q", snap.LocalChassisName, "node1")
	}
	if !snap.HasLocalRouters {
		t.Error("HasLocalRouters should be true")
	}
	if len(snap.LocalRouters) != 2 {
		t.Errorf("LocalRouters length = %d, want 2", len(snap.LocalRouters))
	}
	if snap.LocalRouters[0].RouterName != "router1" {
		t.Errorf("LocalRouters[0].RouterName = %q, want %q", snap.LocalRouters[0].RouterName, "router1")
	}
	if len(snap.FIPs) != 2 {
		t.Errorf("FIPs length = %d, want 2", len(snap.FIPs))
	}
	if len(snap.SNATIPs) != 1 {
		t.Errorf("SNATIPs length = %d, want 1", len(snap.SNATIPs))
	}

	// Verify snapshot is a copy (modifying snap doesn't affect original).
	snap.FIPs[0] = "modified"
	if c.state.FIPs[0] == "modified" {
		t.Error("GetState should return a copy of FIPs, not a reference")
	}

	snap.LocalRouters[0].RouterName = "modified"
	if c.state.LocalRouters[0].RouterName == "modified" {
		t.Error("GetState should return a copy of LocalRouters, not a reference")
	}
}

func TestGetStateSnapshotNoLocalRouters(t *testing.T) {
	c := NewOVNClient(Config{}, nil)
	c.state.LocalChassisName = "node1"

	snap := c.GetState()

	if snap.HasLocalRouters {
		t.Error("HasLocalRouters should be false when no routers are set")
	}
	if len(snap.LocalRouters) != 0 {
		t.Errorf("LocalRouters length = %d, want 0", len(snap.LocalRouters))
	}
}

func TestUniqueNetworks(t *testing.T) {
	_, net1, _ := net.ParseCIDR("10.0.0.0/24")
	_, net2, _ := net.ParseCIDR("172.16.0.0/16")
	_, net1dup, _ := net.ParseCIDR("10.0.0.0/24")

	tests := []struct {
		name  string
		input []*net.IPNet
		want  []string
	}{
		{"nil", nil, []string{}},
		{"empty", []*net.IPNet{}, []string{}},
		{"single", []*net.IPNet{net1}, []string{"10.0.0.0/24"}},
		{"dedup", []*net.IPNet{net1, net2, net1dup}, []string{"10.0.0.0/24", "172.16.0.0/16"}},
		{"sorted", []*net.IPNet{net2, net1}, []string{"10.0.0.0/24", "172.16.0.0/16"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uniqueNetworks(tt.input)
			gotStrs := make([]string, 0, len(got))
			for _, n := range got {
				gotStrs = append(gotStrs, n.String())
			}
			if !reflect.DeepEqual(gotStrs, tt.want) {
				t.Errorf("uniqueNetworks() = %v, want %v", gotStrs, tt.want)
			}
		})
	}
}

func TestGetStateIncludesDiscoveredNetworks(t *testing.T) {
	_, net1, _ := net.ParseCIDR("198.51.100.0/24")
	c := NewOVNClient(Config{}, nil)
	c.state.DiscoveredNetworks = []*net.IPNet{net1}

	snap := c.GetState()
	if len(snap.DiscoveredNetworks) != 1 {
		t.Fatalf("DiscoveredNetworks length = %d, want 1", len(snap.DiscoveredNetworks))
	}
	if snap.DiscoveredNetworks[0].String() != "198.51.100.0/24" {
		t.Errorf("DiscoveredNetworks[0] = %q, want %q", snap.DiscoveredNetworks[0], "198.51.100.0/24")
	}

	// Verify snapshot is a copy.
	snap.DiscoveredNetworks[0] = nil
	if c.state.DiscoveredNetworks[0] == nil {
		t.Error("GetState should return a copy of DiscoveredNetworks")
	}
}

func TestSBDatabaseModel(t *testing.T) {
	m, err := sbDatabaseModel()
	if err != nil {
		t.Fatalf("sbDatabaseModel() error: %v", err)
	}
	if m.Name() != "OVN_Southbound" {
		t.Errorf("model name = %q, want %q", m.Name(), "OVN_Southbound")
	}
}

func TestNBDatabaseModel(t *testing.T) {
	m, err := nbDatabaseModel()
	if err != nil {
		t.Fatalf("nbDatabaseModel() error: %v", err)
	}
	if m.Name() != "OVN_Northbound" {
		t.Errorf("model name = %q, want %q", m.Name(), "OVN_Northbound")
	}
}
