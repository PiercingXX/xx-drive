# xx-drive — ship backlog

Sourced from a full-tree review of the running code (server, store, fsdrv, web UI, CLI, Android, deploy), not from README claims. `go test ./...` and `go vet` are green; Android unit tests only cover the theme-sync receiver. That is not the same as ship-ready.

**Verdict today: not good to go.** Do not put real files behind this until P0 is done and the smoke list at the bottom is green.

> **2026-08-26 status:** all P0 code fixes landed with regression tests (`go test -count=1 ./...`, `go vet`, `gofmt`, `./gradlew testDebugUnitTest` all green; live-server curl smoke of upload/conflict/version/rename/trash/share flows passed). A post-fix review pass found and fixed 2 HIGH + 3 MEDIUM follow-ups. What still blocks "good to go" is exactly what needs hardware/browsers: the phone smoke list below and a browser click-through of the SPA changes.

Work top to bottom. Check a box only when the code is fixed *and* there is a test (or a phone/browser smoke) that would have caught the original bug.

---

## P0 — data loss

These can destroy or silently replace user files.

- [x] **CLI two-way sync clobbers local edits.** Local-only uploads send `conflict=rename`, then the baseline is recorded as if the remote canonical path matched. Next pass treats remote as the authority and `pullTo` overwrites the local edit. The both-changed branch has the same baseline bug (`base[rp] = r` while local content stays at `rp`).
  - `cmd/xxdrive-cli/sync.go` (~209, ~200)
  - Fix: local-only updates must overwrite (server snapshots a version). True conflicts must leave both sides identical at `rp` after copies are saved, then write *that* state into the baseline.
  - Tests: three-way matrix (local-only, remote-only, both-changed, both-deleted, same size/mtime). There are currently **zero CLI tests**. → DONE: full matrix in `cmd/xxdrive-cli/sync_test.go` against a real httptest server.

- [x] **CLI `up` never overwrites.** Always sends `conflict=rename`, so replacing a remote file leaves the original in place and writes a sibling. Combined with the web UI doing the same, first-party clients almost never hit the overwrite+version path the server tests cover.
  - `cmd/xxdrive-cli/main.go` (~358)
  - Default `up` to overwrite+version; keep `conflict=rename` for camera backup and true sync conflicts only.

- [x] **Trash is not atomic.** `Delete` renames the payload into `trash/<user>/<id>` *then* writes `<id>.json`. If the JSON write fails, the original path is gone, `ListTrash` ignores payloads without JSON, and the janitor only walks `.json` files. Content is stranded until someone inspects the disk.
  - `internal/fsdrv/fsdrv.go` (~569)
  - Write metadata first (or a temp sidecar), then rename; roll back on failure. Or store both in one directory and rename once. → sidecar protocol with rollback + failure-injection test.

- [x] **Janitor treats corrupt trash metadata as expired.** `PurgeOldTrash` `RemoveAll`s the payload when JSON unmarshal fails *or* `DeletedAt == 0` (Unix epoch). A glitch permanently destroys restoreable trash.
  - `internal/fsdrv/fsdrv.go` (~712)
  - Skip + log on unmarshal failure. Purge only when `DeletedAt` is a plausible timestamp strictly older than cutoff. → tested.

- [x] **Version history does not follow rename/move.** Versions are keyed by logical path in SQLite and stored under `versions/<user>/<pathHash(logical)>/`. After rename, `GET /api/versions?path=<new>` is empty; the old path’s index still points at blobs `OpenVersion` will not find for the new path.
  - `internal/api/handlers.go` (~417)
  - Relocate version index rows and `pathHash` directories on rename/move (including directory prefixes). Test: overwrite → rename → list versions → restore. → done, incl. dir-prefix moves; live-smoke verified.

