# xx-drive HTTP API (v1)

All endpoints are JSON unless noted. Errors: `{"error":"..."}` with 4xx/5xx status.

## Authentication

Two credential styles work on every `/api/*` endpoint:

- **Cookie** (`xxd_session`, HttpOnly, SameSite=Lax) — set by login; used by the web UI.
  Mutating requests via cookie must be same-origin (Origin/Referer checked).
- **Bearer token** — `Authorization: Bearer <token>` from the login response; used by CLI/apps.

| Method | Path | Body / Query | Response |
|---|---|---|---|
| POST | /api/auth/login | `{username,password}` | `{token,user:{username,isAdmin}}` + cookie |
| POST | /api/auth/logout | — | `{ok}` |
| GET | /api/auth/me | — | `{username,isAdmin,fabric}` |
| POST | /api/auth/password | `{current_password,new_password}` (min 8 chars) | `{}` — self-service rotation; 401 if the current password is wrong |
| GET | /api/auth/sessions | — | `[{label,createdAt,lastSeen,expiresAt,current}]` |
| POST | /api/auth/sessions/revoke-others | — | `{revoked:n}` |
| POST | /api/auth/fabric | `{token:"v1..."}` or `Authorization: Bearer v1...` | estate SSO: validates a fabric token, exchanges it for a session `{token,user}` |

Rate limit: >10 failed logins per IP+username per 15 min → 429.

Cookies (`xxd_session`, share-grant cookies) carry `Secure` when the
server runs TLS itself (`-tls-cert/-tls-key`), when `-base-url` starts
with `https://`, or when `-secure-cookies` is passed — set that flag when
TLS terminates at a reverse proxy (the server does not read
`X-Forwarded-Proto`).

## Files

Paths are absolute within the user's drive and always start with `/`.
Uploads stage to a temp file and commit atomically via rename.

| Method | Path | Notes |
|---|---|---|
| GET | /api/files/list?path=/x | → `{path, entries:[{name,path,isDir,size,mtime,starred}]}` |
| POST | /api/files/mkdir | `{path}` |
| POST | /api/files/upload?path=/dir/name.ext&conflict=rename | multipart field `file`. Omitting `conflict` **overwrites** the target and snapshots the prior content as a version. `conflict=rename` never overwrites: the incoming file becomes `name (conflict from DEVICE TIME).ext` with HTTP **201**. Optional header `If-Match` (weak etag, format `"hex(mtime)-hex(size)"`) for optimistic concurrency → 412 on mismatch. Optional `X-Device` names conflict copies. Response `{entry,sha256,conflicted}`. |
| GET | /api/files/download?path=/x&inline=1 | Range supported; `inline=1` renders instead of attaching |
| GET | /api/files/zip?path=/folder | streams a .zip of the subtree |
| POST | /api/files/rename | `{path,newName}` |
| POST | /api/files/move | `{path,destDir}` |
| POST | /api/files/copy | `{path,destDir,newName?}` |
| POST | /api/files/delete | `{path}` → soft-delete to trash `{trashId}` |

Conflict-copy naming: `name (conflict from DEVICE yyyy-MM-dd HH:mm:ss).ext`.

## Trash

| Method | Path | Notes |
|---|---|---|
| GET | /api/trash | `[{id,name,origPath,isDir,size,deletedAt}]` |
| POST | /api/trash/restore | `{id}` → `{restoredTo}` (collision → numbered suffix) |
| POST | /api/trash/delete | `{id}` permanent |
| POST | /api/trash/empty | — |

Retention: items auto-purge after `-trash-days` (default 30).

## Versions

Every overwrite snapshots prior content (max 32 per path, pruned hourly).

| Method | Path | Notes |
|---|---|---|
| GET | /api/versions?path=/f | `[{versionId,size,createdAt}]` |
| POST | /api/versions/restore | `{path,versionId}` — current content is snapshotted first (restores are reversible) |
| GET | /api/versions/download?path=&versionId= | attachment |

## Search / Stars / Events

| Method | Path | Notes |
|---|---|---|
| GET | /api/search?q=text | case-insensitive filename substring, max 200 |
| POST | /api/star/toggle | `{path}` → `{starred}` |
| GET | /api/starred | `[entries]` |
| GET | /api/events?limit=100 | `[{id,kind,detail,createdAt}]` |

Event kinds: login, upload, mkdir, rename, move, copy, delete, restore, purge,
empty_trash, share_create, share_revoke, version_restore, revoke_sessions,
change_password, admin_*.

## Share links

Tokens are 24-byte random base64url capability URLs. Stored hashed; revocation
is instant. Passwords are PBKDF2-HMAC-SHA256 (600k iterations).

| Method | Path | Notes |
|---|---|---|
| GET | /api/shares | `[{tokenHash,path,hasPassword,allowDownload,expiresAt,createdAt,url}]` |
| POST | /api/shares | `{path,password?,expiresInDays?,allowDownload?}` → `{token,...}` |
| DELETE | /api/shares/{tokenHash16} | revoke |

Public endpoints (no auth):

| Method | Path | Notes |
|---|---|---|
| GET | /s/{token} | HTML page (password form when locked; listing + preview otherwise) |
| POST | /s/{token} | form `password=...` (plus optional hidden `sub` to land back on a subfolder) → grant cookie (24 h) + redirect |
| GET | /s/{token}/list?path=&sub= | JSON listing of shared subtree |
| GET | /s/{token}/download?path=&sub=&inline=1 | file or .zip |

**Addressing a target inside a folder share — `path` vs `sub`:**

- `path` is the **full logical path** (`?path=/photos/vacation.jpg`) and
  must resolve at or under the share root.
- `sub` is **relative to the share root** (`?sub=vacation.jpg` for a
  share of `/photos`). For backward compatibility an absolute `sub` that
  already starts with the share root is accepted as-is (it is not
  doubled).
- Both are cleaned, reject literal `..` segments outright, and are then
  checked against the containment rule
  `target == share.path || strings.HasPrefix(target, share.path+"/")` —
  anything outside the shared subtree (e.g. `?path=/otheruser/x`) is a
  404, never a share escape.

**View-only shares and `inline=1`:** when the share was created with
`allowDownload=false`, downloads are refused with 403 **except** for
`inline=1` requests whose target name has an image extension (`.jpg`,
`.jpeg`, `.png`, `.gif`, `.webp`, `.avif`, `.bmp`) or video extension
(`.mp4`, `.webm`, `.mov`, `.m4v`) — inline previews only. Everything
else under `inline=1` (documents, archives, whole-folder zips) is also
403.

Both full tokens and 16-char hash prefixes resolve.

## Admin

| Method | Path | Notes |
|---|---|---|
| GET | /api/admin/users | `[...]` |
| POST | /api/admin/users | `{username,password,isAdmin}` (password ≥ 8) |
| POST | /api/admin/users/set-state | `{username,disabled}` — disabling kills sessions |
| POST | /api/admin/users/password | `{username,password}` |
| POST | /api/admin/users/delete | `{username}` — permanent. Account must already be disabled. Cannot delete yourself. Removes `files/`, `trash/`, `versions/` for that user and the metadata row. |

## Ops

- `GET /healthz` → `ok`
- Security headers on everything: CSP, nosniff, DENY framing, strict referrer.
- PWA: `/manifest.webmanifest`, `/sw.js` (app-shell caching only; API never cached).
