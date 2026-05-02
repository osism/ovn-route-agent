# Integration tests

Integration tests exercise the agent against a real OVN/OVS/FRR/nftables stack
on a single Linux host. They live behind the `integration` build tag so the
default `go test ./...` run does not pick them up.

## Layout

```
test/integration/
  README.md                        — this file
  setup.sh                         — apt + service bootstrap (run once per host)
  smoke_test.go                    — connect / initial-reconcile / clean-shutdown smoke test
  scenarios_helpers_test.go        — shared per-scenario boilerplate (startScenario, readyAgent)
  scenario_fip_test.go             — FIP add/remove, gatewayless gw, multi-router on one chassis
  scenario_failover_test.go        — failover, stale-chassis cleanup, drain & restore-drained
  testenv/                         — Setup, Teardown, RunAgent, MakeLocalRouter, Assert*, …
```

Scenario tests pause `ovn-northd` for the duration of each test (via
`testenv.PauseOVNNorthd`) so the test driver can write SB Port_Binding rows
directly without northd garbage-collecting them. `ovn-controller` keeps
running. `setup.sh` is unchanged from the harness foundation.

## Local prerequisites

- Linux host (Ubuntu 24.04 recommended; the harness skips on macOS / Windows).
- Root privileges (`CAP_NET_ADMIN` is required for netlink operations in
  `routing_linux.go`).
- Go toolchain matching `go.mod`.
- A host you do not mind mutating: `setup.sh` installs packages, creates
  `br-ex`, configures FRR with a `vrf-provider` VRF, and so on.

## Running locally

```sh
sudo ./test/integration/setup.sh    # one-time, installs packages and starts services
make test-integration                # builds the binary and runs the tagged tests
```

`make test-integration` builds `./ovn-network-agent` first, then exports
`OVN_AGENT_BINARY=$PWD/ovn-network-agent` and invokes
`go test -tags=integration -v -count=1 ./test/integration/...`.

To point the harness at a binary built elsewhere:

```sh
sudo OVN_AGENT_BINARY=/path/to/ovn-network-agent \
    go test -tags=integration -v -count=1 ./test/integration/...
```

The tests skip (rather than fail) when:

- not running on Linux, or
- not running as root, or
- `br-ex`, `nft`, `ovs-vsctl`, `ovs-ofctl`, or `vtysh` is missing.

Run `setup.sh` if you see skip messages for the bridge or binaries.

## Adding new tests

1. Create a `*_test.go` file in `test/integration/` with the build tag:
   ```go
   //go:build integration

   package integration
   ```
2. Call `testenv.Setup(t)` first; it registers a cleanup that scrubs leftover
   nft tables / kernel routes / FRR static routes between tests.
3. Drive the agent via `testenv.RunAgent(t, cfg)` and verify outcomes with the
   `Assert*` helpers (`AssertKernelRoute`, `AssertOVSFlow`, `AssertFRRRoute`,
   `AssertNftRule`).
4. Always call `proc.Stop(timeout)` — if the test forgets, `t.Cleanup`
   SIGKILLs the process, but state cleanup may be incomplete.

## CI

`.github/workflows/integration.yml` runs the same flow on `ubuntu-latest`:
`setup.sh` → `make build` → `go test -tags=integration ...` under sudo.
