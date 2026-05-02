# ovn-network-agent

Event-driven network agent for OVN-based OpenStack environments. A real-time daemon that watches OVN databases directly via the OVSDB protocol to synchronize Floating IP routes and optionally forward traffic from anycast VIPs to internal backends.

## Table of contents

- [How it works](#how-it-works)
- [Building](#building)
- [Configuration](#configuration)
  - [Config file (YAML)](#config-file-yaml)
  - [Example](#example)
  - [Reference](#reference)
- [Installation](#installation)
  - [Debian package](#debian-package)
  - [Binary](#binary)
  - [From source](#from-source)
  - [Check status](#check-status)
- [Prerequisites](#prerequisites)
- [Multi-router support](#multi-router-support)
- [Gatewayless provider networks](#gatewayless-provider-networks)
  - [Background: the problem](#background-the-problem)
  - [Solution: the virtual gateway ("magic gateway")](#solution-the-virtual-gateway-magic-gateway)
  - [Creating a gatewayless provider network](#creating-a-gatewayless-provider-network)
  - [Failover behavior](#failover-behavior)
  - [MAC-tweak flows on br-ex](#mac-tweak-flows-on-br-ex)
  - [Hairpin OVS flows on br-ex](#hairpin-ovs-flows-on-br-ex)
- [Port forwarding (DNAT)](#port-forwarding-dnat)
  - [Background: the problem](#background-the-problem-1)
  - [Solution: connmark-based return routing](#solution-connmark-based-return-routing)
  - [Why conntrack-based fwmark instead of simpler alternatives?](#why-conntrack-based-fwmark-instead-of-simpler-alternatives)
  - [The `forward_veth_guard` chain](#the-forward_veth_guard-chain)
  - [VIP address management](#vip-address-management)
  - [Packet flow: DNAT forward and return path](#packet-flow-dnat-forward-and-return-path)
  - [Prerequisites](#prerequisites-1)
  - [Example configuration](#example-configuration)
  - [Sticky load balancing (multi-backend)](#sticky-load-balancing-multi-backend)
  - [Hairpin NAT](#hairpin-nat)
- [Gateway drain mode](#gateway-drain-mode)
  - [Background: the problem](#background-the-problem-2)
  - [Solution: pre-shutdown priority drain](#solution-pre-shutdown-priority-drain)
  - [Shutdown sequence](#shutdown-sequence)
  - [Priority semantics](#priority-semantics)
  - [Configuration](#configuration-1)
  - [When to disable drain](#when-to-disable-drain)
- [Metrics](#metrics)
- [Architecture](#architecture)
  - [Control plane](#control-plane)
  - [Data plane](#data-plane)
- [Origin](#origin)
- [License](#license)

## How it works

The agent monitors the OVN Southbound and Northbound databases and performs targeted writes to both OVN NB (default routes, static MAC bindings) and the local system (kernel routes, IP rules, FRR static routes, OVS flows on the provider bridge).

1. **Connects** to OVN Southbound and Northbound databases via OVSDB IDL
2. **Watches** for changes in real-time:
   - `Port_Binding` table (SB) — detects gateway chassis failover (chassisredirect changes bypass debouncing for fast reaction) and extracts SNAT IPs from `NatAddresses` on gateway patch ports for immediate route announcement before NB NAT entries exist
   - `Chassis` table (SB) — detects chassis membership changes
   - `NAT` table (NB) — detects Floating IP and SNAT assignments
   - `Logical_Router` / `Logical_Router_Port` tables (NB) — maps NAT entries to their owning routers and auto-discovers provider network CIDRs from `Logical_Router_Port.Networks`
3. **Sets up** the provider bridge at startup:
   - Adds a **link-local IP** to the bridge device so the kernel can perform ARP resolution
   - Enables **proxy ARP** on the bridge device so the kernel responds to ARP requests for FIP addresses
4. **Reacts** instantly to changes — for each router whose gateway is active on this chassis:
   - Ensures a **default route** (`0.0.0.0/0 via <virtual-gw>`) and **static MAC binding** in OVN NB so reply traffic exits the logical router correctly (see [Gatewayless provider networks](#gatewayless-provider-networks))
   - Installs **OVS MAC-tweak flows** on the provider bridge so the kernel accepts packets from OVN (rewrites destination MAC to `br-ex` MAC)
   - Installs **OVS hairpin flows** (per FIP, priority 910) that reflect same-chassis cross-router traffic back into OVN via `output:in_port`, rewriting both source MAC (`br-ex` MAC) and destination MAC (owning router port MAC) — see [Hairpin OVS flows on br-ex](#hairpin-ovs-flows-on-br-ex)
   - Ensures `/32` **kernel routes** (with IP rules when using a dedicated routing table) and **FRR static routes** in the VRF for each FIP/SNAT IP
   - If configured, reconciles the **FRR prefix-list** with `permit <network> ge 32 le 32` entries for each discovered provider network
   - If no routers are locally active: removes all managed routes
5. **Port forwarding (DNAT)** — optionally forwards traffic from anycast VIP addresses (on a loopback interface in the VRF) to internal backends. Supports multiple backends per rule with sticky source-IP hashing (`jhash`) for consistent client-to-backend mapping. Client IPs are preserved using connmark-based return routing through the veth pair. Backends on the same host are handled via dedicated OUTPUT chains (`output_ctzone`, `output_fwmark`) since locally-delivered traffic bypasses the FORWARD/POSTROUTING path. Per-rule `masquerade` control allows mixing local and remote backends on the same VIP. Hairpin masquerade (`hairpin_masquerade: true`) solves the hairpin NAT problem where FIPs on the same node try to connect to a port-forwarded VIP — only source-masquerades traffic from provider networks, leaving external clients unaffected. A `forward_veth_guard` nftables chain restricts the veth return path to legitimate traffic only. Requires `nft` binary and `veth_leak_enabled: true`.
6. **Reconciles** periodically as a safety net (default: every 60s)
7. **Detects stale chassis** — when a node dies without graceful shutdown, surviving agents detect its chassis disappearing from the SB Chassis table and clean up its managed OVN NB entries (static routes and MAC bindings) after a configurable grace period (default: 5m, configurable via `stale_chassis_grace_period`, set to `0` to disable). Random jitter (0-30s) prevents multiple agents from cleaning up simultaneously.
8. **Drains gateways** on shutdown (SIGINT/SIGTERM) — before cleanup, the agent lowers its `Gateway_Chassis` priority to 0 in OVN NB, causing `ovn-northd` to migrate chassisredirect ports to standby chassis (priority >= 1). On the next startup, drained entries are restored to priority 1 (standby level). The active chassis automatically maintains a minimum priority of 2 (above all possible standby peers) during reconciliation, preventing reverse failover even when a peer restores to priority 1. This eliminates the traffic disruption window between BGP route withdrawal and OVN BFD failover detection (see [Gateway drain mode](#gateway-drain-mode)). Enabled by default (`drain_on_shutdown: true`, `drain_timeout: 60s`).
9. **Cleans up** after drain — removes all managed routes, OVS flows, and the bridge IP before exiting (configurable via `cleanup_on_shutdown`)

## Building

Requires Go 1.25+ (see `go.mod` for the exact minimum).

```bash
# Standard build (linux)
make build

# Static binary for deployment (CGO_ENABLED=0, linux/amd64)
make build-static

# Run tests
make test

# Lint
make fmt
make vet

# Install to /usr/local/bin
sudo make install
```

Produces a single binary `ovn-network-agent`.

## Configuration

Settings are loaded with the following priority (highest wins):

**CLI flags > environment variables > config file > defaults**

### Config file (YAML)

```bash
ovn-network-agent --config /etc/ovn-network-agent/config.yaml
# or via environment variable
OVN_NETWORK_CONFIG=/etc/ovn-network-agent/config.yaml ovn-network-agent
```

See [`ovn-network-agent.yaml.sample`](ovn-network-agent.yaml.sample) for a full example.

### Example

Config file `/etc/ovn-network-agent/config.yaml` with the base settings:

```yaml
ovn_sb_remote: "tcp:10.10.0.1:6642,tcp:10.10.0.2:6642,tcp:10.10.0.3:6642"
ovn_nb_remote: "tcp:10.10.0.1:6641,tcp:10.10.0.2:6641,tcp:10.10.0.3:6641"

# Optional: provider networks are auto-discovered from OVN when omitted
# network_cidr:
#   - "192.0.2.0/24"
#   - "198.51.100.0/24"
```

Run with the config file, overriding log level and enabling dry-run via CLI flags:

```bash
ovn-network-agent --config /etc/ovn-network-agent/config.yaml --log-level debug --dry-run
```

CLI flags take precedence over values in the config file.

### Reference

| Flag | Env Var | Config key | Default | Description |
|------|---------|------------|---------|-------------|
| `--config` | `OVN_NETWORK_CONFIG` | — | | Path to YAML config file |
| `--ovn-sb-remote` | `OVN_NETWORK_OVN_SB_REMOTE` | `ovn_sb_remote` | *(required)* | OVN Southbound DB remote, comma-separated for cluster failover |
| `--ovn-nb-remote` | `OVN_NETWORK_OVN_NB_REMOTE` | `ovn_nb_remote` | *(required)* | OVN Northbound DB remote, comma-separated for cluster failover |
| `--bridge-dev` | `OVN_NETWORK_BRIDGE_DEV` | `bridge_dev` | `br-ex` | Provider bridge device |
| `--vrf-name` | `OVN_NETWORK_VRF_NAME` | `vrf_name` | `vrf-provider` | VRF name for FRR routes |
| `--veth-nexthop` | `OVN_NETWORK_VETH_NEXTHOP` | `veth_nexthop` | `169.254.0.1` | Nexthop for FRR static routes |
| `--network-cidr` | `OVN_NETWORK_NETWORK_CIDR` | `network_cidr` | *(empty = auto-discover)* | Filter FIPs by CIDRs; when empty, networks are auto-discovered from OVN `Logical_Router_Port.Networks` |
| `--gateway-port` | `OVN_NETWORK_GATEWAY_PORT` | `gateway_port` | *(empty = all)* | Chassisredirect port filter; empty = track all routers automatically |
| `--route-table-id` | `OVN_NETWORK_ROUTE_TABLE_ID` | `route_table_id` | `0` | Routing table ID for FIP routes (1-252); 0 = main table |
| `--bridge-ip` | `OVN_NETWORK_BRIDGE_IP` | `bridge_ip` | `169.254.169.254` | Link-local IP added to the bridge device for ARP resolution |
| `--ovs-wrapper` | `OVN_NETWORK_OVS_WRAPPER` | `ovs_wrapper` | *(empty)* | Command prefix for containerized OVS (e.g. `docker exec openvswitch_vswitchd`) |
| `--reconcile-interval` | `OVN_NETWORK_RECONCILE_INTERVAL` | `reconcile_interval` | `60s` | Full reconciliation interval |
| `--log-level` | `OVN_NETWORK_LOG_LEVEL` | `log_level` | `info` | Log level (debug, info, warn, error) |
| `--dry-run` | `OVN_NETWORK_DRY_RUN` | `dry_run` | `false` | Connect and reconcile but only log what would be done |
| `--cleanup-on-shutdown` | `OVN_NETWORK_CLEANUP_ON_SHUTDOWN` | `cleanup_on_shutdown` | `true` | Remove all managed routes on shutdown; set to `false` to keep routes in place |
| `--drain-on-shutdown` | `OVN_NETWORK_DRAIN_ON_SHUTDOWN` | `drain_on_shutdown` | `true` | Drain HA gateways before shutdown by lowering `Gateway_Chassis` priority to 0 (see [Gateway drain mode](#gateway-drain-mode)) |
| `--drain-timeout` | `OVN_NETWORK_DRAIN_TIMEOUT` | `drain_timeout` | `60s` | Maximum time to wait for gateway drain before proceeding with shutdown |
| `--frr-prefix-list` | `OVN_NETWORK_FRR_PREFIX_LIST` | `frr_prefix_list` | `ANNOUNCED-NETWORKS` | FRR prefix-list name to manage dynamically; adds `permit <network> ge 32 le 32` entries for each discovered provider network (set to empty string to disable) |
| `--stale-chassis-grace-period` | `OVN_NETWORK_STALE_CHASSIS_GRACE_PERIOD` | `stale_chassis_grace_period` | `5m` | Grace period before cleaning up OVN NB entries from chassis that have disappeared from the SB Chassis table; set to `0` to disable |
| `--metrics-listen` | `OVN_NETWORK_METRICS_LISTEN` | `metrics_listen` | *(empty = disabled)* | Bind address for the Prometheus `/metrics` endpoint (e.g. `127.0.0.1:9273`); see [Metrics](#metrics) |
| `--veth-leak-enabled` | `OVN_NETWORK_VETH_LEAK_ENABLED` | `veth_leak_enabled` | `true` | Enable automatic veth VRF route leaking |
| `--veth-provider-ip` | `OVN_NETWORK_VETH_PROVIDER_IP` | `veth_provider_ip` | *(nexthop+1)* | IP of the veth-provider side (auto-computed from `veth_nexthop` + 1) |
| `--veth-leak-table-id` | `OVN_NETWORK_VETH_LEAK_TABLE_ID` | `veth_leak_table_id` | `200` | Routing table for the leak default route (1-252, must differ from `route_table_id`) |
| `--veth-leak-rule-priority` | `OVN_NETWORK_VETH_LEAK_RULE_PRIORITY` | `veth_leak_rule_priority` | `2000` | Policy rule priority for veth leak rules |
| `--port-forward-dev` | `OVN_NETWORK_PORT_FORWARD_DEV` | `port_forward_dev` | `loopback1` | Loopback device for VIP addresses in VRF |
| `--port-forward-table-id` | `OVN_NETWORK_PORT_FORWARD_TABLE_ID` | `port_forward_table_id` | `201` | Routing table for DNAT return traffic (1-252, must differ from `route_table_id` and `veth_leak_table_id`) |
| `--port-forward-ct-zone` | `OVN_NETWORK_PORT_FORWARD_CT_ZONE` | `port_forward_ct_zone` | `64000` | Conntrack zone for DNAT flows (1-65535, must not collide with OVN zones) |
| `--port-forward-l3mdev-accept` | `OVN_NETWORK_PORT_FORWARD_L3MDEV_ACCEPT` | `port_forward_l3mdev_accept` | `false` | Set `udp/tcp_l3mdev_accept=1` for cross-VRF same-host DNAT backends |
| — | — | `port_forwards` | *(empty)* | List of VIPs with DNAT rules (YAML only, see [sample config](ovn-network-agent.yaml.sample)) |
| `--version` | — | — | — | Print version and exit |

## Installation

Pre-built binaries and Debian packages for `amd64` and `arm64` are available on the [GitHub Releases](https://github.com/osism/ovn-network-agent/releases) page.

### Debian package

```bash
# Download the .deb package (replace VERSION and ARCH as needed)
curl -LO https://github.com/osism/ovn-network-agent/releases/download/vVERSION/ovn-network-agent_VERSION_ARCH.deb

# Example: v0.1.0, amd64
curl -LO https://github.com/osism/ovn-network-agent/releases/download/v0.1.0/ovn-network-agent_0.1.0_amd64.deb

# Install
sudo dpkg -i ovn-network-agent_0.1.0_amd64.deb
```

The package installs:

- `/usr/bin/ovn-network-agent` — the binary
- `/lib/systemd/system/ovn-network-agent.service` — systemd service
- `/etc/default/ovn-network-agent` — environment defaults (preserved on upgrade)
- `/etc/ovn-network-agent/config.yaml.sample` — sample configuration

After installation, create your configuration and start the service:

```bash
sudo cp /etc/ovn-network-agent/config.yaml.sample /etc/ovn-network-agent/config.yaml
sudo vi /etc/ovn-network-agent/config.yaml
sudo systemctl enable --now ovn-network-agent
```

### Binary

```bash
# Download the static binary (replace ARCH as needed: amd64 or arm64)
curl -LO https://github.com/osism/ovn-network-agent/releases/download/vVERSION/ovn-network-agent-linux-ARCH

# Example: v0.1.0, amd64
curl -LO https://github.com/osism/ovn-network-agent/releases/download/v0.1.0/ovn-network-agent-linux-amd64

# Install
sudo install -m 0755 ovn-network-agent-linux-amd64 /usr/local/bin/ovn-network-agent
```

Set up the systemd service and configuration manually:

```bash
sudo cp ovn-network-agent.service /etc/systemd/system/
sudo cp ovn-network-agent.default /etc/default/ovn-network-agent

sudo mkdir -p /etc/ovn-network-agent
sudo cp ovn-network-agent.yaml.sample /etc/ovn-network-agent/config.yaml
sudo vi /etc/ovn-network-agent/config.yaml

sudo systemctl daemon-reload
sudo systemctl enable --now ovn-network-agent
```

### From source

```bash
make build-static
sudo install -m 0755 ovn-network-agent /usr/local/bin/ovn-network-agent
```

### Check status

```bash
sudo systemctl status ovn-network-agent
sudo journalctl -u ovn-network-agent -f
```

## Prerequisites

- **OVN**: TCP access to OVN Southbound and Northbound databases on the control nodes (the agent runs on network/gateway nodes where no local DB sockets exist)
- **FRR**: `vtysh` must be available and the VRF + BGP configuration must already exist
- **Linux**: Provider bridge (e.g. `br-ex`) must exist
- **VRF route leaking**: The agent automatically creates and manages a veth pair connecting the default VRF to `vrf-provider` (enabled by default via `--veth-leak-enabled`). Per-network routes are reconciled dynamically based on auto-discovered or configured provider networks.
- **nftables**: `nft` binary must be in PATH (required for port forwarding / DNAT)
- **Permissions**: Root or `CAP_NET_ADMIN` for netlink route manipulation

## Multi-router support

The agent automatically discovers **all** chassisredirect port bindings in the OVN Southbound database and determines which logical routers are active on the local chassis. Only the FIPs/SNATs belonging to locally-active routers are managed.

This means a single agent instance handles the common multi-router scenario where OVN distributes different routers across different gateway nodes:

```
net-01 runs agent → sees router-A, router-D active locally → routes their FIPs
net-02 runs agent → sees router-B, router-E active locally → routes their FIPs
net-03 runs agent → sees router-C, router-F active locally → routes their FIPs
```

On failover (e.g. router-A moves from net-01 to net-02), the agent on net-01 removes router-A's routes and the agent on net-02 adds them.

To restrict the agent to a single router (legacy behavior), set `gateway_port` to a specific chassisredirect port name.

## Gatewayless provider networks

### Background: the problem

In a traditional OpenStack deployment, the provider network has a real upstream gateway (e.g. a physical router at `.1`). OVN uses this gateway IP as the nexthop for its default route, so SNAT reply traffic naturally exits the logical router and reaches the physical network.

When the provider network is configured with **`disable_gateway_ip: true`** (gatewayless mode), there is no physical upstream gateway at all — all external traffic is routed purely via BGP `/32` announcements. This creates a problem: OVN's logical router has no nexthop for its default route, so reply traffic (after SNAT) has no way to leave the logical router.

### Solution: the virtual gateway ("magic gateway")

The agent solves this by inventing a **virtual gateway IP** that does not correspond to any real device. It picks the **last usable host address** in the provider subnet (broadcast address minus one):

| Subnet | Virtual gateway IP |
|--------|--------------------|
| `198.51.100.0/24` | `198.51.100.254` |
| `192.168.42.0/23` | `192.168.43.254` |
| `10.0.0.0/16` | `10.0.255.254` |
| `172.16.0.0/30` | `172.16.0.2` |

The computation uses the first IPv4 CIDR found on the logical router's external port (`Logical_Router_Port.Networks`).

For each locally-active router, the agent writes two entries into the OVN Northbound database:

1. **Default route** — `0.0.0.0/0 via <virtual-gw>` on the logical router, so OVN knows where to send reply traffic after SNAT.
2. **Static MAC binding** — maps the virtual gateway IP to the local `br-ex` MAC address, so OVN can resolve the nexthop without sending ARP requests that nobody would answer.

Together, these two entries trick OVN into forwarding SNAT reply packets out of the logical router's external port onto `br-ex`, where the kernel and FRR take over for BGP delivery. The virtual gateway IP itself is never used as an actual destination — it only serves as the logical nexthop that makes OVN's routing pipeline work.

Both entries are tagged with `ExternalIDs["ovn-network-agent"] = "managed"` so the agent can track and clean them up. Additionally, managed static routes carry `ExternalIDs["ovn-network-agent-chassis"]` set to the owning chassis hostname, enabling stale chassis cleanup by surviving agents when a node dies without graceful shutdown. If a default route already exists that was **not** created by the agent (i.e. a real gateway configured by OpenStack), the agent leaves it untouched.

### Creating a gatewayless provider network

The key difference to a normal provider network is that the subnet has **no gateway IP** and the **last usable address** (`.254` in this example) is kept free — the agent will use it as the virtual gateway.

**Ansible (openstack.cloud collection):**

```yaml
- name: Create public network
  openstack.cloud.network:
    cloud: admin
    state: present
    name: public
    external: true
    provider_network_type: flat
    provider_physical_network: physnet1
    mtu: 1500

- name: Create public subnet (gatewayless)
  openstack.cloud.subnet:
    cloud: admin
    state: present
    name: subnet-public-001
    network_name: public
    cidr: 198.51.100.0/24
    enable_dhcp: false
    allocation_pool_start: 198.51.100.1
    allocation_pool_end: 198.51.100.253
    # no gateway_ip → OpenStack sets disable_gateway_ip: true
```

**OpenStack CLI equivalent:**

```bash
openstack network create --external --provider-network-type flat \
  --provider-physical-network physnet1 --mtu 1500 public

openstack subnet create --network public --subnet-range 198.51.100.0/24 \
  --no-dhcp --allocation-pool start=198.51.100.1,end=198.51.100.253 \
  --gateway none subnet-public-001
```

Note that the allocation pool ends at `.253` — address `.254` is reserved for the agent's virtual gateway. The `--gateway none` flag (or omitting `gateway_ip` in Ansible) tells OpenStack not to assign a real gateway, which is exactly what triggers the gatewayless scenario that the agent handles.

### Failover behavior

On HA failover (chassisredirect port moves to a different chassis), the agent on the new active node **updates the static MAC binding** to point to its own `br-ex` MAC. This ensures reply traffic is forwarded to the correct physical node without requiring any change to the logical route itself.

### MAC-tweak flows on br-ex

Packets leaving OVN via `br-int` arrive on the patch port of `br-ex` with a destination MAC set by OVN's logical pipeline — not the bridge's own MAC. The Linux kernel would drop these packets because the destination MAC does not match any local interface. To fix this, the agent installs OVS flows (cookie `0x999`, priority 900) on `br-ex` that **rewrite the destination MAC** to the bridge's own MAC for all packets arriving on the patch port:

```
cookie=0x999,priority=900,ip,in_port=<patch-port>,actions=mod_dl_dst:<br-ex-mac>,NORMAL
```

This allows the kernel to accept and route the packets normally (via the `/32` kernel routes and policy rules into `vrf-provider` for BGP delivery).

### Hairpin OVS flows on br-ex

When two OVN logical routers are both active on the same chassis, a FIP on router-A trying to reach a FIP on router-B creates an asymmetric failure: OVN sends the packet out via the localnet port to `br-ex`, the MAC-tweak flow delivers it to the kernel, but the kernel has no local address for the destination FIP and either drops or loops the packet. The same traffic works fine from a *different* chassis because it arrives via the physical network and OVN processes it correctly.

The agent installs per-IP **hairpin flows** (cookie `0x998`, priority 910) that intercept packets from OVN destined for a locally-managed FIP and **reflect them back through the same patch port** using `output:in_port`. OVN then processes the reflected packet as if it arrived from the external network, applying the correct DNAT/ICMP handling on the destination router.

Both source and destination MACs are rewritten:

- **`dl_src`** is set to the `br-ex` MAC so the reflected packet appears as external traffic to OVN, avoiding loop detection
- **`dl_dst`** is set to the owning router port's MAC so OVN's L2 lookup on the external logical switch delivers the packet to the correct router (without this, the original destination MAC may be unresolved when OVN's ARP resolution between co-located routers has not completed)

```
cookie=0x998,priority=910,ip,in_port=<patch-port>,ip_dst=<fip>/32,actions=mod_dl_src:<br-ex-mac>,mod_dl_dst:<router-port-mac>,output:in_port
```

Priority 910 ensures hairpin fires **before** the MAC-tweak flow (priority 900), so locally-managed IPs are reflected into OVN while all other traffic still falls through to MAC-tweak and exits to the physical network normally. The hairpin flows are reconciled alongside the MAC-tweak flows and removed when no routers are locally active.

## Port forwarding (DNAT)

### Background: the problem

Some services running on the gateway nodes themselves (not inside VMs) need to be reachable via the same anycast VIP addresses that BGP announces to the external fabric. Examples include DNS resolvers, monitoring collectors, or API proxies that run directly on the network nodes.

These services listen on internal addresses (e.g. `10.0.0.200:1053`) but need to be reachable from outside via a public VIP (e.g. `198.51.100.10:53`). A simple `iptables DNAT` rule handles the destination translation, but the **return path** is the hard part: the backend's reply has a source address (`10.0.0.200`) that doesn't match any provider network — so it would be routed via the default VRF instead of through `vrf-provider` where BGP can deliver it to the external client.

The naive fix would be SNAT/masquerade, which rewrites the source to the VIP address. But this **destroys the client IP** — the backend sees the VIP as the source instead of the real client, breaking logging, rate limiting, ACLs, and any protocol that depends on client identity.

### Solution: connmark-based return routing

The agent uses nftables with connection tracking marks (connmarks) to steer DNAT return traffic through the veth pair into `vrf-provider` — without masquerade, preserving the original client IP end-to-end. For remote backends (different network segment, reply must return to this node), per-rule masquerade can be enabled via `masquerade: true` on individual rules or inherited from the VIP level. Local backends (same host) must NOT be masqueraded — their replies are handled by dedicated OUTPUT chains instead.

The mechanism works in six stages (the first four handle remote backends, stages 5-6 add same-host backend support):

**1. DNAT (prerouting)** — Translates the destination for incoming traffic:

```
# Single backend:
ip daddr 198.51.100.10 tcp dport 53 dnat to 10.0.0.200:1053

# Multiple backends (sticky source-IP hashing):
ip daddr 198.51.100.10 udp dport 53 dnat to jhash ip saddr mod 3 map { \
    0 : 10.0.0.200:1053, 1 : 10.0.0.201:1053, 2 : 10.0.0.202:1053 }
```

Traffic arriving at the VIP is rewritten to the internal backend address. If `dest_port` is configured, port translation also occurs (e.g. public port 53 → backend port 1053). When multiple backends are configured via `dest_addrs`, a Jenkins hash on the client source IP distributes traffic with sticky affinity (see [Sticky load balancing](#sticky-load-balancing-multi-backend)).

**2. Conntrack zone assignment (prerouting_ctzone, raw priority)** — Assigns a shared conntrack zone before conntrack processing. This is critical because DNAT'd traffic crosses VRF boundaries (original enters on the provider VRF, reply enters on the default VRF). Without a shared zone, conntrack cannot correlate them and reverse NAT fails silently. The zone number defaults to 64000 (configurable via `port_forward_ct_zone`) to avoid collisions with OVN/OVS conntrack zones:

```
# Original direction: client → VIP:port
ip daddr 198.51.100.10 tcp dport 53 ct zone set 64000

# Reply direction: backend:port → client
ip saddr 10.0.0.200 tcp sport 1053 ct zone set 64000
```

**3. Fwmark tagging (prerouting_fwmark, filter priority)** — Marks DNAT'd packets with direction-specific fwmarks for policy routing. Two marks steer traffic into different routing tables:

```
# Original direction (client→backend): fwmark 0x100 → lookup main
# Escapes the VRF so DNAT'd traffic reaches the backend via the default VRF
ct direction original ct status dnat ct original daddr { 198.51.100.10 } meta mark set 0x100

# Reply direction (backend→client): fwmark 0x200 → lookup table 201
# Routes reply through veth pair back into vrf-provider for BGP delivery
ct direction reply ct status dnat ct original daddr { 198.51.100.10 } meta mark set 0x200
```

**4. Same-host conntrack zone (output_ctzone, raw priority)** — Mirrors `prerouting_ctzone` for the OUTPUT hook. When a DNAT backend runs on the same host, the packet is delivered locally (INPUT chain, not FORWARD). The reply from the local process (e.g. docker-proxy) originates in OUTPUT, not PREROUTING. Without this chain, conntrack cannot find the DNAT entry (wrong zone) and reverse NAT fails:

```
# Same rules as prerouting_ctzone, but in the output hook:
ip daddr 198.51.100.10 tcp dport 53 ct zone set 64000
ip saddr 10.0.0.200 tcp sport 1053 ct zone set 64000
```

**5. Same-host fwmark (output_fwmark, type route)** — Mirrors the reply-direction mark from `prerouting_fwmark` for locally generated replies. Uses `type route` so the mark change triggers a routing re-evaluation, steering the reply through the veth pair back into `vrf-provider`:

```
ct direction reply ct status dnat ct original daddr { 198.51.100.10 } meta mark set 0x200
```

**6. Policy routing** — Two fwmark-based `ip rule` entries steer DNAT'd traffic bidirectionally:

```
ip rule: fwmark 0x100 → lookup main       (priority 150, original direction)
ip rule: fwmark 0x200 → lookup table 201   (priority 151, reply direction)
table 201: default via 169.254.0.2 dev veth-default
```

The forward rule escapes the VRF so packets reach the backend. The reply rule sends the backend's response back through the veth pair into `vrf-provider`, where FRR/BGP delivers it to the external client. The client IP is preserved throughout — no masquerade anywhere in the path.

A `postrouting_fwmark_clear` chain clears the `0x200` fwmark before packets cross the veth pair, preventing a routing loop where the mark would match again inside the provider VRF.

**7. Per-rule masquerade (postrouting_snat, optional)** — When `masquerade: true` is set on a rule (or inherited from the VIP), SNAT is applied to traffic going to that specific backend. The masquerade rule matches on the post-DNAT destination address, so only remote backends are affected:

```
# Only masquerades traffic to remote backend 10.0.0.100, not to local backends
ip daddr 10.0.0.100 tcp dport 443 ct status dnat masquerade
```

This per-backend granularity is essential when a VIP has both local and remote backends: local backends must NOT be masqueraded (their replies are handled by the output chains), while remote backends MUST be masqueraded so replies return to this node for reverse NAT.

**8. Hairpin masquerade (postrouting_snat, optional)** — When `hairpin_masquerade: true` is set on a VIP, SNAT is applied **only to traffic whose source is within a provider network**. This solves the hairpin NAT problem: a VM with a Floating IP (FIP) on the same node that connects to the VIP gets its source address masqueraded, so the backend always replies through this node and conntrack can perform the reverse DNAT. Traffic from external clients (source outside provider networks) is never masqueraded — their client IPs are preserved end-to-end.

The hairpin masquerade rule uses `ct original daddr` to match the pre-DNAT destination (the VIP), ensuring only traffic belonging to connections that were originally destined for this specific VIP is affected:

```
# Traffic from provider net → VIP, DNAT'd: masquerade so backend replies here
ip saddr 5.182.234.0/24 ct original daddr 194.93.78.239 ct status dnat masquerade
```

Unlike the VIP-level `masquerade: true` (which masquerades ALL traffic), hairpin masquerade is source-selective. It can be combined with per-rule `masquerade` on the same VIP — both rules coexist in `postrouting_snat`. The rules are only generated when provider networks are known; if the agent starts before OVN has delivered network discovery, they appear on the first reconciliation cycle.

### Why conntrack-based fwmark instead of simpler alternatives?

| Approach | Client IP preserved? | Problem |
|----------|---------------------|---------|
| SNAT/masquerade (global) | No | Backend sees VIP as source, not the real client |
| Source-based routing (`ip rule from <backend>`) | Yes | Catches **all** traffic from the backend, not just DNAT replies — breaks normal connectivity |
| Conntrack + fwmark | Yes | Only marks packets belonging to DNAT'd connections — surgical, no side effects |
| Conntrack + fwmark + per-rule masquerade | Depends | Best of both: client IP preserved for local backends, masquerade only where needed (remote backends) |
| Conntrack + fwmark + hairpin masquerade | Depends | Client IP preserved for external clients; source-selective SNAT fixes hairpin for FIPs on the same node |

The conntrack-based approach selectively routes just the DNAT return traffic without affecting any other traffic from the backend. It uses `ct status dnat` and `ct direction` to identify packets belonging to DNAT'd connections and assigns direction-specific fwmarks for policy routing. Per-rule masquerade adds surgical SNAT only for remote backends that need it, while local backends (same host) use the OUTPUT chains for return routing with the original client IP preserved. Hairpin masquerade adds a further refinement: source-selective SNAT only for provider-network traffic, solving the asymmetric routing that occurs when a FIP on the same node connects to the VIP.

### The `forward_veth_guard` chain

The veth pair between default VRF and `vrf-provider` is a controlled leak — only specific traffic should traverse it backwards (from default VRF into `vrf-provider`). Without a guard, any packet in the default VRF that happens to be routed via the veth pair could leak into `vrf-provider`.

The `forward_veth_guard` nftables chain enforces a whitelist on traffic exiting through `veth-default`:

```
chain forward_veth_guard {
    type filter hook forward priority filter; policy accept;

    # Allow legitimate veth-leak return traffic (SNAT replies from provider networks)
    oifname "veth-default" ip saddr { 192.0.2.0/24, 198.51.100.0/24 } accept

    # Allow DNAT reply traffic (identified by fwmark 0x200 from prerouting_fwmark)
    oifname "veth-default" meta mark 0x200 accept

    # Drop everything else — prevents unintended traffic from leaking into vrf-provider
    oifname "veth-default" drop
}
```

The provider network CIDRs in the first rule are populated dynamically from the same auto-discovered (or manually configured) networks used by the rest of the agent. They are updated on every reconciliation cycle.

### VIP address management

Each VIP can optionally be managed by the agent (`manage_vip: true`). When enabled, the agent adds the VIP as a `/32` address on the configured loopback interface (default: `loopback1`) inside `vrf-provider`. This is the address that FRR announces via BGP to make the VIP reachable from the external fabric.

When `manage_vip: false`, the VIP address must already exist on the interface (e.g. configured statically or by another tool).

### Packet flow: DNAT forward and return path

#### Forward path (external client → backend)

```
 External client
 src=203.0.113.50  dst=198.51.100.10:53
         │
         │ BGP route: 198.51.100.10/32
         ▼
 ┌──────────────────────────────────────────────────────────────┐
 │                       vrf-provider                           │
 │                                                              │
 │  lo: 198.51.100.10/32 (VIP, managed by agent)                │
 │  FRR/BGP announces this /32 to external fabric               │
 │                                                              │
 │  Packet arrives via BGP peering                              │
 │  /32 route: 198.51.100.10 via 169.254.0.1                    │
 │                                                              │
 │  veth-provider (169.254.0.2/30)                              │
 └──────────────┬───────────────────────────────────────────────┘
                │ veth pair
 ┌──────────────▼───────────────────────────────────────────────┐
 │  veth-default (169.254.0.1/30)        Default VRF            │
 │                                                              │
 │  nft prerouting_ctzone:                                      │
 │  ├─ ct zone set 64000 (shared zone for cross-VRF conntrack)  │
 │  nft prerouting_dnat:                                        │
 │  ├─ DNAT 198.51.100.10:53 → 10.0.0.200:1053                  │
 │  nft prerouting_fwmark:                                      │
 │  ├─ ct direction original + ct status dnat → fwmark 0x100    │
 │  ip rule: fwmark 0x100 → lookup main (escapes VRF)           │
 │                                                              │
 │  Kernel delivers to local backend process                    │
 │  └─ dst=10.0.0.200:1053 (client IP 203.0.113.50 preserved)   │
 └──────────────────────────────────────────────────────────────┘
```

#### Return path — remote backend (backend → external client)

When the backend is on a different host, the reply arrives via the network and enters PREROUTING:

```
 ┌──────────────────────────────────────────────────────────────┐
 │                        Default VRF                           │
 │                                                              │
 │  Remote backend replies (arrives via network):               │
 │  └─ src=10.0.0.200:1053  dst=203.0.113.50                    │
 │  nft prerouting_fwmark (reply direction):                    │
 │  ├─ ct direction reply + ct status dnat → fwmark 0x200       │
 │  Conntrack un-DNATs source address:                          │
 │  ├─ src becomes 198.51.100.10:53                             │
 │  ip rule: fwmark 0x200 → lookup table 201                    │
 │  └─ table 201: default via 169.254.0.2 dev veth-default      │
 │  nft postrouting_fwmark_clear:                               │
 │  ├─ clears fwmark 0x200 before crossing veth (prevents loop) │
 │                                                              │
 │  veth-default (169.254.0.1/30)                               │
 └──────────────┬───────────────────────────────────────────────┘
                │ veth pair
 ┌──────────────▼───────────────────────────────────────────────┐
 │                       vrf-provider                           │
 │  veth-provider (169.254.0.2/30)                              │
 │                                                              │
 │  FRR/BGP delivers reply to external client                   │
 │  └─ src=198.51.100.10:53  dst=203.0.113.50                   │
 └──────────────┬───────────────────────────────────────────────┘
                │
                ▼
         External client
         (client IP preserved end-to-end)
```

#### Return path — same-host backend (local process → external client)

When the backend runs on the same node, the forward packet is delivered locally (INPUT chain), and the reply originates from the OUTPUT hook. The `output_ctzone` and `output_fwmark` chains handle this path:

```
 ┌──────────────────────────────────────────────────────────────┐
 │                        Default VRF                           │
 │                                                              │
 │  Local backend replies (OUTPUT hook, not PREROUTING):        │
 │  └─ src=10.0.0.200:1053  dst=203.0.113.50                    │
 │  nft output_ctzone (raw priority):                           │
 │  ├─ ct zone set 64000 (same zone as prerouting_ctzone)       │
 │  Conntrack finds DNAT entry in zone 64000, un-DNATs:         │
 │  ├─ src becomes 198.51.100.10:53                             │
 │  nft output_fwmark (type route → triggers re-routing):       │
 │  ├─ ct direction reply + ct status dnat → fwmark 0x200       │
 │  ip rule: fwmark 0x200 → lookup table 201                    │
 │  └─ table 201: default via 169.254.0.2 dev veth-default      │
 │  nft postrouting_fwmark_clear:                               │
 │  ├─ clears fwmark 0x200 before crossing veth (prevents loop) │
 │                                                              │
 │  veth-default (169.254.0.1/30)                               │
 └──────────────┬───────────────────────────────────────────────┘
                │ veth pair
 ┌──────────────▼───────────────────────────────────────────────┐
 │                       vrf-provider                           │
 │  veth-provider (169.254.0.2/30)                              │
 │                                                              │
 │  FRR/BGP delivers reply to external client                   │
 │  └─ src=198.51.100.10:53  dst=203.0.113.50                   │
 └──────────────┬───────────────────────────────────────────────┘
                │
                ▼
         External client
         (client IP preserved end-to-end)
```

### Prerequisites

- **`nft` binary** must be in PATH (the agent shells out to `nft -f -` for atomic ruleset application)
- **IPv4 only** — VIP and backend addresses must be IPv4; IPv6 is not supported for port forwarding
- **`veth_leak_enabled: true`** (default) — port forwarding requires the veth pair for the return path
- **IP forwarding** on the veth interfaces — enabled automatically by the agent at startup

### Example configuration

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
```

### Sticky load balancing (multi-backend)

When a rule specifies multiple backends via `dest_addrs`, the agent generates nftables rules using `jhash ip saddr` (Jenkins hash on the client's source IP) to consistently map the same client to the same backend:

```
ip daddr 198.51.100.10 udp dport 53 dnat to jhash ip saddr mod 3 map { \
    0 : 10.0.0.200:1053, \
    1 : 10.0.0.201:1053, \
    2 : 10.0.0.202:1053  \
}
```

**Properties:**

- **Sticky**: The same client IP always reaches the same backend (deterministic hash)
- **Distributed**: Different clients are spread evenly across all backends
- **Conntrack-aware**: Within an established conntrack entry, replies naturally stay on the same backend; `jhash` ensures that *new* connections from the same client also land on the same backend
- **NAT-friendly**: Clients behind the same NAT gateway (same source IP) share a backend, which is typically the desired behavior for DNS and similar services

**Limitations:**

- Not a consistent hash (like Maglev or ketama): when a backend is added or removed, `mod N` changes and approximately `(N-1)/N` of clients may be remapped. For DNS stickiness this is acceptable in practice.
- `dest_addr` (single) and `dest_addrs` (list) are mutually exclusive per rule. Use `dest_addr` for single-backend rules and `dest_addrs` for multi-backend.
- Maximum 256 backends per rule.

### Hairpin NAT

**The problem:** A VM with a Floating IP (FIP) in the provider network (e.g. `5.182.234.153`) tries to reach a port-forwarded VIP (e.g. `194.93.78.239`) on the same node. ICMP to the VIP succeeds because the VIP address is local (`loopback1`) and the kernel responds directly — DNAT is never involved. TCP connections time out because:

1. The VM's packet is DNAT'd: `src=5.182.234.153 dst=194.93.78.239:80` → `dst=backend_ip:80`
2. The backend replies to `5.182.234.153` directly — but without SNAT the reply may not return through this node (asymmetric routing), so conntrack never sees it and the reverse DNAT fails silently

**The fix:** Enable `hairpin_masquerade: true` on the VIP. The agent adds a source-selective SNAT rule that masquerades only traffic from provider networks:

```
# nftables postrouting_snat chain (generated when hairpin_masquerade: true)
ip saddr 5.182.234.0/24 ct original daddr 194.93.78.239 ct status dnat masquerade
```

With this rule active:
1. The backend receives the packet with `src=<node-control-plane-IP>` instead of the FIP
2. The backend replies to the node's control-plane IP (always reachable)
3. Conntrack reverses both SNAT and DNAT: the VM receives the reply from `194.93.78.239`

External clients (source outside provider networks) are unaffected — their IPs are still preserved end-to-end.

**Difference from `masquerade: true`:** The VIP-level `masquerade` masquerades ALL traffic. Hairpin masquerade only masquerades source IPs within the provider networks, leaving external client IPs intact.

**Note:** Hairpin masquerade rules require the provider networks to be known. On the very first startup (before OVN discovery completes), the rules are absent. They are installed automatically on the first reconciliation cycle once OVN reports the provider network CIDRs.

## Gateway drain mode

### Background: the problem

When the agent shuts down (e.g. for a rolling upgrade or node maintenance), two things happen nearly simultaneously:

1. **BGP withdrawal** — FRR withdraws the `/32` routes for all FIPs on this node, so the external fabric stops sending traffic here within seconds.
2. **OVN BFD failover** — OVN detects that the gateway chassis is gone and migrates chassisredirect ports to standby chassis. This relies on BFD timeouts (typically 3×1s = 3 seconds) or periodic probing.

The problem is the **gap between these two events**. During the window where BGP has already withdrawn routes but OVN has not yet completed failover, traffic that was already in flight (or cached by upstream routers) arrives at the node and gets blackholed — OVN still considers this chassis active, but the routes are gone. This causes a brief but measurable traffic disruption on every shutdown.

### Solution: pre-shutdown priority drain

The agent solves this by **draining gateways before cleanup**. On SIGINT/SIGTERM, before removing any routes or closing OVN connections, the agent:

1. **Lowers its `Gateway_Chassis` priority to 0** in the OVN Northbound database for all locally-active router ports. Since standby chassis have priority >= 1, `ovn-northd` immediately begins migrating chassisredirect ports to standby chassis.
2. **Polls the SB `Port_Binding` table** until all chassisredirect ports have moved away from this chassis (or the drain timeout expires).
3. **Proceeds with normal cleanup** — by this point OVN has already migrated traffic to another chassis, so the BGP withdrawal and route cleanup cause zero disruption.

On the **next startup**, before the first reconciliation, the agent detects drained entries (priority 0 on the local chassis) and **restores them to priority 1** (standby level). This re-adds the chassis to the HA group as a standby. The active chassis maintains a minimum priority of 2 via an automatic **priority lead boost** during reconciliation (see [Priority semantics](#priority-semantics)), which is strictly above the restore level of 1 — preventing reverse failover without requiring a priority tie to trigger the boost.

This inverts the shutdown order: OVN failover happens **first** (triggered by the priority change), and BGP withdrawal happens **after** traffic has already moved. The result is a hitless shutdown.

### Shutdown sequence

```
  SIGINT / SIGTERM received
          │
          ▼
  ┌───────────────────────────────────────────────────────┐
  │  1. DRAIN (if drain_on_shutdown=true)                 │
  │                                                       │
  │  For each Gateway_Chassis on this node (priority > 0):│
  │  ├─ Set priority to 0 in OVN NB                       │
  │  │  (batched in a single OVSDB transaction)           │
  │  │                                                    │
  │  ovn-northd recalculates chassisredirect bindings     │
  │  ├─ Standby chassis (priority >= 1) become active     │
  │  ├─ Traffic migrates to standby nodes                 │
  │  │                                                    │
  │  Poll SB Port_Binding until no chassisredirect        │
  │  ports remain on this chassis (or timeout expires)    │
  └───────────────────────┬───────────────────────────────┘
                          │
                          ▼
  ┌───────────────────────────────────────────────────────┐
  │  2. CLEANUP (if cleanup_on_shutdown=true)             │
  │                                                       │
  │  Remove kernel routes, FRR routes, OVS flows,         │
  │  bridge IP, nftables rules                            │
  │  (traffic already moved — no disruption)              │
  └───────────────────────┬───────────────────────────────┘
                          │
                          ▼
                    Agent exits
```

```
  Agent startup
          │
          ▼
  ┌───────────────────────────────────────────────────────┐
  │  RESTORE (if drain_on_shutdown=true)                  │
  │                                                       │
  │  For each Gateway_Chassis on this node with           │
  │  priority == 0:                                       │
  │  ├─ Set priority to 1 (standby level)                 │
  │  │  (batched in a single OVSDB transaction)           │
  │  │                                                    │
  │  Chassis rejoins HA group as standby                  │
  └───────────────────────┬───────────────────────────────┘
                          │
                          ▼
  ┌───────────────────────────────────────────────────────┐
  │  RECONCILE (includes priority lead boost)             │
  │                                                       │
  │  If this chassis is the active gateway:               │
  │  ├─ Compare local priority with peers in HA group     │
  │  ├─ If local priority <= max peer priority            │
  │  │  OR local priority < 2 (minimum active priority):  │
  │  │  boost to max(max peer + 1, 2)                     │
  │  │                                                    │
  │  This ensures the active chassis always has           │
  │  priority >= 2, strictly above the restore level (1), │
  │  preventing reverse failover even when all peers      │
  │  are drained.                                         │
  └───────────────────────┬───────────────────────────────┘
                          │
                          ▼
                 Normal reconciliation loop
```

### Priority semantics

The agent lowers the priority to **0** rather than 1 because in typical Neutron L3 HA setups, standby chassis already have priority 1. Lowering to the same value would not trigger migration. Priority 0 is below any standby chassis, guaranteeing that `ovn-northd` redistributes the chassisredirect port.

On the next startup, drained entries (priority 0) are restored to **1** (standby level), not to their original priority. This is intentional: restoring the original priority would risk making this chassis the highest-priority gateway again, triggering a reverse failover.

To prevent reverse failover, the agent implements an **active priority lead boost**: during each reconciliation, the active gateway chassis ensures its `Gateway_Chassis` priority is both strictly higher than all peers and at least **2** (the minimum active priority). The minimum of 2 is critical because without it, an active chassis at priority 1 with a drained peer at priority 0 would see "already has the lead" and skip boosting — then when the peer restores to 1, both are at the same priority and OVN's tiebreaker can pick either one, causing an unintended switchback. The boost target is `max(max peer priority + 1, 2)`. This ensures:

- After a failover, the new active chassis immediately establishes priority dominance (>= 2) even while the old chassis is still drained at 0
- When the old chassis restarts and restores to priority 1, the active chassis is already at 2 — no tie, no switchback
- The boost is idempotent: once the lead is established, subsequent reconciliations are no-ops

### Configuration

Drain mode is **enabled by default** with a 60-second timeout:

```yaml
# Enable/disable drain (default: true)
drain_on_shutdown: true

# Maximum time to wait for migration (default: 60s)
# After this timeout, the agent proceeds with shutdown even if some
# gateways have not yet migrated.
drain_timeout: "60s"
```

Or via CLI flags:

```bash
ovn-network-agent --drain-on-shutdown=false                 # disable drain
ovn-network-agent --drain-timeout 120s                      # increase timeout
```

Or via environment variables:

```bash
OVN_NETWORK_DRAIN_ON_SHUTDOWN=false                         # disable drain
OVN_NETWORK_DRAIN_TIMEOUT=120s                              # increase timeout
```

### When to disable drain

- **Single-chassis deployments** — if there is no standby chassis, lowering the priority has no effect and the timeout just delays shutdown.
- **Non-HA routers** — routers without multiple `Gateway_Chassis` entries cannot fail over; drain is a no-op (the agent detects this and skips immediately).
- **Environments where Neutron manages priorities** — if an external system actively manages `Gateway_Chassis` priorities and would conflict with the agent's changes.

## Metrics

The agent can expose Prometheus-formatted metrics on an optional HTTP endpoint. Enable it by setting `--metrics-listen` (or `OVN_NETWORK_METRICS_LISTEN` / `metrics_listen`) to a `host:port` such as `127.0.0.1:9273`. The endpoint is **off by default**; bind to `127.0.0.1` for node-local scraping, or to `0.0.0.0` for a remote scraper.

```bash
ovn-network-agent --metrics-listen 127.0.0.1:9273
curl -s http://127.0.0.1:9273/metrics
```

Two paths are served:

- `/metrics` — Prometheus exposition format
- `/healthz` — returns `200 ok` for liveness probes

All metrics are prefixed with `ovn_network_agent_`:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `reconcile_total` | counter | `trigger`={`event`,`periodic`,`startup`} | Total reconcile cycles |
| `reconcile_duration_seconds` | histogram | — | Per-cycle reconcile duration |
| `reconcile_in_progress` | gauge | — | `1` while a reconcile is running |
| `desired_ips` | gauge | — | Unique IPs the agent currently routes (FIPs + SNATs + port-forward VIPs) |
| `local_routers` | gauge | — | Routers whose chassisredirect port is locally active |
| `effective_networks` | gauge | — | Effective network filter count (manual or auto-discovered) |
| `route_readds_total` | counter | `plane`={`kernel`,`frr`} | Routes re-added by post-change verification |
| `consecutive_readds` | gauge | — | Consecutive cycles requiring re-adds (sustained non-zero indicates instability) |
| `ovn_connection_state` | gauge | `database`={`sb`,`nb`} | `1` when the OVN client is connected, `0` otherwise |
| `drain_duration_seconds` | histogram | — | Duration of gateway drain on shutdown |
| `drain_total` | counter | `outcome`={`completed`,`timeout`,`error`,`noop`} | Drain operations |
| `stale_chassis_cleanup_total` | counter | `outcome`={`success`,`error`} | Stale chassis cleanup events |
| `missing_chassis` | gauge | — | Chassis currently tracked as missing from the SB Chassis table |

### Suggested alerts

- `consecutive_readds >= 3` — persistent route instability (FRR or kernel races).
- `ovn_connection_state{database="nb"} == 0` for >2m — NB DB unreachable; agent cannot write OVN state.
- `rate(route_readds_total[10m]) > 0` — flapping routes.
- `histogram_quantile(0.95, rate(reconcile_duration_seconds_bucket[5m])) > 5` — slow reconciles.

## Architecture

### Control plane

The agent monitors OVN databases and writes routing state into four subsystems. On every change (or periodically as safety net) it reconciles the desired state:

```
 ┌───────────────┐                                                  ┌───────────────┐
 │  OVN SB DB    │                                                  │  OVN NB DB    │
 │               │                                                  │               │
 │ Port_Binding  │─── read ───┐                          ┌── read ──│ NAT           │
 │ Chassis       │            │                          │  write ──│ Logical_Router│
 └───────────────┘            │                          │          │ Static_Route  │
                              │                          │          │ MAC_Binding   │
                              ▼                          ▼          └───────────────┘
                  ┌──────────────────────────────────────────────┐
                  │                 ovn-network-agent            │
                  │                                              │
                  │  ┌────────────┐    ┌──────────────────────┐  │
                  │  │  OVSDB IDL │    │   Event Processing   │  │
                  │  │  Monitors  │───►│                      │  │
                  │  └────────────┘    │  Fast: failover <10ms│  │
                  │                    │  Normal: debounce 500ms │
                  │                    └──────────┬───────────┘  │
                  │                               │              │
                  │                    ┌──────────▼───────────┐  │
                  │                    │     Reconciler       │  │
                  │                    │                      │  │
                  │                    │  1. Compute desired  │  │
                  │                    │     IPs (FIPs+SNATs) │  │
                  │                    │  2. Ensure OVS flows │  │
                  │                    │  3. Ensure OVN GW ──────────► OVN NB
                  │                    │     routing          │  │    (default route
                  │                    │  4. Sync routes      │  │     + MAC binding)
                  │                    │  5. Verify + re-add  │  │
                  │                    └──┬──────┬──────┬─────┘  │
                  └───────────────────────┼──────┼──────┼────────┘
                                          │      │      │
                           ┌──────────────┘      │      └───────────────┐
                           ▼                     ▼                      ▼
                 ┌───────────────────┐ ┌───────────────────┐ ┌───────────────────┐
                 │ Kernel (netlink)  │ │ OVS (ovs-ofctl)   │ │ FRR (vtysh)       │
                 │                   │ │                   │ │                   │
                 │ /32 routes +rules │ │ MAC-tweak flows   │ │ ip route in VRF   │
                 │ proxy ARP on br-ex│ │ Hairpin flows     │ │ → BGP announce    │
                 │                   │ │ on br-ex          │ │                   │
                 └───────────────────┘ └───────────────────┘ └───────────────────┘
```

For each locally-active router the agent:

1. Writes a **default route** (`0.0.0.0/0 via <virtual-gw>`) and **static MAC binding** into OVN NB — the [virtual gateway](#gatewayless-provider-networks) makes reply traffic exit the logical router without a real upstream gateway
2. Installs **OVS MAC-tweak flows** on `br-ex` — rewrites the destination MAC on packets arriving from OVN's patch port so the kernel accepts them
3. Installs **OVS hairpin flows** on `br-ex` — reflects same-chassis cross-router traffic back into OVN via `output:in_port` with rewritten MACs
4. Creates `/32` **kernel routes** (with `ip rule` entries when using a dedicated routing table) on `br-ex` so the kernel can receive packets for each FIP
5. Creates `/32` **FRR static routes** in `vrf-provider` so BGP announces each FIP to the external fabric
6. Triggers a **BGP outbound soft-refresh** only when routes are removed (withdrawals) — additions rely on FRR's normal route redistribution to avoid disrupting existing BGP announcements
7. **Verifies** all desired routes (FRR and kernel) after every route change and re-adds any that went missing as a safety net

### Data plane

This diagram shows the complete packet path on a gateway node. The upper half (default VRF) handles OVN traffic and kernel routing. The lower half (`vrf-provider`) handles BGP announcement and external delivery. The veth pair managed by the agent (`--veth-leak-enabled`) bridges the two VRFs.

```
 ┌──────────────────────────────────────────────────────────────────────────────────────┐
 │                                    Gateway Node                                      │
 │                                                                                      │
 │  ┌───────────────────────────────── Default VRF ──────────────────────────────────┐  │
 │  │                                                                                │  │
 │  │   ┌───────────────────────────┐             ┌──────────────────────────────┐   │  │
 │  │   │  br-ex (provider bridge)  │ patch port  │  br-int (OVN integration)    │   │  │
 │  │   │                           │◄───────────►│                              │   │  │
 │  │   │  proxy ARP enabled        │             │  OVN Logical Router:         │   │  │
 │  │   │  bridge IP 169.254.169.254│             │   DNAT: FIP → VM IP          │   │  │
 │  │   │  /32 route per FIP        │             │   SNAT: VM IP → FIP          │   │  │
 │  │   │  MAC-tweak flows (0x999)  │             │   default route → .254 ¹     │   │  │
 │  │   │  Hairpin flows   (0x998)  │             │   MAC binding → br-ex MAC    │   │  │
 │  │   │                           │             │                              │   │  │
 │  │   └─────────┬─────────────────┘             │                              │   │  │
 │  │        physical NIC                         │  ¹ virtual gateway (last     │   │  │
 │  │        (uplink)                             │    usable IP in subnet)      │   │  │
 │  │             │                               └──────────────┬───────────────┘   │  │
 │  │             │                                              │                   │  │
 │  │             │                                     VM (10.0.0.5)                │  │
 │  │             │                                     FIP: 203.0.113.10            │  │
 │  │                                                                                │  │
 │  │   ┌─────────────────┐                                                          │  │
 │  │   │  veth-default   │    ip rule: from <provider-net> → lookup table 200       │  │
 │  │   │  169.254.0.1/30 │    table 200: default via 169.254.0.2                    │  │
 │  │   └────────┬────────┘                                                          │  │
 │  └────────────┼───────────────────────────────────────────────────────────────────┘  │
 │          veth pair                                                                   │
 │  ┌────────────┼────────────────────── vrf-provider ──────────────────────────────┐   │
 │  │            │                                                                  │   │
 │  │   ┌────────▼────────┐        ┌──────────────────────────┐                     │   │
 │  │   │  veth-provider  │        │        FRR / BGP         │                     │   │
 │  │   │  169.254.0.2/30 │        │   announces /32 routes   │                     │   │
 │  │   └─────────────────┘        │   via BGP peering        │                     │   │
 │  │                              └─────────────┬────────────┘                     │   │
 │  │                                            │                                  │   │
 │  │   <net>/24 via 169.254.0.1                 │  (→ default VRF, return path)    │   │
 │  │   <FIP>/32 via 169.254.0.1                 │  (agent-managed, per FIP)        │   │
 │  └────────────────────────────────────────────┼──────────────────────────────────┘   │
 └───────────────────────────────────────────────┼──────────────────────────────────────┘
                                                 │ BGP peering (/32 FIP routes)
                                                 ▼
                                         ┌───────────────┐
                                         │   External    │
                                         │  BGP Router / │
                                         │    Fabric     │
                                         └───────────────┘
```

#### Forward path (external client → VM)

1. External router learns `198.51.100.10/32` via BGP from FRR in `vrf-provider`
2. Packet (`dst=198.51.100.10`) arrives at `br-ex` via the physical NIC
3. Kernel finds the `/32` route on `br-ex` (scope link); proxy ARP resolves the FIP to the bridge MAC
4. OVS MAC-tweak flow rewrites the destination MAC to `br-ex` MAC and passes the packet to `br-int`
5. OVN Logical Router applies DNAT: `198.51.100.10` → `10.0.0.5`
6. Packet is delivered to the VM on its internal network

#### Return path (VM → external client)

1. VM sends reply (`src=10.0.0.5`, `dst=external client`)
2. OVN Logical Router applies SNAT: source becomes `198.51.100.10`
3. OVN forwards via default route (`0.0.0.0/0 via .254` — the virtual gateway) + static MAC binding (`.254` → `br-ex` MAC) → packet exits through `br-ex`
4. Packet leaves `br-ex` with `src=198.51.100.10` (falls in a provider network range)
5. Policy rule `from <net> → lookup table 200` matches the source address
6. Table 200 routes via `169.254.0.2` → veth pair → packet enters `vrf-provider`
7. FRR/BGP in `vrf-provider` delivers the packet to the external fabric

#### VRF route leaking

The agent creates `/32` FRR routes inside `vrf-provider`, but reply traffic from OVN arrives in the default VRF on `br-ex`. A veth pair bridges the two VRFs so that:

- **Default VRF → `vrf-provider`**: An `ip rule` matches the source address of reply packets against the discovered (or configured) provider networks and redirects them into routing table 200. Table 200 has a default route via `169.254.0.2` (the `veth-provider` end), which moves the packet into `vrf-provider` for BGP delivery.
- **`vrf-provider` → Default VRF**: Network routes in `vrf-provider` (e.g. `192.0.2.0/24 via 169.254.0.1`) send return traffic back through the veth pair into the default VRF for normal kernel delivery.

The agent creates the veth pair and assigns link-local addresses at startup (`--veth-leak-enabled`, on by default). Per-network policy rules and routes are reconciled dynamically — networks are either auto-discovered from OVN `Logical_Router_Port.Networks` or taken from the static `network_cidr` configuration. On shutdown, all resources are cleaned up.

## Origin

This agent is based on the shell script [`ovn-network-agent.sh`](./contrib/ovn-network-agent.sh) which served as the original prototype. The built-in veth VRF leak functionality (`--veth-leak-enabled`) replaces the standalone script [`veth-vrf-leak.sh`](./contrib/veth-vrf-leak.sh).

## License

Apache License 2.0 — see [LICENSE](./LICENSE).
