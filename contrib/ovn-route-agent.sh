#!/usr/bin/env bash
set -euo pipefail

# ============================================================================
# ovn-route-agent.sh — Floating IP /32 route sync based on OVN Gateway Chassis
#
# DEPRECATED: This shell script is the original prototype that served as the
# starting point for the ovn-route-agent Go daemon. It is kept here for
# reference only. Use the Go binary for production deployments.
# See: https://github.com/osism/ovn-route-agent
#
# What it does:
#   1. Determines whether this node is the active OVN gateway chassis by
#      querying the OVN Southbound DB (chassisredirect Port_Binding).
#   2. Collects Floating IPs (dnat_and_snat) and SNAT IPs from the OVN
#      Northbound DB (or via the openstack CLI as a fallback).
#   3. On the active gateway: ensures /32 kernel routes on the provider
#      bridge (br-ex) and FRR static routes in the VRF for BGP announcement.
#      On standby nodes: removes all managed routes.
#
# Requirements:
#   - Docker (for ovn-sbctl / ovn-nbctl via the OVN controller container)
#   - FRR with vtysh and a pre-configured VRF + BGP setup
#   - Root privileges (ip route, vtysh)
#
# Usage:
#   Run via cron or systemd timer on each gateway node, e.g. every 30s:
#     */1 * * * * /usr/local/bin/ovn-route-agent.sh
#     (cron minimum is 1 min; use a systemd timer for 30s intervals)
# ============================================================================

HOSTNAME=$(hostname)
LOG_PREFIX="ovn-route-agent"
OVN_CONTAINER="ovn_controller"  # Adjust if different

# Floating IP range — adjust to your setup (example uses RFC 5737 TEST-NET-1)
FIP_NETWORK="192.0.2"
FIP_POOL_START=10
FIP_POOL_END=20

# VRF veth next-hop
VETH_NEXTHOP="169.254.0.1"

log() { echo "$(date -Iseconds) ${LOG_PREFIX}: $*"; }

# ============================================================================
# 1. Determine active gateway chassis
# ============================================================================
ACTIVE_CHASSIS=$(docker exec ${OVN_CONTAINER} ovs-vsctl get open . external-ids:system-id 2>/dev/null | tr -d '"')
CR_LRP_CHASSIS=$(docker exec ${OVN_CONTAINER} ovn-sbctl --no-leader-only find Port_Binding type=chassisredirect 2>/dev/null \
    | grep -oP 'chassis.*\[\K[^\]]+' | head -1)

if [[ -z "$CR_LRP_CHASSIS" ]]; then
    log "WARNING: Could not determine active gateway chassis. Skipping."
    exit 0
fi

# Resolve chassis name
ACTIVE_HOST=$(docker exec ${OVN_CONTAINER} ovn-sbctl --no-leader-only get Chassis "${CR_LRP_CHASSIS}" hostname 2>/dev/null | tr -d '"')

if [[ -z "$ACTIVE_HOST" ]]; then
    log "WARNING: Could not resolve chassis hostname. Skipping."
    exit 0
fi

IS_ACTIVE=false
if [[ "$ACTIVE_HOST" == "$HOSTNAME" ]]; then
    IS_ACTIVE=true
fi

log "Active gateway: ${ACTIVE_HOST} | This node: ${HOSTNAME} | Active here: ${IS_ACTIVE}"

# ============================================================================
# 2. Get list of assigned Floating IPs from Neutron
# ============================================================================
# Reads currently assigned FIPs via openstack CLI.
# Falls back to scanning the FIP range if openstack CLI is not available.

get_assigned_fips() {
    # Try openstack CLI first
    if command -v openstack &>/dev/null; then
        openstack floating ip list --status ACTIVE -f value -c "Floating IP Address" 2>/dev/null
        return
    fi

    # Fallback: check OVN NB for NAT entries
    if docker exec ${OVN_CONTAINER} ovn-nbctl --no-leader-only lb-list &>/dev/null; then
        docker exec ${OVN_CONTAINER} ovn-nbctl --no-leader-only find NAT type=dnat_and_snat 2>/dev/null \
            | grep -oP 'external_ip\s*:\s*"\K[^"]+' \
            | grep "^${FIP_NETWORK}\."
        return
    fi

    log "WARNING: Cannot determine assigned FIPs. No openstack CLI or OVN NB access."
}

ASSIGNED_FIPS=$(get_assigned_fips)

if [[ -z "$ASSIGNED_FIPS" ]]; then
    log "No assigned Floating IPs found."
fi

# ============================================================================
# 3. Sync routes
# ============================================================================
add_fip_route() {
    local fip="$1"

    # Kernel route to br-ex
    if ! ip route show "${fip}/32" dev br-ex &>/dev/null | grep -q "${fip}"; then
        ip route add "${fip}/32" dev br-ex 2>/dev/null || true
        log "  ADD kernel: ${fip}/32 dev br-ex"
    fi

    # FRR static route for BGP announcement
    if ! vtysh -c "show ip route vrf vrf-provider ${fip}/32" 2>/dev/null | grep -q "static"; then
        vtysh \
            -c "conf t" \
            -c "vrf vrf-provider" \
            -c "ip route ${fip}/32 ${VETH_NEXTHOP}" \
            -c "exit-vrf" \
            -c "end" 2>/dev/null
        log "  ADD FRR:    ${fip}/32 via ${VETH_NEXTHOP} vrf vrf-provider"
    fi
}

del_fip_route() {
    local fip="$1"

    # Kernel route
    if ip route show "${fip}/32" dev br-ex 2>/dev/null | grep -q "${fip}"; then
        ip route del "${fip}/32" dev br-ex 2>/dev/null || true
        log "  DEL kernel: ${fip}/32 dev br-ex"
    fi

    # FRR static route
    if vtysh -c "show ip route vrf vrf-provider ${fip}/32" 2>/dev/null | grep -q "static"; then
        vtysh \
            -c "conf t" \
            -c "vrf vrf-provider" \
            -c "no ip route ${fip}/32 ${VETH_NEXTHOP}" \
            -c "exit-vrf" \
            -c "end" 2>/dev/null
        log "  DEL FRR:    ${fip}/32 vrf vrf-provider"
    fi
}

# Also include the router gateway IP (SNAT source)
SNAT_IPS=$(docker exec ${OVN_CONTAINER} ovn-nbctl --no-leader-only find NAT type=snat 2>/dev/null \
    | grep -oP 'external_ip\s*:\s*"\K[^"]+' \
    | grep "^${FIP_NETWORK}\." || true)

ALL_FIPS=$(echo -e "${ASSIGNED_FIPS}\n${SNAT_IPS}" | sort -u | grep -v '^$' || true)

if [[ "$IS_ACTIVE" == "true" ]]; then
    log "Ensuring routes for active FIPs..."
    while IFS= read -r fip; do
        [[ -z "$fip" ]] && continue
        add_fip_route "$fip"
    done <<< "$ALL_FIPS"
else
    log "Not active gateway — removing all FIP routes..."
    # Remove any existing FIP routes in our range
    for ip_route in $(ip route show dev br-ex 2>/dev/null | grep -oP "${FIP_NETWORK}\.\d+"); do
        del_fip_route "$ip_route"
    done
fi

log "Sync complete."
