#!/usr/bin/env bash
# Stale-chassis cleanup scenario for the containerlab E2E harness.
#
# Goal (issue #111): when a gateway disappears WITHOUT a graceful
# shutdown (no SIGTERM, no drain), the NB DB rows the agent owned on
# its behalf must be garbage-collected by surviving peers after
# `stale_chassis_grace_period` elapses — otherwise NB grows unboundedly
# across container churn and stale entries can black-hole traffic.
#
# Why a hard kill (and not `docker stop`):
# `docker stop` delivers SIGTERM, which the agent traps for a graceful
# shutdown — that path is exercised by #105's failover scenario via
# `ovn-ctl stop_controller` and is intentionally NOT what this test
# covers. `docker kill -s KILL` skips the agent's cleanup path entirely;
# the process is reaped without ever running its EXIT hooks. The
# surviving agents are the only ones that can clean up after the dead
# chassis — which is exactly the behaviour under test.
#
# Why we also `ovn-sbctl chassis-del` after the kill:
# A killed ovn-controller does NOT remove its own SB Chassis row — the
# row was written by ovn-controller itself and is only released on
# graceful shutdown. In production the row is reaped by an external
# reaper (e.g. neutron-ovn-agent on chassis-down, ovn-northd's own
# stale-chassis sweeper on recent OVN versions, or an HA orchestrator
# observing the node down). The cleanup path under test only fires
# when surviving agents see the chassis as absent from SB, so the
# scenario removes the row explicitly after the kill to simulate that
# external reaper. Without this, gateway-1 stays "alive" in SB
# forever and the agent's stale-cleanup loop has nothing to mark
# missing.
#
# Why a sentinel route is seeded:
# In the baseline lab the only managed NB rows the agent writes are
# default routes via the virtual gateway IP. When the master fails
# over, the new master `ensureDefaultRoute` re-tags that row from
# `gateway-1` to itself (see ovn_gateway.go:218) instead of deleting
# it — so there is nothing left in NB tagged for the dead chassis,
# and the cleanup path is never exercised. To make the cleanup path
# observable in this lab, we seed one extra managed static route
# tagged with `ovn-network-agent-chassis=gateway-1` BEFORE the kill;
# the unique sentinel prefix is not the default route, so no
# surviving agent will re-claim it. The agent's
# CleanupStaleChassisManagedEntries deletes it after the grace period.
#
# Pre-condition: the lab is up. When SANITY_GATE=1 (default) this
# scenario runs `baseline.sh` first so a broken green path is reported
# as a baseline regression instead of being attributed to the
# stale-chassis path.
#
# Why an external NB drain after the kill:
# In production, when a node dies hard, an external HA orchestrator
# (BFD-based monitor, Pacemaker, neutron-ovn-agent, …) reacts by
# lowering the dead node's Gateway_Chassis priorities to 0 — the
# exact same NB mutation the agent's own DrainGateways performs on
# graceful shutdown (see ovn_gateway.go:589 / agent.go:164). Without
# this step OVN re-elects gateway-1 as master the moment
# `docker start` brings it back, because its priority-30 entry is
# still in NB. Setting priority 0 after the kill keeps gateway-2
# (priority 20) stable as master across the teardown.
#
# Teardown: gateway-1 is `docker start`ed so the artifact collector
# can still `docker exec` into it for OVS/FRR dumps. The lab is left
# in a "single-master on gateway-2, gateway-1 drained" state — not
# baseline-HA-symmetric, but baseline-green: the workload is on
# gateway-3 and gateway-2 is still advertising the FIPs, so
# `make e2e-baseline` keeps passing against the same lab. The
# containerlab veth `gateway-1:eth1 ↔ upstream:eth1` IS destroyed by
# the kill and is not re-wired on restart, but that no longer
# matters: gateway-1 is at priority 0 and never gets elected. The
# follow-up failover scenario, however, can no longer find a master
# to fail — local users who want to chain `make e2e-failover` after
# `make e2e-stale-chassis` must recycle the lab with
# `make e2e-down && make e2e-up` first. CI handles this
# automatically — the workflow's `make e2e-down` step runs with
# `if: always()` regardless of the scenario outcome.
#
# Environment overrides:
#   LAB                    container-name prefix (defaults to the topology name "ovn-e2e")
#   MASTER                 chassis to kill (default gateway-1, the priority-30 chassis)
#   PEERS                  surviving chassis (space-separated, default "gateway-2 gateway-3")
#   CENTRAL                OVN central container (defaults to clab-${LAB}-central)
#   LR_NAME                logical router to attach the sentinel route to (default lr0)
#   LR_PUBLIC_PORT         LR external port name (default lr0-public — used for the drain step)
#   STALE_GRACE_PERIOD     agent grace period (informational; matches gwnode-config.yaml, default 30s)
#   STALE_TIMEOUT          seconds to wait for cleanup after the kill (default 150)
#   SENTINEL_PREFIX        unique prefix for the sentinel managed route (default 203.0.113.42/32)
#   SENTINEL_NEXTHOP       nexthop for the sentinel route (default 169.254.0.1, the leak link-local)
#   ARTIFACTS_DIR          directory to write before/after NB snapshots + peer cleanup log (default empty = skip)
#   SANITY_GATE            run baseline.sh first when 1 (default 1)

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
MASTER="${MASTER:-gateway-1}"
MASTER_NODE="clab-${LAB}-${MASTER}"
PEERS="${PEERS:-gateway-2 gateway-3}"
CENTRAL="${CENTRAL:-clab-${LAB}-central}"
LR_NAME="${LR_NAME:-lr0}"
LR_PUBLIC_PORT="${LR_PUBLIC_PORT:-lr0-public}"
STALE_GRACE_PERIOD="${STALE_GRACE_PERIOD:-30s}"
STALE_TIMEOUT="${STALE_TIMEOUT:-150}"
SENTINEL_PREFIX="${SENTINEL_PREFIX:-203.0.113.42/32}"
SENTINEL_NEXTHOP="${SENTINEL_NEXTHOP:-169.254.0.1}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-}"
SANITY_GATE="${SANITY_GATE:-1}"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

