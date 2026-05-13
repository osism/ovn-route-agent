# Configure the agent

Settings are loaded with the following priority (highest wins):

**CLI flags > environment variables > config file > defaults**

For the full list of every flag, env var, and config key, see the
[configuration reference](../reference/configuration).

## Config file (YAML)

```bash
ovn-network-agent --config /etc/ovn-network-agent/config.yaml
# or via environment variable
OVN_NETWORK_CONFIG=/etc/ovn-network-agent/config.yaml ovn-network-agent
```

See
[`ovn-network-agent.yaml.sample`](https://github.com/osism/ovn-network-agent/blob/main/ovn-network-agent.yaml.sample)
for a full example.

## Example

Config file `/etc/ovn-network-agent/config.yaml` with the base settings:

```yaml
ovn_sb_remote: "tcp:10.10.0.1:6642,tcp:10.10.0.2:6642,tcp:10.10.0.3:6642"
ovn_nb_remote: "tcp:10.10.0.1:6641,tcp:10.10.0.2:6641,tcp:10.10.0.3:6641"

# Optional: provider networks are auto-discovered from OVN when omitted
# network_cidr:
#   - "192.0.2.0/24"
#   - "198.51.100.0/24"
```

Run with the config file, overriding log level and enabling dry-run via CLI
flags:

```bash
ovn-network-agent --config /etc/ovn-network-agent/config.yaml --log-level debug --dry-run
```

CLI flags take precedence over values in the config file.

## Prerequisites

- **OVN**: TCP access to OVN Southbound and Northbound databases on the
  control nodes (the agent runs on network/gateway nodes where no local DB
  sockets exist).
- **FRR**: `vtysh` must be available and the VRF + BGP configuration must
  already exist.
- **Linux**: provider bridge (e.g. `br-ex`) must exist.
- **VRF route leaking**: the agent automatically creates and manages a veth
  pair connecting the default VRF to `vrf-provider` (enabled by default via
  `--veth-leak-enabled`). Per-network routes are reconciled dynamically based
  on auto-discovered or configured provider networks.
- **nftables**: `nft` binary must be in `PATH` (required for port forwarding /
  DNAT).
- **Permissions**: root or `CAP_NET_ADMIN` for netlink route manipulation.

## Where to go next

- [Configuration reference](../reference/configuration) — every setting in one
  table.
- [Install the agent](installation) — packaging-specific install paths.
- [Configure gateway drain](gateway-drain) — recommended for HA deployments.
