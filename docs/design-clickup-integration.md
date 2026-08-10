# ClickUp integration (L1 ticket bridge)

Status: **L1 shipped** (revised after advisor scrutinize, then implemented). Mirrors Linear L1; defers webhooks / write-back / agent.

## Problem

Some mapped projects track engineering work in **ClickUp**, not Linear or only-GitHub. Today grokwork can bind GitHub issues (`#N`) and Linear identifiers (`ENG-123`), surface them on status/brief/prompt/PR body, list them in the web UI, and **Fix with Grok** from an issue page. A ClickUp-primary team gets none of that: chat refs are ignored, there is no list page, and sessions cannot carry a ClickUp task as a first-class bound ticket.

## Goals (L1 only)

1. **Opt-in per project** — same shape as `projects.<name>.linear`.
2. **Bind ClickUp tasks onto a session** (max 5, shared with GH/Linear via `Entry.Issues`).
3. **Resolve metadata** (title, status, url, ids) via ClickUp REST when an API key is available.
4. **Discord:** auto-bind from task text when refs are unambiguous; `@Grok /link` / `/unlink` / `/link clear`; status/brief/hand-off lines; remote-work prompt requires PR (or direct-ship commit) body lines.
5. **Web:** list + detail under `/projects/{p}/clickup…`, **Fix with Grok** reusing `bot.StartFix` with a new kind.
6. **No write-back** to ClickUp status in L1 (same non-goal as Linear L1: state is driven by humans / ClickUp’s own GitHub integration if they use it, not by grokwork calling `PUT /task`).

## Non-goals (explicit)

| Out of scope | Why |
|--------------|-----|
| Full two-way field sync | Same as Linear; drifts and fights ClickUp |
| Moving task status from the bot | L2+; agent must not invent status names |
| Webhooks → Discord | Linear L3 analogue; separate train |
| OAuth app for multi-tenant ClickUp | Personal API token per project is enough for private deploy |
| Creating tasks from grokwork | L2; L1 is bind + fix only |
| Replacing cases | Customer support stays cases; ClickUp is eng tickets |
| Auto-parsing bare native task ids (`9hx`) from free text | Too short / ambiguous; false positives |

## Design principles (load-bearing)

1. **ClickUp is another `TrackedIssue` provider**, not a parallel store. Reuse `Entry.Issues`, `UpsertIssue`, max 5, Fixes/Refs keywords, `FindByIssue`-style reverse scan. Do not mint a CaseKey-shaped id.
2. **One-way bind into the session.** Resolve fills title/state/url; never `issueUpdate`-equivalent in L1. Prompt says: put the display id in PR title/body; do not call ClickUp APIs from the agent.
3. **Parse only what cannot be another system’s id.** Linear’s free-text regex claims *any* `PREFIX-N` when Linear is enabled (it does **not** filter by `teamKey`). ClickUp free-text binds only:
   - ClickUp URLs (native and custom-id forms), and/or
   - configured **custom task id prefix** (`DEV-42`), never “any PREFIX-N”.
   When both systems are enabled, the **bot layer** must filter Linear binds whose team key equals the project’s ClickUp `customIdPrefix` (see Coexistence).
4. **Secrets stay off Discord and audit detail.** API key via config or env; form is write-only; audit never carries the key.
5. **Containment:** every web page is mux `requireAuth` **plus in-handler** `ensureProjectAccess` (Linear’s pattern in `workflow.go`); list scope is the configured List, not the whole Workspace.

## ClickUp API facts (L1 needs)

| Need | API |
|------|-----|
| Auth | Header `Authorization: <personal token>` (`pk_…`) — not Bearer for personal keys |
| Get by native id | `GET /v2/task/{task_id}?include_markdown_description=true` |
| Get by custom id | same path with `?custom_task_ids=true&team_id={workspaceId}` |
| List recent | `GET /v2/list/{list_id}/task` (home list only; page size ≤100) |
| Task shape we keep | `id`, `custom_id`, `name`, `url`, `status.status`, markdown/text description, list id |

Workspace id is called `team_id` in the API. We store it as `workspaceId` in config to match human ClickUp vocabulary and avoid Discord “team” confusion.

**List query params (explicit, not defaults):** pass `order_by=updated` for parity with Linear’s `orderBy: updatedAt`. Decide `include_closed=true` so completed tasks still appear on the board (default ClickUp API omits closed tasks and the page would look broken for done work). Cap client-side to ~50 like Linear’s list cap.

## Config

```json
"projects": {
  "app": {
    "clickup": {
      "enabled": true,
      "apiKey": "",
      "workspaceId": "1234567",
      "listId": "15505202",
      "customIdPrefix": "DEV"
    }
  }
}
```