log() { printf '[stale-chassis] %s\n' "$*" >&2; }

nbctl() { docker exec "${CENTRAL}" ovn-nbctl "$@"; }
sbctl() { docker exec "${CENTRAL}" ovn-sbctl "$@"; }

# Write `content` to `path` under ARTIFACTS_DIR when it is set; quietly
# no-op otherwise. Keeps the local-run path free of stray /tmp files
# while still bundling triage evidence into the CI artifact tarball.
write_artifact() {
    local path="$1"
    if [ -z "${ARTIFACTS_DIR}" ]; then
        return 0
    fi
    mkdir -p "${ARTIFACTS_DIR}"
    cat >"${ARTIFACTS_DIR}/${path}"
}

# Run baseline.sh as a sanity gate so a broken green path fails fast
# under the right label. Disabled by SANITY_GATE=0 — useful when
# iterating locally on the stale-chassis behaviour itself.
sanity_gate() {
    if [ "${SANITY_GATE}" != "1" ]; then
        log "SANITY_GATE=${SANITY_GATE}: skipping baseline pre-check"
        return 0
    fi
    if [ ! -x "${BASELINE}" ]; then
        log "baseline script not executable at ${BASELINE} — skipping pre-check"
        return 0
    fi
    log "running baseline.sh as a sanity gate (set SANITY_GATE=0 to skip)"
    "${BASELINE}"
}

# Resolve the SB Chassis UUID for the named chassis. Returns empty
# string when no such chassis exists (e.g. already killed and reaped).
chassis_uuid() {
    local name="$1"
    sbctl --bare --columns=_uuid find Chassis name="${name}" 2>/dev/null \
        | tr -d '\r' | head -n1
}

# List the UUIDs of NB static routes tagged with
# `ovn-network-agent=managed` AND `ovn-network-agent-chassis=<name>`.
# Empty stdout means no rows match.
managed_route_uuids_for_chassis() {
    local name="$1"
    nbctl --bare --columns=_uuid find Logical_Router_Static_Route \
        external_ids:ovn-network-agent=managed \
        external_ids:ovn-network-agent-chassis="${name}" 2>/dev/null \
        | tr -d '\r' | grep -v '^$' || true
}

# Full row dump of any NB static route tagged for the given chassis.
# Used for triage on failure (written into the artifact bundle).
managed_route_rows_for_chassis() {
    local name="$1"
    nbctl find Logical_Router_Static_Route \
        external_ids:ovn-network-agent=managed \
        external_ids:ovn-network-agent-chassis="${name}" 2>/dev/null || true
}

