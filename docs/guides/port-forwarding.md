# Set up port forwarding (DNAT)

This guide walks through configuring the agent to forward traffic from anycast
VIP addresses to internal backends. For the conceptual model — connmarks,
fwmarks, VRF crossing, packet-flow diagrams — see
[Port forwarding (DNAT)](../explanation/port-forwarding).

## Prerequisites

- **`nft` binary** must be in `PATH` (the agent shells out to `nft -f -` for
  atomic ruleset application).
- **IPv4 only** — VIP and backend addresses must be IPv4; IPv6 is not
  supported for port forwarding.
- **`veth_leak_enabled: true`** (default) — port forwarding requires the veth
  pair for the return path.
- **IP forwarding** on the veth interfaces — enabled automatically by the
  agent at startup.

## Example configuration

```yaml
port_forward_dev: "loopback1"   # VIP addresses go on this interface in vrf-provider
port_forward_table_id: 201      # dedicated routing table for DNAT return traffic
# port_forward_ct_zone: 64000   # conntrack zone (default 64000, must not collide with OVN zones)
# port_forward_l3mdev_accept: false  # set true if same-host backends are in a different VRF than the VIP

port_forwards:
  - vip: "198.51.100.10"
    manage_vip: true             # agent adds 198.51.100.10/32 to loopback1
    masquerade: true             # VIP-level default: rules inherit this unless overridden
    rules:
      # Local backend (same host): override masquerade to false.
      # Reply is handled by output_ctzone/output_fwmark chains.
      - proto: udp
        port: 53
        dest_addr: "10.0.0.200"
        dest_port: 1053
        masquerade: false
      - proto: tcp
        port: 53
        dest_addr: "10.0.0.200"
        dest_port: 1053
        masquerade: false
      # Remote backend (different host): inherits masquerade: true from VIP.
      # SNAT ensures replies return to this node for reverse NAT.
      - proto: tcp
        port: 443
        dest_addr: "10.0.0.100"
      # Multiple backends with sticky hashing:
      - proto: udp
        port: 5353
        dest_addrs:
          - "10.0.0.200"
          - "10.0.0.201"
          - "10.0.0.202"
        dest_port: 1053

  # VIP with hairpin_masquerade: fixes connections from FIPs on the same node.
  # External clients are NOT masqueraded (client IP preserved end-to-end).
  - vip: "198.51.100.20"
    manage_vip: true
    hairpin_masquerade: true     # SNAT only for source IPs within provider networks
    rules:
      - proto: tcp
        port: 80
        dest_addr: "10.0.0.100"
      - proto: tcp
        port: 443
        dest_addr: "10.0.0.100"

  # VIP that must be reachable from both FIP-equipped VMs and instances
  # behind a router without a FIP. hairpin_masquerade covers the former,
  # router_masquerade the latter — both rules coexist in postrouting_snat
  # and leave every other external client IP untouched.
  - vip: "198.51.100.30"
    manage_vip: true
    hairpin_masquerade: true     # FIPs on the same node
    router_masquerade: true      # instances behind a router (no FIP)
    rules:
      - proto: tcp
        port: 443
        dest_addr: "10.0.0.100"
```

## VIP address management

Each VIP can optionally be managed by the agent (`manage_vip: true`). When
enabled, the agent adds the VIP as a `/32` address on the configured loopback
interface (default: `loopback1`) inside `vrf-provider`. This is the address
that FRR announces via BGP to make the VIP reachable from the external fabric.

When `manage_vip: false`, the VIP address must already exist on the interface
(e.g. configured statically or by another tool).

## Sticky load balancing (multi-backend)

