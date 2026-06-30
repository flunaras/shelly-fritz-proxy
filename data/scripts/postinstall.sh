#!/bin/sh
# postinstall - reload systemd so it picks up the new unit. We do NOT
# enable or start the service automatically; the user must do that
# explicitly once they have edited the config file.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

cat <<EOF
shelly-fritz-proxy installed.

Next steps:
  1. Edit /etc/shelly-fritz-proxy/shelly-fritz-proxy.conf and set at
     minimum fritz-user, fritz-password and fritz-unit.
  2. systemctl enable --now shelly-fritz-proxy
  3. journalctl -u shelly-fritz-proxy -f

The default unit binds to port 80 with CAP_NET_BIND_SERVICE under a
dynamic user. To use a different port, override ExecStart with
'systemctl edit shelly-fritz-proxy'.
EOF
