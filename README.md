# ovn-route-agent

Event-driven Floating IP route synchronization agent for OVN-based OpenStack environments. A real-time daemon that watches OVN databases directly via the OVSDB protocol.

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
   - Ensures `/32` **kernel routes** (with IP rules when using a dedicated routing table) and **FRR static routes** in the VRF for each FIP/SNAT IP
   - If configured, reconciles the **FRR prefix-list** with `permit <network> ge 32 le 32` entries for each discovered provider network
   - If no routers are locally active: removes all managed routes
5. **Reconciles** periodically as a safety net (default: every 60s)
6. **Cleans up** on shutdown (SIGINT/SIGTERM) — removes all managed routes, OVS flows, and the bridge IP before exiting (configurable via `cleanup_on_shutdown`)

## Building

Requires Go 1.22+.

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

Produces a single binary `ovn-route-agent`.

## Configuration

Settings are loaded with the following priority (highest wins):

**CLI flags > environment variables > config file > defaults**

### Config file (YAML)

```bash
ovn-route-agent --config /etc/ovn-route-agent/config.yaml
# or via environment variable
OVN_ROUTE_CONFIG=/etc/ovn-route-agent/config.yaml ovn-route-agent
```

See [`ovn-route-agent.yaml.sample`](ovn-route-agent.yaml.sample) for a full example.

### Example

Config file `/etc/ovn-route-agent/config.yaml` with the base settings:

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
ovn-route-agent --config /etc/ovn-route-agent/config.yaml --log-level debug --dry-run
```

CLI flags take precedence over values in the config file.

### Reference

| Flag | Env Var | Config key | Default | Description |
|------|---------|------------|---------|-------------|
| `--config` | `OVN_ROUTE_CONFIG` | — | | Path to YAML config file |
| `--ovn-sb-remote` | `OVN_ROUTE_OVN_SB_REMOTE` | `ovn_sb_remote` | *(required)* | OVN Southbound DB remote, comma-separated for cluster failover |
| `--ovn-nb-remote` | `OVN_ROUTE_OVN_NB_REMOTE` | `ovn_nb_remote` | *(required)* | OVN Northbound DB remote, comma-separated for cluster failover |
| `--bridge-dev` | `OVN_ROUTE_BRIDGE_DEV` | `bridge_dev` | `br-ex` | Provider bridge device |
| `--vrf-name` | `OVN_ROUTE_VRF_NAME` | `vrf_name` | `vrf-provider` | VRF name for FRR routes |
| `--veth-nexthop` | `OVN_ROUTE_VETH_NEXTHOP` | `veth_nexthop` | `169.254.0.1` | Nexthop for FRR static routes |
| `--network-cidr` | `OVN_ROUTE_NETWORK_CIDR` | `network_cidr` | *(empty = auto-discover)* | Filter FIPs by CIDRs; when empty, networks are auto-discovered from OVN `Logical_Router_Port.Networks` |
| `--gateway-port` | `OVN_ROUTE_GATEWAY_PORT` | `gateway_port` | *(empty = all)* | Chassisredirect port filter; empty = track all routers automatically |
| `--route-table-id` | `OVN_ROUTE_ROUTE_TABLE_ID` | `route_table_id` | `0` | Routing table ID for FIP routes (1-252); 0 = main table |
| `--bridge-ip` | `OVN_ROUTE_BRIDGE_IP` | `bridge_ip` | `169.254.169.254` | Link-local IP added to the bridge device for ARP resolution |
| `--ovs-wrapper` | `OVN_ROUTE_OVS_WRAPPER` | `ovs_wrapper` | *(empty)* | Command prefix for containerized OVS (e.g. `docker exec openvswitch_vswitchd`) |
| `--reconcile-interval` | `OVN_ROUTE_RECONCILE_INTERVAL` | `reconcile_interval` | `60s` | Full reconciliation interval |
| `--log-level` | `OVN_ROUTE_LOG_LEVEL` | `log_level` | `info` | Log level (debug, info, warn, error) |
| `--dry-run` | `OVN_ROUTE_DRY_RUN` | `dry_run` | `false` | Connect and reconcile but only log what would be done |
| `--cleanup-on-shutdown` | `OVN_ROUTE_CLEANUP_ON_SHUTDOWN` | `cleanup_on_shutdown` | `true` | Remove all managed routes on shutdown; set to `false` to keep routes in place |
| `--frr-prefix-list` | `OVN_ROUTE_FRR_PREFIX_LIST` | `frr_prefix_list` | `ANNOUNCED-NETWORKS` | FRR prefix-list name to manage dynamically; adds `permit <network> ge 32 le 32` entries for each discovered provider network (set to empty string to disable) |
| `--veth-leak-enabled` | `OVN_ROUTE_VETH_LEAK_ENABLED` | `veth_leak_enabled` | `true` | Enable automatic veth VRF route leaking |
| `--veth-provider-ip` | `OVN_ROUTE_VETH_PROVIDER_IP` | `veth_provider_ip` | *(nexthop+1)* | IP of the veth-provider side (auto-computed from `veth_nexthop` + 1) |
| `--veth-leak-table-id` | `OVN_ROUTE_VETH_LEAK_TABLE_ID` | `veth_leak_table_id` | `200` | Routing table for the leak default route (1-252, must differ from `route_table_id`) |
| `--veth-leak-rule-priority` | `OVN_ROUTE_VETH_LEAK_RULE_PRIORITY` | `veth_leak_rule_priority` | `2000` | Policy rule priority for veth leak rules |
| `--version` | — | — | — | Print version and exit |

## Installation

Pre-built binaries and Debian packages for `amd64` and `arm64` are available on the [GitHub Releases](https://github.com/osism/ovn-route-agent/releases) page.

### Debian package

```bash
# Download the .deb package (replace VERSION and ARCH as needed)
curl -LO https://github.com/osism/ovn-route-agent/releases/download/vVERSION/ovn-route-agent_VERSION_ARCH.deb