- [x] **Trashing a file drops the version index immediately.** Restore brings back current bytes only; history is gone (blobs remain as unreferenced files until disk fills).
  - `internal/api/handlers.go` (~476)
  - Keep version rows/blobs until the trash item is purged. Relocate them on restore. → done; ownership recorded at delete time so purge/restore never touch a recreated file's history (tested).

- [x] **Interrupted downloads become real files.** CLI stages to `<file>.xxpart`. `walkLocal` only skips a *directory* named `.xxpart` or names prefixed `.xxdrive-`. A leftover `photo.jpg.xxpart` is uploaded. Server `List` skips `.xxpartial*` (different prefix), so neither side ignores the other.
  - `cmd/xxdrive-cli/sync.go` (~99, ~295)
  - Skip suffix `.xxpart` and prefix `.xxpartial` in `walkLocal`, `List`, and `WalkTree`. Delete stale `.xxpart` after a successful pull of the same path. → both sides skip both patterns; stale `.xxpart` cleaned after pulls.

- [x] **CLI remote-delete of a local file is a hard delete.** When the remote side is gone, sync does `os.Remove` locally — not trash, not a recycle bin. A bad baseline (see first item) can delete the user’s only copy.
  - `cmd/xxdrive-cli/sync.go` (~242)
  - At minimum, send local deletes to OS trash / a `.xxdrive-trash` folder, or refuse to delete local files unless `-allow-delete` is set. Never hard-delete on a first sync with an empty/corrupt baseline. → deletes go to `.xxdrive-trash/` AND require a baseline entry; tested.

---

## P0 — isolation and path safety

- [x] **Username `.` / `..` escapes the per-user tree.** `CreateUser` rejects `/`, `\`, and NUL but allows `.` and `..`. `ResolveUserPath` does `filepath.Join(d.root, "files", username)`, so `..` cleans to the data root. An admin-created user (or `XXD_ADMIN_USER=..` on first boot) can list `xxdrive.db`, `files/`, `trash/`, and other users’ trees, then download them.
  - `internal/store/store.go` (~219)
  - `internal/fsdrv/fsdrv.go` (~127)
  - Require a single safe segment (reject `.`, `..`, empty, and anything `filepath.Clean` would rewrite). Same check on fabric ids before prefixing `fabric_`. Assert containment against the *pre-clean* `files/<username>` directory. → done at both layers + store tests.

- [x] **Zip / public folder download follows planted symlinks.** `ResolveUserPath` / `OpenFile` refuse symlink *components* (`TestSymlinkEscape`). `handleZip` / `zipShared` then `os.Open` the `WalkTree` absolute path, which follows a symlink-to-file. A link dropped in via rsync/shell/backup restore can be read out, including targets outside the data root. Directory copy already skips non-regular files; zip does not.
  - `internal/api/handlers.go` (~386)
  - `Lstat` each WalkTree path and skip/error on symlinks and specials before `os.Open`. Extend `TestSymlinkEscape` to zip and public download. → `fsdrv.OpenRegular` used by both zips; external+dangling link tests pass.

---

## P0 — public shares

File-share tests pass. Folder shares and the browser password flow do not.

- [x] **Password-protected share pages have no password field.** CSS styles `form`/`input`, `POST /s/{token}` implements grant cookies, HTML never contains a form. A browser user cannot unlock the link; only a scripted POST (as in `TestShareFlow`) works.
  - `internal/api/shares.go` (~227, template)
  - When `needPw` is true, render a POST form. Hide Download until a grant exists. → done; live-smoke: form renders, download 401 pre-grant, unlock then works. Password attempts now rate-limited (10/15min/IP+share).

- [x] **Public folder shares are broken.** `handlePublicDownload` reads `sub` and ignores `path`, but HTML and `docs/API.md` emit `?path=<full logical path>`. Clicking a file zips the whole share (or 400s). Directory links go to `/s/{token}/list?sub=<absolute path>` (JSON, not the HTML page). `joinSub` concatenates share root + validated sub, so `sub=/photos/vacation` on share `/photos` becomes `/photos/photos/vacation`. There is no “requested path is under `sh.Path`” check — any `path`/`sub` fix *must* add that or `?path=/other` becomes a share escape.
  - `internal/api/shares.go` (~444, ~joinSub)
  - Resolve `path` or `sub` to a logical path, require `full == sh.Path || strings.HasPrefix(full, sh.Path+"/")`, and point HTML entries at `/s/{token}?sub=<path relative to the share>`.
  - Test: share a directory, list a child, download a child file, reject a path outside the share. → all four in `shares_test.go`; escape smoke-tested.

- [x] **View-only shares are bypassed with `inline=1`.** `allowDownload=false` rejects a normal download, then allows any request with `inline=1`. `serveFileDownload` streams the whole file; there is no image/video check. Adding `&inline=1` recovers a “view-only” zip, pdf, or document. `docs/API.md` documents this as preview-only; the code does not enforce it.
  - `internal/api/shares.go` (~448)
  - When `AllowDownload` is false, allow inline only for a small image/video MIME allowlist. Everything else 403. → tested + smoke-verified (txt inline → 403).

- [x] **Copied share URLs double the origin.** `handleShareList` already returns an absolute URL when `-base-url` is set (`deploy/install.sh` always sets it). The shares table and the existing-links dialog do `location.origin + sh.url`, producing `https://drive.example.comhttps://drive.example.com/s/...`. Create-share (uses `r.token`) is fine; the list/revoke copy path used after the first create is not.
  - `internal/webfs/static/app.js` (~580, ~835)
  - If `sh.url` is already `http://`/`https://`, copy it as-is. Prefer one canonical absolute URL from the API. → SPA helper + API canonical URL asserted by test.

