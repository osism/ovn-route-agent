#!/bin/sh
set -e

if [ "$1" = "remove" ]; then
    systemctl stop ovn-network-agent 2>/dev/null || true
    systemctl disable ovn-network-agent 2>/dev/null || true
fi