| Field | Role |
|-------|------|
| `enabled` | Opt-in; parse/bind/list/fix all gated on this |
| `apiKey` | Secret; empty → fall back to `CLICKUP_API_KEY_<PROJECT>` at **read time** |
| `workspaceId` | Required for custom-id resolve; also documents which Workspace the key is for |
| `listId` | Single list for web list page (L1: one list; multi-list is L2) |
| `customIdPrefix` | Optional. When set, free-text `PREFIX-N` (case-insensitive prefix) binds as ClickUp. When empty, **only URLs** auto-bind |

Accessors (mirror Linear):

- `ProjectClickUpEnabled` / `ProjectClickUpAPIKey` / `WorkspaceID` / `ListID` / `CustomIdPrefix` / `CanResolve` / `SetProjectClickUp` / `AnyProjectClickUpEnabled`
- `ProjectClickUpAPIKey`: config value if set, else `CLICKUP_API_KEY_` + `ProjectEnvKeySuffix(project)` (same read-time env pattern as `ProjectLinearAPIKey` / `linearAPIKeyFromEnv` — there is **no** generic secret walk in `config.Load`; Load only trims fields)
- `CanResolve` = enabled && apiKey non-empty. Custom-id resolve additionally needs `workspaceId`.

Config UI: project settings → Integrations, clone Linear form pattern (`project_config_integrations.tmpl`): enabled checkbox, workspace id, list id, custom id prefix, password-style API key input (“leave blank to keep”), clear-key checkbox, `APIKeySet` badge when a key is stored. `SetProjectClickUp` mirrors `SetProjectLinear` (empty apiKey leaves stored key; `clearAPIKey` clears).

Audit for config save: name + enabled only (mirror `setProjectLinear` — never the key).

### Coexistence with Linear (load-bearing)

Both may be enabled on one project. **Important code fact:** `ParseLinearIssueRefs` / `linearIdentifierRE` match *any* `TEAM-123` when Linear is enabled; `teamKey` is only used for the Linear **list** page, not for parse. Calling both binders without a filter double-binds `DEV-12` (Linear + ClickUp), burns two of five slots, and injects Linear’s “GitHub integration drives state” prompt line for a non-Linear ticket.

| Input | Bind as |
|-------|---------|
| `https://app.clickup.com/t/{nativeId}` | ClickUp (native id) |
| `https://app.clickup.com/t/{workspaceId}/{CUSTOM-ID}` | ClickUp (custom id + workspace from URL) |
| `https://linear.app/…/issue/ENG-1` | Linear |
| Bare `DEV-12` when `customIdPrefix=DEV` and ClickUp enabled | **ClickUp only** |
| Bare `ENG-12` when Linear enabled and prefix ≠ `ENG` | Linear (existing) |
| Bare `ENG-12` when `customIdPrefix=ENG` **and** Linear enabled | **Linear wins**; ClickUp prefix-parse is **disabled** for that project with a Load warning (filter would hide a real Linear team) |
| Bare native id `9hx` | **Never** free-text auto-parse |

**Bot-layer filter (required):** when ClickUp is enabled and `customIdPrefix` is non-empty **and** that prefix is **not** equal (case-insensitive) to the project’s Linear `teamKey`:

- `bindLinearIssuesFromText` drops any parsed Linear ref whose team key equals `customIdPrefix` before resolve/upsert.
- `handleLink`’s Linear-preference path must apply the same drop (today `looksLikeLinearRef` can refuse with “Linear is not enabled” before ClickUp is tried — reorder: try ClickUp parse **before** the Linear-disabled refusal).

When `customIdPrefix` equals Linear `teamKey`: Load warns; bare `PREFIX-N` stays Linear-only; ClickUp free-text prefix parse is off; operators use ClickUp URLs or change the prefix.

sessionstore parsers stay config-free; the filter lives in bot (has `*config.Config`).

## Session model

Extend `sessionstore.TrackedIssue` (additive JSON fields; scalars on the struct are clone-safe via the existing `Issues` slice detach):

```go
// Provider: "" | "github" | "linear" | "clickup"
const ProviderClickUp = "clickup"

// ClickUp fields (Provider == "clickup")
ClickUpID   string `json:"clickupId,omitempty"` // native id (9hx)
CustomID    string `json:"customId,omitempty"`  // DEV-42 when present
// Display: prefer CustomID, else ClickUpID (reuse Identifier for display
// key if convenient, but prefer dedicated fields so Linear Identifier
// semantics stay untouched — implementer picks one approach and tests it)
// Title, State, URL already exist
ListID      string `json:"listId,omitempty"`
WorkspaceID string `json:"workspaceId,omitempty"`
```

