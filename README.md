# xx-drive

Self-hosted private cloud storage — a single static Go binary for your Debian
server, with a web UI, a Linux CLI sync client, and an Android app. Your files
stay as plain files on disk (rsync-friendly), metadata lives in SQLite.

Built from cleanroom behavioral specs of Dropbox and Synology Drive plus a
survey of Nextcloud/Seafile/Syncthing-class products: the safety-net triad
(**soft-delete trash + file versioning + conflict copies — never data loss**),
share links with password/expiry, search, stars, activity feed, and multi-user
admin — without the operational weight of a PHP/container stack.

## Components

| Piece | What it is | Where |
|---|---|---|
| Server | Single Go binary; HTTP API + embedded web UI + PWA. Plain-files storage, SQLite WAL metadata. | `cmd/xxdrive-server`, `internal/*` |
| Web UI | Vanilla-JS SPA served by the binary (no build step). Browse/upload/download, share links, versions, trash, search, stars, activity, admin. Installable PWA on Android/desktop Chrome. | `internal/webfs/static/` |
| Linux CLI | `xxdrive-cli`: login, ls/mkdir/rm/mv/cp/up/down, **two-way `sync`/`watch`** with baseline-based conflict copies. | `cmd/xxdrive-cli` |
| Android app | Kotlin WebView client with native upload-picker + DownloadManager bridges and an optional camera auto-backup WorkManager worker. Buildable source (needs Android Studio). | `android/` |
| Deploy | systemd unit + Debian install script. | `deploy/` |

## Quick start (server)

```bash
# dev/test run
go build -o xxdrive-server ./cmd/xxdrive-server
XXD_ADMIN_PASSWORD=secret123 ./xxdrive-server -addr :8080 -data ./data
```

Production on Debian:

```bash
sudo ./deploy/install.sh https://drive.example.com
journalctl -u xxdrive | grep "created admin user" -A4   # one-time admin password
```

Put Caddy/nginx in front for TLS, or pass `-tls-cert/-tls-key` directly.
Flags/env: `-addr`, `-data`, `-base-url`, `-max-upload-mb`, `-trash-days`,
`XXD_ADMIN_USER`, `XXD_ADMIN_PASSWORD`.

## Linux client

```bash
go build -o xxdrive-cli ./cmd/xxdrive-cli
./xxdrive-cli login https://drive.example.com alice
./xxdrive-cli sync ~/Drive /alice        # two-way, conflict-safe
./xxdrive-cli watch ~/Drive /alice       # continuous every 30s (-interval N)
```

## Android

Open `android/` in Android Studio → Run. See `android/README.md`.
(No prebuilt APK ships here; the project builds a debug APK in one click.)

## Security model

- PBKDF2-HMAC-SHA256 (600k) password hashing; constant-time checks everywhere.
- Sessions: 256-bit tokens stored hashed server-side; HttpOnly SameSite=Lax
  cookies; bearer tokens for native clients; sliding 30-day expiry; revoke-others.
- CSRF: cookie-authenticated mutations require same-origin Origin/Referer;
  bearer clients are exempt (no ambient credentials). No state changes via GET.
- Path safety: single choke-point resolver (`fsdrv.ResolveUserPath`) with lexical
  containment + per-component symlink refusal + percent-encoding rejection,
  covered by a traversal fuzz corpus test.
- Share links: hashed capability tokens, optional password/expiry, view-only
  mode, instant revocation; anonymous endpoints rate-limitable at the proxy.
- Uploads: size-capped, streamed to temp, atomic rename commit; If-Match ETag
  concurrency control returning 412 instead of clobbering.

## Explicit v1 non-goals

Block/delta sync, chunked-resumable uploads, internal ACL folder sharing /
team folders, server-side thumbnails, quotas, WebDAV, smart-sync placeholders.
All are additive follow-ups; the storage layout was chosen so none require a
migration.

## Layout

```
cmd/xxdrive-server/   server entrypoint
cmd/xxdrive-cli/      Linux client (+ two-way sync engine)
internal/store/       SQLite metadata (users/sessions/shares/stars/events/versions)
internal/fsdrv/       plain-file storage driver + path-safety choke point
internal/api/         HTTP handlers, middleware, public share pages
internal/webfs/       embedded SPA + PWA assets
android/              Kotlin client source project
deploy/               systemd unit + install script
docs/API.md           frozen v1 API contract
```