---

## P0 — Android actually works

The app installs, launches, and draws the login screen. That is the entire list of what is proven on a phone. The gaps below are missing wiring, not missing tests.

- [x] **Native login never authenticates the WebView.** `LoginActivity` stores a bearer token in `Session`. `MainActivity` loads `Session.baseUrl + "/"` with no `CookieManager.setCookie`, no `shouldInterceptRequest` Authorization header, and no token injection. The SPA `boot()` calls `GET /api/auth/me` with cookies only, gets 401, and shows the web login form again. Native file-picker uploads go through that same cookie-only XHR, so they fail too.
  - `android/.../MainActivity.kt` (~90)
  - `internal/webfs/static/app.js` (~95)
  - After login, set `xxd_session=<token>` on the server origin. Confirm `/api/auth/me` succeeds in the WebView *before* first paint of the file list. → `CookieManager.setCookie` callback + 300ms fallback before `loadUrl`. A Bearer `shouldInterceptRequest` proxy was tried and removed: OkHttp `.use` closed the body, and intercepting `/api/files/download` would buffer whole files. *Phone confirmation still pending (smoke list below).*

- [x] **Camera backup silently no-ops after process death.** `doWork` uses `Session.isLoggedIn` but never calls `Session.init`. There is no `Application` class. WorkManager can run the worker without an Activity; `isLoggedIn` is false and the worker returns `Result.success()`.
  - `android/.../PhotoUploadWorker.kt` (~36)
  - `android/.../Session.kt` (~12)
  - Call `Session.init(applicationContext)` at the start of `doWork`. Better: a custom `Application` that inits Session once. → both done (`XxDriveApp` + idempotent init).

- [x] **Camera backup watermark skips failed photos forever.** `KEY_LAST_TS` is set to `System.currentTimeMillis()` if *any* file in the batch succeeds. Later failures with older `DATE_TAKEN` are never retried. Comment in the worker claims the opposite.
  - `android/.../PhotoUploadWorker.kt` (~57)
  - Advance the watermark to the last successfully uploaded `dateTaken`, only past files that actually uploaded. → advances only through the all-successful prefix of the batch; unit-tested incl. fail-first.

