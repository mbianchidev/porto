set -eu
export DEBIAN_FRONTEND=noninteractive
if ! command -v dbus-daemon >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends dbus
fi
systemctl daemon-reload
systemctl reset-failed dbus.service systemd-logind.service
systemctl start dbus.service
systemctl is-active --quiet dbus.service
if ! systemctl start systemd-logind.service; then
  systemctl status --no-pager systemd-logind.service >&2
  journalctl --no-pager -u systemd-logind.service -n 40 >&2
  exit 1
fi
systemctl is-active --quiet systemd-logind.service
