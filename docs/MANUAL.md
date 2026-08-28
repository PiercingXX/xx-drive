# xx-drive — Operator/Developer Manual

xx-drive is self-hosted private cloud storage: one static Go binary that
serves an HTTP API, an embedded web UI, and a Linux CLI sync client, plus
a separate Android app source project. Files live as plain files on disk
(rsync-friendly); all metadata lives in SQLite. This manual documents
current behavior, re-verified against the source after a round of P0
correctness fixes (sync data loss, trash atomicity, path safety, public
shares, Android wiring — see `todo.md` for the authoritative open-items
list). Where a claim could not be verified this manual says so
explicitly. If this manual and the code disagree, the code wins — file a
fix.

## 1. What xx-drive is & its role

xx-drive is the Skippy estate's **self-hosted private cloud storage**
add-on: a Dropbox/Synology-Drive-class personal file store, built from
cleanroom behavioral specs of Dropbox and Synology Drive plus a survey of
Nextcloud/Seafile/Syncthing-class products — without the operational
weight of a PHP/container stack (`README.md`).

- **Honest status (2026-08).** An earlier independent audit called the
  initial commit **SOUND, no blockers**; subsequent review disagreed and
  surfaced real P0 bugs (CLI sync clobbering local edits, non-atomic
  trash, username `..` escape, broken folder shares, unwired Android
  auth). Those P0s are **now fixed with tests**, but none of it has been
  exercised on a real phone yet. The server is stood up on the live node
  (see §7); what remains unproven is the Android client on a physical
  device. Treat `todo.md` and the README Status section as the source of
  truth for what is proven vs pending — not the old audit verdict.
- **Estate placement.** Per the constellation map
  (`/media/Working-Storage/GitHub/Skippy-Project/AGENTS.md`), xx-drive runs
  **on the skippy-tel-network fabric**, single-node on Dutchman. It is
  deployed via the systemd unit in `deploy/` with its data on the ZFS pool
  at `/srv/deep/xx-drive` — see §7.
- **The safety-net triad.** The design goal, stated in the README, is
  "never data loss": soft-delete trash + file versioning + conflict
  copies, on every write path, always. Share links (password/expiry),
  search, stars, an activity feed, and multi-user admin round out the
  feature set.
- **Explicit v1 non-goals** (README): block/delta sync, chunked-resumable
  uploads, internal ACL folder sharing / team folders, server-side
  thumbnails, quotas, WebDAV, smart-sync placeholders. All are additive —
  the on-disk layout was chosen so none require a migration.
- **No cloud dependency.** No cloud SDKs, no telemetry, no network calls
  beyond the Go standard library — consistent with the estate's
  no-cloud-wired-in doctrine.

## 2. Architecture

### 2.1 The server binary

One Go binary, `cmd/xxdrive-server/main.go`, wires together three
internal packages and serves everything from a single `net/http` process:

| Package | Job |
|---|---|
| `internal/store` | SQLite (WAL) metadata: users, sessions, shares, stars, events, version index, etag cache. `store.Open` sets `SetMaxOpenConns(1)` — modernc's pure-Go SQLite driver is happiest with serialized writes. |
| `internal/fsdrv` | Plain-files-on-disk storage driver **and** the path-safety choke point (`ResolveUserPath`) — see §5. |
| `internal/api` | HTTP handlers, auth/CSRF/rate-limit middleware, public share pages, request logging, security headers. |
| `internal/webfs` | The embedded SPA + PWA (`//go:embed static`), served for every path the API mux doesn't claim, with SPA fallback to `index.html` for unknown paths. |

At startup `main.go`: creates the data dir (`0o700`), opens the store,
inits the fsdrv driver (which also creates `files/ trash/ versions/ tmp/`
under the data root), **bootstraps an admin user if the users table is
empty**, builds the HTTP handler chain, starts the hourly janitor
goroutine, and calls `ListenAndServe` (or `ListenAndServeTLS` if
`-tls-cert`/`-tls-key` are both set).

### 2.2 Web UI

Vanilla-JS SPA, no build step: `internal/webfs/static/app.js` exercises
every `/api/*` endpoint (files, trash, versions, shares, search, stars,
events, admin) plus `index.html`, `style.css`,
`manifest.webmanifest`, and `sw.js` (app-shell caching only — the service
worker never caches API responses, per `docs/API.md`). Installable as a
PWA on Android/desktop Chrome. Served directly by the Go binary; no
separate frontend deploy step.

### 2.3 Linux CLI sync engine

`cmd/xxdrive-cli` (`xxdrive-cli` binary) does simple one-shot commands
(`login`, `ls`, `mkdir`, `rm`, `mv`, `cp`, `up`, `down`) plus a genuine
**two-way sync engine** (`cmd/xxdrive-cli/sync.go`). The engine's contract
is "no data loss, ever":

