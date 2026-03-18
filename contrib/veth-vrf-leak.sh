#!/usr/bin/env bash
#
# veth-vrf-leak.sh - VRF Route Leaking via veth Pair
#
# This script sets up a veth pair to enable selective route leaking
# between the default VRF and the "vrf-provider" VRF. This allows
# specific networks (listed in a configuration file) to communicate
# across VRF boundaries.
#
# How it works:
#
#   1. A veth pair (veth-default / veth-provider) is created.
#      veth-default remains in the default VRF, veth-provider is
#      placed into vrf-provider.
#
#   2. Both ends are assigned link-local addresses (169.254.0.0/30).
#
#   3. Static ARP entries are configured to ensure ARP resolution
#      works reliably across the VRF boundary.
#
#   4. Routing entries:
#      - Table 200 (default VRF): default via 169.254.0.2
#        -> forwards traffic through the veth pair into vrf-provider
#      - vrf-provider: routes for the configured networks via 169.254.0.1
#        -> forwards return traffic back through the veth pair into the default VRF
#
#   5. Policy rules: traffic originating from the configured networks
#      is redirected into table 200 via "ip rule".
#
# Traffic flow (example: network 198.51.100.0/24 from networks.txt):
#
#   ┌────────────────────────────────────────────────────────────────────┐
#   │                        Linux Network Stack                         │
#   │                                                                    │
#   │  Default VRF                              vrf-provider             │
#   │  ───────────                              ────────────             │
#   │                                                                    │
#   │  ┌───────────────┐     veth pair      ┌───────────────┐            │
#   │  │ veth-default  │◄──────────────────►│ veth-provider │            │
#   │  │ 169.254.0.1/30│                    │ 169.254.0.2/30│            │
#   │  └───────────────┘                    └───────────────┘            │
#   │                                                                    │
#   │  Policy Rule:                         Route:                       │
#   │   from 198.51.100.0/24                    198.51.100.0/24          │
#   │     -> lookup table 200                 via 169.254.0.1            │
#   │                                         (-> default VRF)           │
#   │  Table 200:                                                        │
#   │   default via 169.254.0.2                                          │
#   │     (-> vrf-provider)                                              │
#   │                                                                    │
#   │  ┌─────────────┐                       ┌────────────┐              │
#   │  │   br-ex     │                       │  Provider  │              │
#   │  │  (Bridge)   │                       │  Routing   │              │
#   │  └─────────────┘                       └────────────┘              │
#   └────────────────────────────────────────────────────────────────────┘
#
#   Forward path (-> provider):
#     Packet from 198.51.100.0/24
#       -> policy rule matches -> table 200
#       -> default via 169.254.0.2 (veth-default -> veth-provider)
#       -> packet is now in vrf-provider -> provider routing takes over
#
#   Return path (<- provider):
#     Reply packet destined for 198.51.100.0/24
#       -> route in vrf-provider: 198.51.100.0/24 via 169.254.0.1
#       -> veth-provider -> veth-default
#       -> packet is back in default VRF -> normal delivery
#
# Usage:
#   ./veth-vrf-leak.sh up   [networks-file]   # set up
#   ./veth-vrf-leak.sh down [networks-file]   # tear down
#
# The networks file contains one CIDR network per line (blank lines and
# lines starting with # are ignored).
# Default: /etc/ovn-route-agent/networks.txt
#
set -euo pipefail

ACTION="${1:-up}"
NETWORKS_FILE="${2:-/etc/ovn-route-agent/networks.txt}"

VETH_DEFAULT_IP="${VETH_DEFAULT_IP:-169.254.0.1}"
VETH_PROVIDER_IP="${VETH_PROVIDER_IP:-169.254.0.2}"

if [[ ! -f "$NETWORKS_FILE" ]]; then
  echo "ERROR: Networks file not found: $NETWORKS_FILE"
  exit 1
fi

mapfile -t NETWORKS < <(grep -v '^\s*$' "$NETWORKS_FILE" | grep -v '^\s*#')

if [[ ${#NETWORKS[@]} -eq 0 ]]; then
  echo "ERROR: No networks found in $NETWORKS_FILE"
  exit 1
fi

case "$ACTION" in
  up)
    echo "INFO: [1/5] Ensuring br-ex is up..."
    ip link set br-ex up

    echo "INFO: [2/5] Creating veth pair..."
    ip link add veth-default type veth peer name veth-provider
    ip link set veth-provider master vrf-provider
    ip addr add "${VETH_DEFAULT_IP}/30" dev veth-default
    ip addr add "${VETH_PROVIDER_IP}/30" dev veth-provider
    ip link set veth-default up
    ip link set veth-provider up

    echo "INFO: [3/5] Adding static ARP entries for veth pair..."
    MAC_DEFAULT=$(cat /sys/class/net/veth-default/address)
    MAC_PROVIDER=$(cat /sys/class/net/veth-provider/address)
    echo "INFO:   veth-default MAC=${MAC_DEFAULT}, veth-provider MAC=${MAC_PROVIDER}"
    ip neigh replace "${VETH_PROVIDER_IP}" lladdr "${MAC_PROVIDER}" dev veth-default nud permanent
    ip neigh replace "${VETH_DEFAULT_IP}" lladdr "${MAC_DEFAULT}" dev veth-provider nud permanent

    echo "INFO: [4/5] Adding routes..."
    ip route add default via "${VETH_PROVIDER_IP}" table 200
    for NETWORK in "${NETWORKS[@]}"; do
      echo "INFO:   Adding route for ${NETWORK}"
      ip route add "${NETWORK}" via "${VETH_DEFAULT_IP}" vrf vrf-provider
    done

    echo "INFO: [5/5] Adding policy rules..."
    for NETWORK in "${NETWORKS[@]}"; do
      echo "INFO:   Adding policy rule for ${NETWORK}"
      ip rule add from "${NETWORK}" lookup 200 prio 2000
    done

    echo ""
    echo "Done. (${#NETWORKS[@]} networks configured)"
    ;;

  down)
    echo "INFO: Removing policy rules..."
    for NETWORK in "${NETWORKS[@]}"; do
      echo "INFO:   Removing policy rule for ${NETWORK}"
      ip rule del from "${NETWORK}" lookup 200 prio 2000 2>/dev/null || true
    done

    echo "INFO: Removing routes..."
    ip route del default via "${VETH_PROVIDER_IP}" table 200 2>/dev/null || true
    for NETWORK in "${NETWORKS[@]}"; do
      echo "INFO:   Removing route for ${NETWORK}"
      ip route del "${NETWORK}" via "${VETH_DEFAULT_IP}" vrf vrf-provider 2>/dev/null || true
    done

    echo "INFO: Removing veth pair..."
    ip link del veth-default 2>/dev/null || true

    echo "Done."
    ;;

  *)
    echo "Usage: $0 {up|down} [networks-file]"
    exit 1
    ;;
esac
