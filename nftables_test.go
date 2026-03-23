package main

import (
	"net"
	"strings"
	"testing"
)

func TestBuildNftRuleset(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
				{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"},
				{Proto: "tcp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
				{Proto: "udp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
			},
		},
	}
	_, provNet, _ := net.ParseCIDR("198.51.100.0/24")

	result := buildNftRuleset(forwards, []*net.IPNet{provNet})

	// Verify table structure
	if !strings.Contains(result, "table ip ovn-route-agent {") {
		t.Error("missing table declaration")
	}

	// Verify conntrack zone chain
	if !strings.Contains(result, "chain prerouting_ctzone") {
		t.Error("missing prerouting_ctzone chain")
	}
	if !strings.Contains(result, "type filter hook prerouting priority raw") {
		t.Error("ctzone chain must be raw priority (before conntrack)")
	}
	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 80 ct zone set 1") {
		t.Error("missing ctzone rule for original direction (VIP:80)")
	}
	if !strings.Contains(result, "ip saddr 10.0.0.100 tcp sport 80 ct zone set 1") {
		t.Error("missing ctzone rule for reply direction (backend:80)")
	}

	// Verify DNAT rules
	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 80 dnat to 10.0.0.100:80") {
		t.Error("missing DNAT rule for port 80")
	}
	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 53 dnat to 10.0.0.200:1053") {
		t.Error("missing DNAT rule for port 53 with port translation")
	}

	// Verify fwmark chain marks DNAT'd packets (both directions)
	if !strings.Contains(result, "chain prerouting_fwmark") {
		t.Error("missing prerouting_fwmark chain")
	}
	if !strings.Contains(result, "ct direction original ct status dnat ct original daddr 198.51.100.10 meta mark set 0x100") {
		t.Error("missing forward fwmark rule for DNAT'd packets")
	}
	if !strings.Contains(result, "ct direction reply ct status dnat ct original daddr 198.51.100.10 meta mark set 0x200") {
		t.Error("missing reply fwmark rule for DNAT'd return packets")
	}

	// Verify guard chain allows provider network
	if !strings.Contains(result, "ip saddr { 198.51.100.0/24 } accept") {
		t.Error("missing provider network allow rule in guard chain")
	}

	// Verify guard chain allows DNAT reply traffic
	if !strings.Contains(result, `oifname "veth-default" meta mark 0x200 accept`) {
		t.Error("missing DNAT reply accept rule in guard chain")
	}

	// Verify guard chain drops unauthorized traffic
	if !strings.Contains(result, `oifname "veth-default" drop`) {
		t.Error("missing drop rule in guard chain")
	}
}

func TestBuildNftRulesetMultipleVIPs(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
			},
		},
		{
			VIP: "198.51.100.20",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"},
				{Proto: "udp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
			},
		},
	}

	result := buildNftRuleset(forwards, nil)

	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 80") {
		t.Error("missing DNAT rule for first VIP")
	}
	if !strings.Contains(result, "ip daddr 198.51.100.20 tcp dport 443") {
		t.Error("missing DNAT rule for second VIP")
	}
	if !strings.Contains(result, "ip daddr 198.51.100.20 udp dport 53 dnat to 10.0.0.200:1053") {
		t.Error("missing DNAT rule for second VIP port 53")
	}

	// Verify fwmark chain uses set syntax for multiple VIPs (both directions)
	if !strings.Contains(result, "ct direction original ct status dnat ct original daddr { 198.51.100.10, 198.51.100.20 } meta mark set 0x100") {
		t.Errorf("forward fwmark should use set syntax for multiple VIPs, got:\n%s", result)
	}
	if !strings.Contains(result, "ct direction reply ct status dnat ct original daddr { 198.51.100.10, 198.51.100.20 } meta mark set 0x200") {
		t.Errorf("reply fwmark should use set syntax for multiple VIPs, got:\n%s", result)
	}
}

func TestBuildNftRulesetNoProviderNetworks(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
			},
		},
	}

	result := buildNftRuleset(forwards, nil)

	// Guard chain should not have provider network allow rule
	if strings.Contains(result, "ip saddr {") {
		t.Error("should not have provider network accept rule when no provider networks given")
	}

	// Should still have the drop rule
	if !strings.Contains(result, `oifname "veth-default" drop`) {
		t.Error("missing drop rule in guard chain")
	}
}

func TestBuildNftRulesetDestPortDefault(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100", DestPort: 0},
			},
		},
	}

	result := buildNftRuleset(forwards, nil)

	// dest_port=0 should use port as default
	if !strings.Contains(result, "dnat to 10.0.0.100:80") {
		t.Error("dest_port=0 should default to port value")
	}
}

func TestBuildNftRulesetSamePortDifferentProto(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
				{Proto: "udp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
			},
		},
	}

	result := buildNftRuleset(forwards, nil)

	if !strings.Contains(result, "tcp dport 53 dnat to 10.0.0.200:1053") {
		t.Error("missing TCP DNAT rule for port 53")
	}
	if !strings.Contains(result, "udp dport 53 dnat to 10.0.0.200:1053") {
		t.Error("missing UDP DNAT rule for port 53")
	}
}

func TestBuildNftRulesetMultipleProviderNetworks(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP:   "198.51.100.10",
			Rules: []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"}},
		},
	}
	_, net1, _ := net.ParseCIDR("198.51.100.0/24")
	_, net2, _ := net.ParseCIDR("203.0.113.0/24")

	result := buildNftRuleset(forwards, []*net.IPNet{net1, net2})

	if !strings.Contains(result, "198.51.100.0/24, 203.0.113.0/24") {
		t.Errorf("guard chain should contain both provider networks, got:\n%s", result)
	}
}

func TestBuildNftRulesetMaxPort(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP:   "198.51.100.10",
			Rules: []PortForwardRule{{Proto: "tcp", Port: 65535, DestAddr: "10.0.0.100", DestPort: 65535}},
		},
	}

	result := buildNftRuleset(forwards, nil)

	if !strings.Contains(result, "tcp dport 65535 dnat to 10.0.0.100:65535") {
		t.Error("missing rule for max port 65535")
	}
}

func TestBuildNftRulesetMasquerade(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP:        "198.51.100.10",
			Masquerade: true,
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"},
			},
		},
		{
			VIP: "198.51.100.20",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.200"},
			},
		},
	}

	result := buildNftRuleset(forwards, nil)

	// Should have postrouting SNAT chain
	if !strings.Contains(result, "chain postrouting_snat") {
		t.Fatal("missing postrouting_snat chain")
	}
	if !strings.Contains(result, "type nat hook postrouting priority srcnat") {
		t.Error("missing srcnat hook in SNAT chain")
	}
	// Only masquerade-enabled VIP should get a masquerade rule
	if !strings.Contains(result, "ct status dnat ct original daddr 198.51.100.10 masquerade") {
		t.Errorf("missing masquerade rule for masquerade-enabled VIP, got:\n%s", result)
	}
	// Non-masquerade VIP should NOT have a masquerade rule
	if strings.Contains(result, "198.51.100.20 masquerade") {
		t.Error("non-masquerade VIP should not have masquerade rule")
	}
}

func TestBuildNftRulesetNoMasquerade(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
			},
		},
	}

	result := buildNftRuleset(forwards, nil)

	// Should NOT have postrouting SNAT chain
	if strings.Contains(result, "postrouting_snat") {
		t.Error("should not have SNAT chain when no VIP has masquerade enabled")
	}
}

