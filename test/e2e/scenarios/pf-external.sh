#!/usr/bin/env bash
# Port-forward (DNAT) scenario for the containerlab E2E harness.
#
# Goal (issue #109): exercise the OVN port-forward / DNAT data path with
# traffic from an external client. A `client-1 → VIP:80` connection is
# DNAT'ed by OVN to a backend in the workload network; the backend MUST
# observe the original client source IP end-to-end. The "no SNAT on
# the way in" property is what OpenStack users rely on for source-IP
# based access control and audit logging — a stray SNAT rule that
# rewrites the client's underlay IP to the LR's internal address or
# the gateway chassis address would silently break that contract while
# still letting "connection succeeded" probes pass.
#
# What the scenario adds (and tears down again):
#   * Load_Balancer  pf-external  vips={"192.0.2.50:80"="192.168.10.10:8080"}
#                                 protocol=tcp           attached to lr0
#   * static route on upstream:   192.0.2.50/32 via <master underlay IP>
#   * scope-link route on master: 192.0.2.50/32 dev br-ex
#   * /tmp/pf-backend.log inside the workload host (collected as an
#     artifact regardless of pass/fail when ARTIFACTS_DIR is set)
#
# Why a Load_Balancer (and not a plain dnat_and_snat NAT row):
# `dnat_and_snat` performs SNAT on the way in as well, which is exactly
# the property the issue tells us to disprove. OVN's Load_Balancer
# does pure DNAT for external clients (no SNAT), matches a specific
# VIP:port, and supports the VIP-port → backend-port translation the
# issue calls for (`:80 → :8080`). This is also the canonical OVN
# port-forwarding primitive — `lr-lb-add` is the path
# neutron-ovn-agent / kube-ovn use in production. The hint in the
# issue about a future `ovn-nbctl lr-nat-add` wrapper refers to the
# proposed `clientctl` helper (issue #46); for a self-contained
# scenario, `ovn-nbctl lb-add` plus `lr-lb-add` are the right
# primitives today.
#
# Why two extra kernel routes have to be plumbed by hand:
# The agent only installs FRR/kernel routes for `dnat_and_snat` (FIPs)
# and `snat` (gateway SNAT IPs) NB rows — it does not yet propagate
# Load_Balancer VIPs into vrf-provider or the default-netns br-ex
# scope-link route (an obvious follow-up, but out of scope for #109).
# So the scenario seeds:
#   * a static route on `upstream` so client-1's packets head to the
#     master chassis, and
#   * a scope-link /32 in the master's default netns so the leaked
#     packet (vrf-provider → veth-default → default netns) is steered
#     onto br-ex, where OVS NORMAL forwards it through the patch port
#     into OVN's pipeline.
# The reverse path piggy-backs on the agent's existing
# `from 192.0.2.0/24 lookup 200` policy rule plus table-200's default
# route via `veth-provider`, so no extra work is needed for replies.
#
# Pre-condition: lab is up. When SANITY_GATE=1 (default) this scenario
# runs `baseline.sh` first so a broken green path is reported as a
# baseline regression instead of being attributed to port-forward.
#
# Teardown: the Load_Balancer, both kernel routes and the HTTP backend
# are removed on EXIT (via trap), returning the lab to baseline state.
# The CI workflow additionally invokes the same teardown step with
# `if: always()` so a fatal interpreter error here (which skips the
# trap) is still cleaned up.
#
# Environment overrides (used by the CI workflow):
#   LAB                container-name prefix (defaults to the topology name "ovn-e2e")
#   MASTER             chassis owning cr-lr0-public (defaults to gateway-1)
#   WORKLOAD_HOST      chassis hosting the FIP-A backend / pf-backend (defaults to gateway-3)
#   CENTRAL            OVN central container (defaults to clab-${LAB}-central)
#   LR_NAME            logical router (defaults to lr0)
#   VIP                external VIP that client-1 dials (default 192.0.2.50)
#   VIP_PORT           external TCP port on the VIP (default 80)
#   BACKEND_IP         internal backend address (default 192.168.10.10, == ls0-vm1)
#   BACKEND_PORT       internal TCP port on the backend (default 8080)
#   BACKEND_NETNS      netns on WORKLOAD_HOST where pf-backend runs (default vm1)
#   LB_NAME            Load_Balancer row name (default pf-external)
#   LB_PROTO           Load_Balancer protocol (default tcp)
#   MASTER_UNDERLAY    underlay IP of MASTER on its /30 to upstream (default 100.64.1.2)
#   UPSTREAM           upstream container name (defaults to clab-${LAB}-upstream)
#   CLIENT             external client container (defaults to clab-${LAB}-client-1)
#   CLIENT_DEV         client interface holding the underlay address (default eth1)
#   LOG_FILE           in-container path of the backend source-IP log (default /tmp/pf-backend.log)
#   RECONCILE_TIMEOUT  seconds to wait for the LB data path to come up (default 60)
#   CURL_TIMEOUT       per-attempt connect+read timeout for curl (default 3)
#   ARTIFACTS_DIR      directory to copy the backend log into (default empty = skip)
#   SANITY_GATE        run baseline.sh first when 1 (default 1)

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
MASTER="${MASTER:-gateway-1}"
MASTER_NODE="clab-${LAB}-${MASTER}"
WORKLOAD_HOST="${WORKLOAD_HOST:-gateway-3}"
WORKLOAD_NODE="clab-${LAB}-${WORKLOAD_HOST}"
CENTRAL="${CENTRAL:-clab-${LAB}-central}"
LR_NAME="${LR_NAME:-lr0}"
VIP="${VIP:-192.0.2.50}"
VIP_PORT="${VIP_PORT:-80}"
BACKEND_IP="${BACKEND_IP:-192.168.10.10}"
BACKEND_PORT="${BACKEND_PORT:-8080}"
BACKEND_NETNS="${BACKEND_NETNS:-vm1}"
LB_NAME="${LB_NAME:-pf-external}"
LB_PROTO="${LB_PROTO:-tcp}"
MASTER_UNDERLAY="${MASTER_UNDERLAY:-100.64.1.2}"
UPSTREAM="${UPSTREAM:-clab-${LAB}-upstream}"
CLIENT="${CLIENT:-clab-${LAB}-client-1}"
CLIENT_DEV="${CLIENT_DEV:-eth1}"
LOG_FILE="${LOG_FILE:-/tmp/pf-backend.log}"
RECONCILE_TIMEOUT="${RECONCILE_TIMEOUT:-60}"
CURL_TIMEOUT="${CURL_TIMEOUT:-3}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-}"
SANITY_GATE="${SANITY_GATE:-1}"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