- [x] **Every download is saved as `Downloads/xx-drive` with no extension.** Subsequent downloads collide. `WRITE_EXTERNAL_STORAGE` is absent, so `setDestinationInExternalPublicDir` is unreliable on API 26–28 (`minSdk 26`).
  - `android/.../MainActivity.kt` (~82)
  - Parse `Content-Disposition` / URL path for a filename, fall back to a unique name, and use a destination that works on 26–28 without the missing permission. → RFC 6266/5987 parser (unit-tested), app-private Downloads dir.

- [x] **Auto-backup toggle never completes the permission grant.** Checking the box with no media permission reverts the checkbox and calls `requestPermissions`, but `SettingsActivity` has no `onRequestPermissionsResult`. After the user grants access, the box stays off and WorkManager is not scheduled until they toggle again.
  - `android/.../SettingsActivity.kt` (~43)
  - Enable and schedule on grant; show a reason if denied. → done.

Phone smoke required after the above (none of these have been done even once) — **this is now the main blocker for "good to go"**, along with a browser click-through of the SPA changes:

- [ ] Login with a real server → file list, not the web login form.
- [ ] Native picker upload of a photo → it appears in the listing.
- [ ] Download a named file → it lands in Downloads with the original name, twice without collision.
- [ ] Enable camera backup, kill the app, take a photo, wait for the worker → file at `/Camera Uploads/<date>/`.
- [ ] Logout, cold start, login again.

---

## P1 — web UI correctness

*(All items below implemented; verified by `node --check`, API-contract cross-checks, and code review. A browser click-through is still owed — see the definition of done.)*

- [x] **Conflict-copy uploads look like failures.** Server returns HTTP 201 for a conflict copy (`handlers.go` ~301). The SPA only treats `xhr.status === 200` as success, so a re-upload of an existing name is marked failed (`HTTP 201`) and the listing is not refreshed. Combined with always sending `conflict=rename` (~493), the UI never overwrites, so the versions dialog’s “Overwrite the file to create history” path is unreachable from the PWA.
  - `internal/webfs/static/app.js` (~477, ~493)
  - Treat 2xx as success and surface `conflicted`. Add an explicit Replace control that omits `conflict=rename`. → done (201 smoke-verified server-side).

- [x] **Search results do not open the file.** Clicking a hit navigates to the parent folder only. No preview, no download from the result row.
  - `internal/webfs/static/app.js` (~530) → hits open preview + per-row download.

- [x] **Drag-and-drop / file picker are files only.** No `webkitdirectory`, no folder tree from a dropped directory. Users cannot upload a folder from the browser. → `webkitdirectory` input + `webkitGetAsEntry` DnD recursion.

- [x] **No account / sessions UI.** `GET /api/auth/sessions` and `POST /api/auth/sessions/revoke-others` exist and are tested. The SPA never calls them. A stolen laptop cannot be kicked from the web UI.
  - `internal/api/server.go` (~88)
  - `internal/webfs/static/index.html` → Account modal with session list + revoke-others.

- [x] **No self-service password change.** Only an admin can set a password (`/api/admin/users/password`). There is no “change my password” for a normal user, including the bootstrap admin after first login (they must use the admin panel, which is easy to miss). → new `POST /api/auth/password` endpoint (tested) + SPA form.

- [x] **No fabric SSO entry in the login form.** `POST /api/auth/fabric` is implemented and tested. The web login card is username/password only. Estate SSO is dead for browsers until something posts a token here.
  - `internal/webfs/static/index.html` (~15)
  - `internal/api/handlers.go` (~93) → estate-token section added to the login card.

- [x] **Size column is not sortable.** Name and Modified are; Size is a dead `<th>`.
  - `internal/webfs/static/app.js` (~276) → numeric sort wired.

---

## P1 — CLI completeness and crashes

