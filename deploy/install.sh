#!/usr/bin/env bash
# Install xx-drive on a Debian/Ubuntu server as a systemd service.
#
# Run as root:
#   ./install.sh https://drive.example.com
#
# Optional environment (all default to a plain-HTTP loopback deploy):
#   XXD_TLS_CERT / XXD_TLS_KEY    paths to a cert/key pair — adds -tls-cert/-tls-key
#                                 so the server terminates TLS itself. If you front
#                                 the server with Caddy/nginx instead, leave these
#                                 unset and pass XXD_SECURE_COOKIES=1.
#   XXD_SECURE_COOKIES            "1" adds -secure-cookies (set this whenever TLS
#                                 terminates at a reverse proxy).
#   FABRIC_CLUSTER_KEYS_PATH      path to the estate cluster keyring JSON — adds
#                                 -keyring and enables fabric SSO.
#   XXD_ADMIN_PASSWORD            known password for the first-boot admin user.
#                                 Written root-only to /etc/default/xxdrive;
#                                 DELETE that entry after first login.
#   XXD_DATA_DIR                  server data directory (SQLite + files/trash/
#                                 versions/tmp). Defaults to the live box's ZFS
#                                 pool path /srv/deep/xx-drive so install never
#                                 creates a second data tree under /var/lib/xxdrive.
#                                 The value is templated into the unit's -data flag
#                                 and ReadWritePaths, so unit and data dir always agree.
#
# See deploy/Caddyfile.example / deploy/nginx.conf.example for TLS termination,
# and the drop-in example printed at the end of a successful install.
set -euo pipefail

BASE_URL="${1:-}"
if [[ -z "$BASE_URL" ]]; then
  echo "usage: $0 https://drive.example.com"
  exit 1
fi

# Server data directory. Defaults to the live box's ZFS pool path so this
# install never creates a second data tree. The value is templated into the
# unit's -data flag and ReadWritePaths below.
DATA_DIR="${XXD_DATA_DIR:-/srv/deep/xx-drive}"

# Build (requires Go >= 1.25; go.mod pins go 1.25.0) or use a prebuilt binary
# placed next to this script.
cd "$(dirname "$0")/.."
# Prefer the repo's bin/go wrapper (execs the pinned module-cache toolchain
# with a writable GOCACHE) when a system `go` isn't on PATH.
if [[ -x bin/go && -z "$(command -v go 2>/dev/null || true)" ]]; then
  PATH="$PWD/bin:$PATH"
fi
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
useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin xxdrive 2>/dev/null || true
mkdir -p "$DATA_DIR"
chown xxdrive:xxdrive "$DATA_DIR"

echo "==> installing binaries + unit"
install -m 0755 /tmp/xxdrive-server /usr/local/bin/xxdrive-server
[[ -f /tmp/xxdrive-cli ]] && install -m 0755 /tmp/xxdrive-cli /usr/local/bin/xxdrive-cli

# Template the base URL into the unit, then append any extra server flags.
EXTRA_FLAGS=""
if [[ -n "${XXD_TLS_CERT:-}" && -n "${XXD_TLS_KEY:-}" ]]; then
  EXTRA_FLAGS+=" -tls-cert ${XXD_TLS_CERT} -tls-key ${XXD_TLS_KEY}"
elif [[ -n "${XXD_TLS_CERT:-}" || -n "${XXD_TLS_KEY:-}" ]]; then
  echo "!! set BOTH XXD_TLS_CERT and XXD_TLS_KEY (or neither)" >&2
  exit 1
fi
if [[ "${XXD_SECURE_COOKIES:-0}" == "1" ]]; then
  EXTRA_FLAGS+=" -secure-cookies"
fi
if [[ -n "${FABRIC_CLUSTER_KEYS_PATH:-}" ]]; then
  EXTRA_FLAGS+=" -keyring ${FABRIC_CLUSTER_KEYS_PATH}"
fi
sed -e "s|https://CHANGE-ME.example.com|${BASE_URL}|g" \
    -e "s|/srv/deep/xx-drive|${DATA_DIR}|g" deploy/xxdrive.service > /etc/systemd/system/xxdrive.service
if [[ -n "$EXTRA_FLAGS" ]]; then
  sed -i "s|^ExecStart=.*$|&${EXTRA_FLAGS}|" /etc/systemd/system/xxdrive.service
fi

# Known first-boot admin password goes into an EnvironmentFile the unit loads
# (root-only readable). bootstrapAdmin only uses it while the users table is
# empty, but the secret would still sit on disk — remove it after first login.
if [[ -n "${XXD_ADMIN_PASSWORD:-}" ]]; then
  # systemd parses EnvironmentFile values itself (not shell %q rules), so
  # refuse characters whose meaning differs between the two parsers rather
  # than storing a silently-wrong password.
  if [[ "$XXD_ADMIN_PASSWORD" == *['"\$\']* || "$XXD_ADMIN_PASSWORD" == *$'\n'* ]]; then
    echo "!! XXD_ADMIN_PASSWORD contains a character that is unsafe in a systemd EnvironmentFile (\", \\, \$, newline)." >&2
    echo "   Pick a simpler bootstrap password, or set it by hand instead:" >&2
    echo "     systemctl edit xxdrive   # [Service] / Environment=XXD_ADMIN_PASSWORD=..." >&2
    exit 1
  fi
  install -m 0600 /dev/null /etc/default/xxdrive
  printf 'XXD_ADMIN_PASSWORD=%s\n' "$XXD_ADMIN_PASSWORD" > /etc/default/xxdrive
fi

echo "==> enabling service"
systemctl daemon-reload
systemctl enable --now xxdrive
sleep 1
systemctl --no-pager status xxdrive | head -12

cat <<NOTE

=== Installation complete ===

First start: if no admin existed, one was created.

  * With XXD_ADMIN_PASSWORD set at install time: log in with that password,
    then remove it from /etc/default/xxdrive.
  * Without it: read the generated one-time password now:

      journalctl -u xxdrive | grep "created admin user" -A4

Lost the admin password later? Reset it (prompts twice, no echo):

      systemctl stop xxdrive
      sudo -u xxdrive xxdrive-server -data ${DATA_DIR} -passwd admin
      systemctl start xxdrive

TLS: this unit serves plain HTTP on 127.0.0.1:8080. Either terminate TLS at a
reverse proxy (deploy/Caddyfile.example or deploy/nginx.conf.example) AND add
`-secure-cookies` to ExecStart, e.g.:

      systemctl edit xxdrive
        [Service]
        ExecStart=
        ExecStart=/usr/local/bin/xxdrive-server -addr 127.0.0.1:8080 \
          -data ${DATA_DIR} -base-url https://drive.example.com \
          -secure-cookies

...or re-run this script with XXD_TLS_CERT/XXD_TLS_KEY to have the server do
TLS itself. Fabric SSO: re-run with FABRIC_CLUSTER_KEYS_PATH=/path/to/keyring.json
(or add `-keyring ...` in the same drop-in).

Then:
  * Log into the web UI at your base URL and change the admin password.
  * Linux clients:  xxdrive-cli login <url> <user>   then  xxdrive-cli sync ~/Drive /<user-dir>
NOTE
