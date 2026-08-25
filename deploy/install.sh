#!/usr/bin/env bash
# Install xx-drive on a Debian/Ubuntu server as a systemd service.
# Run as root:  ./install.sh [base-url]
set -euo pipefail

BASE_URL="${1:-}"
if [[ -z "$BASE_URL" ]]; then
  echo "usage: $0 https://drive.example.com"
  exit 1
fi

# Build (requires Go >= 1.22) or use a prebuilt binary placed next to this script.
cd "$(dirname "$0")/.."
if command -v go >/dev/null 2>&1; then
  echo "==> building binaries"
  CGO_ENABLED=0 go build -o /tmp/xxdrive-server ./cmd/xxdrive-server
  CGO_ENABLED=0 go build -o /tmp/xxdrive-cli ./cmd/xxdrive-cli
else
  echo "!! go not found — expecting prebuilt ./xxdrive-server in repo root"
  [[ -f xxdrive-server ]] || { echo "no binary and no toolchain; abort"; exit 1; }
  cp xxdrive-server /tmp/xxdrive-server
fi

echo "==> creating service user"
useradd --system --home-dir /var/lib/xxdrive --shell /usr/sbin/nologin xxdrive 2>/dev/null || true
mkdir -p /var/lib/xxdrive
chown xxdrive:xxdrive /var/lib/xxdrive

echo "==> installing binaries + unit"
install -m 0755 /tmp/xxdrive-server /usr/local/bin/xxdrive-server
[[ -f /tmp/xxdrive-cli ]] && install -m 0755 /tmp/xxdrive-cli /usr/local/bin/xxdrive-cli
sed "s|https://CHANGE-ME.example.com|${BASE_URL}|g" deploy/xxdrive.service > /etc/systemd/system/xxdrive.service

echo "==> enabling service"
systemctl daemon-reload
systemctl enable --now xxdrive
sleep 1
systemctl --no-pager status xxdrive | head -12

cat <<'NOTE'

=== Installation complete ===
The first start generated an admin password — read it once with:

    journalctl -u xxdrive | grep "created admin user" -A4

Then:
  * Put a TLS reverse proxy (Caddy/nginx) in front of port 8080, or edit
    /etc/systemd/system/xxdrive.service to add -tls-cert/-tls-key.
  * Log into the web UI at your base URL and change the admin password.
  * Linux clients:  xxdrive-cli login <url> <user>   then  xxdrive-cli sync ~/Drive /<user-dir>
NOTE
