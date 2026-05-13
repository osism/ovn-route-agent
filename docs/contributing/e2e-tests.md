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
- `gateway-1` (the priority-30 chassis): a veth pair
  `vm1-host` ↔ `vm1-eth0` — the host side is bound to `br-int` with
  `external_ids:iface-id=ls0-vm1`, the peer side lives inside a `vm1`
  network namespace configured with `192.168.10.10/24` and a default
  route via `192.168.10.1`. This is the responder behind the FIP.

## Running locally

From the repository root:

```sh
make e2e-up          # build images + containerlab deploy + bootstrap
make e2e-baseline    # run the baseline reachability scenario
make e2e-down        # containerlab destroy
```

`make e2e-baseline` invokes `test/e2e/scenarios/baseline.sh`, which
pings the FIP `192.0.2.10` from `clab-ovn-e2e-client-1` and waits up
to 30s for the agent's reconcile loop to install the routes. The exit
code mirrors `ping`'s — any packet loss fails the scenario. Override
`FIP`, `RECONCILE_TIMEOUT`, `PING_COUNT` or `PING_TIMEOUT` in the
environment when triaging.

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

The job:

1. installs containerlab via the upstream one-liner installer,
2. runs `make e2e-up` to build the images, deploy the topology and
   seed the NB DB,
3. runs `test/e2e/scenarios/baseline.sh`,
4. on failure: dumps lab state via
   `test/e2e/scenarios/collect-artifacts.sh` and uploads the result
   as an artifact named `e2e-artifacts-<run id>-<attempt>`,
5. always runs `make e2e-down` so containers and docker networks do
   not leak between runs.

The whole job is capped at 15 minutes — matching the budget in issue
[#45](https://github.com/osism/ovn-network-agent/issues/45).

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