# Seed a sentinel managed static route tagged for MASTER. Done in a
# single `ovn-nbctl` transaction so the row + the LR mutation are
# atomic. Re-running is safe: the find/create pair only creates when
# no row already matches the sentinel prefix.
seed_sentinel_route() {
    local existing
    existing=$(nbctl --bare --columns=_uuid find Logical_Router_Static_Route \
        ip_prefix="\"${SENTINEL_PREFIX}\"" \
        external_ids:ovn-network-agent-chassis="${MASTER}" 2>/dev/null \
        | tr -d '\r' | head -n1 || true)
    if [ -n "${existing}" ]; then
        log "sentinel route already present (uuid=${existing}); skipping create"
        return 0
    fi
    log "seeding sentinel managed route ${SENTINEL_PREFIX} via ${SENTINEL_NEXTHOP} tagged for ${MASTER}"
    nbctl --id=@route create Logical_Router_Static_Route \
        ip_prefix="\"${SENTINEL_PREFIX}\"" \
        nexthop="\"${SENTINEL_NEXTHOP}\"" \
        external_ids:ovn-network-agent=managed \
        external_ids:ovn-network-agent-chassis="${MASTER}" \
        -- add Logical_Router "${LR_NAME}" static_routes @route >/dev/null
}

# Remove the sentinel route if it is still present at teardown time.
# Idempotent — the post-cleanup expectation is that the row is already
# gone, so the find returns empty and we no-op. Best-effort: a teardown
# failure must never mask the scenario's own pass/fail signal.
remove_sentinel_route() {
    local existing
    existing=$(nbctl --bare --columns=_uuid find Logical_Router_Static_Route \
        ip_prefix="\"${SENTINEL_PREFIX}\"" 2>/dev/null \
        | tr -d '\r' | head -n1 || true)
    if [ -z "${existing}" ]; then
        return 0
    fi
    log "teardown: removing residual sentinel route ${SENTINEL_PREFIX}"
    nbctl --if-exists lr-route-del "${LR_NAME}" "${SENTINEL_PREFIX}" || true
}

# Grep the docker logs of each surviving gateway for the agent's
# `stale chassis route removed` log line referencing the dead chassis.
# Returns 0 (with the matching line(s) on stdout) as soon as one peer
# logs the cleanup, non-zero if no peer logged it.
find_peer_cleanup_log() {
    local peer
    for peer in ${PEERS}; do
        local matches
        matches=$(docker logs "clab-${LAB}-${peer}" 2>&1 \
            | grep -E 'msg="stale chassis route removed"' \
            | grep "chassis=${MASTER}" || true)
        if [ -n "${matches}" ]; then
            printf '[%s] %s\n' "${peer}" "${matches}"
            return 0
        fi
    done
    return 1
}

# Restart the killed master container so `docker exec` works again
# (the artifact collector needs OVS/FRR dumps from gateway-1 — its
# logs survive even on an exited container, but `ovs-vsctl show` and
# friends need a live PID). Because the scenario externally drained
# gateway-1's Gateway_Chassis priority to 0 after the kill, OVN does
# not re-elect gateway-1 on restart and gateway-2 stays master —
# baseline reachability through the FIPs keeps working. The lab is
# now HA-asymmetric (single-master), not baseline-symmetric;
# subsequent `make e2e-failover` runs against the same lab will not
# behave usefully (no priority-30 master to fail), so chain
# `make e2e-down && make e2e-up` before another destructive
# scenario. Best-effort: failures here are logged but never raised.
restore_master() {
    log "teardown: starting ${MASTER_NODE} (so the artifact collector can exec into it)"
    docker start "${MASTER_NODE}" >/dev/null \
        || log "teardown: docker start returned non-zero; continuing"

    remove_sentinel_route

    log "teardown: lab is now single-master on a peer of ${MASTER}; ${MASTER} stays drained (priority 0)."
    log "teardown: baseline reachability via the new master still works; failover semantics do not."
    log "teardown: chain 'make e2e-down && make e2e-up' before another destructive scenario."
}

