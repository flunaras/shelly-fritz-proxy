#!/bin/sh
# postremove - reload systemd after the unit file is gone. On purge
# (Debian) or full erase (RPM), preserve user-edited /etc/.../*.conf so
# reinstalling the package brings the previous configuration back.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi
