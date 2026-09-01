#!/usr/bin/env bash
# Installs the API to /opt/ipapi and starts it. Run as root from inside the
# extracted release directory:
#
#   sudo ./deploy/install.sh
set -euo pipefail

PREFIX=/opt/ipapi
USER_NAME=ipapi
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
for cmd in curl unzip gzip systemctl; do
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

echo ">> installing systemd units"
install -m 0644 "$SRC_DIR"/deploy/ipapi.service        /etc/systemd/system/
install -m 0644 "$SRC_DIR"/deploy/ipapi-update.service /etc/systemd/system/
install -m 0644 "$SRC_DIR"/deploy/ipapi-update.timer   /etc/systemd/system/

systemctl daemon-reload
systemctl enable --now ipapi.service
systemctl enable --now ipapi-update.timer

sleep 2
echo ">> health check"
curl -fsS http://127.0.0.1:8080/healthz && echo
echo
echo "installed. useful commands:"
echo "  systemctl status ipapi          # 状态"
echo "  journalctl -u ipapi -f          # 实时日志"
echo "  systemctl list-timers ipapi-*   # 下次数据更新时间"
echo "  systemctl start ipapi-update    # 立即更新数据（热重载，不中断）"
