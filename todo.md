# XX-Drive — Remaining work

**2026-09-04.** Go server + CLI + Android WebView client exist. Phone
smoke is unchecked. Laundry-bot “download fix” is **not** a regression:
public Downloads is the product.

Package: `com.piercingxx.xxdrive`  
Self-hosted private cloud. Server is the SoR. The Android app is login +
WebView + native upload/download/camera backup.

```
Status: go test ./... green. Android unit tests exist. Zero phone
confirmation. Settings shows last-success, last-10 photo failures, and
metered / waiting-for-unmetered. WebView/PWA file list stays
server-dark; native chrome follows launcher.
```

---

## Locked now (2026-09-04)

| ID | Decision |
|---|---|
| Dr1 | **Named downloads land in public Downloads** (`setDestinationInExternalPublicDir`). Keep it. The old todo’s “app-private” claim was wrong. |
| Dr2 | `watch` 30s poll is acceptable for v1. |
| Dr3 | CLI identity is size+mtime; content hash deferred. |

---

## One door / sibling (estate — 2026-09-17)

**xx-apps is the hub. This repo does not implement the sidecar.**
Supersedes the 2026-09-04 “this repo implements the sidecar” lock.

**D1.** Origin is the tel HTTPS URL the user typed in xx-apps (Tailscale
HTTPS is valid smoke), not house `/`, not `:845x`.
**D2.** Fabric `user_id` is the store key. Join ClusterKeyring. No
standing local-admin + distinct fabric password after first-run.
**D4.** Hub-down: this app uses stored origin + fabric token. If the
token is missing, one-app login (URL + username + password).

- [ ] Dr-E1 — Go server validates ClusterKeyring; `user_id` is the
      filesystem key. Fail-closed when the ring is configured.
  - verify: `go test ./internal/fabric/...` and isolation still fails
    closed for user B
- [ ] Dr-E2 — Android client: probe xx-apps hub discovery; reuse
      session; no second password prompt when hub is up.
  - files: android/ app login
  - verify: Android unit test hub-up / hub-down / manual login
- [ ] Dr-E3 — Do **not** mill a loopback hub or CalDAV server here.
  - verify: no new bind of `:0` hub in this tree
- [ ] Dr-E4 — Default-off in xx-apps until granted. Disable-user wipe
      is xx-apps’ job; this APK just dies when uninstalled.

**Stop:** a second hub; a standing second password; Tailscale port UX.

---

## Phone smoke (the gate)

- [ ] Login → file list on the real node.
- [ ] Picker upload of a real file; it appears in the web UI.
- [ ] Named download ×2 into **public Downloads**; second file with the
  same name gets a unique suffix (`DownloadNames`), no overwrite.
- [ ] Camera backup after process death still uploads new photos.
- [ ] Logout → cold start → login again. Token gone after logout.
- [ ] Video in the WebView actually plays (cookie injection).

**Accept:** dated notes on caiman against the live box. Do not put
irreplaceable files behind this node until this list is green.

---

## Browser click-through (same week)

- [ ] Folder share + password share.
- [ ] Conflict / overwrite creates a version.
- [ ] Share URL copies and opens.

---

## Theme (locked)

**WebView/PWA file list stays server-dark; native chrome follows launcher.**
No CSS/PWA bridge. Login, settings, and window bars take XX-Launcher
presets; the file list is the server's own dark web UI.

---

## Do not start (out of v1)

Block/delta sync, ACLs, thumbnails, quotas, WebDAV, smart-sync
placeholders, Play listing, ad-hoc ProGuard, LICENSE file.

---

## Stop conditions

- Reverting downloads to app-private “because the old todo said so” → reject.
- Claiming P0 done from a laundry-bot checkbox → reject.
- Real files on the node before phone smoke → reject (operator rule).
