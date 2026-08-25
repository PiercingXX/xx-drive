# xx-drive — Operator/Developer Manual

xx-drive is self-hosted private cloud storage: one static Go binary that
serves an HTTP API, an embedded web UI, and a Linux CLI sync client, plus
a separate Android app source project. Files live as plain files on disk
(rsync-friendly); all metadata lives in SQLite. This manual documents
**verified behavior only** — read directly from this repo's source at
commit `e122f08` ("Initial commit: xx-drive self-hosted private cloud
storage") and cross-checked against `README.md` and `docs/API.md` while
writing it. Where a claim could not be verified this manual says so
explicitly. If this manual and the code disagree, the code wins — file a
fix.

## 1. What xx-drive is & its role

xx-drive is the Skippy estate's **self-hosted private cloud storage**
add-on: a Dropbox/Synology-Drive-class personal file store, built from
cleanroom behavioral specs of Dropbox and Synology Drive plus a survey of
Nextcloud/Seafile/Syncthing-class products — without the operational
weight of a PHP/container stack (`README.md`).

- **Just validated.** Cloned, built, tested, and live-smoke-tested by an
  independent audit on 2026-08-25; verdict **SOUND**, no blockers. See
  `AUDIT-xxdrive.md` (audit scratch artifact, not part of this repo) for
  the full write-up; this manual folds its verified facts in throughout.
- **Estate placement.** Per the constellation map
  (`/media/Working-Storage/GitHub/Skippy-Project/Notes.md`), xx-drive will
  run **on the skippy-tel-network fabric**, single-node on Dutchman for
  now. It is not yet wired into an `AddonSpec` — see §7.
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

Vanilla-JS SPA, no build step: `internal/webfs/static/app.js` is 948 LOC
that exercises every `/api/*` endpoint (files, trash, versions, shares,
search, stars, events, admin) plus `index.html`, `style.css`,
`manifest.webmanifest`, and `sw.js` (app-shell caching only — the service
worker never caches API responses, per `docs/API.md`). Installable as a
PWA on Android/desktop Chrome. Served directly by the Go binary; no
separate frontend deploy step.

### 2.3 Linux CLI sync engine

`cmd/xxdrive-cli` (`xxdrive-cli` binary) does simple one-shot commands
(`login`, `ls`, `mkdir`, `rm`, `mv`, `cp`, `up`, `down`) plus a genuine
**two-way sync engine** (`cmd/xxdrive-cli/sync.go`):

- **Baseline-based three-way reconcile.** Each sync pair keeps a JSON
  baseline file (`~/.config/xxdrive/sync-<hash>.json` by default) mapping
  remote-relative paths to `{size, mtime}` as of the last completed sync.
  Every `sync`/`watch` pass computes `local`, `remote`, and `baseline`
  snapshots and walks the **union** of all three path sets.
- **Per-path classification:** local-only changed → push (upload, or a
  local delete propagates as a remote delete); remote-only changed →
  pull (download, or a remote delete propagates locally); changed on
  **both** sides → conflict: both versions are kept, never overwritten —
  the local copy is pushed as `name (conflict from HOST TIME).ext` and
  the server's copy is pulled down as `name (conflict from server
  TIME).ext`. Same-content changes on both sides collapse to a baseline
  update with no transfer.
- **Atomic transfers.** Downloads (`pullTo`) stream to `<file>.xxpart` and
  `os.Rename` into place only on success; a partial/interrupted transfer
  never corrupts the destination. `walkLocal` and the server's directory
  listing both skip `.xxpart`/`.xxdrive-*` in-progress artifacts so they
  never get treated as real content.
- **`watch`** just loops `syncOnce` every `-interval` seconds (default
  30) until interrupted.
- Known cosmetic weakness (audit INFO, not a security issue): the
  baseline filename hash (`basePathFor`) is a hand-rolled `byte*31+i`
  fold, not a real hash function — collision-prone name derivation that
  could in theory collide two different sync pairs' state files onto the
  same filename. Not exploitable, just fragile; a real hash (e.g.
  `sha256`) would be a safe drop-in fix if ever revisited.

### 2.4 Android app

`android/` is a **buildable Kotlin source project** (Android Studio,
Gradle) — no prebuilt APK ships in the repo, by design. Five real
classes, 434 LOC total (per audit):

- `LoginActivity` — server URL + username/password entry.
- `MainActivity` — hosts the server's own PWA in a `WebView`, and
  bridges the two things a plain WebView can't do: native file-picker
  uploads (`onShowFileChooser` → `ActivityResultContracts`) and
  authenticated downloads via the system `DownloadManager`.
- `PhotoUploadWorker` — a `CoroutineWorker` (WorkManager) camera
  auto-backup job. Uploads new MediaStore images to
  `/Camera Uploads/<yyyy-MM-dd>/<name>` using the server's
  `conflict=rename` upload mode (never overwrites), tracked via a
  `last_photo_ts` watermark in `SharedPreferences` that only advances
  once at least one file in a batch succeeds (so failures retry next
  run). Configurable to Wi-Fi-only, runs roughly every 30 minutes per
  `android/README.md`.
- `Session` — tiny credential store (`SharedPreferences`); the app's own
  `README.md` flags this as a hardening TODO ("switch to
  `EncryptedSharedPreferences` with a `MasterKey`" — not yet done).
- `SettingsActivity` — the auto-upload toggle and related prefs.

`AndroidManifest.xml` currently sets `android:usesCleartextTraffic="true"`
so LAN/HTTP testing works out of the box; the app's own README says to
remove that attribute for a production HTTPS deployment.

### 2.5 Data model & on-disk layout

Metadata lives in one SQLite (WAL mode) database,
`<data-dir>/xxdrive.db`, opened with
`_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)`
(`internal/store/store.go`). Tables: `users`, `sessions`, `shares`,
`stars`, `events`, `versions`, `etag_cache`. Every table **except
`users`/`sessions`** is rebuildable from the plain-file layout — the
audit calls this out explicitly.

File content itself never touches SQLite. Under the data root:

```
<data-dir>/
  xxdrive.db SQLite WAL metadata (users/sessions/shares/stars/events/versions/etags)
  files/<username>/... each user's drive tree — plain files, rsync-friendly
  trash/<username>/<id>/ soft-deleted payload (file or whole directory)
  trash/<username>/<id>.json {origPath, name, isDir, deletedAt}
  versions/<user>/<sha256(logicalPath)>/<versionID> prior content of a file (max 32 kept per path)
  tmp/ staging dir for atomic uploads (.xxpart-style temp + rename)