- **Baseline-based three-way reconcile.** Each sync pair keeps a JSON
  baseline file (`~/.config/xxdrive/sync-<sha256>.json`) mapping
  remote-relative paths to `{size, mtime, etag}` as of the last completed
  sync. Every `sync`/`watch` pass computes `local`, `remote`, and
  `baseline` snapshots and walks the **union** of all three path sets.
- **Local-only change → local wins.** The local edit **overwrites** the
  remote copy; the server snapshots the previous content as a version
  first, so nothing is lost. The push carries an `If-Match` header built
  from the baseline's etag: if another machine overwrote the remote since
  the listing, the server answers 412 and both versions survive via the
  conflict path instead of a blind clobber. After a successful push the
  local mtime is converged to the stored remote entry so the next pass is
  a clean no-op.
- **Remote-only change → pull.** Download (or remote delete propagates —
  see below). Downloads stage to `<file>.xxpart` and rename into place
  only on success, so an interrupted transfer never corrupts the
  destination; a stale `.xxpart` from a failed pull is removed after
  success.
- **Changed on both sides → conflict copies.** The local version is first
  parked server-side as `name (conflict from HOST TIME).ext`; then BOTH
  sides converge on the canonical remote content at the original path,
  the conflict copy is mirrored locally too, and the reconciled state is
  written to the baseline. Neither version is ever lost or overwritten.
  An upload rejected with 412 is treated as both-changed for the same
  reason.
- **Deletes are soft, and guarded by a baseline entry.** A local delete
  propagates as a normal remote delete (server-side trash). A remote
  delete moves the untouched local copy into
  `<localDir>/.xxdrive-trash/<timestamp>/` — **never a hard delete**, and
  only for paths that actually have a baseline entry, so an empty or
  corrupt baseline can never destroy the only copy on a first sync.
- **Same size+mtime = same content (documented limitation).** Change
  detection is metadata-level (`size`, `mtime`); files with identical
  size+mtime but different bytes converge without copies, and a
  same-size edit within one Unix second can read as unchanged or
  conflict spuriously. Accepted for v1 (todo P2); a content hash would be
  the real fix.
- **Staging artifacts are invisible to sync.** `walkLocal` skips names
  with suffix `.xxpart` and prefix `.xxpartial` (plus `.xxdrive-*`
  directories), and the server's `List`/`WalkTree` skip the same — an
  interrupted transfer never becomes user data on either side.
- **`watch`** loops `syncOnce` every `-interval` seconds (default 30),
  single-flight: an overrun pass makes later ticks skip instead of
  stacking passes.
- Baseline state files are keyed by the full sha256 of
  `<localDir>|<remoteDir>` — fixed-length digest, no collision-prone name
  folding.

### 2.4 Android app

`android/` is a **buildable Kotlin source project** (Android Studio,
Gradle) — no prebuilt APK ships in the repo, by design. The classes that
matter:

- `LoginActivity` — server URL + username/password entry.
- `MainActivity` — hosts the server's own PWA in a `WebView`. Auth is now
  **wired**: the bearer token is placed on the server origin as the
  `xxd_session` cookie (`CookieManager.setCookie` callback, with a short
  UI-thread fallback) *before* `loadUrl`, so the SPA's first
  `/api/auth/me` carries the session. API requests ride that cookie —
  WebView POSTs have no interceptable body, and a Bearer GET-proxy that
  closed the OkHttp response stream would have broken listing and
  previews. Native file-picker uploads
  (`onShowFileChooser`) and authenticated downloads via the system
  `DownloadManager`: requests carry both bearer and cookie headers, land
  in the **app-private** Downloads dir
  (`Android/data/<pkg>/files/Download/`, no storage permission needed on
  minSdk 26–28) under the **original filename** parsed from
  `Content-Disposition`/URL, uniqued so repeat downloads don't collide.
  Main-frame failures render an inline error page instead of a white
  screen.
- `PhotoUploadWorker` — a `CoroutineWorker` (WorkManager) camera
  auto-backup job uploading new MediaStore images to
  `/Camera Uploads/<yyyy-MM-dd>/<name>` with `conflict=rename` (never
  overwrites). The watermark advances only to the newest `dateTaken`
  that **actually uploaded**, so a failed photo older than newer
  successes is re-queried and retried next run. `Session.init` runs in
  the `XxDriveApp` Application class *and* defensively at the top of
  `doWork`, so process death can't silently no-op the worker.
  Wi-Fi-only configurable, ~every 30 minutes per `android/README.md`.
- `SettingsActivity` — auto-upload toggle: checking it with no media
  permission reverts the box and requests the permission;
  `onRequestPermissionsResult` **completes the grant** (persists ON +
  schedules WorkManager immediately; denial keeps it off with an
  explanation toast). Logout calls `POST /api/auth/logout` best-effort
  (short timeouts), then clears the WebView cookie jar and the local
  token so the session does not linger for up to 30 days.