- [x] **Baseline filename can panic and can collide.** `basePathFor` folds the pair string into bytes and slices `fmt.Sprintf("%x", h)[:24]`. Any pair shorter than 12 characters panics (`sync /d /x` if those dirs exist). It is not a hash: different pairs can share one baseline file and silently cross-reconcile two jobs.
  - `cmd/xxdrive-cli/sync.go` (~77)
  - SHA-256 (or similar) of the pair, fixed-length hex name, never slice without a length check. → full 64-char sha256; tested.

- [ ] **`watch` is a 30s poll loop.** No `fsnotify`, no backoff, no single-flight if a pass overruns the interval. Fine for a LAN folder of hundreds of files; will hurt on a large tree.
  - `cmd/xxdrive-cli/sync.go` (~63) → *single-flight guard added (passes no longer stack); still an interval poll by design — fsnotify/backoff remain open.*

- [x] **Missing verbs the API already has.** No `logout`, `trash`/`restore`, `versions`, `share`, `search`, `star`, `whoami`, `sessions`. Users who live in the CLI cannot manage the safety nets they are told exist. → all added + integration-tested.

- [x] **No `If-Match` on upload.** Concurrent `up`/`sync` from two machines can clobber without 412. The server supports it; the CLI never sends it. → `up --if-match`; sync derives etags from baseline, 412 routes to conflict path; tested.

- [x] **Credentials are plaintext `~/.config/xxdrive/config.json` (mode 0600).** Acceptable for v1 on a single-user box; document it. Do not copy this file into backups that leave the machine. → documented in MANUAL.

---

## P1 — server hardening

- [x] **Session / share-grant cookies omit `Secure` behind a reverse proxy.** `Secure` is set only when `s.cfg.TLSCert != ""`. Production topology is systemd on `127.0.0.1:8080` behind Caddy/nginx (`deploy/xxdrive.service`), so in-process TLS is off and the cookie is not `Secure` even when the browser only sees HTTPS.
  - `internal/api/handlers.go` (~80)
  - `internal/api/shares.go` (~203)
  - Add `-secure-cookies` and/or trust `X-Forwarded-Proto` from the proxy; default Secure on when `BaseURL` is `https://`. → shared `secureCookies()` (TLS ∥ https BaseURL ∥ `-secure-cookies`); matrix-tested + smoke-verified.

- [x] **Login timing enumerates usernames.** Missing users skip PBKDF2 and return near-instantly; a real user with a wrong password costs ~400ms. Rate limit (10 fails / 15 min / IP+user) only partly covers this.
  - `internal/api/handlers.go` (~61)
  - Always run a dummy PBKDF2 on unknown users (or hash against a dummy record). → dummy-hash seam, deterministic test; disabled accounts burn cost too.

- [x] **Share-grant map is unbounded in-memory.** `s.pubGr` grows per successful password submit, pruned only on access, wiped on restart (so every reboot re-prompts). Fine for a family box; not fine if a share link is scraped.
  - `internal/api/shares.go` (~193)
  - Cap, TTL sweep in the janitor, or persist grants hashed in SQLite. → capped at 1024 w/ eviction + TTL sweep wired into the janitor.

- [x] **16-char share URLs scan every user’s shares.** `resolveShare` for a hash prefix calls `allLiveShares()` which lists every user. Personal-instance scale only.
  - `internal/api/shares.go` (~139, ~154)
  - Index by token-hash prefix. → in-memory prefix index, invalidated on create/revoke; revoke-then-resolve tested.

- [x] **Dead CSRF constant.** `csrfHeader = "X-Requested-With"` is declared and unused. CSRF is Origin/Referer only. Either use it as a second check or delete it.
  - `internal/api/server.go` (~22) → now used: cookie-authed mutations pass on same-origin OR absent-Origin+X-Requested-With; admin/logout/revoke-others routes moved under the mutating wrapper (cross-origin admin POST rejected by test).

