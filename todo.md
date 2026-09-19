# XX-Drive — Android & Linux apps

**2026-09-19.** Synology Drive / Dropbox, minus the company. Same product on
both. Same look, same verbs. Server is the Go binary in this repo. Files
stay files on disk. Trash, versions, conflict copies — always. Never silent
clobber.

Android WebView client: **operator smoke 2026-09-19, real phone.** Linux
GTK client: **not built.** Smoke Arch first. Same `linux/install.sh` on
Void / Alpine / Debian — a script, not a platform.

```
Package (Android): com.piercingxx.xxdrive
SoR: the Go server
Clients: native. WebView/PWA is a browser fallback, not the product.
```

Done means a person who used Synology Drive or Dropbox can live here
instead. Browse, sync chosen folders, share, versions, trash, camera
backup, offline, search — and when it breaks, a log that actually says
why.

---

## Locked

| ID | Decision |
|---|---|
| Dr1 | Named one-shot downloads land in public Downloads. Keep it. |
| Dr2 | Poll is acceptable until a real watcher exists. Prefer inotify/FSEvents-class on Linux; WorkManager + `FileObserver`/MediaStore on Android. |
| Dr3 | Sync identity may start as size+mtime. Content hash before calling this reliable. |
| Dr4 | **Native clients.** Android: Kotlin/Compose. Linux: Python + GTK4/libadwaita. Not a WebView. Not “open the PWA.” |
| Dr5 | Both clients look and work the same. Brand, layout, verbs, empty states, errors. Linux is Linux — paths, portals, XDG. |
| Dr6 | One family theme, set once. |
| Dr7 | Files stay files. SQLite is metadata. No CGO. No container as the product. |
| Dr8 | xx-apps is the hub. Origin is the tel HTTPS URL typed in xx-apps. Fabric `user_id` is the store key. Hub-down: stored origin + token, else one-app login. |
| Dr9 | **Linux install is a script, not a platform.** `linux/install.sh` detects pacman / xbps / apk / apt / dnf / zypper. GTK4 + libadwaita + Python from distro packages. App under `~/.local`. XDG `.desktop` always. systemd `--user` only if systemd is there (Void runit / Alpine OpenRC skip it). Idempotent. Not Flatpak. Not AUR. Not a distro. |
| Dr10 | **Chosen-directory autosync is the product.** Default is none. User picks local folders ↔ remote paths. Both OSes. |
| Dr11 | **Logs live on the device.** On-phone / on-box only. No network. No bodies, tokens, or file bytes in log lines. Export/share is a user action. |
| Dr12 | Conflicts are copies, never last-write-wins. Visible in the app, not only on disk. |

**Stop:** a second hub; a standing second password; Tailscale port UX;
WebView as the shipped UI; a Linux GUI that is a different product; an
install that only works on Arch (or only Debian, or only systemd);
silent data loss; a sync error with no log line.

---

## Already true (do not re-litigate)

Server (tested): list, mkdir, upload, download, zip, rename, move, copy,
trash, versions, search, stars, events, share links (password / expiry /
view-only), admin, fabric login, `/healthz`.

CLI: `sync` / `watch` with baseline + conflict copies. Scripts, not the
Linux app.

Android today: WebView + picker upload + public Downloads + camera backup
worker + `AppLog` (rotating file, ring, last-crash). Operator smoke
2026-09-19.

---

## Android smoke — operator 2026-09-19

- [x] Login → file list on the real node
- [x] Picker upload
- [x] Named download to public Downloads
- [x] Camera backup path
- [x] Logout → cold start → login again
- [ ] Confirm: same-name download ×2 gets a unique suffix, no overwrite
- [ ] Confirm: video actually plays

Do not put irreplaceable files behind this node until Wave 3 is green on
both clients.

---

## Wave 0 — Logging (both clients, first)

Errors you cannot see did not happen. Build this before fancy sync.

Android already has `AppLog`. Keep it. Finish it. Mirror it on Linux.

- [ ] Levels D/I/W/E. Rotating file + ring + last-crash.
- [ ] Every sync/auth/IO failure writes **why** (HTTP status, path, errno). Never swallow.
- [ ] In-app log screen. Copy / share / export a file.
- [ ] Crash handler writes `last-crash.txt` before death.
- [ ] Linux: `~/.local/state/xx-drive/drive.log` (XDG). Same shape as Android.
- [ ] Redact: tokens, passwords, file bytes. Path names are allowed.
- [ ] Unit tests: rotate, redact, crash file.

---

## Wave 1 — Native Android (Compose)

Replace the WebView. Same API.

- [ ] Login / hub-up / hub-down / logout. Token in EncryptedSharedPreferences.
- [ ] File tree: list, mkdir, rename, move, copy, delete → trash.
- [ ] Upload (picker + share-into). Download (Dr1).
- [ ] Preview: images, video, text. Other types → download.
- [ ] Search. Stars. Activity.
- [ ] Trash: list, restore, empty.
- [ ] Versions: list, restore, download.
- [ ] Share: create link, password, expiry, copy URL, revoke.
- [ ] Camera backup (existing worker). Last-success, last-10 failures, metered.
- [ ] Sync status on the main surface (idle / syncing / waiting / error). Tap → log.
- [ ] Empty, error, offline, progress — family states, not toast-only.
- [ ] Re-smoke on the phone. Log export from a real failure.