- `Session` — bearer token in `EncryptedSharedPreferences`; server URL in
  plain prefs so a keystore recovery does not force retyping it.

**On-phone smoke is still pending.** All of the wiring above is proven by
code review and JVM unit tests only — no login, upload, download, or
backup run has succeeded (or been attempted) on a physical device yet.
See the README Status section before trusting the Android client.

`AndroidManifest.xml` currently sets `android:usesCleartextTraffic="true"`
so LAN/HTTP testing works out of the box; the app's own README says to
remove that attribute for a production HTTPS deployment.

### 2.5 Data model & on-disk layout

Metadata lives in one SQLite (WAL mode) database,
`<data-dir>/xxdrive.db`, opened with
`_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)`
(`internal/store/store.go`). Tables: `users`, `sessions`, `shares`,
`stars`, `events`, `versions`, `etag_cache`. Every table **except
`users`/`sessions`** is rebuildable from the plain-file layout.

File content itself never touches SQLite. Under the data root:

```
<data-dir>/
  xxdrive.db              SQLite WAL metadata (users/sessions/shares/stars/events/versions/etags)
  files/<username>/...    each user's drive tree — plain files, rsync-friendly
  trash/<username>/<id>/       soft-deleted payload (file or whole directory)
  trash/<username>/<id>.json   {origPath, name, isDir, deletedAt}
  versions/<user>/<sha256(logicalPath)>/<versionID>   prior content of a file (max 32 kept per path)
  tmp/                     staging dir for atomic uploads (.xxpart-style temp + rename)
```

`internal/fsdrv.New` creates `files/ trash/ versions/ tmp/` under the
(symlink-resolved) data root on startup if missing.

## 3. How it's run

**Binary / entrypoint:** `cmd/xxdrive-server` builds to `xxdrive-server`
(pure Go, CGO-free via modernc's SQLite driver — `deploy/install.sh`
builds with `CGO_ENABLED=0`, correct for that driver). The CLI is a
separate binary, `cmd/xxdrive-cli` → `xxdrive-cli`.

```bash
go build -o xxdrive-server ./cmd/xxdrive-server
go build -o xxdrive-cli    ./cmd/xxdrive-cli
```

**Port / bind:** default listen address is `:8080` (all interfaces), but
the server now **refuses to start on a non-loopback address without TLS**
unless `-i-know` is passed — cleartext credentials would cross the
network. Bind `127.0.0.1:8080`, set `-tls-cert/-tls-key`, or pass
`-i-know` to accept the risk (the README quick start uses a loopback
bind). **The production systemd unit binds `127.0.0.1:8080`**
(`deploy/xxdrive.service`). The check resolves hostnames too: "localhost"
counts as loopback only when all its DNS addresses are loopback, and
unparseable addresses fail closed (`addrIsLoopback`, unit-tested in
`cmd/xxdrive-server/main_test.go`).

**Health:** `GET /healthz` → HTTP 200, body `ok` (plain text, no auth
required, registered first in `routes()`). **This is `/healthz`,
matching radio's convention — NOT jal's `/api/health`.**

**Data dir:** `-data` flag / `XXD_DATA_DIR` env, default `./data` for a
dev run. The systemd unit uses `/srv/deep/xx-drive` — the live box's ZFS
pool, matching the constellation map's precedent (jal's data dir) rather
than the install directory. `install.sh` defaults `XXD_DATA_DIR` to this
path and templates it into the unit's `-data` flag and `ReadWritePaths`,
so the unit and the data dir always agree and an install never creates a
second data tree.

**systemd unit** (`deploy/xxdrive.service`): runs as a dedicated
`xxdrive` system user, binds `127.0.0.1:8080`, `Restart=on-failure` /
`RestartSec=5`. Data lives at `/srv/deep/xx-drive` on the ZFS pool (not
`StateDirectory`). Hardening flags present: `NoNewPrivileges`,
`ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
`ReadWritePaths=/srv/deep/xx-drive` (only), `ProtectKernelTunables`,
`ProtectControlGroups`, `RestrictSUIDSGID`, `MemoryDenyWriteExecute`,
`LockPersonality`.

**Install script** (`deploy/install.sh`, run as root):
`./install.sh https://drive.example.com` — builds both binaries (requires
Go ≥ 1.25; `go.mod` pins `go 1.25.0`; or uses a prebuilt `xxdrive-server`
in the repo root if no `go` toolchain is present), creates the `xxdrive`
system user, installs binaries to `/usr/local/bin/`, templates the base
URL into the systemd unit, and enables+starts the service.

Optional install-time environment extends the generated `ExecStart`
instead of hand-editing the unit:

- `XXD_TLS_CERT` + `XXD_TLS_KEY` → `-tls-cert/-tls-key` (server does TLS itself)
- `XXD_SECURE_COOKIES=1` → `-secure-cookies` (use when TLS terminates at a proxy)
- `FABRIC_CLUSTER_KEYS_PATH=/path/keyring.json` → `-keyring ...` (estate SSO)
- `XXD_ADMIN_PASSWORD=...` → written to root-only `/etc/default/xxdrive`
  (loaded via `EnvironmentFile`) for a known first-boot password; delete
  it after first login.