- [x] **`HashPassword` / `newID` / bootstrap password ignore `rand.Read` errors.** `newID` panics; HashPassword and the generated admin password do not even check.
  - `internal/store/store.go` (~32)
  - `internal/fsdrv/fsdrv.go` (~917)
  - `cmd/xxdrive-server/main.go` (~108) → all checked; bootstrap aborts startup on entropy failure; `newToken` too.

- [x] **No graceful shutdown.** `ListenAndServe` is `log.Fatal`. In-flight uploads and the janitor are killed. systemd `Restart=on-failure` will loop this.
  - `cmd/xxdrive-server/main.go` (~80)
  - `http.Server.Shutdown` on SIGTERM, stop the janitor, wait for in-flight mutations. → done; binary smoke: SIGTERM drains, exit 0.

- [x] **Janitor and the API race on version blobs.** `PruneVersions` then `os.Remove` of blobs has no lock against a concurrent restore/download of that version. → process-wide `VersionBlobMu` RWMutex across open/prune/relocate.

- [x] **No admin user delete.** Disable + password reset only. Disabled users’ files stay on disk forever with no documented reclaim path. → `POST /api/admin/users/delete` (must already be disabled, cannot delete self); reclaim `files/` `trash/` `versions/` + metadata. SPA Delete button on disabled rows.

---

## P1 — Android hardening (after P0 wiring)

- [x] **Cleartext HTTP is on in the release-shaped manifest.** `android:usesCleartextTraffic="true"` so LAN testing works. Restrict to `debug` builds (network security config) before any APK leaves the house.
  - `android/app/src/main/AndroidManifest.xml` (~14) → NSC: release denies, debug allows; verified in packaged APKs via aapt2.

- [x] **Bearer token in plain `SharedPreferences`.** Switch `Session.kt` to `EncryptedSharedPreferences` + `MasterKey`. → done behind a testable store seam; corrupt-keystore recovery keeps baseUrl (plain prefs), drops only the token.

- [x] **No release signing config, minify off.** `assembleRelease` is not store-ready.
  - `android/app/build.gradle.kts` (~18) → signing from uncommitted `keystore.properties`/env, unsigned fallback logs clearly. Minify intentionally stays off (v1 non-goal below).

- [x] **Exported theme receiver has no permission.** Any app can broadcast `xx.launcher.THEME_CHANGED` at this package and recolor chrome. Require a signature / known-sender permission matching the launcher contract.
  - `android/app/src/main/AndroidManifest.xml` (~31) → signature-level `com.piercingxx.xxdrive.permission.THEME_SYNC`; NOTE: xx-launcher must declare `<uses-permission>` for it.

- [x] **WebView is a blank `WebViewClient()`.** No SSL error handling, no 4xx/5xx page, no offline message, no back-stack integration with the SPA’s own history beyond `canGoBack()`. A bad URL or expired cert is a white screen. → SSL errors cancelled + inline escaped error page with Retry; main-frame errors handled.

- [x] **Login form does not prefill the last server URL** even though `Session.baseUrl` is stored. Failed-then-retry is extra typing. → prefilled; survives logout and keystore recovery.

- [x] **Logout does not invalidate the server session.** `Session.clear()` + cancel WorkManager only. The bearer token stays valid for up to 30 days. Call `POST /api/auth/logout` with the token first. → best-effort server logout before local teardown.

- [x] **No Android tests outside theme sync.** 23 unit tests, all on `ThemeSyncReceiver` / `ThemePreset`. Add tests for `Session.init` required-before-use, PhotoUploadWorker watermark, download filename parsing, cookie/header injection. Add an instrumentation smoke if a device/emulator is available. → 70+ unit tests across auth/watermark/filenames/session stores; instrumentation smoke still needs a device.

- [ ] **WebView content stays dark on every launcher preset.** Deliberate (server CSS). If family theming is a product requirement for the file list, the PWA needs a theme endpoint or a JS bridge; do not inject CSS blindly.

---

