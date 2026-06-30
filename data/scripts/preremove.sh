#!/bin/sh
# preremove - stop the service before package files are removed.
set -e

if command -v systemctl >/dev/null 2>&1; then
    if systemctl is-active --quiet shelly-fritz-proxy; then
        systemctl stop shelly-fritz-proxy || true
    fi
    if systemctl is-enabled --quiet shelly-fritz-proxy 2>/dev/null; then
        systemctl disable shelly-fritz-proxy || true
    fi
fi