Reverse-proxy configs that pair with the loopback bind live in
`deploy/Caddyfile.example` and `deploy/nginx.conf.example`.

**Graceful shutdown:** SIGTERM/SIGINT stops the listener and drains
in-flight requests for up to 15 seconds (`http.Server.Shutdown`), then
gives the janitor up to 5 more seconds to finish its sweep before exit —
an in-flight upload or purge is never torn down mid-write by a normal
restart.

**The one-time admin password:** on first run, if the `users` table is
empty, `bootstrapAdmin` creates an admin user. If `XXD_ADMIN_PASSWORD` is
unset, an 18-byte random password is generated and printed to stdout
**once**:

```
=== First run: created admin user ===
  username: admin
  password: <random>
  (shown once — change it after first login)
```

Under systemd, retrieve it with:

```bash
journalctl -u xxdrive | grep "created admin user" -A4
```

There is **no hardcoded default credential**.

**Lost the admin password?** Reset it with the built-in recovery mode
(prompts twice, no echo; same PBKDF2 hash format and 8-character minimum
as the API):

```bash
systemctl stop xxdrive
sudo -u xxdrive xxdrive-server -data /srv/deep/xx-drive -passwd admin
systemctl start xxdrive
```

Run it as the service user so `xxdrive.db` ownership stays correct, and
stop the unit first so two processes don't write the database at once.
`-passwd <user>` works for any local user (including a disabled one —
the new password takes effect once re-enabled) and exits without ever
binding a socket.

**Hourly janitor:** an in-process goroutine (`startJanitor` in
`main.go`) runs once at startup and then every hour via `time.Ticker`,
doing four things: (1) purge trash items older than `-trash-days` via
`fs.PurgeOldTrash` — corrupt or implausible metadata is **skipped**
(payload preserved), never destroyed — and drop each purged item's
version history (index rows + blob dirs) exactly like an interactive
permanent purge; (2) prune version history to the newest 32 per
`(user, path)` via `st.PruneVersions(32)`, removing the pruned blobs
from disk; (3) sweep expired public share-password grants; (4) remove
stale upload temp files in `tmp/` older than 24 hours. No external cron
is needed — this covers what jal's own self-restarting crawl does for a
different kind of background work.

**Backups (SQLite WAL — read this before you `cp`):** `xxdrive.db` is a
live WAL database (`xxdrive.db-wal` / `-shm` sit next to it). A naive
`cp xxdrive.db backup.db` while the server is running can capture a torn
snapshot and silently lose recent metadata (sessions, shares, version
index). Either:

```bash
# Consistent online snapshot (recommended):
sqlite3 /srv/deep/xx-drive/xxdrive.db ".backup '/backups/xxdrive-backup.db'"

# ...or stop the unit and copy cold:
systemctl stop xxdrive && cp -a /srv/deep/xx-drive/xxdrive.db /backups/ && systemctl start xxdrive
```

Either way, back up the content directories too — they are plain files,
not in SQLite: `files/` (all user data), `trash/` (soft-deleted payloads
+ their `.json` sidecars), and `versions/` (prior file versions). The DB
alone is not a restore point: every table except `users`/`sessions` is
rebuildable from those directories, but not vice versa. On ZFS, a
recursive snapshot of the whole data dir achieves the same thing
atomically.

## 4. Configuration & env vars

Some flags read from an env var first if the flag isn't passed
(`envOr` helper in `main.go`) — those are marked with an env var below;
the rest are flag-only.