main() {
    sanity_gate

    local master_uuid
    master_uuid=$(chassis_uuid "${MASTER}")
    if [ -z "${master_uuid}" ]; then
        log "ERROR: ${MASTER} is not registered as a SB Chassis — cannot test stale cleanup"
        return 1
    fi
    log "${MASTER} chassis UUID = ${master_uuid}"

    seed_sentinel_route

    local before_uuids
    before_uuids=$(managed_route_uuids_for_chassis "${MASTER}")
    if [ -z "${before_uuids}" ]; then
        log "ERROR: no managed NB rows are tagged for ${MASTER} after seeding — aborting"
        return 1
    fi
    log "NB rows tagged for ${MASTER} before kill:"
    printf '%s\n' "${before_uuids}" | sed 's/^/    /' >&2

    {
        printf '# NB rows tagged for chassis=%s before kill\n' "${MASTER}"
        printf '# chassis_uuid=%s\n\n' "${master_uuid}"
        managed_route_rows_for_chassis "${MASTER}"
    } | write_artifact "nb-before-kill.txt"

    # The hard kill skips the agent's SIGTERM handler entirely. OVN
    # does NOT auto-remove the SB Chassis row when ovn-controller dies
    # — that row is only released on graceful shutdown. Without the
    # `chassis-del` below the surviving agents would keep seeing
    # gateway-1 as alive forever and never run their cleanup loop.
    # See the header comment for why this is the right simulation of
    # production behaviour.
    trap restore_master EXIT
    log "hard-killing ${MASTER_NODE} (SIGKILL — agent has no chance to clean up)"
    docker kill -s KILL "${MASTER_NODE}" >/dev/null

    # Drain the dead chassis in NB by setting its Gateway_Chassis
    # priority to 0. This is the same NB mutation DrainGateways
    # writes on graceful shutdown (ovn_gateway.go:589) — we run it
    # externally here because the killed agent never got to do it.
    # In production an HA orchestrator does this step (see header).
    # Without it OVN re-elects gateway-1 as master on `docker start`
    # because the priority-30 entry survives in NB.
    log "draining ${MASTER}'s Gateway_Chassis priority to 0 (simulates HA orchestrator)"
    nbctl lrp-set-gateway-chassis "${LR_PUBLIC_PORT}" "${MASTER}" 0 >/dev/null

    log "removing ${MASTER}'s SB Chassis row (simulates the external reaper)"
    sbctl --if-exists chassis-del "${MASTER}" >/dev/null

    # Poll the NB DB until the rows tagged for the dead chassis are
    # gone. The deadline allows for: one reconcile cycle to mark the
    # chassis missing (now immediate, since SB no longer lists it),
    # the configured grace period + up to 30s jitter, and one more
    # reconcile cycle to actually execute the cleanup.
    log "waiting up to ${STALE_TIMEOUT}s for surviving agents to clean up rows tagged for ${MASTER}"
    log "    (configured stale_chassis_grace_period in gwnode-config.yaml: ${STALE_GRACE_PERIOD})"
    local start_ts deadline
    start_ts=$(date +%s)
    deadline=$(( start_ts + STALE_TIMEOUT ))
    local after_uuids="${before_uuids}"
    while (( $(date +%s) < deadline )); do
        after_uuids=$(managed_route_uuids_for_chassis "${MASTER}")
        if [ -z "${after_uuids}" ]; then
            log "NB rows tagged for ${MASTER} were removed after $(( $(date +%s) - start_ts ))s"
            break
        fi
        sleep 2
    done

    {
        printf '# NB rows tagged for chassis=%s after wait (timeout=%ds)\n' \
            "${MASTER}" "${STALE_TIMEOUT}"
        managed_route_rows_for_chassis "${MASTER}"
    } | write_artifact "nb-after-kill.txt"

    if [ -n "${after_uuids}" ]; then
        log "ERROR: NB still contains rows tagged for ${MASTER} after ${STALE_TIMEOUT}s:"
        printf '%s\n' "${after_uuids}" | sed 's/^/    /' >&2
        return 1
    fi

    # Prove the deletion was deliberate (a surviving agent emitted the
    # cleanup log line) rather than coincidental (some other code path
    # deleted the row).
    log "verifying a surviving peer emitted the cleanup log line referencing ${MASTER}"
    local cleanup_log
    if ! cleanup_log=$(find_peer_cleanup_log); then
        log "ERROR: no surviving peer (${PEERS}) logged 'stale chassis route removed' for chassis=${MASTER}"
        printf '# no peer logged the cleanup\n' | write_artifact "peer-cleanup.log"
        return 1
    fi
    log "peer cleanup log:"
    printf '%s\n' "${cleanup_log}" | sed 's/^/    /' >&2
    printf '%s\n' "${cleanup_log}" | write_artifact "peer-cleanup.log"

    log "stale-chassis cleanup confirmed"
}

main "$@"
