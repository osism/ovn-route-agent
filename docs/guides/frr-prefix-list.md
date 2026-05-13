# Configure the FRR prefix list

By default the agent maintains an FRR prefix-list named `ANNOUNCED-NETWORKS`
that controls which prefixes are eligible for BGP redistribution. On every
reconciliation cycle, the agent emits `permit <network> ge 32 le 32` entries
for each discovered (or manually configured) provider network so FRR will
re-advertise the per-FIP `/32` static routes the agent writes.

## Defaults

The relevant setting is `frr_prefix_list` (CLI: `--frr-prefix-list`, env:
`OVN_NETWORK_FRR_PREFIX_LIST`). It defaults to `ANNOUNCED-NETWORKS`.

```yaml
# Override the prefix-list name
frr_prefix_list: "MY-ANNOUNCED-PREFIXES"

# Or disable prefix-list management entirely
frr_prefix_list: ""
```

When set to an empty string the agent does not touch the prefix list at all
— useful if you manage BGP filtering with an external tool.

## What the agent emits

Once provider networks are known (auto-discovered from
`Logical_Router_Port.Networks` or specified via `network_cidr`), the agent
keeps the prefix list synchronised with `permit <network> ge 32 le 32`
entries for each network. Entries for networks the agent no longer manages
are removed during the next reconciliation.

The prefix list itself must already be referenced from your BGP route-map /
redistribute statements. The agent only manages the prefix-list contents — it
does not modify your BGP configuration.

## Where to go next

- [Configuration reference](../reference/configuration) — every flag, env
  var, and config key.
- [Architecture](../explanation/architecture) — where the prefix list fits in
  the control plane.