## P1 — tests and CI

Current: Go 22 tests (api, fabric, fsdrv, token). **No tests** in `internal/store`, `internal/webfs`, `cmd/xxdrive-cli`, `cmd/xxdrive-server`. Android: theme receiver only. **No `.github/workflows`.**

- [x] CLI sync three-way tests (the P0 matrix). → `cmd/xxdrive-cli/sync_test.go` + verb/integration suites.
- [x] Store tests: username validation, fabric shadow create, session hash-at-rest, share revoke, version index relocate. → `internal/store/store_test.go` + api-level relocate/purge-split tests.
- [x] Folder share + password-form HTML + view-only `inline=1` + zip-symlink tests (the P0 share/isolation items). → `internal/api/shares_test.go`, `versions_test.go`.
- [x] Rename-then-versions + trash-then-restore-keeps-history. → both covered incl. recreated-file history split.
- [ ] Web UI: at least a node/jsdom or go test that fetches `/` and checks share-copy URL joining / 201 upload handling. Optional; browser smoke may be enough for v1. *(not done — 201 handling smoke-tested via curl; JS behavior needs the browser pass)*
- [x] CI: `go test ./...`, `go vet`, `gofmt -l`, Android `./gradlew test` on a JDK 17 runner. Block merge on red. → `.github/workflows/ci.yml` (jobs: go, android).

---

## P1 — deploy / ops

- [x] **`install.sh` Go version comment is wrong.** Says “Go >= 1.22”; `go.mod` pins `1.25.0`. → fixed.
- [x] **Install does not template TLS / Secure cookies / keyring.** Operator must hand-edit the unit for `-tls-cert`, `FABRIC_CLUSTER_KEYS_PATH`, `XXD_ADMIN_PASSWORD`. Document the exact drop-in, or add flags to the script. → script flags + `/etc/default/xxdrive` EnvironmentFile templating.
- [x] **No Caddy/nginx example** in `deploy/`. The systemd unit binds `127.0.0.1:8080` and then the README says “put a proxy in front” with no config. → `deploy/Caddyfile.example`, `deploy/nginx.conf.example`.
- [x] **Lost-admin-password has no operator command.** Journal scrape or raw `sqlite3` insert of a PBKDF2 row. Add `xxdrive-server passwd` or a documented `xxdrive-cli` admin path. → `xxdrive-server -passwd <user>`; end-to-end tested against a temp DB.
- [x] **SQLite WAL backup story is missing.** `xxdrive.db` + `-wal`/`-shm` plus `files/ trash/ versions/`. A naive `cp` of the db while running can lose metadata. Document `sqlite3 .backup` / stop the unit / zfs snapshot. → MANUAL ops section.
- [x] **Health is `/healthz`, unauthenticated.** Fine; make sure the proxy does not expose it on the public internet if you care about fingerprinting (low). → noted in both proxy examples.
- [x] **Default `-addr` is `:8080` (all interfaces).** systemd binds localhost; a `go run` / README quick start does not. Keep the quick start, but refuse to start without TLS if addr is not loopback unless `-i-know` is passed — or at least keep the existing WARNING and make install.sh the only networked path. → fail-closed guard + `-i-know`; README quick start updated; binary smoke-tested.

---

## P2 — product completeness (after P0/P1)

Not blockers for a single-operator tailnet, but they are holes in what the UI already implies.