| Flag | Env var | Default | Notes |
|---|---|---|---|
| `-addr` | `XXD_ADDR` | `:8080` | Listen address. Non-loopback without TLS refuses to start unless `-i-know`. Use `127.0.0.1:PORT` behind a proxy. |
| `-data` | `XXD_DATA_DIR` | `./data` | Data directory (created `0o700` if missing). |
| `-base-url` | `XXD_BASE_URL` | `""` | Public base URL, used to build absolute share links (`shareURLPrefix`). An `https://` value also forces `Secure` on cookies. |
| `-max-upload-mb` | *(flag only)* | `10240` (10 GiB) | Max upload size in MB; enforced via `LimitReader`/`MaxBytesReader` in `fsdrv.Upload`. |
| `-trash-days` | *(flag only)* | `30` | Trash retention before the janitor purges an item. |
| `-tls-cert` | `XXD_TLS_CERT` | `""` | TLS cert path. If both cert+key are set, the server calls `ListenAndServeTLS` directly. |
| `-tls-key` | `XXD_TLS_KEY` | `""` | TLS key path. |
| `-secure-cookies` | *(flag only)* | `false` | Force `Secure` on session/share cookies. **Set this when TLS terminates at a reverse proxy** — the server does not trust `X-Forwarded-Proto`. `Secure` is otherwise automatic when `-tls-cert` is set or `-base-url` starts with `https://`. |
| `-i-know` | *(flag only)* | `false` | Allow a non-loopback bind without TLS (cleartext credentials). Escape hatch for the loopback guard. |
| `-passwd <user>` | *(flag only)* | — | Interactive admin-password recovery; sets a new password for `<user>` and exits. See §3. |
| *(no flag)* | `XXD_ADMIN_USER` | `admin` | Username for the bootstrap admin (only used when the users table is empty). |
| *(no flag)* | `XXD_ADMIN_PASSWORD` | *(random, printed once)* | Bootstrap admin password. Set this in production instead of relying on the printed-once random value if you want a known first password. |
| `-keyring` | `FABRIC_CLUSTER_KEYS_PATH` | `""` | Path to the shared estate cluster keyring (the ClusterKeyring JSON `ClusterKeyring.save` writes). Enables **estate SSO** (§5.1). **Optional** — with no keyring, only local admin/password auth is served, so the operator is never locked out. On the deployed box this is `/srv/deep/skippy-tel-deploy/fabric-keys.json`. |

`SessionTTL` (sliding 30-day expiry) and `MaxUploadMB`/`TrashRetentionDays`
defaults are also enforced defensively inside `api.New` if a zero/negative
value is passed through `Config`.

If no `-tls-cert`/`-tls-key`, the server logs a warning on every start:
`"WARNING: running without TLS — put behind a reverse proxy or set
-tls-cert/-tls-key"`.

## 5.1 Estate SSO (fabric identity)

xx-drive joins the estate's single-account fabric, so one estate account
logs into xx-drive exactly as it does into xx-note / xx-calendar /
xx-vitals. xx-chat is the account authority (owns the password/PBKDF2 and
mints tokens at `POST /api/v1/fabric/login`); every app — xx-drive
included — validates a **ClusterKeyring v1** bearer token **locally**, with
no call back to xx-chat. The validator (`internal/fabric/token.go`) is the
same stdlib HMAC verifier xx-note uses, keyed on the shared ring at
`FABRIC_CLUSTER_KEYS_PATH`.

**Two ways in (both resolve to the same estate identity):**

- **Bearer token** (apps / CLI / Android): send
  `Authorization: Bearer <v1-token>` on every request. Any credential shaped
  like a v1 token (`v1.<key>.<body>.<sig>`) is validated against the ring;
  a local opaque session token — which never contains dots — still takes the
  legacy session path, so the two never collide.
- **Browser SSO** — `POST /api/auth/fabric` `{"token":"v1..."}` (or the
  same `Authorization: Bearer` header): the token is validated once and
  exchanged for a normal `xxd_session` cookie bound to that estate identity.

**How identity maps to storage isolation.** The validated token yields the
estate `user_id` (xx-chat `users.id`) — taken from the token **only**, never
from any body/query/path. On first sight xx-drive get-or-creates a local
**shadow user** whose username is `fabric_<user_id>` and whose `fabric_id`
column holds that id (a partial-unique index guarantees one row per estate
identity; the shadow row has an unusable password hash, so it can never log
in by password). That `fabric_<user_id>` string is then the ordinary
per-user isolation key everywhere: the `files/<username>/…` path-containment
root in `fsdrv` and the integer user-id foreign key on every metadata table
(shares, stars, versions, events, etag cache). So the existing per-user
isolation is preserved unchanged — it is simply now keyed on the estate
`user_id`. A two-user adversarial test
(`TestFabricTwoUserIsolation`) proves user A's token can never list,
download, or delete user B's files.

**Local admin auth is retained as a fallback.** The bootstrap admin
(`XXD_ADMIN_USER`/`XXD_ADMIN_PASSWORD`, PBKDF2, `POST /api/auth/login`)
keeps working alongside estate SSO — this is deliberate so the operator is
never locked out and a box with no keyring configured is still fully usable.

## 5. Security model

The design centers on the OWASP file-storage threat set. Auth
enforcement (401 on unauthenticated API), traversal blocking (raw `../`
and percent-encoded `%2f`/`%2e` forms), wrong-password rejection, and
share containment are all covered by the Go test suite (`go test ./...`
— api, fabric, fsdrv, store, token, cli sync matrix). No auth bypass,
path traversal, injection, committed secret, or unsafe default
credential is known in the current code; what remains *unproven* is the
Android client on a real phone (see §2.4).