Rules:

- `IsClickUp()` / `IsLinear()` / github default remain mutually exclusive providers.
- `IssueKey()`: `clickup:custom:dev-42` if custom id set, else `clickup:id:{native}`, else url key.
- `sameIssue`: never across providers; ClickUp matches on native id **or** normalized custom id **or** IssueKey. **Caveat:** a custom-id-only bind (no API key) and a later native-id-only bind (URL) do **not** dedupe until one side has both ids from resolve. L1 accepts this; recovery is `/link clear` + relink after key is configured. `UpsertIssue` merge is pairwise only — a later resolve of one row does not collapse an existing duplicate.
- `DisplayRef()`: custom id if present, else native id, else URL.
- `PRBodyLine()`: `Fixes DEV-42` or `Fixes 9hx` or `Fixes <url>` — **link convention only**, not merge automation (ClickUp does not auto-close from GitHub keywords).
- `ParseClickUpIssueRefs(text, prefix)`:
  - Strip Discord `<>` wraps.
  - URLs (verified shapes):
    1. `https://app.clickup.com/t/{taskId}` — single path segment after `/t/` → native id (alphanumeric, not pure digits-only if that would steal GitHub — see `/link` chain).
    2. `https://app.clickup.com/t/{workspaceId}/{CUSTOM-ID}` — two segments; second must match `PREFIX-N` shape → set `CustomID` + `WorkspaceID` from path (hands resolve `team_id` for free). If second segment is not `PREFIX-N`, **do not bind** (avoid treating garbage as a task id).
  - If `prefix != ""`, bare `PREFIX-N` with **that prefix only** (not Linear’s any-team regex).
  - Do **not** match speculative `/t/p/…` unless verified later.
- **Query parsing for `/unlink` / `FindIssue` / `RemoveIssue`:** today `RemoveIssue`/`FindIssue` call `parseLinearQuery` first, which claims any `PREFIX-N` as Linear; cross-provider `sameIssue` then never matches a ClickUp-bound row, so `/unlink DEV-42` silently fails. **Plan requirement:** make ambiguous `PREFIX-N` query parsing **provider-agnostic** — build candidate targets for both Linear identifier and ClickUp custom id, and match **either** against bound issues. Removal by a human-typed display key must find whatever is bound under that key. (Prefer fixing in `sessionstore` query helpers over a bot-only `RemoveIssueMatching` that multiplies call sites.)
- `IssueTitlePrefix` / status lines / prompt: treat ClickUp like Linear (show state + title when known).

## Package `internal/clickup`

Thin REST client (mirror `internal/linear`):

```go
type Client struct {
    APIKey string
    Base   string // default https://api.clickup.com  (paths are /api/v2/… — do NOT double /api)
    HTTP   *http.Client // 15s timeout
}

type Task struct {
    ID          string
    CustomID    string
    Name        string
    URL         string
    Status      string
    Description string // markdown preferred
    ListID      string
    WorkspaceID string
}

func (c *Client) GetTask(ctx, taskID string, opts GetOpts) (Task, error)
// opts: CustomTaskIDs bool, WorkspaceID string, Markdown bool

func (c *Client) ListTasks(ctx, listID string, limit int) ([]Task, error)
// order_by=updated, include_closed=true
```

Implementation notes:

- Cap response body (1 MiB) like Linear.
- Map HTTP non-2xx to `clickup: HTTP N: …` truncated.
- `GetTask`: if resolving a custom id, set `custom_task_ids=true&team_id=…` (workspace required; fail clearly if missing).
- No GraphQL; pure JSON.
- Unit tests with `httptest.Server` only — no live network.

## Bot surface

| Existing | Change |
|----------|--------|
| `bindLinearIssuesFromText` | Filter out refs whose team key equals project `customIdPrefix` when ClickUp claims that prefix (see Coexistence) |
| `bindClickUpIssuesFromText` | New: enabled + prefix/URL parse + resolve |
| Task pipeline after parse | Call GH → Linear (filtered) → ClickUp when respective flags on |
| `resolveClickUpIssues` | New: fill ClickUpID, CustomID, Title, State, URL, ListID, WorkspaceID |
| `/link` / `/unlink` | Reorder: try ClickUp URL / configured PREFIX-N / native id **before** Linear-disabled refusal. Native `/link 9hx`: resolve-or-refuse (do not bind an unverified opaque token). Pure digits stay GitHub-first (`/link 123` → `#123`). Unlink uses provider-agnostic PREFIX-N matching (see Session model) |
| `issueBindingPromptMode` | Branch for ClickUp: state/title lines; PR/commit body lines; “do not invent other ticket ids”; **do not** tell the agent to call ClickUp APIs; **do not** claim Linear’s GitHub integration drives ClickUp state |
| `StartFix` | `FixKindClickUp`; `ErrClickUpDisabled`; `FindByClickUpIssue`; `BuildClickUpFixPrompt` |
| Active-fix annotation | Sibling of `ActiveFixLinearIssues` for web FIXING chip |