```

`internal/fsdrv.New` creates `files/ trash/ versions/ tmp/` under the
(symlink-resolved) data root on startup if missing.

## 3. How it's run

**Binary / entrypoint:** `cmd/xxdrive-server` builds to `xxdrive-server`
(pure Go, CGO-free via modernc's SQLite driver — `deploy/install.sh`
builds with `CGO_ENABLED=0`, correct for that driver). The CLI is a
separate binary, `cmd/xxdrive-cli` → `xxdrive-cli`.

```bash
go build -o xxdrive-server./cmd/xxdrive-server
go build -o xxdrive-cli./cmd/xxdrive-cli
```

**Port / bind:** default listen address is `:8080` (all interfaces) —
that is what the README's dev quick-start uses. **The production systemd
unit binds `127.0.0.1:8080`** (`deploy/xxdrive.service`), which is what
this estate should always use; see §5 for why the README's `:8080`
example matters for security.

**Health:** `GET /healthz` → HTTP 200, body `ok` (plain text, no auth
required, registered first in `routes()`). **This is `/healthz`,
matching radio's convention — NOT jal's `/api/health`.**

**Data dir:** `-data` flag / `XXD_DATA_DIR` env, default `./data` for a
dev run. The systemd unit uses `/var/lib/xxdrive`. Per the constellation
map's precedent (jal's data dir), xx-drive's data dir should live on the
**ZFS pool**, not the install directory, once it's actually stood up —
this manual does not assert that placement has happened yet.

**systemd unit** (`deploy/xxdrive.service`): runs as a dedicated
`xxdrive` system user, `StateDirectory=xxdrive`, binds
`127.0.0.1:8080`, `Restart=on-failure` / `RestartSec=5`. Hardening flags
present: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`,
`PrivateTmp`, `ReadWritePaths=/var/lib/xxdrive` (only),
`ProtectKernelTunables`, `ProtectControlGroups`, `RestrictSUIDSGID`,
`MemoryDenyWriteExecute`, `LockPersonality`.

