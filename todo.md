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

## One door / device hub (estate — 2026-09-04)

Locked with `skippy-tel-network/todo.md` WS0. Estate contract (no APK)
is tested there. **This repo implements the sidecar.** Work top to
bottom. Check a box only when code and a test exist.

**D1.** Phone UX is a **dedicated tel hostname** (operator names it),
not house `/`, not `:8450`–`:8454` / `:8447`/`:8448`/`:8449`.
**D2.** Fabric `user_id` is the store key. Bootstrap admin first-run
only. Fail-closed once the ring is configured. No standing second
password.
**D4.** User may point this app at **any** origin + user/pass (estate
tel hostname, Synology, or any compatible server). Hub piggyback is
for siblings on the estate session, not a lock-in.

- [ ] Connection UI: one origin URL + username/password. No per-app
      Tailscale port presets as the normal path.
  - verify: unit test — saved config has a single origin; fixtures
    with `:8450`–`:8454` / `:8448` as the default fail
- [ ] Fabric login: `user_id` is the store key; first-run admin only;
      configured ring is fail-closed. No third password.
  - verify: existing isolation test still fails closed for user B;
    after first-run a standing local-admin + distinct fabric password
    is not required
- [ ] **Sidecar hub** on `127.0.0.1` only (new listener — WebView today
      is not a server). Foreground service. Signature-checked discovery.
      Holds the session for siblings.
  - verify: bind on a non-loopback address is refused; unsigned
    discovery is rejected (JVM or contract fixture)
- [ ] **CalDAV** on the hub at `127.0.0.1` so DAVx⁵ can piggyback.
      xx-calendar APK stays `INTERNET`-free (other repo).
  - verify: hub fixture answers CalDAV; calendar is not given a WAN
      URL as the only path when the hub is up
- [ ] Hub down / GrapheneOS kill: siblings detect dead hub and use
      their stored origin + creds. No crash loop.
  - verify: stop-hub fixture; no tight retry storm
- [ ] Phone smoke below still uses the user-chosen origin (estate or
      Synology), not a hardcoded Tailscale port.

**Stop:** inventing live `:845x` as the UX; a second standing password
after first-run; hub on `0.0.0.0`; adding `INTERNET` to calendar.

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