Fix prompt template (parity with Linear):

```
Fix ClickUp task {display}: {title}
URL: {url}
Status: {state}

{description}

Bind this task (Fixes). Put {display} in the PR title and body
(Fixes {display}). Do not call ClickUp APIs. Do not merge.
```

Direct-to-primary: same Fixes lines in **commit** message (existing `issueBindingPromptMode` direct path).

## Web surface

| Route | Behavior |
|-------|----------|
| `GET /projects/{p}/clickup` | List tasks from `listId` (404 if disabled; error page if no key). Gates: mux `requireAuth` + in-handler `ensureProjectAccess` |
| `GET /projects/{p}/clickup/{id}` | Detail by native or custom id; sessions hub via `FindByClickUpIssue`. Same gates |
| `POST /projects/{p}/clickup/{id}/fix` | Fix with Grok. Gates: `requireFeature("startSessions")` + `requireMember` + bot `requireCanStartFix` (builder-class ship caps). Mirror Linear fix route registration exactly |

Nav: workspace sidebar link **ClickUp** when `ProjectClickUpEnabled` (mirror Linear).

Templates: clone `linear_issues` / `linear_detail` → `clickup_*`; FIXING chip from active Fixes sessions.

Config POST: `/config/projects/{name}/clickup` via `projectConfigRedirect`; audit detail = project name + enabled only.

Search (`GET /search`): fold ClickUp-bound sessions by `IssueKey` like Linear (no live ClickUp search in L1 — only session store). Visibility still applied before ranking.

## Security / audit

- **Pages:** `requireAuth` + `ensureProjectAccess` (not auth-only).
- **Fix POST:** `requireFeature("startSessions")` + `requireMember` + `requireCanStartFix` in bot; hidden button is not a gate.
- **Audit:** fix-start / link actions with `detail.provider=clickup` and id fields only; **no** task title/description (eng text can be sensitive).
- **API key:** never in Discord, never in audit, never in SSE; form write-only with `APIKeySet` badge.

## Phased delivery

### PR C1 — model + client + config (no UX)

- `internal/clickup` client + tests (`order_by`, `include_closed`, custom_task_ids)
- `ProjectClickUpConfig` + accessors + env + `config.example.json`
- `TrackedIssue` ClickUp fields + parse (both URL shapes) + sameIssue + PRBodyLine + title prefix + provider-agnostic PREFIX-N query match for Remove/Find + tests
- Load warning when `customIdPrefix` equals Linear `teamKey` (prefix-parse disabled)

### PR C2 — Discord bind + prompt + /link

- bot Linear filter when ClickUp claims prefix
- bind/resolve on task text
- `/link` reorder + resolve-or-refuse native id + `/unlink` via shared query match
- `issueBindingPrompt` ClickUp branch (no Linear-integration wording)
- status/brief lines

### PR C3 — Web list/detail + Fix

- routes with exact Linear gating pattern, templates, nav
- `StartFix` ClickUp kind + tests (mirror `TestFixLinear*`)
- FIXING annotation

### Optional shrink (if collision work slips)

Ship C1 + C3 + URL-and-`/link`-only binding first; free-text `PREFIX-N` parse becomes a fast-follow. That parse is the entire Linear collision surface. Default plan still ships free-text with the bot filter because `@Grok fix DEV-12` is the headline UX for ClickUp-primary teams.

### Later (not this plan)

- **L2:** comment on task / optional complete comment / multi-list / create task
- **L3:** webhooks
- **L4:** write status transitions (opt-in mapping)

## Simpler alternatives considered

| Alternative | Rejected because |
|-------------|------------------|
| Agent-only: tell Grok to use ClickUp MCP/API | No durable bind, no list UI, secrets in agent env, violates “bot owns deterministic ops” |
| Only store URL as free text on goal | Loses Fixes/Refs, FindByIssue reuse, PR body contract, max-5 upsert semantics |
| Generic “external ticket” provider | Premature; Linear + ClickUp differ on id shapes and list scoping |
| Finish Linear L2 before any ClickUp | Different customers; L1 is small and parallelizable if scoped tightly |
| Parse all `PREFIX-N` as ClickUp when enabled | Collides with Linear; false binds |
| URL-only L1 (no free-text PREFIX) | Valid shrink if filter work is delayed; not the default because bare `DEV-12` is the main chat UX |

