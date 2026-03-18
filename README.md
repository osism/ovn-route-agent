# ovn-route-agent

Event-driven Floating IP route synchronization agent for OVN-based OpenStack environments. A real-time daemon that watches OVN databases directly via the OVSDB protocol.

## How it works

The agent monitors the OVN Southbound and Northbound databases and performs targeted writes to both OVN NB (default routes, static MAC bindings) and the local system (kernel routes, IP rules, FRR static routes, OVS flows on the provider bridge).

1. **Connects** to OVN Southbound and Northbound databases via OVSDB IDL
2. **Watches** for changes in real-time:
   - `Port_Binding` table (SB) — detects gateway chassis failover
   - `Chassis` table (SB) — detects chassis membership changes
   - `NAT` table (NB) — detects Floating IP and SNAT assignments
   - `Logical_Router` / `Logical_Router_Port` tables (NB) — maps NAT entries to their owning routers
3. **Sets up** the provider bridge at startup:
   - Adds a **link-local IP** to the bridge device so the kernel can perform ARP resolution
   - Enables **proxy ARP** on the bridge device so the kernel responds to ARP requests for FIP addresses
4. **Reacts** instantly to changes — for each router whose gateway is active on this chassis:
   - Ensures a **default route** (`0.0.0.0/0`) and **static MAC binding** in OVN NB so reply traffic exits the router correctly (no OpenStack gateway IP needed)
   - Installs **OVS MAC-tweak flows** on the provider bridge
   - Ensures `/32` **kernel routes** (with IP rules when using a dedicated routing table) and **FRR static routes** in the VRF for each FIP/SNAT IP
   - If no routers are locally active: removes all managed routes
5. **Reconciles** periodically as a safety net (default: every 60s)
6. **Cleans up** on shutdown (SIGINT/SIGTERM) — removes all managed routes and OVS flows before exiting (configurable via `cleanup_on_shutdown`)

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
network_cidr:
  - "192.0.2.0/24"
  - "198.51.100.0/24"
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
| `--network-cidr` | `OVN_ROUTE_NETWORK_CIDR` | `network_cidr` | *(empty = all)* | Filter FIPs by CIDRs (comma-separated for CLI/env, list for YAML) |
| `--gateway-port` | `OVN_ROUTE_GATEWAY_PORT` | `gateway_port` | *(empty = all)* | Chassisredirect port filter; empty = track all routers automatically |
| `--route-table-id` | `OVN_ROUTE_ROUTE_TABLE_ID` | `route_table_id` | `0` | Routing table ID for FIP routes (1-252); 0 = main table |
| `--bridge-ip` | `OVN_ROUTE_BRIDGE_IP` | `bridge_ip` | `169.254.169.254` | Link-local IP added to the bridge device for ARP resolution |
| `--ovs-wrapper` | `OVN_ROUTE_OVS_WRAPPER` | `ovs_wrapper` | *(empty)* | Command prefix for containerized OVS (e.g. `docker exec openvswitch_vswitchd`) |
| `--reconcile-interval` | `OVN_ROUTE_RECONCILE_INTERVAL` | `reconcile_interval` | `60s` | Full reconciliation interval |
| `--log-level` | `OVN_ROUTE_LOG_LEVEL` | `log_level` | `info` | Log level (debug, info, warn, error) |
| `--dry-run` | `OVN_ROUTE_DRY_RUN` | `dry_run` | `false` | Connect and reconcile but only log what would be done |
| `--cleanup-on-shutdown` | `OVN_ROUTE_CLEANUP_ON_SHUTDOWN` | `cleanup_on_shutdown` | `true` | Remove all managed routes on shutdown; set to `false` to keep routes in place |
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
- **VRF route leaking**: A veth pair connecting the default VRF to `vrf-provider` must be set up — see [`contrib/veth-vrf-leak.sh`](./contrib/veth-vrf-leak.sh) for a ready-made setup script and [`contrib/networks.txt.sample`](./contrib/networks.txt.sample) for the network list format
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

```
                    ┌──────────────────────────┐
                    │     ovn-route-agent      │
                    │                          │
  OVN SB DB ◄───────┤  OVSDB IDL Monitor       │
  (Port_Binding,    │         │                │
   Chassis)         │    Event Handler         │
                    │         │                │
  OVN NB DB ◄──────►┤  OVSDB IDL Monitor +     │
  (NAT, LR, LRP,    │  Gateway Route Writer    │
   Static Routes,   │         │                │
   MAC Bindings)    │         │                │
                    │    ┌────▼─────┐          │
                    │    │ Reconcile│          │
                    │    └────┬─────┘          │
                    │         │                │
                    │    ┌────▼─────┐          │
                    │    │ Routing  │          │
                    │    └─┬──┬──┬──┘          │
                    └──────┼──┼──┼─────────────┘
                           │  │  │
            ┌──────────────┘  │  └─────────────┐
            ▼                 ▼                ▼
  Kernel (netlink)     OVS (ovs-ofctl)   FRR (vtysh)
  /32 routes + rules   MAC-tweak flows   ip route in VRF
  proxy ARP on br-ex   on br-ex          → BGP announcement
```

## Origin

This agent is based on the shell script [`ovn-route-agent.sh`](./contrib/ovn-route-agent.sh) which served as the original prototype.

## License

Apache License 2.0 — see [LICENSE](./LICENSE).
