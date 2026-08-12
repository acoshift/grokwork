# Machine API v1

Private JSON API for remote agents (cloud VM, CI, MCP). Not a public SaaS.

Enable with `"api": { "enabled": true }` in `config.json`. Default off.

## Auth

```
Authorization: Bearer gw_<publicId>_<secret>
```

Cookies are ignored. CSRF does not apply. `GET /api/v1/health` is unauthenticated.

## Operator recipe

1. Create a project capability template `automation-runner` with **only** `startSessions`.
2. Create a team that names that template; add the token actor `token:<id>`.
3. Mint at `/config/api-tokens` (web auth + admin). Copy the secret once.
4. Set `api.enabled` true. The VM must already reach `httpListen` (Tailscale / private bind).

Open-LAN mint is refused. First token without web auth: set `GROK_WORK_API_BOOTSTRAP_TOKEN` (full wire token) and `GROK_WORK_API_BOOTSTRAP_PROJECTS` (comma list). Bootstrap inserts only if that public id is **absent** (a revoked row blocks re-insert). Caps default to startSessions-only.

## Endpoints

| Method | Path | Notes |
|--------|------|--------|
| GET | `/api/v1/health` | `{ok, api}` — no secrets |
| GET | `/api/v1/whoami` | actor, projects, effective caps |
| GET | `/api/v1/projects` | names only |
| POST | `/api/v1/projects/{p}/issues` | `kind` required: `feature` or `bug` |
| POST | `/api/v1/projects/{p}/sessions` | always web-native `w_*`; poll status |
| GET | `/api/v1/sessions/{id}` | owner/co-owner only; else 404 |
| POST | `/api/v1/sessions/{id}/continue` | owner/co-owner + startSessions |
| POST | `/api/v1/sessions/{id}/cancel` | owner/co-owner |

Creating POSTs accept `Idempotency-Key`. Same body replays; different body → 409.

Idle for pollers: `running == false && queueLen == 0`. Do not wait for label `done`.

## Examples

```bash
curl -sS -X POST "$GROKWORK_URL/api/v1/projects/app/sessions" \
  -H "Authorization: Bearer $GROKWORK_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"prompt":"Fix the nil panic in checkout","mode":"investigate"}'
```

```bash
curl -sS -X POST "$GROKWORK_URL/api/v1/projects/app/issues" \
  -H "Authorization: Bearer $GROKWORK_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title":"Mobile checkout flaky","kind":"bug","body":"Seen in prod"}'
```

See `docs/design-remote-agent-api.md` for the full design (K22–K26).
