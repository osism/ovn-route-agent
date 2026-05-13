#!/usr/bin/env bash
# Bootstrap the OVN northbound DB with a canned topology after
# `containerlab deploy`. Idempotent — re-running is a no-op.
#
# Topology written here:
#   * Logical switch  ls0   (192.168.10.0/24)
#   * Logical router  lr0
#     - lr0-ls0      (192.168.10.1/24)  attaches lr0 to ls0
#     - lr0-public   (192.0.2.1/24)     external/provider port
#   * Gateway_Chassis bindings on lr0-public:
#         gateway-1 priority 30  (active)
#         gateway-2 priority 20
#         gateway-3 priority 10
#   * Two dnat_and_snat NATs (FIPs):
#         192.0.2.10 → 192.168.10.10
#         192.0.2.11 → 192.168.10.11
#
# Usage:
#   ./test/e2e/bootstrap.sh                  # talks to ovn-nbctl in the central container
#   OVN_CENTRAL=other-name ./bootstrap.sh    # override the container name
#   OVN_NBCTL="ovn-nbctl --db=tcp:..." ./bootstrap.sh   # talk directly to NB

set -euo pipefail

OVN_CENTRAL="${OVN_CENTRAL:-clab-ovn-e2e-central}"
OVN_NBCTL="${OVN_NBCTL:-docker exec ${OVN_CENTRAL} ovn-nbctl}"

LS_NAME="${LS_NAME:-ls0}"
LR_NAME="${LR_NAME:-lr0}"
LR_TENANT_PORT="${LR_TENANT_PORT:-${LR_NAME}-ls0}"
LR_PUBLIC_PORT="${LR_PUBLIC_PORT:-${LR_NAME}-public}"
LR_TENANT_MAC="${LR_TENANT_MAC:-02:00:00:00:01:01}"
LR_PUBLIC_MAC="${LR_PUBLIC_MAC:-02:00:00:00:02:01}"
LR_TENANT_CIDR="${LR_TENANT_CIDR:-192.168.10.1/24}"
LR_PUBLIC_CIDR="${LR_PUBLIC_CIDR:-192.0.2.1/24}"

GATEWAYS=(
    "gateway-1 30"
    "gateway-2 20"
    "gateway-3 10"
)

FIPS=(
    "192.0.2.10 192.168.10.10"
    "192.0.2.11 192.168.10.11"
)

log() { printf '[bootstrap] %s\n' "$*" >&2; }

# Run an ovn-nbctl command via the configured transport.
nbctl() {
    # shellcheck disable=SC2086  # we want word-splitting on OVN_NBCTL
    ${OVN_NBCTL} "$@"
}

ensure_switch() {
    log "ensuring logical switch ${LS_NAME}"
    nbctl ls-add "${LS_NAME}" -- --may-exist ls-add "${LS_NAME}"
}

ensure_router() {
    log "ensuring logical router ${LR_NAME}"
    nbctl --may-exist lr-add "${LR_NAME}"
}

ensure_router_ports() {
    log "ensuring ${LR_TENANT_PORT} on ${LR_NAME} (${LR_TENANT_CIDR})"
    nbctl --may-exist lrp-add "${LR_NAME}" "${LR_TENANT_PORT}" \
        "${LR_TENANT_MAC}" "${LR_TENANT_CIDR}"

    log "ensuring ${LS_NAME} ↔ ${LR_NAME} switch port"
    nbctl --may-exist lsp-add "${LS_NAME}" "${LS_NAME}-${LR_NAME}"
    nbctl lsp-set-type        "${LS_NAME}-${LR_NAME}" router
    nbctl lsp-set-addresses   "${LS_NAME}-${LR_NAME}" router
    nbctl lsp-set-options     "${LS_NAME}-${LR_NAME}" \
        router-port="${LR_TENANT_PORT}"

    log "ensuring ${LR_PUBLIC_PORT} on ${LR_NAME} (${LR_PUBLIC_CIDR})"
    nbctl --may-exist lrp-add "${LR_NAME}" "${LR_PUBLIC_PORT}" \
        "${LR_PUBLIC_MAC}" "${LR_PUBLIC_CIDR}"
}

ensure_gateway_chassis() {
    for entry in "${GATEWAYS[@]}"; do
        local chassis priority
        read -r chassis priority <<<"${entry}"
        log "binding ${LR_PUBLIC_PORT} to ${chassis} priority ${priority}"
        # lrp-set-gateway-chassis replaces the prior priority for the same
        # (port, chassis) tuple, which keeps the operation idempotent.
        nbctl lrp-set-gateway-chassis "${LR_PUBLIC_PORT}" \
            "${chassis}" "${priority}"
    done
}

ensure_fips() {
    for entry in "${FIPS[@]}"; do
        local external internal
        read -r external internal <<<"${entry}"
        log "ensuring FIP ${external} → ${internal}"
        nbctl --may-exist lr-nat-add "${LR_NAME}" \
            dnat_and_snat "${external}" "${internal}"
    done
}

main() {
    log "OVN_NBCTL = ${OVN_NBCTL}"
    ensure_switch
    ensure_router
    ensure_router_ports
    ensure_gateway_chassis
    ensure_fips
    log "bootstrap complete"
}

main "$@"
