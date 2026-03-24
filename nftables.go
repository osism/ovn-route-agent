package main

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

const (
	nftTableName       = "ovn-route-agent"
	dnatFwmark         = 0x100 // fwmark on original-direction DNAT'd packets → lookup main
	dnatReplyFwmark    = 0x200 // fwmark on reply-direction DNAT'd packets → lookup VRF
	dnatFwmarkPriority = 150   // ip rule priority; must be < 1000 (l3mdev VRF rule)
	dnatReplyPriority  = 151   // ip rule priority for reply return path
	dnatCTZone         = 1     // conntrack zone shared by both directions of DNAT'd flows
)

// buildNftRuleset generates the complete nftables ruleset for port forwarding.
// It produces up to six chains:
//   - prerouting_ctzone: assigns a shared conntrack zone to both directions
//     of DNAT'd flows (runs at raw priority, before conntrack)
//   - prerouting_dnat: DNAT rules for each VIP:port → backend
//   - prerouting_fwmark: marks DNAT'd packets for policy routing
//   - forward_veth_guard: whitelist-based security for veth return path
//   - postrouting_fwmark_clear: clears reply fwmark before veth crossing
//   - postrouting_snat: masquerade for masquerade-enabled VIPs (optional)
//
// The conntrack zone is critical: DNAT'd traffic crosses VRF boundaries
// (original arrives on provider/VRF, reply arrives on control-plane/default VRF).
// Without a shared zone, conntrack cannot match the reply to the original
// connection and the reverse NAT fails silently.
//
// Safety: all interpolated values (VIPs, protocols, dest addresses) must be
// pre-validated by validateConfig before reaching this function.
func buildNftRuleset(forwards []PortForwardVIP, providerNetworks []*net.IPNet) string {
	// Collect all VIP addresses (needed for fwmark chain and SNAT).
	allVIPs := make([]string, 0, len(forwards))
	for _, pf := range forwards {
		allVIPs = append(allVIPs, pf.VIP)
	}

	// Collect VIPs that need SNAT (masquerade: true). We use explicit
	// SNAT to the VIP instead of masquerade, because the VRF's egress
	// interface (br-ex) may not carry a routable IP.
	var snatVIPs []string
	for _, pf := range forwards {
		if pf.Masquerade {
			snatVIPs = append(snatVIPs, pf.VIP)
		}
	}
	sort.Strings(snatVIPs)

	var b strings.Builder

	fmt.Fprintf(&b, "table ip %s {\n", nftTableName)

	// Chain: assign a shared conntrack zone for DNAT'd flows.
	// Priority raw (-300) runs BEFORE conntrack (-200). This is essential
	// because the original direction enters on a VRF interface (provider)
	// while the reply enters on a non-VRF interface (control-plane).
	// Without a shared zone, conntrack cannot correlate them and the
	// reverse NAT (un-masquerade / un-DNAT) fails silently.
	b.WriteString("    chain prerouting_ctzone {\n")
	b.WriteString("        type filter hook prerouting priority raw; policy accept;\n")
	for _, pf := range forwards {
		for _, r := range pf.Rules {
			addrs := r.destAddrs()
			if len(addrs) == 0 {
				continue
			}
			destPort := r.DestPort
			if destPort == 0 {
				destPort = r.Port
			}
			// Match original direction: client → VIP:port
			fmt.Fprintf(&b, "        ip daddr %s %s dport %d ct zone set %d\n",
				pf.VIP, r.Proto, r.Port, dnatCTZone)
			// Match reply direction: backend:port → client
			// Each backend address needs its own ctzone entry.
			for _, addr := range addrs {
				fmt.Fprintf(&b, "        ip saddr %s %s sport %d ct zone set %d\n",
					addr, r.Proto, destPort, dnatCTZone)
			}
		}
	}
	b.WriteString("    }\n")

	// Chain: DNAT rules.
	// For rules with a single backend: direct DNAT.
	// For rules with multiple backends: jhash on source IP for sticky
	// load balancing — the same client IP always maps to the same backend.
	b.WriteString("    chain prerouting_dnat {\n")
	b.WriteString("        type nat hook prerouting priority dstnat; policy accept;\n")
	for _, pf := range forwards {
		for _, r := range pf.Rules {
			destPort := r.DestPort
			if destPort == 0 {
				destPort = r.Port
			}
			addrs := r.destAddrs()
			if len(addrs) == 0 {
				continue
			}
			if len(addrs) == 1 {
				fmt.Fprintf(&b, "        ip daddr %s %s dport %d dnat to %s:%d\n",
					pf.VIP, r.Proto, r.Port, addrs[0], destPort)
			} else {
				// jhash: consistent source-IP hashing distributes clients
				// across backends. Same client IP → same backend (sticky).
				fmt.Fprintf(&b, "        ip daddr %s %s dport %d dnat to jhash ip saddr mod %d map { ",
					pf.VIP, r.Proto, r.Port, len(addrs))
				for i, addr := range addrs {
					if i > 0 {
						b.WriteString(", ")
					}
					fmt.Fprintf(&b, "%d : %s:%d", i, addr, destPort)
				}
				b.WriteString(" }\n")
			}
		}
	}
	b.WriteString("    }\n")

	// Chain: policy-route DNAT'd packets in both directions.
	// Priority filter (0) runs after dstnat (-100) so ct status dnat is
	// visible. Two fwmarks steer traffic into different routing tables:
	//   - Original direction (client→backend): fwmark 0x100 → lookup main
	//     so the packet escapes the VRF and reaches the backend via the
	//     default VRF's routing (e.g. control-plane network).
	//   - Reply direction (backend→client): fwmark 0x200 → lookup VRF
	//     so the response exits via the provider network where the VIP
	//     source address is legitimate (not filtered as spoofed).
	b.WriteString("    chain prerouting_fwmark {\n")
	b.WriteString("        type filter hook prerouting priority filter; policy accept;\n")
	if len(allVIPs) == 1 {
		fmt.Fprintf(&b, "        ct direction original ct status dnat ct original daddr %s meta mark set 0x%x\n",
			allVIPs[0], dnatFwmark)
		fmt.Fprintf(&b, "        ct direction reply ct status dnat ct original daddr %s meta mark set 0x%x\n",
			allVIPs[0], dnatReplyFwmark)
	} else {
		vipSet := strings.Join(allVIPs, ", ")
		fmt.Fprintf(&b, "        ct direction original ct status dnat ct original daddr { %s } meta mark set 0x%x\n",
			vipSet, dnatFwmark)
		fmt.Fprintf(&b, "        ct direction reply ct status dnat ct original daddr { %s } meta mark set 0x%x\n",
			vipSet, dnatReplyFwmark)
	}
	b.WriteString("    }\n")

	// Chain: veth forward guard (security)
	// Whitelists legitimate veth-leak return traffic and drops everything
	// else going backwards through the veth pair.
	b.WriteString("    chain forward_veth_guard {\n")
	b.WriteString("        type filter hook forward priority filter; policy accept;\n")
	// Allow existing veth leak return traffic (source in provider networks)
	if len(providerNetworks) > 0 {
		nets := make([]string, len(providerNetworks))
		for i, n := range providerNetworks {
			nets[i] = n.String()
		}
		fmt.Fprintf(&b, "        oifname \"%s\" ip saddr { %s } accept\n",
			vethDefaultName, strings.Join(nets, ", "))
	}
	// Allow DNAT reply traffic returning to the VRF via veth pair.
	fmt.Fprintf(&b, "        oifname \"%s\" meta mark 0x%x accept\n",
		vethDefaultName, dnatReplyFwmark)
	// Drop everything else going backwards through veth
	fmt.Fprintf(&b, "        oifname \"%s\" drop\n", vethDefaultName)
	b.WriteString("    }\n")

	// Chain: clear DNAT reply fwmark before packets cross the veth pair.
	// Without this, the fwmark persists into the provider VRF and the
	// ip rule matches again, creating a routing loop (veth-default →
	// veth-provider → table 201 → veth-default → …).
	b.WriteString("    chain postrouting_fwmark_clear {\n")
	b.WriteString("        type filter hook postrouting priority filter; policy accept;\n")
	fmt.Fprintf(&b, "        oifname \"%s\" meta mark 0x%x meta mark set 0\n",
		vethDefaultName, dnatReplyFwmark)
	b.WriteString("    }\n")

	// Chain: SNAT for VIPs with masquerade enabled. Uses interface masquerade
	// so the outgoing interface's IP becomes the source. This is essential
	// because DNAT'd traffic is policy-routed via the main table and may
	// exit on a different network (e.g. control-plane) than the VIP's
	// provider network — using the VIP as source would be filtered as
	// spoofed by intermediate routers.
	if len(snatVIPs) > 0 {
		b.WriteString("    chain postrouting_snat {\n")
		b.WriteString("        type nat hook postrouting priority srcnat; policy accept;\n")
		for _, vip := range snatVIPs {
			fmt.Fprintf(&b, "        ct status dnat ct original daddr %s masquerade\n", vip)
		}
		b.WriteString("    }\n")
	}

	b.WriteString("}\n")
	return b.String()
}
