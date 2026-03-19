#!/bin/sh
set -e

if [ "$1" = "remove" ]; then
    systemctl stop ovn-route-agent 2>/dev/null || true
    systemctl disable ovn-route-agent 2>/dev/null || true
fi
