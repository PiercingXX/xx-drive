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
| GET | /api/auth/me | — | `{username,isAdmin}` |
| GET | /api/auth/sessions | — | `[{label,createdAt,lastSeen,expiresAt,current}]` |
| POST | /api/auth/sessions/revoke-others | — | `{revoked:n}` |

Rate limit: >10 failed logins per IP+username per 15 min → 429.

## Files

Paths are absolute within the user's drive and always start with `/`.
Uploads stage to a temp file and commit atomically via rename.

| Method | Path | Notes |
|---|---|---|
| GET | /api/files/list?path=/x | → `{path, entries:[{name,path,isDir,size,mtime,starred}]}` |
| POST | /api/files/mkdir | `{path}` |
| POST | /api/files/upload?path=/dir/name.ext&conflict=rename | multipart field `file`. Optional header `If-Match` (weak etag) for optimistic concurrency → 412 on mismatch. Optional `X-Device` names conflict copies. Response `{entry,sha256,conflicted}`. Overwrites snapshot the old content as a version. |
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
empty_trash, share_create, share_revoke, version_restore, admin_*.

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
| GET | /s/{token} | HTML page (list/preview) |
| POST | /s/{token} | form `password=...` → grant cookie (24 h) + redirect |
| GET | /s/{token}/list?sub=/sub | JSON listing of shared subtree |
| GET | /s/{token}/download?path=&inline=1 | file or .zip; blocked when allowDownload=false unless inline preview |

Both full tokens and 16-char hash prefixes resolve.

## Admin

| Method | Path | Notes |
|---|---|---|
| GET | /api/admin/users | `[...]` |
| POST | /api/admin/users | `{username,password,isAdmin}` (password ≥ 8) |
| POST | /api/admin/users/set-state | `{username,disabled}` — disabling kills sessions |
| POST | /api/admin/users/password | `{username,password}` |

## Ops

- `GET /healthz` → `ok`
- Security headers on everything: CSP, nosniff, DENY framing, strict referrer.
- PWA: `/manifest.webmanifest`, `/sw.js` (app-shell caching only; API never cached).
