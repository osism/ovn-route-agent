#!/usr/bin/env bash
# Entrypoint for the containerlab gateway-node image.
#
# Starts the supporting daemons in a fixed order — OVS local DB, OVS
# vswitchd (userspace datapath), ovn-controller, FRR — then execs the
# ovn-network-agent in the foreground so the container's lifecycle is
# tied to the agent process.

set -euo pipefail

log() { printf '[gwnode] %s\n' "$*" >&2; }

# Use the chassis name the OVN central bootstrap script seeds. Falls back
# to the container hostname so the image still works when launched outside
# the canned topology.
CHASSIS_NAME="${CHASSIS_NAME:-$(hostname -s)}"
OVN_SB_REMOTE="${OVN_SB_REMOTE:-tcp:central:6642}"
ENCAP_IP="${ENCAP_IP:-127.0.0.1}"
BRIDGE_DEV="${BRIDGE_DEV:-br-ex}"
BRIDGE_MAPPING="${BRIDGE_MAPPING:-physnet1:${BRIDGE_DEV}}"

start_ovs() {
    log "starting Open vSwitch (userspace datapath)"
    mkdir -p /var/run/openvswitch /var/log/openvswitch /etc/openvswitch
    # ovs-ctl honours the existing conf.db when present and creates a new
    # one otherwise, which keeps re-runs idempotent.
    /usr/share/openvswitch/scripts/ovs-ctl --system-id="${CHASSIS_NAME}" start
    # Wait for ovs-vsctl to respond.
    for _ in $(seq 1 30); do
        if ovs-vsctl show >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "ovs-vsctl did not respond after start" >&2
    exit 1
}

configure_ovs() {
    log "configuring Open_vSwitch external_ids for ovn-controller"
    # ovn-bridge-datapath-type=netdev hints to ovn-controller to create
    # br-int with the userspace datapath, so the lab does not depend on
    # the openvswitch kernel module being loaded on the host.
    ovs-vsctl set Open_vSwitch . \
        external_ids:ovn-remote="${OVN_SB_REMOTE}" \
        external_ids:ovn-encap-type=geneve \
        external_ids:ovn-encap-ip="${ENCAP_IP}" \
        external_ids:system-id="${CHASSIS_NAME}" \
        external_ids:hostname="${CHASSIS_NAME}" \
        external_ids:ovn-bridge-mappings="${BRIDGE_MAPPING}" \
        external_ids:ovn-bridge-datapath-type=netdev

    log "ensuring ${BRIDGE_DEV} exists (datapath_type=netdev)"
    ovs-vsctl --may-exist add-br "${BRIDGE_DEV}" \
        -- set bridge "${BRIDGE_DEV}" datapath_type=netdev
    ip link set "${BRIDGE_DEV}" up
}

start_ovn_controller() {
    log "starting ovn-controller"
    /usr/share/ovn/scripts/ovn-ctl start_controller
    for _ in $(seq 1 30); do
        if ovs-vsctl br-exists br-int 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    echo "ovn-controller did not create br-int" >&2
    exit 1
}

start_frr() {
    log "starting FRR"
    # /usr/lib/frr/frrinit.sh is the canonical service entrypoint shipped
    # by the FRR Debian package; it launches the daemons listed in
    # /etc/frr/daemons.
    /usr/lib/frr/frrinit.sh start
    for _ in $(seq 1 30); do
        if vtysh -c 'show version' >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "FRR did not become ready" >&2
    exit 1
}

main() {
    start_ovs
    configure_ovs
    start_ovn_controller
    start_frr

    log "exec ovn-network-agent"
    exec /usr/local/bin/ovn-network-agent \
        --config /etc/ovn-network-agent/config.yaml \
        "$@"
}

main "$@"
