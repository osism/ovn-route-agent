package main

import (
	"testing"
)

func TestHairpinFlow(t *testing.T) {
	tests := []struct {
		name      string
		cookie    string
		ofport    string
		ip        string
		bridgeMAC string
		routerMAC string
		ipv6      bool
		want      string
	}{
		{
			"basic IPv4 hairpin flow",
			"0x998", "42", "5.182.234.199", "aa:bb:cc:dd:ee:ff", "fa:16:3e:6f:a1:64", false,
			"cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port",
		},
		{
			"different IPv4 IP and ofport",
			"0x998", "7", "192.0.2.1", "11:22:33:44:55:66", "fa:16:3e:ab:cd:ef", false,
			"cookie=0x998,priority=910,ip,in_port=7,ip_dst=192.0.2.1/32,actions=mod_dl_src:11:22:33:44:55:66,mod_dl_dst:fa:16:3e:ab:cd:ef,output:in_port",
		},
		{
			"SNAT router external IP",
			"0x998", "3", "5.182.234.128", "82:ba:92:54:47:48", "fa:16:3e:45:06:3e", false,
			"cookie=0x998,priority=910,ip,in_port=3,ip_dst=5.182.234.128/32,actions=mod_dl_src:82:ba:92:54:47:48,mod_dl_dst:fa:16:3e:45:06:3e,output:in_port",
		},
		{
			"IPv6 FIP",
			"0x998", "42", "2001:db8::1", "aa:bb:cc:dd:ee:ff", "fa:16:3e:00:00:01", true,
			"cookie=0x998,priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1/128,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:01,output:in_port",
		},
		{
			"IPv6 SNAT",
			"0x998", "5", "2001:db8:cafe::1", "aa:bb:cc:dd:ee:ff", "fa:16:3e:00:00:02", true,
			"cookie=0x998,priority=910,ipv6,in_port=5,ipv6_dst=2001:db8:cafe::1/128,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:02,output:in_port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HairpinFlow(tt.cookie, tt.ofport, tt.ip, tt.bridgeMAC, tt.routerMAC, tt.ipv6)
			if got != tt.want {
				t.Errorf("HairpinFlow() = %q, want %q", got, tt.want)
			}
		})
	}
}

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