- **Path safety — single choke point.** Every request-derived path must
  pass through `fsdrv.ResolveUserPath` (`internal/fsdrv/fsdrv.go`)
  exactly once. It: rejects NUL bytes and backslashes outright; lexically
  cleans and anchors the path (`filepath.Clean("/" + rel)`); rejects any
  percent-encoded `%2f`/`%5c`/`%2e` (net/http already decodes once, so
  these would be literal characters — forbidden anyway as a
  double-decode-confusion defense); enforces a containment check against
  the **pre-clean** per-user directory (`files/<username>` joined
  literally — so a hostile username that `filepath.Join` would rewrite
  can never satisfy a post-clean check); and then **walks every existing
  path component with `os.Lstat`, refusing any component that is a
  symlink** — so content planted via out-of-band shell access cannot be
  used to smuggle a request outside the user root. Backed by a traversal
  fuzz corpus test (`TestTraversalCorpus`) and a dedicated symlink-escape
  test (`TestSymlinkEscape`) in `internal/fsdrv/fsdrv_test.go`.
  - **Usernames are strict single segments.** `store.CreateUser` (the
    only path a username or fabric id can enter the system) requires a
    non-empty name ≤64 chars with no `/`, `\`, NUL, not `.` or `..`,
    and untouched by `filepath.Clean` (`validSegmentName`). The old
    `..`-escapes-the-tree bug cannot recur.
  - **Zips never follow planted symlinks.** Both the authenticated
    `/api/files/zip` walker and the public share zipper `Lstat` each
    walked path and open only regular files (`fsdrv.OpenRegular`);
    symlinks, specials, and unreadable entries are excluded from the
    archive entirely.
  - Public share targets resolve through `shareTarget`, which accepts
    `path` (full logical) or `sub` (share-relative), rejects literal
    `..` segments before cleaning, and enforces
    `full == sh.Path || strings.HasPrefix(full, sh.Path+"/")` — an
    anonymous visitor cannot climb above the shared subtree.
- **Passwords.** PBKDF2-HMAC-SHA256, 600,000 iterations (OWASP 2023
  guidance), 16-byte random salt, stored as
  `pbkdf2-sha256$<iter>$<salt-hex>$<dk-hex>`
  (`internal/store/store.go`). Comparison uses
  `subtle.ConstantTimeCompare`.
- **Sessions & tokens.** 256-bit random session tokens; **stored hashed
  (SHA-256) server-side** — the raw token only ever exists in the
  client's cookie or bearer header. Sliding 30-day expiry (refreshed on
  use, throttled to avoid a write on every request). `SetDisabled(...,
  true)` kills all of a user's sessions immediately. Session and
  share-grant cookies are `HttpOnly`, `SameSite=Lax`, and carry `Secure`
  whenever the server does TLS itself, `-base-url` is `https://…`, **or
  `-secure-cookies` is passed** (the reverse-proxy case). Share tokens
  follow the same hashed-at-rest pattern (24-byte random, base64url
  capability tokens) and are never logged (`logReq` explicitly skips the
  query string for any `/s/` path).
- **CSRF.** Cookie-authenticated *mutating* requests (`authedMutating`)
  require a same-origin `Origin` or `Referer` header; if both are absent,
  a non-empty `X-Requested-With` header is accepted as a fallback (fetch/
  XHR from another origin cannot send it cross-site without CORS),
  otherwise rejected with 403. Bearer-token clients (CLI, Android) are
  exempt because they carry no ambient browser credential. No state
  change ever happens on a GET. Mutations are additionally serialized
  per-user via a `sync.Map` of mutexes.
- **SQL.** Every query in `internal/store/store.go` is parameterized;
  `LIKE` patterns used for path-prefix matching escape `% _ \` via
  `EscapeLike`. No injection surface found by review or testing to date.
- **Uploads.** Size-capped (`io.LimitReader(r, maxSize+1)` +
  `http.MaxBytesReader` on JSON bodies), streamed to a temp file in
  `tmp/`, then committed via atomic `os.Rename`. `If-Match` (weak etag)
  support returns 412 instead of silently clobbering concurrent writes.
- **Headers, on every response** (`securityHeaders` middleware):
  `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
  `Referrer-Policy: strict-origin-when-cross-origin`, and a CSP of
  `default-src 'self'; img-src 'self' data: blob:; media-src 'self'
  blob:; style-src 'self' 'unsafe-inline'; script-src 'self';
  connect-src 'self'; frame-ancestors 'none'; base-uri 'self'`.
- **Admin bootstrap** has no hardcoded default credential (see §3).
  Admins cannot disable or delete their own account. Permanent user
  delete (`POST /api/admin/users/delete`) requires the target to already
  be disabled, then removes `files/<user>`, `trash/<user>`,
  `versions/<user>` and the metadata row.
- **No secrets committed**; `.gitignore` covers `/data/`, both binaries,
  Android build output, and `local.properties`.

### Findings from the earlier audit — current state

The initial-commit audit listed four LOW/INFO items. All are now
addressed in code; kept here so the history is traceable:

- **FIXED — cookie `Secure` flag depended on in-process TLS.** The
  recommended topology (TLS-terminating reverse proxy in front) left
  `TLSCert` empty and cookies un-`Secure`. Now: cookies carry `Secure`
  when `-tls-cert` is set, `-base-url` starts with `https://`, or the new
  `-secure-cookies` flag is passed (`Server.secureCookies`). Proxy-based
  deploys should pass `-secure-cookies`; `install.sh` templates it via
  `XXD_SECURE_COOKIES=1`.
- **FIXED — non-loopback bind without TLS was possible.** The server now
  refuses to start on a non-loopback address without `-tls-cert/-tls-key`
  unless `-i-know` is passed (§3); `addrIsLoopback` resolves hostnames
  and fails closed.
- **FIXED — username enumeration via login timing.** Nonexistent
  usernames now burn an equivalent PBKDF2 computation (`dummyHash`) so
  timing no longer distinguishes "no such user" from "wrong password";
  covered by a hardening test.
- **FIXED — unbounded in-memory share-grant map.** Grants are capped at
  1024 entries with soonest-expiring-first eviction, swept hourly by the
  janitor, and expired entries are dropped lazily on access.
- **FIXED — collision-prone baseline filename hash.** CLI sync baseline
  files are keyed by full sha256 of `<localDir>|<remoteDir>`.
- **INFO — no `LICENSE` file.** Private repo, cleanroom estate work; add
  one only if the repo ever leaves the estate. Deliberately not added.

Known-remaining gaps live in `todo.md`: P2 items (sync identity is still
size+mtime only, web UI polish, PWA cache rotation) and — most
importantly — the pending on-phone smoke list for the Android client.

## 6. The clients

| Client | Auth | Connects via |
|---|---|---|
| Web UI | Session cookie (`xxd_session`, HttpOnly, SameSite=Lax) set by `POST /api/auth/login` | Served by the same binary at `/`; every write goes through `authedMutating`'s same-origin CSRF check since it's cookie-based. |
| Linux CLI (`xxdrive-cli`) | Bearer token, saved to `~/.config/xxdrive/config.json` (mode `0600`) after `xxdrive login <url> <user>` | `Authorization: Bearer <token>` header on every request; exempt from the CSRF same-origin check (no ambient cookie). `X-Client: cli` on login labels the session `cli:<ip>` in the sessions list. |
| Android app | Bearer token from `LoginActivity`, stored in `EncryptedSharedPreferences` (`Session.kt`, AES256-GCM under an Android Keystore `MasterKey`; construction auto-recovers once by deleting a corrupt or legacy plaintext pref file). The token is also injected into the WebView as the `xxd_session` cookie before first load, so cookie-authenticated SPA requests work natively. | WebView hosts the server's own PWA at the configured base URL. Same-origin `GET/HEAD /api/*` WebView requests are additionally re-proxied with `Authorization: Bearer`; POST/XHR uploads ride the injected session cookie. Native bridges (file-picker upload, `DownloadManager` downloads with bearer+cookie headers, `PhotoUploadWorker` camera backup) call the same `/api/*` endpoints. `usesCleartextTraffic="true"` is on by default for LAN/dev testing — remove for a production HTTPS deployment. **On-phone smoke still pending** (§2.4). |

**Full CLI verb list** (`xxdrive help`; every verb maps to a documented
endpoint in `docs/API.md`):

```
login <url> <user>            whoami                        sessions [revoke-others]
logout                        ls / mkdir / rm / mv / cp     trash ls|restore|delete|empty
up [--if-match ETAG]          down                          versions ls|restore|get
share ls|create|revoke        search <query>                star <path> / starred
sync <localDir> <remoteDir>   watch <localDir> <remoteDir> [-interval N]
```

Notable behaviors: `up` defaults to overwrite+version (prior content is
snapshotted server-side); `--if-match` fails with 412 on etag mismatch;
`rm` soft-deletes to server trash; `share create` supports
`--no-download --password PW --expires-days N`; conflict-copy naming and
the sync engine are described in §2.3.

All three clients hit the same frozen v1 API contract in
`docs/API.md`. Full endpoint list (auth, files, trash, versions,
search/stars/events, shares, admin, `/healthz`) is documented there and
was spot-checked against `internal/api/server.go`'s `routes()` while
writing this manual — the two agree.

## 7. Running on skippy-tel-network

xx-drive is stood up on the skippy-tel-network fabric, single-node on
Dutchman, via the systemd unit in `deploy/` with its data on the ZFS pool
at `/srv/deep/xx-drive`:

- **Binary/entrypoint:** `cmd/xxdrive-server` → single static binary,
  cross-compilable (`CGO_ENABLED=0`, pure-Go SQLite driver) — a good fit
  for the estate's existing build lanes.
- **Port:** pick a fabric port (default `8080`) and **bind
  `127.0.0.1`** — the server itself now enforces this by refusing a
  non-loopback bind without TLS (§3). Front it with the fabric's TLS
  proxy the same way other add-ons are fronted and pass `-secure-cookies`
  so session cookies get `Secure` (the server does not trust
  `X-Forwarded-Proto`); see `deploy/Caddyfile.example` /
  `deploy/nginx.conf.example` for reference configs.
