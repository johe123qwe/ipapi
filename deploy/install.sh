#!/usr/bin/env bash
# Installs the API to /opt/ipapi and starts it. Run as root from inside the
# extracted release directory:
#
#   sudo ./deploy/install.sh
set -euo pipefail

PREFIX=/opt/ipapi
USER_NAME=ipapi
# Override when 8080 is taken, e.g. PORT=8090 ./deploy/install.sh
PORT="${PORT:-8080}"
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
for cmd in curl unzip gzip systemctl ss; do
  command -v "$cmd" >/dev/null || { echo "missing dependency: $cmd" >&2; exit 1; }
done

echo ">> creating user $USER_NAME"
id -u "$USER_NAME" >/dev/null 2>&1 || \
  useradd --system --home-dir "$PREFIX" --shell /usr/sbin/nologin "$USER_NAME"

echo ">> installing to $PREFIX"
mkdir -p "$PREFIX"/{bin,scripts,deploy,data/src,data/mmdb,data/cache}
install -m 0755 "$SRC_DIR"/bin/ipapi-server  "$PREFIX"/bin/
install -m 0755 "$SRC_DIR"/bin/ipapi-build   "$PREFIX"/bin/
install -m 0755 "$SRC_DIR"/scripts/fetch.sh  "$PREFIX"/scripts/
install -m 0755 "$SRC_DIR"/deploy/update.sh  "$PREFIX"/deploy/
chown -R "$USER_NAME:$USER_NAME" "$PREFIX"

echo ">> building databases (first run downloads ~40MB)"
if [ ! -f "$PREFIX/data/mmdb/geo.mmdb" ]; then
  sudo -u "$USER_NAME" bash -c "cd $PREFIX && ./scripts/fetch.sh && ./bin/ipapi-build -src data/src -out data/mmdb"
else
  echo "   databases already present, skipping"
fi

# Stop any previous instance first so it does not look like a port conflict.
systemctl stop ipapi.service 2>/dev/null || true

echo ">> checking that port $PORT is free"
if ss -ltn "sport = :$PORT" 2>/dev/null | grep -q ":$PORT"; then
  echo
  echo "ERROR: something is already listening on 127.0.0.1:$PORT:" >&2
  ss -ltnp "sport = :$PORT" >&2
  echo >&2
  echo "Re-run with a free port, for example:" >&2
  echo "  sudo PORT=8090 $0" >&2
  echo "and point deploy/nginx.conf at that port." >&2
  exit 1
fi

echo ">> installing systemd units"
install -m 0644 "$SRC_DIR"/deploy/ipapi.service        /etc/systemd/system/
sed -i "s|-addr 127.0.0.1:8080|-addr 127.0.0.1:$PORT|" /etc/systemd/system/ipapi.service
install -m 0644 "$SRC_DIR"/deploy/ipapi-update.service /etc/systemd/system/
install -m 0644 "$SRC_DIR"/deploy/ipapi-update.timer   /etc/systemd/system/

systemctl daemon-reload
systemctl enable --now ipapi.service
systemctl enable --now ipapi-update.timer

echo ">> health check"
ok=0
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" 2>/dev/null; then ok=1; echo; break; fi
  sleep 1
done
if [ "$ok" -ne 1 ]; then
  echo "ERROR: the service did not become healthy within 30s" >&2
  systemctl status ipapi.service --no-pager -l >&2 || true
  journalctl -u ipapi.service -n 40 --no-pager >&2 || true
  exit 1
fi
echo
echo "API listening on 127.0.0.1:$PORT"
echo "installed. useful commands:"
echo "  systemctl status ipapi          # 状态"
echo "  journalctl -u ipapi -f          # 实时日志"
echo "  systemctl list-timers ipapi-*   # 下次数据更新时间"
echo "  systemctl start ipapi-update    # 立即更新数据（热重载，不中断）"