- [x] Keyboard: Delete-to-trash exists; no Select-all, no arrow-key browse, no Enter-to-open. → Ctrl/Cmd+A + Enter-to-open + Escape added; *arrow-key browse still open*.
- [ ] Multi-select share is hidden (`sel.length === 1` only) with no explanation. → button now visible-but-disabled with an explanatory tooltip when selection ≠ 1 (multi-share itself stays out of v1).
- [x] Preview: images via blob URL are never revoked (`URL.createObjectURL`); long sessions leak. → tracked + revoked on close/replace.
- [ ] Video preview uses the cookie session on `<video src=url>` — works in the browser, not in the Android WebView until P0 cookie injection lands. → P0 cookie injection landed; *needs the phone smoke to close*.
- [x] PWA service worker caches `/`, `/index.html`, `/manifest.webmanifest` only — not `/app.js` or `/style.css`. A stale shell with a new JS URL 404s after deploy unless cache names bump. `CACHE = 'xxdrive-shell-v1'` is never rotated in the repo.
  - `internal/webfs/static/sw.js` → `v2`, app.js/style.css/icon precached, rotation protocol commented.
- [ ] No progress / cancel for downloads (uploads have XHR progress).
- [ ] Activity feed does not live-update; no “download” events even though the icon map has one.
- [ ] Admin cannot see disk usage per user (no quotas is a v1 non-goal; a read-only size total is still useful).
- [ ] CLI `sync` identity is size+mtime only. Same-size edits within the same Unix second are treated as unchanged (false negative) or, after a copy, as a conflict (false positive). Acceptable for v1 if documented; a content hash would be the real fix. *(documented in MANUAL §2.3)*
- [ ] Android: no Wi-Fi / metered indicator on the backup toggle while it is running; no last-success timestamp; no per-photo error list.
- [x] Android: `fileChooserLauncher` swallows `ActivityNotFoundException` with a silent `return false`. → Toast added.
- [x] Version string is `1.1.0` in `cmd/xxdrive-server/main.go` while Android `versionName` is `1.0`. Pick one scheme. → both `1.1.0` (versionCode 2).

---

## Out of v1 (do not start unless the contract changes)

These are explicit non-goals in the current design. Listed so they are not accidentally treated as ship blockers:

- Block/delta sync, chunked resumable uploads
- Internal ACL / team folders
- Server-side thumbnails / transcoding
- Quotas
- WebDAV
- Smart-sync placeholders / offline pinning (PWA StorageManager, Android pin)
- Store listing / Play signing / ProGuard
- LICENSE file (private estate repo today)

---

## Docs (fix after the code, not instead of it)

`docs/MANUAL.md` still describes a 2026-08-25 audit as **SOUND, no blockers**, and describes CLI sync / Android login / folder shares as working. That document is wrong. After P0 is fixed:

- [x] Rewrite MANUAL §2.3 (sync), §2.4 (Android), §5 (path safety username segment), §6 (clients) to match the code.
- [x] `docs/API.md` share `path` vs `sub`, and the `inline=1` preview exception. (+ `POST /api/auth/password` documented)
- [x] README “Status” section: keep it brutally honest until the phone smoke list is green; then drop the 🧪 caveat. *(updated; 🧪 stays until the phone smoke passes)*
- [x] `deploy/install.sh` header Go version.
- [x] Remove or update any leftover “audit INFO” that claimed `.xxpart` files are already skipped.

---

## Definition of done (“good to go”)

All of the following, on a real box and a real phone, not just `go test`:

1. P0 boxes above are checked, with tests that fail if they regress.
2. Web: login, upload (including a name collision with a visible conflict *or* a real overwrite+version), download, rename, trash round-trip, restore a version after rename, create a **folder** share, open it in a private window, unlock a **password** share in the browser, copy a share link that actually opens.
3. CLI: `login`, `up` overwrites and leaves a version, `sync` of a tree where only local changed does **not** clobber, a true both-changed path produces two copies and a stable next pass, `watch` survives a ctrl-c without leaving `.xxpart` as real files.
4. Android: the phone smoke list in the Android P0 section.
5. Deploy: `./deploy/install.sh https://…` behind Caddy/nginx, cookies `Secure`, `/healthz` 200, first-run admin password recovered from journal, a second start does not reprint it.
6. CI green on `go test ./...` and `./gradlew test`.

Until that list is true, treat this as a workstation prototype that happens to have careful PBKDF2 and a real trash folder.
