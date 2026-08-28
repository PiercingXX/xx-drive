# xx-drive P0 — fabric XOR separate; deploy matches the box

Operator 2026-08-27. Live Dutchman `127.0.0.1:8080`, data `/srv/deep/xx-drive`,
own admin/session. ClusterKeyring code exists but is not the live path.

**Do not wire fabric until the operator picks: wire XOR permanently separate.**
Default mill path if no new ruling: document permanently separate next to radio
(own session, not the ring) and make deploy/docs tell that one story.

Always `127.0.0.1`. Health `GET /healthz` → `200 ok`.

Exam: `TODO-2026-08-27.md`.

### T1 — docs tell one identity story

MANUAL / README / parent map: xx-drive is not on the fabric ring (unless the
operator has ruled to wire it).

- files: `docs/MANUAL.md`, `README.md`
- verify: rg -n ClusterKeyring docs/MANUAL.md

### T2 — deploy unit matches the box

`127.0.0.1:8080`, data `/srv/deep/xx-drive`, cookie Secure via XXD_TLS_CERT set
/ XXD_TLS_KEY unset. Never README all-interfaces `:8080`.

- files: `docs/MANUAL.md`
- verify: rg -n 127.0.0.1:8080 docs/MANUAL.md