# Example: v0.1.0, amd64
curl -LO https://github.com/osism/ovn-route-agent/releases/download/v0.1.0/ovn-route-agent_0.1.0_amd64.deb

# Install
sudo dpkg -i ovn-route-agent_0.1.0_amd64.deb
```

The package installs:

- `/usr/bin/ovn-route-agent` — the binary
- `/lib/systemd/system/ovn-route-agent.service` — systemd service
- `/etc/default/ovn-route-agent` — environment defaults (preserved on upgrade)
- `/etc/ovn-route-agent/config.yaml.sample` — sample configuration

After installation, create your configuration and start the service:

```bash
sudo cp /etc/ovn-route-agent/config.yaml.sample /etc/ovn-route-agent/config.yaml
sudo vi /etc/ovn-route-agent/config.yaml
sudo systemctl enable --now ovn-route-agent
```

### Binary

```bash
# Download the static binary (replace ARCH as needed: amd64 or arm64)
curl -LO https://github.com/osism/ovn-route-agent/releases/download/vVERSION/ovn-route-agent-linux-ARCH

# Example: v0.1.0, amd64
curl -LO https://github.com/osism/ovn-route-agent/releases/download/v0.1.0/ovn-route-agent-linux-amd64

# Install
sudo install -m 0755 ovn-route-agent-linux-amd64 /usr/local/bin/ovn-route-agent
```

Set up the systemd service and configuration manually:

```bash
sudo cp ovn-route-agent.service /etc/systemd/system/
sudo cp ovn-route-agent.default /etc/default/ovn-route-agent

sudo mkdir -p /etc/ovn-route-agent
sudo cp ovn-route-agent.yaml.sample /etc/ovn-route-agent/config.yaml
sudo vi /etc/ovn-route-agent/config.yaml

sudo systemctl daemon-reload
sudo systemctl enable --now ovn-route-agent
```

### From source

```bash
make build-static
sudo install -m 0755 ovn-route-agent /usr/local/bin/ovn-route-agent
```

### Check status

```bash
sudo systemctl status ovn-route-agent
sudo journalctl -u ovn-route-agent -f
```

## Prerequisites

- **OVN**: TCP access to OVN Southbound and Northbound databases on the control nodes (the agent runs on network/gateway nodes where no local DB sockets exist)
- **FRR**: `vtysh` must be available and the VRF + BGP configuration must already exist
- **Linux**: Provider bridge (e.g. `br-ex`) must exist
- **VRF route leaking**: The agent automatically creates and manages a veth pair connecting the default VRF to `vrf-provider` (enabled by default via `--veth-leak-enabled`). Per-network routes are reconciled dynamically based on auto-discovered or configured provider networks.
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

Both entries are tagged with `ExternalIDs["ovn-route-agent"] = "managed"` so the agent can track and clean them up. If a default route already exists that was **not** created by the agent (i.e. a real gateway configured by OpenStack), the agent leaves it untouched.

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
                  │                 ovn-route-agent              │
                  │                                              │
                  │  ┌────────────┐    ┌──────────────────────┐  │
                  │  │  OVSDB IDL │    │   Event Processing   │  │
                  │  │  Monitors  │───►│                      │  │
                  │  └────────────┘    │  Fast: failover <10ms│  │
                  │                    │  Normal: debounce 600ms │
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
                 │ proxy ARP on br-ex│ │ on br-ex          │ │ → BGP announce    │
                 └───────────────────┘ └───────────────────┘ └───────────────────┘
```

For each locally-active router the agent:

1. Writes a **default route** (`0.0.0.0/0 via <virtual-gw>`) and **static MAC binding** into OVN NB — the [virtual gateway](#gatewayless-provider-networks) makes reply traffic exit the logical router without a real upstream gateway
2. Installs **OVS MAC-tweak flows** on `br-ex` — rewrites the destination MAC on packets arriving from OVN's patch port so the kernel accepts them
3. Creates `/32` **kernel routes** (with `ip rule` entries when using a dedicated routing table) on `br-ex` so the kernel can receive packets for each FIP
4. Creates `/32` **FRR static routes** in `vrf-provider` so BGP announces each FIP to the external fabric
5. Triggers a **BGP outbound soft-refresh** only when routes are removed (withdrawals) — additions rely on FRR's normal route redistribution to avoid disrupting existing BGP announcements
6. **Verifies** all desired routes (FRR and kernel) after every route change and re-adds any that went missing as a safety net

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
 │  │   │                           │             │   MAC binding → br-ex MAC    │   │  │
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

This agent is based on the shell script [`ovn-route-agent.sh`](./contrib/ovn-route-agent.sh) which served as the original prototype. The built-in veth VRF leak functionality (`--veth-leak-enabled`) replaces the standalone script [`veth-vrf-leak.sh`](./contrib/veth-vrf-leak.sh).

## License

Apache License 2.0 — see [LICENSE](./LICENSE).