When a rule specifies multiple backends via `dest_addrs`, the agent generates
nftables rules using `jhash ip saddr` (Jenkins hash on the client's source IP)
to consistently map the same client to the same backend:

```
ip daddr 198.51.100.10 udp dport 53 dnat to jhash ip saddr mod 3 map { \
    0 : 10.0.0.200:1053, \
    1 : 10.0.0.201:1053, \
    2 : 10.0.0.202:1053  \
}
```

**Properties:**

- **Sticky**: the same client IP always reaches the same backend
  (deterministic hash).
- **Distributed**: different clients are spread evenly across all backends.
- **Conntrack-aware**: within an established conntrack entry, replies
  naturally stay on the same backend; `jhash` ensures that *new* connections
  from the same client also land on the same backend.
- **NAT-friendly**: clients behind the same NAT gateway (same source IP) share
  a backend, which is typically the desired behavior for DNS and similar
  services.

**Limitations:**

- Not a consistent hash (like Maglev or ketama): when a backend is added or
  removed, `mod N` changes and approximately `(N-1)/N` of clients may be
  remapped. For DNS stickiness this is acceptable in practice.
- `dest_addr` (single) and `dest_addrs` (list) are mutually exclusive per
  rule. Use `dest_addr` for single-backend rules and `dest_addrs` for
  multi-backend.
- Maximum 256 backends per rule.

## Hairpin NAT

Two flavors of source-selective masquerade are available to fix hairpin NAT,
depending on how the local client reaches the VIP. They can be combined on
the same VIP — the resulting rules coexist in `postrouting_snat`.

### Case 1: instance with a FIP on the same node (`hairpin_masquerade`)

**The problem:** a VM with a Floating IP (FIP) in the provider network (e.g.
`5.182.234.153`) tries to reach a port-forwarded VIP (e.g. `194.93.78.239`)
on the same node. ICMP to the VIP succeeds because the VIP address is local
(`loopback1`) and the kernel responds directly — DNAT is never involved. TCP
connections time out because:

1. The VM's packet is DNAT'd:
   `src=5.182.234.153 dst=194.93.78.239:80` → `dst=backend_ip:80`.
2. The backend replies to `5.182.234.153` directly — but without SNAT the
   reply may not return through this node (asymmetric routing), so conntrack
   never sees it and the reverse DNAT fails silently.

**The fix:** enable `hairpin_masquerade: true` on the VIP. The agent adds a
source-selective SNAT rule that masquerades only traffic from provider
networks:

```
# nftables postrouting_snat chain (generated when hairpin_masquerade: true)
ip saddr 5.182.234.0/24 ct original daddr 194.93.78.239 ct status dnat masquerade
```

With this rule active:

1. The backend receives the packet with `src=<node-control-plane-IP>` instead
   of the FIP.
2. The backend replies to the node's control-plane IP (always reachable).
3. Conntrack reverses both SNAT and DNAT: the VM receives the reply from
   `194.93.78.239`.

External clients (source outside provider networks) are unaffected — their
IPs are still preserved end-to-end.

**Difference from `masquerade: true`:** the VIP-level `masquerade` masquerades
ALL traffic. Hairpin masquerade only masquerades source IPs within the
provider networks, leaving external client IPs intact.

**Note:** hairpin masquerade rules require the provider networks to be known.
On the very first startup (before OVN discovery completes), the rules are
absent. They are installed automatically on the first reconciliation cycle
once OVN reports the provider network CIDRs.

### Case 2: instance behind a router without a FIP (`router_masquerade`)

**The problem:** an instance without its own FIP connects to a port-forwarded
VIP through an OVN router. The router applies SNAT, so the packet that hits
the VIP carries the **router's external IP** as source instead of the instance
address. That external IP is OVN-managed — meaning the backend's reply enters
OVN's pipeline directly, **bypassing this node's conntrack**. The reverse
DNAT never fires, the instance receives a reply from the backend IP instead
of the VIP, and the connection fails.

`hairpin_masquerade` does not fix this cleanly: matching the full provider
CIDR would also rewrite every unrelated external client living in the same
subnet.

**The fix:** enable `router_masquerade: true` on the VIP. The agent
dynamically discovers router SNAT external IPs from OVN NB (rows where
`nat.type=snat` on locally-active routers) and emits a source-selective SNAT
rule that targets **only those specific addresses**:

```
# Single SNAT IP
ip saddr 203.0.113.50 ct original daddr 194.93.78.239 ct status dnat masquerade

# Multiple SNAT IPs (anonymous set)
ip saddr { 203.0.113.50, 203.0.113.51 } ct original daddr 194.93.78.239 ct status dnat masquerade
```

With this rule active the backend sees `src=<node-control-plane-IP>`, replies
through this node, and conntrack reverses both SNAT and DNAT — the instance
receives the reply from the VIP exactly as it expects.

**Difference from `hairpin_masquerade`:** hairpin masquerade matches a
provider-CIDR prefix and therefore also rewrites unrelated external clients
that share the subnet. Router masquerade matches the literal set of router
SNAT IPs surfaced by OVN, leaving every other client untouched.

**Note:** router masquerade rules require at least one SNAT IP to be known.
If the agent starts before OVN has reported any SNAT entry, the rule (and
the entire `postrouting_snat` chain if it would otherwise be empty) is
omitted to prevent accidental masquerade during startup. The rule is
installed on the first reconciliation cycle that delivers SNAT IP data.

### Case 3: all-in-one deployment with a local SNAT source (`snat_to_ip`)

**The problem:** in an all-in-one deployment the OVN gateway, the port-forward
agent, and the backend run on the same node. The kernel's `masquerade` action
picks the egress-interface IP as the SNAT source — which is necessarily a
local address on the gateway. When the backend's reply arrives, the kernel's
`local` routing table (priority `0`) wins over the agent's `fwmark 0x200`
policy rule, so the reply is delivered into `LOCAL_IN` and the un-SNAT'd
egress is dropped. The external client never sees a `SYN-ACK` and the
connection stalls in `SYN_RECV`.

`masquerade` cannot avoid this on its own: there is no way for the kernel
to pick a non-local egress IP when the egress interface is local.

**The fix:** set `snat_to_ip` to a stable IPv4 address that is **not**
configured on any Linux interface in the default routing context. The agent
then emits `snat to <ip>` instead of `masquerade` for every SNAT action
on that VIP (per-rule masquerade, hairpin masquerade, router masquerade).
With a non-local SNAT source, the post-un-DNAT destination on the reply is
not in the default `local` table, the standard `FORWARD` → `POSTROUTING`
path applies, conntrack performs the reverse SNAT, and the packet exits
via the provider-VRF return path.

```yaml
port_forwards:
  - vip: "203.0.113.10"
    manage_vip: true
    masquerade: true             # or hairpin_masquerade / router_masquerade
    snat_to_ip: "169.254.0.2"    # non-local in default VRF (here: veth-provider)
    rules:
      - proto: tcp
        port: 443
        dest_addr: "10.0.0.20"
```

```
# Resulting postrouting_snat rule
ip daddr 10.0.0.20 tcp dport 443 ct status dnat snat to 169.254.0.2
```

Choosing the address is operator policy:

- The veth-provider IP (`veth_provider_ip`, default `169.254.0.2`) is in
  `vrf-provider` and is **not** in the default namespace's `local` table —
  it is a safe choice for all-in-one deployments.
- Any address you allocate on a loopback inside `vrf-provider` works too,
  as long as a return route from the backend to that address exists.
- Do **not** pick an address that is already configured on a default-VRF
  interface — that reintroduces the local-table trap.

**When `snat_to_ip` is not set,** the agent emits `masquerade` exactly as
before. Existing multi-chassis deployments where the masquerade-chosen
egress IP is non-local in the default routing context need no change.