- **Health:** `GET /healthz` → `200 "ok"`, no auth. Remember this is
  `/healthz` (radio's convention), **not** `/api/health` (jal's).
  Unauthenticated by design — if you care about fingerprinting, have the
  proxy restrict `/healthz` to private ranges (both example configs do).
- **Data dir:** `/srv/deep/xx-drive` on the ZFS pool, same precedent as
  jal's data dir — `xxdrive.db` (SQLite WAL, metadata only, rebuildable
  except users/sessions) plus `files/ trash/ versions/ tmp/` (plain
  files, rsync-friendly, so it also plays nicely with the estate's
  Synology backup mesh per the federated-backup doctrine).
- **First run:** capture the one-time admin password from the service
  log (or set `XXD_ADMIN_PASSWORD` up front) and change it via the web
  UI's admin panel immediately.
- **Background work:** the in-process hourly janitor (§3) needs no
  external cron.
- **If wired as a Skippy `AddonSpec`:** declare
  port/entrypoint/health/`data_dir` there **and** update the
  constellation `AGENTS.md` in the same change, per the estate's
  cross-repo contract rule at the top of that file. As of this manual
  xx-drive is validated and documented; wiring it as an add-on is the
  remaining integration step.

## 8. Troubleshooting

- **Server won't start / "create data dir" fatal.** The process needs
  write permission on the `-data`/`XXD_DATA_DIR` path (it calls
  `os.MkdirAll(dataDir, 0o700)`); under systemd this is
  `/srv/deep/xx-drive`, owned by the `xxdrive` service user created by
  `install.sh`.
- **Lost the one-time admin password.** Reset it with the built-in
  recovery mode (§3): stop the unit, run
  `sudo -u xxdrive xxdrive-server -data /srv/deep/xx-drive -passwd <user>`,
  enter the new password twice (no echo), start the unit again. The
  journal scrape (`journalctl -u xxdrive | grep "created admin user" -A4`)
  still works if the original password hasn't scrolled out of the log.
- **401 on every request from a client that was previously logged in.**
  Sessions expire after 30 days of inactivity (sliding), or were
  revoked (`POST /api/auth/sessions/revoke-others`), or the account was
  disabled by an admin (`SetDisabled` also deletes all of that user's
  sessions immediately). Re-run `xxdrive login` / re-authenticate in the
  app.
- **429 "too many failed logins."** Rate limit is 10 failures per
  IP+username per 15-minute rolling window (`loginAllowed`/
  `loginFailed` in `internal/api/server.go`); wait out the window or
  check the username being typed.
- **403 "cross-origin request rejected" from the web UI.** This is the
  CSRF same-origin guard (`authedMutating`/`sameOrigin`) firing on a
  cookie-authenticated mutating request whose `Origin`/`Referer` doesn't
  match `r.Host`. Usually means the app is being accessed through a
  proxy/hostname mismatch (e.g. `-base-url` doesn't match how the proxy
  actually presents the host) — check the fabric proxy's forwarded host
  handling.
- **412 "etag mismatch: file changed elsewhere" on upload.** Expected
  optimistic-concurrency behavior when an `If-Match` header was sent and
  the server-side content changed since the client last read it (someone
  else, or another device, wrote first). Re-fetch and retry, or use
  `conflict=rename` if the intent is to keep both.
- **CLI sync produces unexpected `(conflict from ...)` copies.** This is
  working as designed whenever the same path changed on both sides
  since the last successful sync — both versions are kept deliberately,
  never overwritten; both sides then converge on the canonical remote
  content at the original path (§2.3). Check
  `~/.config/xxdrive/sync-<sha256>.json` (the baseline file) if a sync
  partner appears to have "forgotten" prior state — deleting it forces a
  fresh compare against current local+remote state on the next run (any
  files identical on both sides will not re-conflict; anything genuinely
  diverged will surface as a conflict copy once). Files that vanished
  remotely land in `<localDir>/.xxdrive-trash/<timestamp>/`, not the
  void.
- **Camera auto-backup isn't uploading (Android).** Check
  Settings → *Auto-upload camera photos* is enabled, the network
  constraint (Wi-Fi-only by default) is met, and the app is exempted
  from aggressive OEM battery optimization — `android/README.md` calls
  this out as a known platform limitation, not a bug in the worker
  itself.
- **Trash / versions not being cleaned up.** The janitor runs hourly,
  not on every request — an item won't disappear the instant it crosses
  `-trash-days`, only on the next hourly tick (plus it always runs once
  immediately at server startup).

## 9. Part of the Skippy constellation

This repo is part of the Skippy estate — see the [Skippy manual](https://github.com/PiercingXX/Skippy/blob/main/docs/MANUAL.md) for the constellation overview and cross-repo contracts.