log() { printf '[pf-external] %s\n' "$*" >&2; }

nbctl() { docker exec "${CENTRAL}" ovn-nbctl "$@"; }

# Detect the client-1 underlay address dynamically. The issue's example
# IP `198.51.100.<x>` is illustrative; in this lab client-1 actually
# lives at `10.0.0.2/24` on `eth1`. Reading the address with `ip -4 -o
# addr show` keeps the assertion robust against future topology
# changes that re-number the client subnet.
detect_client_ip() {
    docker exec "${CLIENT}" ip -4 -o addr show dev "${CLIENT_DEV}" \
        | awk '{print $4}' \
        | cut -d/ -f1 \
        | head -n1
}

# Run baseline.sh as a sanity gate so a broken green path fails fast
# under the right label. Disabled by SANITY_GATE=0 — useful when
# iterating locally on the port-forward behaviour itself.
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

# Start the HTTP backend inside the FIP-A workload netns on
# WORKLOAD_HOST. `docker exec -d` detaches: the process keeps running
# inside the container until the teardown step kills it via pkill.
# Truncating the log file up-front prevents a stale line from a prior
# (locally re-run) scenario from satisfying the assertion.
start_backend() {
    log "starting pf-backend on ${WORKLOAD_HOST} netns ${BACKEND_NETNS} (:${BACKEND_PORT}, log ${LOG_FILE})"
    docker exec "${WORKLOAD_NODE}" sh -c ": >'${LOG_FILE}'"
    docker exec -d "${WORKLOAD_NODE}" \
        ip netns exec "${BACKEND_NETNS}" \
        /usr/local/bin/pf-backend \
            -addr ":${BACKEND_PORT}" \
            -log "${LOG_FILE}"
    # Wait for the listener to actually bind. Without this the LB-add
    # below could race the first SYN against an unbound socket — the
    # TCP RST would short-circuit the reconcile wait below and the
    # scenario would look like a real OVN regression.
    local deadline
    deadline=$(( $(date +%s) + 10 ))
    while (( $(date +%s) < deadline )); do
        if docker exec "${WORKLOAD_NODE}" \
                ip netns exec "${BACKEND_NETNS}" \
                ss -ltn "sport = :${BACKEND_PORT}" 2>/dev/null \
                | grep -q "LISTEN"; then
            log "pf-backend listening on ${BACKEND_IP}:${BACKEND_PORT}"
            return 0
        fi
        sleep 1
    done
    log "pf-backend did not bind on :${BACKEND_PORT} within 10s"
    return 1
}

