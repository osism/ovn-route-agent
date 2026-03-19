package main

import (
	"net"
	"reflect"
	"testing"
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