## Test plan (acceptance)

1. Config: enable ClickUp without key → list page shows “no API key”; bind from URL still stores identifier without resolve.
2. Resolve: custom id with workspaceId fills title/status/url/native id; bad id logs warn, bind keeps bare id.
3. Parse: with `customIdPrefix=DEV`, `fix DEV-1` → **one** ClickUp Fixes bind (not also Linear); `ENG-1` still Linear if Linear on.
4. URL native: `https://app.clickup.com/t/9hx` binds native id.
5. URL custom: `https://app.clickup.com/t/1234567/DEV-9` binds CustomID=DEV-9 + WorkspaceID=1234567 (not workspace-as-task-id).
6. `/link` / `/unlink DEV-9` / clear work for ClickUp-bound rows; Linear-off + ClickUp-on `/link DEV-9` does **not** say “Linear is not enabled”.
7. `/link 9hx` with key: resolve success binds; resolve fail refuses (no garbage bind). `/link 123` remains GitHub `#123`.
8. Prompt contains `Fixes DEV-1` (or native) and “do not call ClickUp”; no Linear GitHub-integration line for ClickUp-only issues.
9. Web Fix creates/reuses session, binds Fixes, redirects; disabled → 400; no project access → same as other project pages.
10. List page includes closed tasks (`include_closed`) and is ordered by updated.
11. `go test ./internal/clickup ./internal/sessionstore ./internal/bot ./internal/config ./internal/web -count=1` green.

## Open questions (defaults chosen)

1. **Multi-list?** L1 one `listId`. Default: yes, one list.
2. **PR body automation?** Emit `Fixes {display}` for human/search convention; no auto-close expectation.
3. **Native id `/link 9hx`?** Allowed when ClickUp enabled **and** API resolve succeeds; refuse on resolve failure. Never free-text auto-parse. Pure digits stay GitHub.
4. **Prefix equals Linear teamKey?** Load warn; bare id → Linear only; ClickUp free-text prefix-parse off.

## Scrutiny history

Advisor scrutinize (Fable, xhigh) on the first draft → **revise, then ship**. Findings folded into this revision:

| # | Severity | Finding | Resolution in this doc |
|---|----------|---------|------------------------|
| 1 | Major | Coexistence table ignored that Linear parses *any* PREFIX-N | Bot-layer filter; Load warn only when prefix == teamKey (prefix-parse disabled) |
| 2 | Major | `/unlink` / FindIssue fail for ClickUp custom ids via `parseLinearQuery` | Provider-agnostic PREFIX-N query match; `/link` reorder before Linear-disabled refusal |
| 3 | Major | Missing `/t/{workspace}/{CUSTOM-ID}` URL shape | Both URL forms specified; drop speculative `/t/p/…` |
| 4 | Minor | Custom vs native id dual rows without resolve | Documented caveat + `/link clear` recovery |
| 5 | Minor | Wrong “generic secret walk” claim | Read-time `ProjectClickUpAPIKey` + form/audit patterns named |
| 6 | Minor | List API defaults hide closed / wrong order | `order_by=updated`, `include_closed=true` |
| 7 | Minor | Vague web gating | Exact Linear gates: `requireAuth` + `ensureProjectAccess`; fix = feature + member + bot caps |
| 8 | Nit | Base URL double `/api` | Base `https://api.clickup.com`, paths `/api/v2/…` |
| 9 | Nit | `/link 9hx` / pure digits | Resolve-or-refuse; digits → GitHub first |

## Success metric

A project with only ClickUp (no Linear) can: configure key+list, see tasks in web (including recently completed), Fix from a task, `@Grok fix DEV-12` (or either URL form) binds **one** ClickUp task, `/unlink DEV-12` removes it, and the opened PR/commit carries `Fixes DEV-12` without any ClickUp write API from the agent.

### Implementation advisor (post-code)

| # | Severity | Finding | Resolution |
|---|----------|---------|------------|
| 1 | Major | Bare PREFIX regexes match inside URLs → dual bind / reverse theft | Mask URLs before bare parse; never drop Linear URL refs; drop bare Linear matching ClickUp-URL custom ids when ClickUp enabled |
| 2 | Major | Search ignored CustomID/ClickUpID | Score + render ClickUp branch in issueSearchHits |
| 3 | Minor | Missing Load warn on prefix==teamKey | config.Load stderr warn |