# Add the static route on `upstream` so client-1's traffic toward the
# VIP is routed to the master chassis's underlay IP. Idempotent via
# `ip route replace`.
ensure_upstream_route() {
    log "adding ${VIP}/32 via ${MASTER_UNDERLAY} on ${UPSTREAM}"
    docker exec "${UPSTREAM}" \
        ip route replace "${VIP}/32" via "${MASTER_UNDERLAY}"
}

# Add the scope-link /32 in the master's default netns so the leaked
# packet (vrf-provider → veth-default → default netns) reaches br-ex,
# where OVS NORMAL hands it to OVN's pipeline. We mirror the shape of
# the agent-managed per-FIP routes (`<FIP>/32 dev br-ex scope link`)
# so a `collect-artifacts.sh` dump of `ip route show table all` looks
# like an extra FIP row rather than something exotic.
ensure_master_scope_route() {
    log "adding ${VIP}/32 dev br-ex scope link on ${MASTER}"
    docker exec "${MASTER_NODE}" \
        ip route replace "${VIP}/32" dev br-ex scope link
}

# Seed the OVN Load_Balancer for VIP:VIP_PORT → BACKEND_IP:BACKEND_PORT
# and attach it to the logical router. Idempotent: `lb-add` with
# `--may-exist` updates the existing VIPs map in-place, and
# `lr-lb-add --may-exist` is a no-op if the LR already references this
# LB.
ensure_lb() {
    log "ensuring Load_Balancer ${LB_NAME} ${VIP}:${VIP_PORT} → ${BACKEND_IP}:${BACKEND_PORT}/${LB_PROTO}"
    nbctl --may-exist lb-add "${LB_NAME}" \
        "${VIP}:${VIP_PORT}" "${BACKEND_IP}:${BACKEND_PORT}" "${LB_PROTO}"
    log "attaching ${LB_NAME} to ${LR_NAME}"
    nbctl --may-exist lr-lb-add "${LR_NAME}" "${LB_NAME}"
}

# Poll the data path from client-1 until curl succeeds against the VIP
# or RECONCILE_TIMEOUT expires. OVN's northd has to translate the new
# Load_Balancer row into SB flows on the master chassis, and the
# kernel route + ARP-via-OVN cycle takes another round trip — the
# whole thing usually lands within a couple of seconds but we give
# the same generous reconcile budget as the hairpin scenario so a
# slow CI runner does not flake.
wait_for_reachability() {
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    log "waiting up to ${RECONCILE_TIMEOUT}s for ${CLIENT} → ${VIP}:${VIP_PORT} to come up"
    while (( $(date +%s) < deadline )); do
        if docker exec "${CLIENT}" \
                curl --silent --show-error \
                    --max-time "${CURL_TIMEOUT}" \
                    --output /dev/null \
                    "http://${VIP}:${VIP_PORT}/" 2>/dev/null; then
            log "VIP reachable from ${CLIENT}"
            return 0
        fi
        sleep 2
    done
    log "VIP unreachable from ${CLIENT} within ${RECONCILE_TIMEOUT}s"
    return 1
}