**Install script** (`deploy/install.sh`, run as root):
`./install.sh https://drive.example.com` — builds both binaries (or uses
a prebuilt `xxdrive-server` in the repo root if no `go` toolchain is
present), creates the `xxdrive` system user, installs binaries to
`/usr/local/bin/`, templates the base URL into the systemd unit, and
enables+starts the service. Its top comment says "requires Go>= 1.22";
`go.mod` actually pins `go 1.25.0` — a cosmetic inconsistency noted by
the audit, not a build blocker (the script's `CGO_ENABLED=0 go build`
works fine with 1.25 installed).

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

There is **no hardcoded default credential** — the audit confirmed this
explicitly.

**Hourly janitor:** an in-process goroutine (`startJanitor` in
`main.go`) runs once at startup and then every hour via `time.Ticker`,
doing three things: (1) purge trash items older than `-trash-days` via
`fs.PurgeOldTrash`; (2) prune version history to the newest 32 per
`(user, path)` via `st.PruneVersions(32)`, removing the pruned blobs
from disk; (3) sweep stale upload temp files in `tmp/` older than 24
hours. No external cron is needed — this covers what jal's own
self-restarting crawl does for a different kind of background work.

## 4. Configuration & env vars

All flags read from an env var first if the flag isn't passed
(`envOr` helper in `main.go`), so either style works.

| Flag | Env var | Default | Notes |
|---|---|---|---|
| `-addr` | `XXD_ADDR` | `:8080` | Listen address. Use `127.0.0.1:PORT` for any networked deploy. |
| `-data` | `XXD_DATA_DIR` | `./data` | Data directory (created `0o700` if missing). |
| `-base-url` | `XXD_BASE_URL` | `""` | Public base URL, used to build absolute share links (`shareURLPrefix`). |
| `-max-upload-mb` | *(flag only)* | `10240` (10 GiB) | Max upload size in MB; enforced via `LimitReader`/`MaxBytesReader` in `fsdrv.Upload`. |
| `-trash-days` | *(flag only)* | `30` | Trash retention before the janitor purges an item. |
| `-tls-cert` | `XXD_TLS_CERT` | `""` | TLS cert path. If both cert+key are set, the server calls `ListenAndServeTLS` directly. |
| `-tls-key` | `XXD_TLS_KEY` | `""` | TLS key path. |
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

The audit's overall read: "notably careful for an initial commit — the
author clearly designed around the OWASP file-storage threat set," and a
live smoke test on an ephemeral instance confirmed auth enforcement (401
on unauthenticated `/api/files/list`), correct login/logout, traversal
blocking (both raw `../` and percent-encoded `%2f`/`%2e` forms → 404),
and wrong-password rejection (401). No auth bypass, path traversal,
injection, committed secret, or unsafe default credential was found.

- **Path safety — single choke point.** Every request-derived path must
  pass through `fsdrv.ResolveUserPath` (`internal/fsdrv/fsdrv.go`)
  exactly once. It: rejects NUL bytes and backslashes outright; lexically
  cleans and anchors the path (`filepath.Clean("/" + rel)`); rejects any
  percent-encoded `%2f`/`%5c`/`%2e` (net/http already decodes once, so
  these would be literal characters — forbidden anyway as a
  double-decode-confusion defense); enforces a containment check
  (`strings.HasPrefix(abs, userRoot+sep)`) against the lexical join; and
  then **walks every existing path component with `os.Lstat`, refusing
  any component that is a symlink** — so content planted via out-of-band
  shell access cannot be used to smuggle a request outside the user
  root. Backed by a traversal fuzz corpus test (`TestTraversalCorpus`)
  and a dedicated symlink-escape test (`TestSymlinkEscape`) in
  `internal/fsdrv/fsdrv_test.go`. Public share `sub` params are
  re-anchored through the same `ValidateRel` (`joinSub` in `shares.go`),
  so an anonymous share visitor cannot climb above the shared subtree.
