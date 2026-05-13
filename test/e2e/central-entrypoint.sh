#!/usr/bin/env bash
# Entrypoint for the containerlab OVN central image.
#
# Starts the OVN ovsdb-server (NB+SB) and ovn-northd, then binds the NB
# and SB DBs to TCP listeners reachable from the gateway-node containers
# over the management network.

set -euo pipefail

log() { printf '[central] %s\n' "$*" >&2; }

NB_LISTEN="${NB_LISTEN:-ptcp:6641:0.0.0.0}"
SB_LISTEN="${SB_LISTEN:-ptcp:6642:0.0.0.0}"

mkdir -p /var/run/ovn /var/log/ovn /etc/ovn

log "starting ovn-northd and the NB/SB ovsdb-servers"
/usr/share/ovn/scripts/ovn-ctl start_northd

log "binding NB to ${NB_LISTEN} and SB to ${SB_LISTEN}"
ovn-nbctl set-connection "${NB_LISTEN}"
ovn-sbctl set-connection "${SB_LISTEN}"

log "ready — NB on tcp/6641, SB on tcp/6642"

# Hand control to a wait that exits when ovsdb-server or ovn-northd dies.
# pidwait would be cleaner but is not packaged on ubuntu:24.04; tail on the
# log files keeps the container alive while still surfacing errors.
exec tail -F /var/log/ovn/ovn-northd.log \
              /var/log/ovn/ovsdb-server-nb.log \
              /var/log/ovn/ovsdb-server-sb.log
