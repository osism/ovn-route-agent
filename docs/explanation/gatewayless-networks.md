# Gatewayless provider networks

## Background: the problem

In a traditional OpenStack deployment, the provider network has a real
upstream gateway (e.g. a physical router at `.1`). OVN uses this gateway IP
as the nexthop for its default route, so SNAT reply traffic naturally exits
the logical router and reaches the physical network.

When the provider network is configured with **`disable_gateway_ip: true`**
(gatewayless mode), there is no physical upstream gateway at all — all
external traffic is routed purely via BGP `/32` announcements. This creates
a problem: OVN's logical router has no nexthop for its default route, so
reply traffic (after SNAT) has no way to leave the logical router.

## Solution: the virtual gateway ("magic gateway")

The agent solves this by inventing a **virtual gateway IP** that does not
correspond to any real device. It picks the **last usable host address** in
the provider subnet (broadcast address minus one):

| Subnet | Virtual gateway IP |
|--------|--------------------|
| `198.51.100.0/24` | `198.51.100.254` |
| `192.168.42.0/23` | `192.168.43.254` |
| `10.0.0.0/16` | `10.0.255.254` |
| `172.16.0.0/30` | `172.16.0.2` |

The computation uses the first IPv4 CIDR found on the logical router's
external port (`Logical_Router_Port.Networks`).

For each locally-active router, the agent writes two entries into the OVN
Northbound database:

1. **Default route** — `0.0.0.0/0 via <virtual-gw>` on the logical router,
   so OVN knows where to send reply traffic after SNAT.
2. **Static MAC binding** — maps the virtual gateway IP to the local `br-ex`
   MAC address, so OVN can resolve the nexthop without sending ARP requests
   that nobody would answer.

Together, these two entries trick OVN into forwarding SNAT reply packets out
of the logical router's external port onto `br-ex`, where the kernel and FRR
take over for BGP delivery. The virtual gateway IP itself is never used as an
actual destination — it only serves as the logical nexthop that makes OVN's
routing pipeline work.

Both entries are tagged with `ExternalIDs["ovn-network-agent"] = "managed"`
so the agent can track and clean them up. Additionally, managed static
routes carry `ExternalIDs["ovn-network-agent-chassis"]` set to the owning
chassis hostname, enabling stale chassis cleanup by surviving agents when a
node dies without graceful shutdown. If a default route already exists that
was **not** created by the agent (i.e. a real gateway configured by
OpenStack), the agent leaves it untouched.

For the OpenStack-side configuration that triggers the gatewayless path
(Ansible / openstack CLI), see
[Create a gatewayless provider network](../guides/gatewayless-provider-network).

## Failover behavior

On HA failover (chassisredirect port moves to a different chassis), the agent
on the new active node **updates the static MAC binding** to point to its
own `br-ex` MAC. This ensures reply traffic is forwarded to the correct
physical node without requiring any change to the logical route itself.

## MAC-tweak flows on br-ex

Packets leaving OVN via `br-int` arrive on the patch port of `br-ex` with a
destination MAC set by OVN's logical pipeline — not the bridge's own MAC.
The Linux kernel would drop these packets because the destination MAC does
not match any local interface. To fix this, the agent installs OVS flows
(cookie `0x999`, priority 900) on `br-ex` that **rewrite the destination
MAC** to the bridge's own MAC for all packets arriving on the patch port:

```
cookie=0x999,priority=900,ip,in_port=<patch-port>,actions=mod_dl_dst:<br-ex-mac>,NORMAL
```

This allows the kernel to accept and route the packets normally (via the
`/32` kernel routes and policy rules into `vrf-provider` for BGP delivery).

## Hairpin OVS flows on br-ex

When two OVN logical routers are both active on the same chassis, a FIP on
router-A trying to reach a FIP on router-B creates an asymmetric failure:
OVN sends the packet out via the localnet port to `br-ex`, the MAC-tweak
flow delivers it to the kernel, but the kernel has no local address for the
destination FIP and either drops or loops the packet. The same traffic works
fine from a *different* chassis because it arrives via the physical network
and OVN processes it correctly.

The agent installs per-IP **hairpin flows** (cookie `0x998`, priority 910)
that intercept packets from OVN destined for a locally-managed FIP and
**reflect them back through the same patch port** using `output:in_port`.
OVN then processes the reflected packet as if it arrived from the external
network, applying the correct DNAT/ICMP handling on the destination router.

Both source and destination MACs are rewritten:

- **`dl_src`** is set to the `br-ex` MAC so the reflected packet appears as
  external traffic to OVN, avoiding loop detection.
- **`dl_dst`** is set to the owning router port's MAC so OVN's L2 lookup on
  the external logical switch delivers the packet to the correct router
  (without this, the original destination MAC may be unresolved when OVN's
  ARP resolution between co-located routers has not completed).

```
cookie=0x998,priority=910,ip,in_port=<patch-port>,ip_dst=<fip>/32,actions=mod_dl_src:<br-ex-mac>,mod_dl_dst:<router-port-mac>,output:in_port
```

Priority 910 ensures hairpin fires **before** the MAC-tweak flow (priority
900), so locally-managed IPs are reflected into OVN while all other traffic
still falls through to MAC-tweak and exits to the physical network normally.
The hairpin flows are reconciled alongside the MAC-tweak flows and removed
when no routers are locally active.
