# Containerlab E2E harness

The containerlab-based end-to-end test environment lives under
[`test/e2e/`](https://github.com/osism/ovn-network-agent/tree/main/test/e2e)
and is the foundation laid out by issue
[#44](https://github.com/osism/ovn-network-agent/issues/44) (parent
issue [#39](https://github.com/osism/ovn-network-agent/issues/39)).

The harness has two layers:

1. **Infrastructure** — image build files, the containerlab topology,
   and a bootstrap script that seeds the OVN NB DB. Delivered by
   [#44](https://github.com/osism/ovn-network-agent/issues/44).
2. **Scenarios** — bash probes under `test/e2e/scenarios/` that drive
   the running lab. The first one,
   [`baseline.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/baseline.sh),
   was added by
   [#45](https://github.com/osism/ovn-network-agent/issues/45) together
   with the CI workflow that runs it.
   [`failover.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/failover.sh)
   ([#105](https://github.com/osism/ovn-network-agent/issues/105))
   builds on the baseline and exercises HA re-election by stopping the
   priority-30 chassis.
   [`stale-chassis.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/stale-chassis.sh)
   ([#111](https://github.com/osism/ovn-network-agent/issues/111))
   hard-kills the priority-30 chassis and asserts that NB rows owned
   by the dead chassis are garbage-collected by surviving peers.
   [`hairpin.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/hairpin.sh)
   ([#108](https://github.com/osism/ovn-network-agent/issues/108))
   adds a second FIP backend co-located with the existing one on the
   active master and exercises the agent's `cookie=0x998`
   `actions=output:in_port` hairpin rule on `br-ex` end-to-end.

## Layout

```
test/e2e/
  Dockerfile.gwnode         — Ubuntu 24.04 + OVS + ovn-controller + FRR + agent
  Dockerfile.central        — ovn-northd + NB/SB ovsdb-server
  gwnode-entrypoint.sh      — starts OVS / ovn-controller / FRR, then execs the agent
  gwnode-config.yaml        — default agent config baked into the gwnode image
  central-entrypoint.sh     — starts ovn-northd + ovsdb-server, exposes 6641/6642
  topology.clab.yml         — containerlab topology (central + 3 gateways + upstream + 2 clients)
  bootstrap.sh              — idempotent OVN NB seed (1 LS, 1 LR with 2 FIPs, HA across 3 chassis)
  scenarios/
    baseline.sh             — baseline reachability scenario (issue #45)
    failover.sh             — HA failover scenario, master chassis loss (issue #105)
    hairpin.sh              — same-chassis hairpin scenario, two FIPs on master (issue #108)
    stale-chassis.sh        — stale chassis cleanup scenario, hard kill (issue #111)
    collect-artifacts.sh    — dump lab state for offline triage
```

## Prerequisites

- Linux host with Docker (privileged mode for containerlab containers).
- [containerlab](https://containerlab.dev/install/) ≥ 0.55.
- `make`, `docker buildx`, and a Go toolchain matching `go.mod` (the
  gwnode image builds the agent from source).

`make e2e-install-tools` bootstraps containerlab on Linux via the
upstream installer (`get.containerlab.dev`); it is a no-op when
`containerlab` is already on PATH.

macOS is **not** a supported host for containerlab itself — upstream
ships no darwin binary. The supported macOS workflow is to run the
lab from a Linux VM (OrbStack, Colima, Docker Desktop's Linux VM,
etc.); see the upstream guide at
<https://containerlab.dev/macos/>. `make e2e-install-tools` exits
with an explanatory error on macOS instead of pretending to install
something.

The lab also runs on a GitHub Actions `ubuntu-latest` runner without
extra setup: Docker is preinstalled and supports privileged
containers, which is what containerlab requires.

## Topology

```
            +--------+
            | client |
            |   -1   |
            +---+----+
                |
+----------+    |    +----------+
|          |----+----|          |
|  client  |         | upstream |--- BGP ---+
|   -2     |---------|  (FRR)   |           |
+----------+         +----+-----+           |
                          |                 |
       +------------------+------------------+
       |                  |                  |
+------+------+    +------+------+    +------+------+
|  gateway-1  |    |  gateway-2  |    |  gateway-3  |
|  (gwnode)   |    |  (gwnode)   |    |  (gwnode)   |
+------+------+    +------+------+    +------+------+
       |                  |                  |
       +-------- mgmt net (containerlab) ----+
                          |
                     +----+----+
                     | central |
                     +---------+
```

- The **management** network is the default containerlab bridge;
  every node has an `eth0` on it. The gateway agents reach
  `central:6642` (OVN SB) and `central:6641` (OVN NB) here.
- The **underlay** is a set of point-to-point links:
  `gateway-N:eth1 ↔ upstream:eth{1,2,3}`. BGP between the gateways
  and the upstream router runs over these links. No real BGP session
  is required for the lab to come up — FRR may stay idle.
- The two **clients** sit behind the upstream router
  (`upstream:eth4` and `upstream:eth5`) and are used as reachability
  probes for scenario-level tests.

## Bootstrap state

`bootstrap.sh` is idempotent — re-running it is a no-op. It first
waits for OVN NB to become reachable from the host, then provisions
the lab in three layers:

**NB DB:**

- tenant logical switch `ls0` (`192.168.10.0/24`),
- external logical switch `ls-public` with a `localnet` port that
  bridges to `physnet1` (which the gwnode entrypoint maps to `br-ex`
  via `ovn-bridge-mappings`),
- logical router `lr0` with two ports:
  `lr0-ls0` (`192.168.10.1/24`) attached to `ls0` and
  `lr0-public` (`192.0.2.1/24`) attached to `ls-public`,
- a Gateway_Chassis distribution on `lr0-public`:
  `gateway-1` priority 30, `gateway-2` priority 20,
  `gateway-3` priority 10,
- a default static route `0.0.0.0/0 → 192.0.2.254` (virtual
  nexthop — see the Static_MAC_Binding entry below),
- a `Static_MAC_Binding` for `192.0.2.254 → 02:00:00:00:fe:01` on
  `lr0-public` so the LR pipeline resolves the default nexthop
  without on-wire ARP. The agent's catch-all flow on `br-ex`
  rewrites `eth.dst` to the kernel side before the packet leaves
  OVN anyway, so the MAC itself is only needed to satisfy the LR
  pipeline.
- two `dnat_and_snat` NATs (FIPs):
  `192.0.2.10 ↔ 192.168.10.10` and
  `192.0.2.11 ↔ 192.168.10.11`,
- a workload LSP `ls0-vm1` on `ls0` with address
  `02:00:00:00:0a:0a 192.168.10.10`.

**Underlay (per gateway):**

- `eth1` is **moved out of `br-ex` into `vrf-provider`** and gets a
  routed `/30` underlay address (`gateway-N:eth1 = 100.64.N.2/30`).
  This is the change that lines the lab up with how the agent ships
  in production: the agent's policy rule
  `from 192.0.2.0/24 lookup 200` and leak-table default route point
  into `vrf-provider`, so `vrf-provider` needs a real underlay
  interface — not a port on `br-ex` that would loop back through
  the same policy rule.

**Outside OVN:**

- `upstream`: per-link `/30`s towards each gateway
  (`eth1 = 100.64.1.1/30`, `eth2 = 100.64.2.1/30`,
  `eth3 = 100.64.3.1/30`), `10.0.0.1/24` on `eth4` (towards
  `client-1`), IPv4 forwarding enabled, FRR with `bgpd` enabled and
  one eBGP neighbor per gateway.
- each gateway's FRR (in `vrf-provider`): eBGP against its specific
  upstream `/30` endpoint, redistributing the FIP `/32` static
  routes that the agent installs in `vrf-provider`. The placeholder
  neighbor pushed by `gwnode-entrypoint.sh` (`192.0.2.1`) is
  replaced by `bootstrap.sh` once the underlay is up.
- `client-1`: `10.0.0.2/24` on `eth1`, default route via `10.0.0.1`.
- `gateway-3` (the priority-10 chassis): a veth pair
  `vm1-host` ↔ `vm1-eth0` — the host side is bound to `br-int` with
  `external_ids:iface-id=ls0-vm1`, the peer side lives inside a `vm1`
  network namespace configured with `192.168.10.10/24` and a default
  route via `192.168.10.1`. This is the responder behind the FIP.
  The workload sits on `gateway-3` (not the master) so failover
  scenarios that stop the master chassis do not also take out the
  responder — see issue
  [#105](https://github.com/osism/ovn-network-agent/issues/105). As a
  side effect, the baseline exercises cross-chassis geneve
  (master `gateway-1` ↔ workload host `gateway-3`).

## Running locally

From the repository root:

```sh
make e2e-up             # build images + containerlab deploy + bootstrap
make e2e-baseline       # run the baseline reachability scenario
make e2e-failover       # run the HA failover scenario (master chassis loss)
make e2e-hairpin        # run the same-chassis hairpin scenario (two FIPs on master)
make e2e-stale-chassis  # run the stale-chassis cleanup scenario (hard kill)
make e2e-down           # containerlab destroy
```

`make e2e-baseline` invokes `test/e2e/scenarios/baseline.sh`, which
pings the FIP `192.0.2.10` from `clab-ovn-e2e-client-1` and waits up
to 30s for the agent's reconcile loop to install the routes. The exit
code mirrors `ping`'s — any packet loss fails the scenario. Override
`FIP`, `RECONCILE_TIMEOUT`, `PING_COUNT` or `PING_TIMEOUT` in the
environment when triaging.

`make e2e-failover` invokes `test/e2e/scenarios/failover.sh`, which
runs the baseline first as a sanity gate (disable with
`SANITY_GATE=0`), simulates chassis loss by stopping `ovn-controller`
on `gateway-1` via `ovn-ctl stop_controller` (clean SIGTERM,
ovn-controller releases its claim on `cr-lr0-public`, OVN re-elects
to the priority-20 chassis), and polls reachability through the FIP
until the new master answers. The deadline is `FAILOVER_TIMEOUT`
(default 30s); after recovery the scenario asserts that
`cr-lr0-public` actually migrated away from `MASTER` (guarding
against a false pass where OVS keeps executing stale flows), then
runs a 5-packet final probe that must return 100% success. An EXIT
trap then starts `ovn-controller` again via `ovn-ctl start_controller`
and waits up to `FAILBACK_TIMEOUT` (default 60s) for `cr-lr0-public`
to bind back **and** for `client-1 → FIP` reachability to come back,
leaving the lab at baseline state. Override `MASTER`, `FIP`,
`FAILOVER_TIMEOUT`, `FAILBACK_TIMEOUT` or `SANITY_GATE` when
triaging.

The scenario stops just `ovn-controller` rather than the whole
container because containerlab wires the per-gateway underlay
(`gateway-N:eth1 ↔ upstream:ethN`) as veth pairs at deploy time and
does not re-establish them on container restart — `docker stop` /
`docker start` would leave the master with no underlay and no BGP
session, so the lab could not be returned to baseline. The OVN HA
mechanism under test (re-election of `cr-lr0-public` after the
priority-30 chassis's claim goes away) is the same either way.

`make e2e-hairpin` invokes `test/e2e/scenarios/hairpin.sh`, which
exercises the agent's same-chassis hairpin OpenFlow rule (`cookie=0x998`,
`actions=output:in_port`) on `br-ex`. The baseline lab seeds a single
FIP-with-backend on `lr0` (`192.0.2.10` → `192.168.10.10` on `vm1`,
hosted on `gateway-3`); the hairpin path can only fire when a second
FIP backend co-located on the same active master exists. The scenario
runs the baseline first as a sanity gate (disable with `SANITY_GATE=0`),
then adds — scenario-locally — a second FIP `192.0.2.12` on `lr0` with
a backing LSP `ls0-vm2` (`192.168.10.12`, MAC `02:00:00:00:0a:0b`) and
a `vm2` netns + veth on `gateway-3` so the new FIP has a real
responder. It polls `gateway-1` for the new hairpin flow on `br-ex`
(default `RECONCILE_TIMEOUT=30s`), asserts that **both** FIPs have a
`cookie=0x998` flow with `actions=output:in_port` (matching the issue's
acceptance criterion of "at least one matching rule per FIP on the
chassis"), and finally runs the probe: `ping -c 5 -W 2 192.0.2.12`
from inside the existing `vm1` netns on `gateway-3`. The probe's exit
code is the scenario's exit code — any packet loss fails the run. The
EXIT trap removes the LSP, NAT, host-side veth and netns added for
the second FIP, returning the lab to baseline so a subsequent
`make e2e-baseline` keeps passing.

The scenario uses **option (2)** from issue #108 (scenario-local
addition of the second FIP) rather than (1) (a third FIP baked into
`bootstrap.sh`). Keeping the baseline minimal means failover and
stale-chassis still observe the same lab the original issues
specified, and the hairpin scenario remains self-contained — its
teardown leaves nothing behind for other scenarios to trip over.

The probe runs from inside `vm1` (the existing FIP_A workload) and
targets FIP_B's external IP. The expected packet flow on a green
run: `vm1` (`192.168.10.10` on `gateway-3`) → geneve → `br-int` on
`gateway-1` → `lr0` pipeline (egress SNAT to `192.0.2.10`, route to
the connected `192.0.2.0/24` via `lr0-public`) → `cr-lr0-public` on
`gateway-1` → `ls-public` localnet → `br-ex`. There the agent's
`cookie=0x998` flow on `gateway-1` matches `ip_dst=192.0.2.12` and
reflects the packet back via `output:in_port` into OVN, where the
LR pipeline now ingresses on the external port, applies DNAT
(`192.0.2.12` → `192.168.10.12`), and forwards through `lr0-ls0` to
`vm2` on `gateway-3`. Without the hairpin flow OVS drops
`output:in_port` by default and the second-hop DNAT never fires,
which is what makes a regression in `ReconcileOVSHairpinFlows`
visible end-to-end. Override `FIP_B`, `FIP_B_INTERNAL`, `MASTER`,
`WORKLOAD_HOST`, `RECONCILE_TIMEOUT` or `SANITY_GATE` when triaging.

`make e2e-stale-chassis` invokes `test/e2e/scenarios/stale-chassis.sh`,
which exercises the agent's garbage-collection path for managed NB
rows after a peer chassis disappears WITHOUT a graceful shutdown. It
runs the baseline first as a sanity gate (disable with
`SANITY_GATE=0`), seeds a sentinel managed static route on `lr0`
tagged with `external_ids:ovn-network-agent-chassis=<MASTER>`, then:

1. **Hard-kills `gateway-1`** with `docker kill -s KILL` — the
   agent's SIGTERM handler is intentionally skipped, so the dead
   chassis leaves no trace cleaned up by itself.
2. **Drains the dead chassis in NB** with
   `ovn-nbctl lrp-set-gateway-chassis lr0-public <MASTER> 0` — the
   same mutation the agent's own `DrainGateways` writes on graceful
   shutdown ([ovn_gateway.go:589](https://github.com/osism/ovn-network-agent/blob/main/ovn_gateway.go#L589)).
   We run it externally because the killed agent never got to do
   it; in production an HA orchestrator (BFD monitor / Pacemaker /
   neutron-ovn-agent) is responsible for this step.
3. **Removes the SB Chassis row** with `ovn-sbctl chassis-del` —
   simulates the external reaper (neutron-ovn-agent on
   `chassis-down`, ovn-northd's own stale-chassis sweeper on recent
   OVN versions, or an HA orchestrator observing the node down).
   A killed ovn-controller does not remove its own SB row, so
   without this surviving agents would keep seeing the dead chassis
   as alive and the cleanup loop would never fire.

Surviving agents on `gateway-2` and `gateway-3` then notice that
the chassis row is gone from SB, wait `stale_chassis_grace_period`
(configured to `30s` in the gwnode E2E config so the scenario stays
inside its CI budget), and remove the rows tagged for the dead
chassis via `CleanupStaleChassisManagedEntries`. The scenario polls
NB for up to `STALE_TIMEOUT` (default 150s) and additionally greps
the surviving agents' `docker logs` for the
`stale chassis route removed` line referencing `chassis=<MASTER>` —
both signals must fire to prove the cleanup was deliberate. On exit
the killed chassis is restarted with `docker start` (so the
artifact collector can still `docker exec` into it) and the
residual sentinel route is removed. Override `MASTER`, `PEERS`,
`STALE_TIMEOUT`, `SENTINEL_PREFIX`, `LR_PUBLIC_PORT` or
`SANITY_GATE` when triaging.

Because the scenario externally drains `gateway-1`'s
`Gateway_Chassis` priority to 0 after the kill, OVN does **not**
re-elect `gateway-1` on `docker start` and `gateway-2` stays master.
`make e2e-baseline` against the same lab keeps passing —
reachability via the new master is intact. The lab is, however,
HA-asymmetric afterwards (single-master, `gateway-1` permanently
drained at priority 0), so `make e2e-failover` against the same lab
has no priority-30 master left to fail and will misbehave. Chain
`make e2e-down && make e2e-up` between destructive scenarios when
running locally. CI handles this automatically through the
workflow's `make e2e-down` step, which runs with `if: always()`
regardless of the scenario outcome.

The explicit `chassis-del` after the kill simulates the external
reaper that would remove a dead chassis row in production
(neutron-ovn-agent on `chassis-down`, ovn-northd's own stale-chassis
sweeper on recent OVN versions, or an HA orchestrator observing the
node down). A killed ovn-controller does **not** remove its own SB
row — the row is only released on graceful shutdown — so without
the explicit deletion, surviving agents would keep seeing the dead
chassis as alive and the cleanup loop would never fire. The path
under test is what the agent does after the row disappears, not how
the row disappears.

A sentinel managed route is needed because the production code path
that the cleanup loop targets (managed static routes tagged with a
chassis name) is not exercised by the baseline lab on its own: the
agent only creates a default route via the virtual gateway IP, and
the new master re-tags that row instead of leaving an orphan for the
cleanup loop to find (see `ensureDefaultRoute` in `ovn_gateway.go`).
Seeding a unique sentinel prefix gives the cleanup loop a row that no
surviving agent will reclaim, so its deletion is unambiguous evidence
of the stale-chassis path running.

Why a hard kill (and not `docker stop` or `ovn-ctl stop_controller`):
`docker stop` delivers SIGTERM, which the agent traps to run its
graceful-shutdown path — that case is what the failover scenario
already covers. The stale-chassis path is specifically for the
non-graceful death case where surviving peers are the only ones that
can clean up. `docker kill -s KILL` skips every signal handler in the
container.

Equivalent manual sequence (useful for triage):

```sh
docker build -f test/e2e/Dockerfile.central -t ovn-network-agent/central:e2e .
docker build -f test/e2e/Dockerfile.gwnode  -t ovn-network-agent/gwnode:e2e  .

sudo containerlab deploy   -t test/e2e/topology.clab.yml
./test/e2e/bootstrap.sh

# Inspect the agent on gateway-1:
docker exec clab-ovn-e2e-gateway-1 ovn-network-agent --help
docker exec clab-ovn-e2e-gateway-1 ovs-vsctl show
# Linux truncates /proc/<pid>/comm to 15 chars, so the agent process must
# be matched via the full cmdline (pgrep -f), not pgrep -x.
docker exec clab-ovn-e2e-gateway-1 pgrep -f /usr/local/bin/ovn-network-agent

# Inspect OVN central:
docker exec clab-ovn-e2e-central ovn-nbctl show

sudo containerlab destroy -t test/e2e/topology.clab.yml --cleanup
```

## Image-size budget

The acceptance criteria require the gwnode image to stay under
**600 MB**. The Dockerfile keeps the build slim by:

- using `--no-install-recommends` for every apt install,
- aggressively purging `software-properties-common` after the
  `cloud-archive:flamingo` PPA is registered,
- removing `/var/lib/apt/lists`, `/var/cache/apt/archives`,
  `/usr/share/doc`, `/usr/share/man`, `/usr/share/locale` after
  install,
- copying only the statically-linked agent binary from the Go build
  stage.

Check the resulting size with:

```sh
docker image inspect ovn-network-agent/gwnode:e2e \
    --format '{{.Size}}' | numfmt --to=iec --suffix=B
```

## Continuous integration

The harness runs on every push to `main` and on manual
`workflow_dispatch` via
[`.github/workflows/e2e.yml`](https://github.com/osism/ovn-network-agent/blob/main/.github/workflows/e2e.yml).
The workflow does **not** run on pull requests: spinning the lab up
adds ~10 minutes to CI on a green run, which is too coarse for the
per-PR feedback loop the rest of the workflows target.

Four jobs run, each on its own runner so a regression in one
scenario is reported in isolation:

- **`baseline`** — installs containerlab, runs `make e2e-up`, executes
  `test/e2e/scenarios/baseline.sh`, dumps + uploads artifacts on
  failure (`e2e-artifacts-<run id>-<attempt>`), and always tears the
  lab down.
- **`failover`** (`needs: baseline`) — same shape as baseline, but
  executes `test/e2e/scenarios/failover.sh`. On failure the artifact
  bundle is uploaded as `e2e-artifacts-failover-<run id>-<attempt>`.
- **`hairpin`** (`needs: baseline`, runs in parallel with `failover`)
  — same shape, but executes `test/e2e/scenarios/hairpin.sh`. The
  job points the scenario's `ARTIFACTS_DIR` at the same artifact
  root so the before/after `cookie=0x998` `dump-flows` snapshots are
  bundled with the lab-state dump. On failure the artifact bundle
  is uploaded as `e2e-artifacts-hairpin-<run id>-<attempt>`.
- **`stale-chassis`** (`needs: failover`) — same shape, but executes
  `test/e2e/scenarios/stale-chassis.sh`. The job points the
  scenario's `ARTIFACTS_DIR` at the same artifact root so the
  before/after NB snapshots and the peer cleanup-log capture are
  bundled with the lab-state dump. On failure the artifact bundle is
  uploaded as `e2e-artifacts-stale-chassis-<run id>-<attempt>`.

All four jobs are capped at 15 minutes — matching the budgets in
issues
[#45](https://github.com/osism/ovn-network-agent/issues/45),
[#105](https://github.com/osism/ovn-network-agent/issues/105),
[#108](https://github.com/osism/ovn-network-agent/issues/108), and
[#111](https://github.com/osism/ovn-network-agent/issues/111).

### Triaging a failed run

The uploaded artifact bundle mirrors the directories the collector
writes:

```
<artifact>/
  inspect/containerlab.txt         — output of `containerlab inspect`
  docker/<node>.log                — `docker logs` per lab container
  ovs/<gateway>/show.txt           — OVS bridges and interfaces
  ovs/<gateway>/br-int-flows.txt   — OpenFlow dump for br-int
  ovs/<gateway>/br-ex-flows.txt    — OpenFlow dump for br-ex
  ovn/nb-show.txt                  — `ovn-nbctl show` on central
  ovn/sb-show.txt                  — `ovn-sbctl show` on central
  ovn/nb-<table>.txt               — full NB row dumps (NAT, Gateway_Chassis, …)
  ovn/sb-<table>.txt               — full SB row dumps (Chassis, Port_Binding, …)
  frr/<gateway>-running-config.txt — `vtysh -c "show running-config"`
  frr/<gateway>-bgp-summary.txt    — `vtysh -c "show bgp summary"`
  kernel/<gateway>-ip-route.txt    — `ip route show table all`
  agent/<gateway>.log              — copy of the gateway container's stdout
  hairpin/hairpin-flows-before.txt — `cookie=0x998` flows on master:br-ex before adding FIP_B (hairpin only)
  hairpin/hairpin-flows-after.txt  — `cookie=0x998` flows on master:br-ex after adding FIP_B (hairpin only)
  stale-chassis/nb-before-kill.txt — NB rows tagged for the killed chassis pre-kill (stale-chassis only)
  stale-chassis/nb-after-kill.txt  — NB rows still tagged for the killed chassis after the cleanup deadline (stale-chassis only)
  stale-chassis/peer-cleanup.log   — surviving peer's `stale chassis route removed` line (stale-chassis only)
```

You can reproduce the same dump on a local lab with:

```sh
./test/e2e/scenarios/collect-artifacts.sh /tmp/e2e-artifacts
```

## Multi-architecture builds

The Go build stage in `Dockerfile.gwnode` honours `TARGETARCH`, and
the Ubuntu/OVS/OVN/FRR packages used at runtime are published for
both `amd64` and `arm64`. Multi-arch publication requires the
`docker-buildx-plugin` (on Debian/Ubuntu Docker CE hosts:
`sudo apt-get install -y docker-buildx-plugin`). Once it is
installed:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
    -f test/e2e/Dockerfile.gwnode -t ovn-network-agent/gwnode:e2e --push .
```

The `make e2e-up` target only builds for the host platform via plain
`docker build`, which is the right default for local development and
CI on a single runner and does not need the buildx plugin.
