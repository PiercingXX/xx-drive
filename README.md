# xx-drive

> A private cloud in one Go binary — your files stay files on disk, and nothing this thing does can lose them.

Self-hosted storage for a Debian box: a single static binary that serves an
HTTP API, an embedded web UI, and a PWA, plus a Linux CLI sync client and an
Android client. Files are plain files on disk (rsync them, `find` them, walk
away with them); metadata lives in SQLite. No PHP, no container stack, no CGO.

Built from cleanroom behavioral specs of Dropbox and Synology Drive plus a
survey of Nextcloud/Seafile/Syncthing-class products: the safety-net triad
(**soft-delete trash + file versioning + conflict copies — never data loss**),
share links with password/expiry, search, stars, activity feed, and multi-user
admin.

<img src="docs/images/screenshot.png" width="270" alt="xx-drive Android login screen on a Pixel 6, AMOLED Night">


## Components

| Piece | What it is | Where |
|---|---|---|
| Server | Single Go binary; HTTP API + embedded web UI + PWA. Plain-files storage, SQLite WAL metadata. | `cmd/xxdrive-server`, `internal/*` |
| Web UI | Vanilla-JS SPA served by the binary (no build step). Browse/upload/download, share links, versions, trash, search, stars, activity, admin. Installable PWA on Android/desktop Chrome. | `internal/webfs/static/` |
| Linux CLI | `xxdrive-cli`: `login`, `ls`/`mkdir`/`rm`/`mv`/`cp`/`up`/`down`, **two-way `sync`/`watch`** with baseline-based conflict copies. | `cmd/xxdrive-cli` |
| Android app | Kotlin WebView client with native upload-picker + DownloadManager bridges and an optional camera auto-backup WorkManager worker. Source only; no prebuilt APK ships here. | `android/` |
| Estate SSO | Validate-only fabric bearer tokens — same v1 token format as xx-note and xx-chat, verified locally with no callback. Optional; local admin auth works without a keyring. | `internal/fabric` |
| Deploy | systemd unit + Debian install script. | `deploy/` |

Full operator/developer manual: [docs/MANUAL.md](docs/MANUAL.md).
Frozen v1 API contract: [docs/API.md](docs/API.md).

## Status 🧪

| Check | Command | Result |
|---|---|---|
| Server build | `go build ./...` | clean, Go 1.25, two direct deps (`x/crypto`, pure-Go `modernc.org/sqlite`) |
| Vet | `go vet ./...` | clean |
| Server tests | `go test ./...` | **22 green, 0 failures** across `internal/api`, `internal/fabric`, `internal/fsdrv`. `internal/store` and `internal/webfs` have no tests |
| Android tests | `cd android && ./gradlew testDebugUnitTest` | **23 green, 0 failures** — all of them cover the theme-sync receiver; the rest of the app has no unit tests |
| Android build | `cd android && ./gradlew assembleDebug` | 7.0 MB debug APK. `minSdk 26`, target/compile SDK 35 |

### What is actually proven on the phone

The Android app installs, launches, and draws its login screen on a Pixel 6
running GrapheneOS without crashing. That is the entire list. No server has
been reached from that device: no login has succeeded, no file has moved, the
WebView has never rendered a file listing. The server-side behavior above is
proven by Go tests on a workstation, not by the phone in the screenshot. Treat
the Android client as unverified past first paint.

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
Flags/env: `-addr`, `-data`, `-base-url`, `-max-upload-mb` (default 10240),
`-trash-days` (default 30), `-keyring`, `XXD_ADMIN_USER`, `XXD_ADMIN_PASSWORD`.

## Linux client

```bash
go build -o xxdrive-cli ./cmd/xxdrive-cli
./xxdrive-cli login https://drive.example.com alice
./xxdrive-cli sync ~/Drive /alice        # two-way, conflict-safe
./xxdrive-cli watch ~/Drive /alice       # continuous every 30s (-interval N)
```

## Android

Open `android/` in Android Studio → Run, or `cd android && ./gradlew
assembleDebug`. See [android/README.md](android/README.md) for the camera
auto-backup worker and the two hardening items that are still open there:
cleartext HTTP is enabled so you can test against a LAN server without TLS,
and the bearer token sits in plain `SharedPreferences`. Fix both before this
faces anything but your own tailnet.

## Theme sync 🎨

XX-Launcher broadcasts `xx.launcher.THEME_CHANGED` with a theme name and a
resolved background ARGB; xx-drive's exported receiver persists the choice and
repaints its native chrome on the next `onCreate`/`onResume`. Eight presets:
AMOLED Night, Graphite, Forest Night, Ocean Drift, Burgundy, Paper, Mist,
Custom. The nine family apps all speak this contract — set the theme once in
the launcher and the estate follows.

**The WebView stays dark on every preset.** That is deliberate, not a bug: the
page inside is the server's own web UI with its own palette, and repainting
someone else's stylesheet from a broadcast is how you get unreadable text.
Native chrome themes; page content does not.

## Security model

- PBKDF2-HMAC-SHA256 (600k) password hashing; constant-time checks everywhere.
- Sessions: 256-bit tokens stored hashed server-side; HttpOnly SameSite=Lax
  cookies; bearer tokens for native clients; sliding 30-day expiry; revoke-others.
- Estate SSO: fabric v1 tokens validated locally against an operator-provisioned
  cluster keyring. This node never signs, only verifies — and only when a
  keyring is configured.
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
internal/fabric/      estate SSO token validator (validate-only)
internal/webfs/       embedded SPA + PWA assets
android/              Kotlin client source project
deploy/               systemd unit + install script
docs/API.md           frozen v1 API contract
docs/MANUAL.md        operator/developer manual
```

## Family

Brand, tokens, and type come from
[piercingxx-branding](https://github.com/PiercingXX/piercingxx-branding).
AMOLED black, Signal white, Space Mono / JetBrains Mono.

Free and ad-free. Collects no personal data. Your files sit on your own
server, as files, and nothing here phones anywhere else.