- **Passwords.** PBKDF2-HMAC-SHA256, 600,000 iterations (OWASP 2023
  guidance), 16-byte random salt, stored as
  `pbkdf2-sha256$<iter>$<salt-hex>$<dk-hex>`
  (`internal/store/store.go`). Comparison uses
  `subtle.ConstantTimeCompare`.
- **Sessions & tokens.** 256-bit random session tokens; **stored hashed
  (SHA-256) server-side** — the raw token only ever exists in the
  client's cookie or bearer header. Sliding 30-day expiry (refreshed on
  use, throttled to avoid a write on every request). `SetDisabled(...,
  true)` kills all of a user's sessions immediately. Share tokens follow
  the same hashed-at-rest pattern (24-byte random, base64url capability
  tokens) and are never logged (`logReq` explicitly skips the query
  string for any `/s/` path).
- **CSRF.** Cookie-authenticated *mutating* requests (`authedMutating`)
  require a same-origin `Origin` or `Referer` header — absent both is
  rejected with 403. Bearer-token clients (CLI, Android) are exempt
  because they carry no ambient browser credential. No state change ever
  happens on a GET. Mutations are additionally serialized per-user via a
  `sync.Map` of mutexes.
- **SQL.** Every query in `internal/store/store.go` is parameterized;
  `LIKE` patterns used for path-prefix matching escape `% _ \` via
  `EscapeLike`. No injection surface found by the audit.
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
  Admins cannot disable their own account (`handleAdminSetState`
  guards `target.ID == UserFrom(r).ID`).
- **No secrets committed**; `.gitignore` covers `/data/`, both binaries,
  Android build output, and `local.properties`.

### Findings carried forward from the audit — address before/while networking this

- **LOW — cookie `Secure` flag depends on in-process TLS.**
  `handleLogin` sets `Secure: s.cfg.TLSCert != ""`. The recommended
  topology (TLS-terminating reverse proxy in front, app plain-HTTP
  behind it) leaves `TLSCert` empty, so the session cookie is **not**
  marked `Secure` even though the browser only ever sees HTTPS.
  **Action for skippy-tel-network:** either run the app with
  `-tls-cert/-tls-key` too, or add a config flag / trust an
  `X-Forwarded-Proto` header from the fabric proxy to force `Secure`
  when fronted by TLS. Not yet fixed in the code as of this manual.
- **LOW — confirm `127.0.0.1` bind before networking.** The README's
  top quick-start example binds `:8080` (all interfaces) with no TLS;
  the systemd unit already correctly binds `127.0.0.1:8080`. Any
  fabric deployment must use the systemd-style bind, never the raw
  quick-start flags, and front it with the fabric's TLS proxy.
- **LOW — username enumeration via login timing** (not itemized as a
  networking blocker but worth knowing): `handleLogin` skips the
  expensive PBKDF2 check entirely when the username doesn't exist, so a
  nonexistent user returns near-instantly versus ~400ms for a real one
  with a wrong password. Rate limiting (10 fails / 15 min / IP+user,
  `loginAllowed`/`loginFailed`) partially mitigates. Not fixed as of
  this manual.
- **LOW — unbounded in-memory share-grant map.** `s.pubGr` accumulates
  password-grant entries per successful share-password submission,
  pruned only opportunistically on access, cleared on restart. Not
  attacker-amplifiable in practice; cosmetic.
- **INFO — no `LICENSE` file.** Private repo, treated as cleanroom
  estate work; the audit flags this as something to add only if the
  repo ever leaves the estate. **This manual does not add a LICENSE
  file** — that decision is left to the operator.
- **INFO — dead constant.** `csrfHeader = "X-Requested-With"`
  (`internal/api/server.go:21`) is declared but unused; CSRF actually
  relies on Origin/Referer checking, not this header. Cosmetic.

## 6. The clients

| Client | Auth | Connects via |
|---|---|---|
| Web UI | Session cookie (`xxd_session`, HttpOnly, SameSite=Lax) set by `POST /api/auth/login` | Served by the same binary at `/`; every write goes through `authedMutating`'s same-origin CSRF check since it's cookie-based. |
| Linux CLI (`xxdrive-cli`) | Bearer token, saved to `~/.config/xxdrive/config.json` (mode `0600`) after `xxdrive login <url> <user>` | `Authorization: Bearer <token>` header on every request; exempt from the CSRF same-origin check (no ambient cookie). `X-Client: cli` on login labels the session `cli:<ip>` in the sessions list. |
| Android app | Bearer token, entered via `LoginActivity`, stored in plain `SharedPreferences` (`Session.kt`) — **hardening TODO, not yet done**: switch to `EncryptedSharedPreferences` | WebView hosts the server's own PWA at the configured base URL; native bridges (file picker upload, `DownloadManager` download, `PhotoUploadWorker` camera backup) call the same `/api/*` endpoints with the bearer token. `usesCleartextTraffic="true"` is on by default for LAN/dev testing — remove for a production HTTPS deployment. |

All three clients hit the same frozen v1 API contract in
`docs/API.md`. Full endpoint list (auth, files, trash, versions,
search/stars/events, shares, admin, `/healthz`) is documented there and
was spot-checked against `internal/api/server.go`'s `routes()` while
writing this manual — the two agree.

## 7. Running on skippy-tel-network

Not yet stood up — this section documents the shape, per the audit and
the source, not a completed deployment.

- **Binary/entrypoint:** `cmd/xxdrive-server` → single static binary,
  cross-compilable (`CGO_ENABLED=0`, pure-Go SQLite driver) — a good fit
  for the estate's existing build lanes.
- **Port:** pick a fabric port (default `8080`) and **bind
  `127.0.0.1`** — never the README's `:8080`-all-interfaces quick-start
  form. Front it with the fabric's TLS proxy the same way other add-ons
  are fronted, and either terminate TLS on the app too or add the
  `X-Forwarded-Proto` trust fix (§5) so session cookies get `Secure`.
- **Health:** `GET /healthz` → `200 "ok"`, no auth. Remember this is
  `/healthz` (radio's convention), **not** `/api/health` (jal's).
- **Data dir:** put it on the ZFS pool, same precedent as jal's data
  dir — `xxdrive.db` (SQLite WAL, metadata only, rebuildable except
  users/sessions) plus `files/ trash/ versions/ tmp/` (plain files,
  rsync-friendly, so it also plays nicely with the estate's Synology
  backup mesh per the federated-backup doctrine).
- **First run:** capture the one-time admin password from the service
  log (or set `XXD_ADMIN_PASSWORD` up front) and change it via the web
  UI's admin panel immediately.
- **Background work:** the in-process hourly janitor (§3) needs no
  external cron.
- **If wired as a Skippy `AddonSpec`:** declare
  port/entrypoint/health/`data_dir` there **and** update the
  constellation `Notes.md` in the same change, per the estate's
  cross-repo contract rule at the top of that file. Neither has
  happened yet as of this manual — xx-drive is validated and documented
  but not yet an add-on.

## 8. Troubleshooting

- **Server won't start / "create data dir" fatal.** The process needs
  write permission on the `-data`/`XXD_DATA_DIR` path (it calls
  `os.MkdirAll(dataDir, 0o700)`); under systemd this is
  `/var/lib/xxdrive`, owned by the `xxdrive` service user created by
  `install.sh`.
- **Lost the one-time admin password.** There is no recovery command in
  this codebase — the audit and source agree there's no backdoor reset.
  Under systemd: `journalctl -u xxdrive | grep "created admin user" -A4`
  (only useful if it hasn't scrolled out of the journal). Otherwise an
  operator with filesystem access would need to insert a new admin row
  directly into `xxdrive.db` via `sqlite3` using
  `store.HashPassword`'s format (`pbkdf2-sha256$600000$<salt-hex>$<dk-hex>`)
  — there is no built-in CLI for this.
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
- **CLI sync produces unexpected `(conflict from...)` copies.** This is
  working as designed whenever the same path changed on both sides
  since the last successful sync — both versions are kept deliberately,
  never overwritten. Check `~/.config/xxdrive/sync-<hash>.json` (the
  baseline file) if a sync partner appears to have "forgotten" prior
  state — deleting it forces a fresh compare against current
  local+remote state on the next run (any files identical on both sides
  will not re-conflict; anything genuinely diverged will surface as a
  conflict copy once).
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
