package main

import (
	"testing"
)

func TestMACTweakFlow(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
		ofport string
		mac    string
		ipv6   bool
		want   string
	}{
		{
			"IPv4 flow",
			"0x999", "42", "aa:bb:cc:dd:ee:ff", false,
			"cookie=0x999,priority=900,ip,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
		},
		{
			"IPv6 flow",
			"0x999", "42", "aa:bb:cc:dd:ee:ff", true,
			"cookie=0x999,priority=900,ipv6,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
		},
		{
			"different ofport and MAC",
			"0x999", "7", "11:22:33:44:55:66", false,
			"cookie=0x999,priority=900,ip,in_port=7,actions=mod_dl_dst:11:22:33:44:55:66,NORMAL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MACTweakFlow(tt.cookie, tt.ofport, tt.mac, tt.ipv6)
			if got != tt.want {
				t.Errorf("MACTweakFlow() = %q, want %q", got, tt.want)
			}
		})
	}
}
