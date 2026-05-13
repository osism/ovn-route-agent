# Configure gateway drain

The agent drains HA gateways on shutdown by default to eliminate the failover
gap between BGP withdrawal and OVN BFD detection. For the conceptual model
and the shutdown sequence diagrams, see
[Gateway drain mode](../explanation/gateway-drain).

## Default behavior

Drain mode is **enabled by default** with a 60-second timeout:

```yaml
# Enable/disable drain (default: true)
drain_on_shutdown: true

# Maximum time to wait for migration (default: 60s)
# After this timeout, the agent proceeds with shutdown even if some
# gateways have not yet migrated.
drain_timeout: "60s"
```

## Override via CLI flags

```bash
ovn-network-agent --drain-on-shutdown=false                 # disable drain
ovn-network-agent --drain-timeout 120s                      # increase timeout
```

## Override via environment variables

```bash
OVN_NETWORK_DRAIN_ON_SHUTDOWN=false                         # disable drain
OVN_NETWORK_DRAIN_TIMEOUT=120s                              # increase timeout
```

## When to disable drain

- **Single-chassis deployments** — if there is no standby chassis, lowering
  the priority has no effect and the timeout just delays shutdown.
- **Non-HA routers** — routers without multiple `Gateway_Chassis` entries
  cannot fail over; drain is a no-op (the agent detects this and skips
  immediately).
- **Environments where Neutron manages priorities** — if an external system
  actively manages `Gateway_Chassis` priorities and would conflict with the
  agent's changes.