---

## Wave 2 — Native Linux (GTK4)

`linux/` in this repo. Not the CLI with a coat of paint.

- [ ] Same surfaces as Wave 1. Same verbs. Same theme.
- [ ] xdg portals for open/save.
- [ ] Session in `~/.config/xx-drive/`. Logout kills it.
- [ ] Hub probe / one-app login, same rules as Android.
- [ ] Camera backup = “watch this folder” (Pictures, or whatever they pick).
- [ ] **`linux/install.sh`**
      - Detect pacman, xbps, apk, apt, dnf, zypper.
      - Install python3, pygobject, GTK4, libadwaita (distro packages).
      - Install app to `~/.local`.
      - Write `~/.local/share/applications/xx-drive.desktop`.
      - Autostart: XDG autostart. systemd `--user` only if `systemctl --user` works.
      - Idempotent. No container. No dedicated distro.
- [ ] Smoke **Arch** (this machine): login, list, upload, download, logout, log screen.
- [ ] Same script, no edits, on Void, Alpine, Debian. Fix the script, not the OS.

---

## Wave 3 — Chosen-directory autosync (the Drive Client)

This is the gap vs Synology Drive / Dropbox. Both clients.

- [ ] Pick N local folders ↔ remote paths. Default none.
- [ ] First sync, then continuous. Pause / resume. Unlink without deleting either side.
- [ ] Content-hash identity before calling this reliable (Dr3).
- [ ] Interrupted upload resumes. Process death resumes. Mesh down queues; mesh up drains.
- [ ] Disk full, 401, 5xx, TLS fail → error state + log line + retry with backoff. Never a silent skip.
- [ ] Conflict copies named and listed in the app.
- [ ] Metered-network policy on Android. Linux: optional “Wi-Fi / this interface only” if cheap; otherwise document “the mesh is the network.”
- [ ] Same folder set, same behavior, both OSes.
- [ ] Tests: create / edit / delete / rename both sides; process kill mid-upload; hash mismatch → copy not clobber.
- [ ] Smoke: Arch workstation + the phone, same account, same two folders, overnight.

**Accept:** dated notes. A file edited on Linux appears on the phone without
opening the app. A file edited on the phone appears in the Linux folder.
A conflict is two files, both readable.

---

## Wave 4 — Offline & reliability

- [ ] Pin files/folders for offline. Mesh down: pinned still opens.
- [ ] Sync queue survives reboot.
- [ ] Partial/temp files never replace a good local file.
- [ ] Device name on conflict copies (`X-Device`).
- [ ] Sessions: list + revoke others, both clients.
- [ ] Password change. Logout all.
- [ ] Notifications: sync error, share created — opt-in, off by default.
- [ ] Overnight soak on Arch + phone. Log has no unexplained E.

---

## Wave 5 — Competitor parity (what Dropbox / Synology Drive users expect)

Server already does most of this. Clients must expose it. Same on both.

- [ ] Browse My Drive as a real tree, not a web page.
- [ ] Selective folder sync (Wave 3) — this is Dropbox selective sync / Synology sync tasks.
- [ ] Camera / Pictures backup.
- [ ] Share links with password and expiry. View-only.
- [ ] Version history per file. Restore.
- [ ] Trash, 30-day restore.
- [ ] Search by name.
- [ ] Starred.
- [ ] Activity feed.
- [ ] Multi-device, one account.
- [ ] Create folder, rename, move, copy, delete.
- [ ] Zip download of a folder.
- [ ] Empty / error / syncing chrome a stranger understands.

**Not this product (Dropbox/Synology extras we are not cloning):**
Paper/docs editing, team-folder ACLs, comments, Play listing, “rewind the
account,” LAN-peer sync, smart-sync placeholders / on-demand stubs,
quotas, WebDAV as the client. Files stay files. If you want on-demand
later, it is a new todo.

---

## Wave 6 — Server, only what the clients still lack

Do not rebuild the binary. Add the smallest wire.

- [ ] Pin list / sync-folder set / device name, if they are not local-only.
      Prefer local-only until two devices must share the folder set.
- [ ] Document in `docs/API.md`. Tests.
- [ ] NAS deploy stays one binary. Distro of the NAS is the NAS’s problem.

---

## Fabric door (keep)

- [ ] Dr-E1 — ClusterKeyring; `user_id` is the filesystem key. Fail-closed.
- [ ] Dr-E2 — Android hub discovery; no second password when hub is up.
- [ ] Dr-E3 — No loopback hub in this tree.
- [ ] Dr-E4 — Default-off in xx-apps until granted.

---

## Stop conditions

- Shipping WebView and calling Compose done → reject.
- Linux GUI ≠ the phone → reject.
- `install.sh` Arch-only / Debian-only / systemd-required → reject.
- Last-write-wins → reject.
- Sync error with no log line → reject.
- Reverting downloads to app-private → reject.
- A new hub, a new password, or a Tailscale port picker → reject.
- Ticking Wave 3 from unit tests without the overnight two-device smoke → reject.
