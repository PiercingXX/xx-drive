# XX-Drive — Remaining work

**2026-09-04.** Go server + CLI + Android WebView client exist. Phone
smoke is unchecked. Laundry-bot “download fix” is **not** a regression:
public Downloads is the product.

Package: `com.piercingxx.xxdrive`  
Self-hosted private cloud. Server is the SoR. The Android app is login +
WebView + native upload/download/camera backup.

```
Status: go test ./... green. Android unit tests exist. Zero phone
confirmation. WebView PWA stays server-dark.
```

---

## Locked now (2026-09-04)

| ID | Decision |
|---|---|
| Dr1 | **Named downloads land in public Downloads** (`setDestinationInExternalPublicDir`). Keep it. The old todo’s “app-private” claim was wrong. |
| Dr2 | `watch` 30s poll is acceptable for v1. |
| Dr3 | CLI identity is size+mtime; content hash deferred. |

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

## Theme (decide by doing)

- [ ] WebView file list either follows launcher presets **or** this file
  permanently says “PWA stays server-dark.” Pick one after the first
  smoke, do not leave it as a ghost P1.

---

## Do not start (out of v1)

Block/delta sync, ACLs, thumbnails, quotas, WebDAV, smart-sync
placeholders, Play listing, ad-hoc ProGuard, LICENSE file, backup-status
UI (P2).

---

## Stop conditions

- Reverting downloads to app-private “because the old todo said so” → reject.
- Claiming P0 done from a laundry-bot checkbox → reject.
- Real files on the node before phone smoke → reject (operator rule).
