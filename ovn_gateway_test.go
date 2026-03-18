package main

import (
	"testing"
)

func TestVirtualGatewayIP(t *testing.T) {
	tests := []struct {
		name     string
		networks []string
		want     string
		wantErr  bool
	}{
		{"/24 subnet", []string{"198.51.100.11/24"}, "198.51.100.254", false},
		{"/24 subnet different IP", []string{"10.0.0.5/24"}, "10.0.0.254", false},
		{"/23 subnet", []string{"192.168.42.1/23"}, "192.168.43.254", false},
		{"/16 subnet", []string{"172.16.0.1/16"}, "172.16.255.254", false},
		{"/28 subnet", []string{"10.0.0.1/28"}, "10.0.0.14", false},
		{"/30 subnet", []string{"10.0.0.1/30"}, "10.0.0.2", false},
		{"multiple networks picks first IPv4", []string{"fe80::1/64", "198.51.100.11/24"}, "198.51.100.254", false},
		{"/32 skipped", []string{"10.0.0.1/32"}, "", true},
		{"empty", []string{}, "", true},
		{"IPv6 only", []string{"fe80::1/64"}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := virtualGatewayIP(tt.networks)
			if (err != nil) != tt.wantErr {
				t.Errorf("virtualGatewayIP(%v) error = %v, wantErr %v", tt.networks, err, tt.wantErr)
				return
			}
			if err == nil && got.String() != tt.want {
				t.Errorf("virtualGatewayIP(%v) = %q, want %q", tt.networks, got.String(), tt.want)
			}
		})
	}
}
