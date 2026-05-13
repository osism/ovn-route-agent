# Metrics

The agent can expose Prometheus-formatted metrics on an optional HTTP
endpoint. Enable it by setting `--metrics-listen` (or
`OVN_NETWORK_METRICS_LISTEN` / `metrics_listen`) to a `host:port` such as
`127.0.0.1:9273`. The endpoint is **off by default**; bind to `127.0.0.1` for
node-local scraping, or to `0.0.0.0` for a remote scraper.

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

## Suggested alerts

- `consecutive_readds >= 3` — persistent route instability (FRR or kernel
  races).
- `ovn_connection_state{database="nb"} == 0` for >2m — NB DB unreachable;
  agent cannot write OVN state.
- `rate(route_readds_total[10m]) > 0` — flapping routes.
- `histogram_quantile(0.95, rate(reconcile_duration_seconds_bucket[5m])) > 5`
  — slow reconciles.
