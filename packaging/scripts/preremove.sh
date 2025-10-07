#!/usr/bin/env sh
set -e

if command -v systemctl >/dev/null 2>&1; then
  systemctl disable --now conservationd.service || true
fi
