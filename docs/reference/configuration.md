# Configuration reference

Settings are loaded with the following priority (highest wins):

**CLI flags > environment variables > config file > defaults**

For task-oriented setup notes (where the config file lives, how to override
via env vars, the example config), see
[Configure the agent](../guides/configuration).

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
| `--drain-on-shutdown` | `OVN_NETWORK_DRAIN_ON_SHUTDOWN` | `drain_on_shutdown` | `true` | Drain HA gateways before shutdown by lowering `Gateway_Chassis` priority to 0 (see [Gateway drain mode](../explanation/gateway-drain)) |
| `--drain-timeout` | `OVN_NETWORK_DRAIN_TIMEOUT` | `drain_timeout` | `60s` | Maximum time to wait for gateway drain before proceeding with shutdown |
| `--frr-prefix-list` | `OVN_NETWORK_FRR_PREFIX_LIST` | `frr_prefix_list` | `ANNOUNCED-NETWORKS` | FRR prefix-list name to manage dynamically; adds `permit <network> ge 32 le 32` entries for each discovered provider network (set to empty string to disable) |
| `--stale-chassis-grace-period` | `OVN_NETWORK_STALE_CHASSIS_GRACE_PERIOD` | `stale_chassis_grace_period` | `5m` | Grace period before cleaning up OVN NB entries from chassis that have disappeared from the SB Chassis table; set to `0` to disable |
| `--metrics-listen` | `OVN_NETWORK_METRICS_LISTEN` | `metrics_listen` | *(empty = disabled)* | Bind address for the Prometheus `/metrics` endpoint (e.g. `127.0.0.1:9273`); see [metrics reference](metrics) |
| `--veth-leak-enabled` | `OVN_NETWORK_VETH_LEAK_ENABLED` | `veth_leak_enabled` | `true` | Enable automatic veth VRF route leaking |
| `--veth-provider-ip` | `OVN_NETWORK_VETH_PROVIDER_IP` | `veth_provider_ip` | *(nexthop+1)* | IP of the veth-provider side (auto-computed from `veth_nexthop` + 1) |
| `--veth-leak-table-id` | `OVN_NETWORK_VETH_LEAK_TABLE_ID` | `veth_leak_table_id` | `200` | Routing table for the leak default route (1-252, must differ from `route_table_id`) |
| `--veth-leak-rule-priority` | `OVN_NETWORK_VETH_LEAK_RULE_PRIORITY` | `veth_leak_rule_priority` | `2000` | Policy rule priority for veth leak rules |
| `--port-forward-dev` | `OVN_NETWORK_PORT_FORWARD_DEV` | `port_forward_dev` | `loopback1` | Loopback device for VIP addresses in VRF |
| `--port-forward-table-id` | `OVN_NETWORK_PORT_FORWARD_TABLE_ID` | `port_forward_table_id` | `201` | Routing table for DNAT return traffic (1-252, must differ from `route_table_id` and `veth_leak_table_id`) |
| `--port-forward-ct-zone` | `OVN_NETWORK_PORT_FORWARD_CT_ZONE` | `port_forward_ct_zone` | `64000` | Conntrack zone for DNAT flows (1-65535, must not collide with OVN zones) |
| `--port-forward-l3mdev-accept` | `OVN_NETWORK_PORT_FORWARD_L3MDEV_ACCEPT` | `port_forward_l3mdev_accept` | `false` | Set `udp/tcp_l3mdev_accept=1` for cross-VRF same-host DNAT backends |
| — | — | `port_forwards` | *(empty)* | List of VIPs with DNAT rules (YAML only, see [sample config](https://github.com/osism/ovn-network-agent/blob/main/ovn-network-agent.yaml.sample)) |
| `--version` | — | — | — | Print version and exit |
