package main

import (
	"errors"
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
	if rm.dryRun {
		t.Error("dryRun should be false by default")
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

func TestDryRunEnsureRemoveRoute(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}

	if err := rm.EnsureRoute("10.0.0.1"); err != nil {
		t.Errorf("EnsureRoute() in dry-run should not error, got: %v", err)
	}
	if err := rm.RemoveRoute("10.0.0.1"); err != nil {
		t.Errorf("RemoveRoute() in dry-run should not error, got: %v", err)
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