# Final assertion — the actual property under test. The backend log
# format is "peer=<host>:<port> method=... path=... host=...". A
# successful run records the client's underlay IP as <host>; any
# stealth SNAT on the way in (LR internal address, gateway chassis
# address, …) would show up here and fail the grep.
assert_source_ip_preserved() {
    local client_ip="$1"
    log "asserting backend log records peer=${client_ip}:* (no SNAT on ingress)"
    local log_content
    log_content="$(docker exec "${WORKLOAD_NODE}" cat "${LOG_FILE}")"
    if [ -z "${log_content}" ]; then
        log "ERROR: ${LOG_FILE} is empty — backend never accepted a connection"
        return 1
    fi
    log "backend log contents:"
    printf '%s\n' "${log_content}" | sed 's/^/    /' >&2
    # Anchor on the colon so a stray IP that happens to share a prefix
    # (e.g. 10.0.0.20 matching a literal 10.0.0.2) does not pass.
    if ! printf '%s\n' "${log_content}" | grep -q "peer=${client_ip}:"; then
        log "ERROR: no log line records peer=${client_ip}:* — source IP was rewritten"
        return 1
    fi
}

# Best-effort artifact handoff. Writes the backend log into
# ARTIFACTS_DIR (created if missing) so the CI workflow's
# upload-artifact step picks it up alongside the
# `collect-artifacts.sh` bundle. Called unconditionally from the
# EXIT trap — the issue asks for "the backend source-IP log [...] on
# failure", and copying it on the green path too is harmless (the
# bundle is only uploaded when the step fails anyway).
copy_backend_log_artifact() {
    if [ -z "${ARTIFACTS_DIR}" ]; then
        return 0
    fi
    if ! docker exec "${WORKLOAD_NODE}" test -f "${LOG_FILE}" 2>/dev/null; then
        return 0
    fi
    mkdir -p "${ARTIFACTS_DIR}"
    if docker exec "${WORKLOAD_NODE}" cat "${LOG_FILE}" \
            >"${ARTIFACTS_DIR}/pf-backend.log" 2>/dev/null; then
        log "wrote ${ARTIFACTS_DIR}/pf-backend.log"
    fi
}

# Tear down everything the scenario added. Best-effort and idempotent:
# a teardown failure must not mask the scenario's own pass/fail
# signal, so every step is independently allowed to fail.
teardown() {
    log "teardown: copy log artifact, stop pf-backend, remove LB + routes"
    copy_backend_log_artifact || true
    docker exec "${WORKLOAD_NODE}" pkill -f /usr/local/bin/pf-backend 2>/dev/null || true
    nbctl --if-exists lr-lb-del "${LR_NAME}" "${LB_NAME}" || true
    nbctl --if-exists lb-del "${LB_NAME}" || true
    docker exec "${MASTER_NODE}" \
        ip route del "${VIP}/32" dev br-ex scope link 2>/dev/null || true
    docker exec "${UPSTREAM}" \
        ip route del "${VIP}/32" via "${MASTER_UNDERLAY}" 2>/dev/null || true
    docker exec "${WORKLOAD_NODE}" rm -f "${LOG_FILE}" 2>/dev/null || true
}

main() {
    sanity_gate

    local client_ip
    client_ip="$(detect_client_ip)"
    if [ -z "${client_ip}" ]; then
        log "ERROR: could not read client underlay IP from ${CLIENT}:${CLIENT_DEV}"
        return 1
    fi
    log "client underlay IP = ${client_ip}"

    trap teardown EXIT
    start_backend
    ensure_upstream_route
    ensure_master_scope_route
    ensure_lb

    if ! wait_for_reachability; then
        return 1
    fi

    assert_source_ip_preserved "${client_ip}"
}

main "$@"
