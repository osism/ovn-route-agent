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
   - Ensures a **default route** (`0.0.0.0/0`) and **static MAC binding** in OVN NB so reply traffic exits the router correctly (no OpenStack gateway IP needed)
   - Installs **OVS MAC-tweak flows** on the provider bridge
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

## Deployment

### Install binary

```bash
make build-static
sudo install -m 0755 ovn-route-agent /usr/local/bin/ovn-route-agent
```

### Install systemd service

```bash
sudo cp ovn-route-agent.service /etc/systemd/system/
sudo cp ovn-route-agent.default /etc/default/ovn-route-agent

# Create configuration directory and config file from sample
sudo mkdir -p /etc/ovn-route-agent
sudo cp ovn-route-agent.yaml.sample /etc/ovn-route-agent/config.yaml

# Edit configuration
sudo vi /etc/ovn-route-agent/config.yaml

sudo systemctl daemon-reload
sudo systemctl enable --now ovn-route-agent
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

The agent supports OpenStack provider networks configured with `disable_gateway_ip: true`. In this setup there is no physical upstream gateway — all traffic is routed via BGP `/32` announcements.

For each locally-active router, the agent automatically:

1. Computes a **virtual gateway IP** (last usable IP in the subnet, e.g. `.254` for a `/24`)
2. Creates a **default route** (`0.0.0.0/0 via <virtual-gw>`) on the OVN logical router
3. Creates a **static MAC binding** mapping the virtual gateway IP to the local `br-ex` MAC

This enables OVN to route reply traffic out the external port without requiring a real gateway. On failover, the MAC binding is automatically updated to point to the new node's `br-ex` MAC.

## Architecture

### Control plane

The agent monitors OVN databases and writes routing state into four subsystems. On every change (or periodically as safety net) it reconciles the desired state:

```
                         ┌──────────────────────────────┐
                         │       ovn-route-agent        │
                         │                              │
   OVN SB DB ◄───────────┤  OVSDB IDL Monitor           │
   (Port_Binding,        │         │                    │
    Chassis)             │    Event Handler             │
                         │         │                    │
   OVN NB DB ◄──────────►┤  OVSDB IDL Monitor +         │
   (NAT, LR, LRP,        │  Gateway Route Writer        │
    Static Routes,       │         │                    │
    MAC Bindings)        │    ┌────▼──────┐             │
                         │    │ Reconcile │             │
                         │    └────┬──────┘             │
                         │         │                    │
                         │    ┌────▼──────┐             │
                         │    │  Routing  │             │
                         │    └─┬──┬──┬───┘             │
                         └──────┼──┼──┼─────────────────┘
                                │  │  │
                 ┌──────────────┘  │  └───────────────┐
                 ▼                 ▼                  ▼
       Kernel (netlink)     OVS (ovs-ofctl)    FRR (vtysh)
       /32 routes + rules   MAC-tweak flows    ip route in VRF
       proxy ARP on br-ex   on br-ex           → BGP announcement
```

For each locally-active router the agent:

1. Writes a **default route** (`0.0.0.0/0`) and **static MAC binding** into OVN NB so reply traffic exits the logical router correctly (gatewayless provider networks only)
2. Installs **OVS MAC-tweak flows** on `br-ex` so packets from OVN reach the kernel with the correct destination MAC
3. Creates `/32` **kernel routes** (with `ip rule` entries when using a dedicated routing table) on `br-ex` so the kernel can receive packets for each FIP
4. Creates `/32` **FRR static routes** in `vrf-provider` so BGP announces each FIP to the external fabric
5. Triggers a **BGP outbound soft-refresh** only when routes are removed (withdrawals) — additions rely on FRR's normal route redistribution to avoid disrupting existing BGP announcements
6. **Verifies** all desired routes (FRR and kernel) after every route change and re-adds any that went missing as a safety net

### Data plane

This diagram shows the complete packet path on a gateway node. The upper half (default VRF) handles OVN traffic and kernel routing. The lower half (`vrf-provider`) handles BGP announcement and external delivery. The veth pair managed by the agent (`--veth-leak-enabled`) bridges the two VRFs.

```
 ┌─────────────────────────────────────────────────────────────────────┐
 │                          Gateway Node                               │
 │                                                                     │
 │  ┌────────────────────────────── Default VRF ─────────────────────┐ │
 │  │                                                                │ │
 │  │  ┌──────────────────────────────────────────────┐              │ │
 │  │  │              br-ex (provider bridge)         │              │ │
 │  │  │  - proxy ARP enabled                         │              │ │
 │  │  │  - bridge IP 169.254.169.254/32              │              │ │
 │  │  │  - /32 kernel route per FIP (scope link)     │              │ │
 │  │  │  - OVS MAC-tweak flows (cookie 0x999)        │              │ │
 │  │  └──────────┬──────────────────────┬────────────┘              │ │
 │  │        physical NIC           patch port                       │ │
 │  │        (uplink)               to br-int                        │ │
 │  │             │                      │                           │ │
 │  │             │                 ┌────▼──────────────────────┐    │ │
 │  │             │                 │  br-int (OVN integration) │    │ │
 │  │             │                 │                           │    │ │
 │  │             │                 │  OVN Logical Router:      │    │ │
 │  │             │                 │   DNAT: FIP → VM IP       │    │ │
 │  │             │                 │   SNAT: VM IP → FIP       │    │ │
 │  │             │                 │   default route → .254    │    │ │
 │  │             │                 │   MAC binding → br-ex MAC │    │ │
 │  │             │                 └──────────────┬────────────┘    │ │
 │  │             │                                │                 │ │
 │  │             │                           VM (10.0.0.5)          │ │
 │  │             │                                                  │ │
 │  │  ┌─────────────────┐                                           │ │
 │  │  │  veth-default   │                                           │ │
 │  │  │  169.254.0.1/30 │                                           │ │
 │  │  └────────┬────────┘                                           │ │
 │  │           │          ip rule: from <net> → lookup table 200    │ │
 │  │       veth pair      table 200: default via 169.254.0.2        │ │
 │  │           │                                                    │ │
 │  └───────────┼────────────────────────────────────────────────────┘ │
 │              │                                                      │
 │  ┌───────────┼─────────────────── vrf-provider ───────────────────┐ │
 │  │           │                                                    │ │
 │  │  ┌────────▼────────┐       ┌────────────────────────┐          │ │
 │  │  │  veth-provider  │       │      FRR / BGP         │          │ │
 │  │  │  169.254.0.2/30 │       │  announces /32 routes  │          │ │
 │  │  └─────────────────┘       └───────────┬────────────┘          │ │
 │  │                                        │                       │ │
 │  │  <net>/24 via 169.254.0.1  (→ default VRF, return path)        │ │
 │  │  <FIP>/32 via 169.254.0.1  (agent-managed, per FIP)            │ │
 │  └────────────────────────────────────────┼───────────────────────┘ │
 └───────────────────────────────────────────┼─────────────────────────┘
                                             │ BGP peering
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
3. OVN forwards via default route (`.254`) + static MAC binding → packet exits through `br-ex`
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
