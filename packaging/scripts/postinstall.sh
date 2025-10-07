#!/usr/bin/env sh
set -e

# Create group for socket access if missing
if ! getent group conservationd >/dev/null 2>&1; then
  if command -v groupadd >/dev/null 2>&1; then
    groupadd --system conservationd || true
  fi
fi

# Reload and enable service when systemd is available
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload || true
  systemctl enable --now conservationd.service || true
fi
