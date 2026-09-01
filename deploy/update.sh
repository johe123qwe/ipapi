#!/usr/bin/env bash
# Re-downloads the source databases, rebuilds the MMDB files and signals the
# running API to pick them up.
#
# The reload is a SIGHUP, not a restart: the server opens the new files and
# swaps them in atomically, so no request is dropped and no privileges beyond
# the service's own user are needed.
set -euo pipefail

cd /opt/ipapi
./scripts/fetch.sh
./bin/ipapi-build -src data/src -out data/mmdb

pid="$(systemctl show -p MainPID --value ipapi.service 2>/dev/null || true)"
if [ -z "$pid" ] || [ "$pid" = "0" ]; then
  pid="$(pgrep -u "$(id -un)" -f '/opt/ipapi/bin/ipapi-server' | head -1 || true)"
fi

if [ -n "$pid" ]; then
  kill -HUP "$pid"
  echo "update complete, reloaded pid $pid"
else
  echo "update complete, but the API is not running" >&2
fi
