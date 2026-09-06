# Design: Today / Waiting-on-you

| Field | Value |
|-------|--------|
| **Status** | Accepted (plan scrutinize 2026-09-06: fix-then-ship findings applied; ready to implement) |
| **Date** | 2026-09-06 |
| **Repo** | `github.com/acoshift/grokwork` |
| **Audience** | Implementors of this train |
| **Related** | [research-single-platform.md](research-single-platform.md) §1, [design-web-primary.md](design-web-primary.md) D4, `TODO.md` (“needs you” personal feed), `internal/inbox`, `internal/web/inbox_page.go`, `internal/web/search.go`, `internal/bot/case_board.go`, `internal/bot/ship_board.go`, `internal/web/reviews.go`, `internal/web/live.go` |

The research note’s #1: the first tab is a **personal ranked queue**, not a project launcher. This design is the PR plan for that slice.

---

## Implementation scrutinize contract (mandatory)

This is part of the plan, not a suggestion. A PR that skips it is not done.

1. **Each PR / step** in [PR Plan](#pr-plan) is its own change. When that step’s code is green (`go test` of the packages it touches), load `/scrutinize` and run it on **that step’s full change** (worktree vs primary tip — not a summary). Fix blocker and major findings before starting the next step. Verdict must be `ship` (or `fix-then-ship` after the fixes land).
2. **Before the train is marked green** (last PR ready to land, or the whole stack about to go to `main`): run `/scrutinize` again on **all changes in the train together**. Cross-PR seams (nav counts × SSE × ACL × SLA clock) are exactly what a per-step review will miss.
3. Do not treat a one-line LGTM, a passing test list, or “the design said so” as scrutinize. Follow the skill: intent → trace real paths → verify each claim → report.
4. UI PRs still need browser (or closest substitute: `GROKWORK_WEB_PREVIEW` / httptest of the page + partial) verification of `/today`, `/projects/{p}/today`, `/`, `/inbox`, `/reviews`, `/ship`, `/cases` — shared nav and live regions.

`AGENTS.md` already requires scrutinize-then-ship on this repo. This section makes it load-bearing **inside the train**, not only at the final commit.

---

## Problem

A signed-in teammate opening grokwork lands on `/`, a **project launcher** (`internal/web/project_home.go` `home`). What needs *them* is scattered:

| Need | Where it lives today | Why that is not the morning tab |
|------|----------------------|----------------------------------|
| Team review waiting on me | `/reviews` + nav `reviews` pill (`pendingReviewCount`) | Team-shaped list, not ranked with everything else |
| Unanswered `DECISION:` | Session page / Discord buttons (`Entry.OpenQuestions`) | No list. `fpHistory` / `appendSessionLiveChrome` do not mention questions, so even the session live region can miss an answer |
| CI red on a PR I own | `/ship?state=failing` | Lead board; no `owner=mine` |
| SLA-breached case I am on | `/cases?owner=mine&sla=breached` | Two filters, and only if you already opened Cases. Escalated-to-me without a breach stays on the case board — not Today. |
| GitHub `review-requested` | GitHub notifications | `ghpr` has no requested-reviewers helper; not wrapped |
| Run finished / review ping | `/inbox` (kinds `run.done`, `review.requested`) | Chronological **feed**. Mark-all-read hides a row that is still true (CI still red, question still open) |

`/inbox` already has an unread pill (`navCounts.Inbox`, `inboxUnreadVisible`, `sse:inbox`). [design-web-primary.md](design-web-primary.md) P4 shipped the store + page; the remaining “unread badge” item in that doc is stale. Inbox copy even says “Things that need you” — but the writers are event-shaped, and GET does not mark read. That is the right feed. It is the wrong *home*.

Linear’s morning is Inbox (events) → My Issues (derived current state). We have the first. This train is the second.

---

## Goal

A signed-in human can open **one URL**, see everything that still needs them across projects they can access, ranked, and click through to the session / PR / case. No new ticket type. No GitHub/Linear clone. Derived from `sessionstore.Entry`, `reviewstore`, and case SLA computed at render time.

Non-goal for this train: new inbox writers (CI / SLA / decision pings). Those are research items #4 / #7 / #8. Today is the join; Inbox stays the log.

---

## Alternatives (rejected)

| Option | Why not |
|--------|---------|
| **Replace `/` with Today** | Project-first IA is load-bearing (`/` is the launcher; `IsDashboard`, `id="page-home"`, phone tab “Projects”). Mixing a personal queue with the project grid fights both live-region triggers (`sse:dashboard, sse:ship, sse:history`) and the “pick a project first” operator path. Revisit only after Today exists. |
| **Waiting-on-you strip on `/` only** | A strip under ten project cards is not the first tab. Fine as a later teaser; not the product. |
| **Fold derived rows into `/inbox`** | Inbox is append-only JSONL + a cursor (`internal/inbox`). Mark-all-read would hide a still-red CI. Mixing derived state into a feed is how GitHub notifications rot. Keep the split. |
| **New `sse:today` domain in v1** | Per-connection fingerprints already exist for inbox (`fpInbox` is explicitly *not* stuffed into `computeLiveRevs` so `TestLiveRevsStableAndChange` stays host-wide). Today can listen to the domains that already move, plus one chrome-fp addition for OpenQuestions. A fourth per-user walk on every 2s tick is a later optimization if the multi-trigger is noisy. |
| **GitHub `review-requested` in PR 1** | No `ghpr` helper; each call is network; nav counts skip remote on the global shell (`partialNavCounts` `remote=1`). Skippable last PR. |
| **Teach `SameActor` about aliases** | Forbidden (`AGENTS.md`). Involvement is `==` / `slices.Contains`, plus `reviewerLookupIDs` (canonical + Discord subject) for reviewstore — the reviews page already does this. |

---

## Key Decisions

1. **New route `/today`, do not replace `/`.** Global shell, first nav link and first phone tab. Workspace twin `/projects/{p}/today` (scoped layout, no Project column). `?project=` on `/today` is a **data filter** like `/ship` and `/search` — **do not** add a `navScopeFromURL` rule for it (that would scope the sidebar from a query param, which those pages deliberately do not).
2. **Derived current state, one row per unit.** A session with both a red PR and an open question is one row with two reason chips, not two rows. Team-review requests are keyed by PR (`reviewstore.Request`) and stay their own rows (they are not `Entry`s).
3. **Visibility before ranking.** Copy search and `ListShipBoardAmong` / `CaseBoardQuery.Among`: drop hidden projects *before* scoring, counting, or capping. A hidden project’s hotter row must not occupy a slot or inflate the nav pill. Admins see all *projects* but still only **their** rows (Today is never “everything in the instance”).
4. **Empty actor → empty page.** `fixActor(ctx).ID == ""` (auth off, or signed-in chrome without an id) matches nothing, same as `CaseBoardQuery.ViewerID` and `/inbox`. Do not dump the host’s work onto an anonymous Today — that is a read primitive.
5. **No `lastPromptPreview` on Today.** Ship board falls back to the last prompt when `Goal` is empty. Today must not: prompts can carry customer data, and the page is the morning surface. Empty goal → “(untitled)”.
6. **Walk sessions for CI, not the merged ship board.** `mergeShipRows` attributes `OwnerID` to the “primary” unit of a multi-session PR. Filtering that row by owner would hide a co-owner’s failing PR or pin it on the wrong person. Today reads `Entry.PRs` on the involved session itself (`NormalizePRs` + `checksLookFailing`).
7. **SSE: compose existing domains; fingerprint OpenQuestions.** `#live-today` uses `class="live-region"` + `hx-target="this"` + `hx-select="unset"` + `hx-trigger="sse:ship, sse:cases, sse:history, sse:inbox"`. Extend `appendSessionLiveChrome` with each open question’s `id|status` so answering a `DECISION:` moves `sse:history`. SLA clock-crossings already move `sse:cases` (`fpCases` folds `SLABreached` + `nextSLARecompute`). CI moves `sse:ship`. New team-review pings move `sse:inbox`. Do **not** add `sse:today` in this train.
8. **Inbox stays the bell.** Do not hide `/inbox`. Today’s nav pill is the derived waiting count, independent of the unread cursor. Inbox copy can stay; Today takes the “needs you now” heading.
9. **Cap and say so.** `todayKindCap = 40` total rows (not per-kind — kinds are reasons on a row). Print `shown / matched` when truncated, same honesty as `searchKindCap`.
10. **GitHub requested-reviewers is PR 5, skippable.** v1 is complete without it.
11. **Phone tabs keep Projects (global) and Overview (workspace).** Desktop `.nav-links` is `display:none` ≤920px; the brand is not a link; `ws-back` goes to `/` not the project overview. Five-tab budget: drop Sessions from the phone bar only (it stays in the desktop sidebar). See Nav section.

---

## What “needs you” means

Actor id is `fixActor(ctx).ID` (canonical-at-mint). A unit **involves** the viewer iff any of:

- `OwnerID == id`
- `slices.Contains(CoOwnerIDs, id)`
- `EngineerID == id` (cases)
- `slices.Contains(WatcherIDs, id)` (web watch stores `actor.ID`; Discord `/watch` stores the snowflake — canonical Discord is the bare snowflake, so `==` still hits)

Plus, independently of involvement on the unit:

- `reviewstore.ListForReviewerAny(reviewerLookupIDs(id), projectFilter, StatusPending)` after `reviewRequestVisible`

**Skip:**

- `Entry.IsPRAsk()` (same as search)
- Non-cases with `sessionstore.IsTerminalLabel(EffectiveLabel())` — leftover questions on a done/abandoned eng session are archaeology.
- Cases: `IsCaseClosed()` only (ignore session label; a case in `fixing` may still say `needs_review`).
- Open questions with `Status == "answered"` or `"dismissed"`. Empty status and `"open"` both count (decision.go only special-cases `"answered"`).
- PRs whose `ghpr.IsTerminal` state is merged/closed
- Checks that do not `checksLookFailing`

**Reasons** (highest wins for sort; all that apply render as chips):

| Rank | Reason | Source |
|------|--------|--------|
| 0 | `sla` | `computeCaseSLA` via the case board path — `SLABreached` on an involved open case |
| 1 | `decision` | unanswered `OpenQuestions` on an involved unit |
| 2 | `ci` | non-terminal PR on that unit with `checksLookFailing` |
| 3 | `review` | pending grokwork team review for this reviewer |

There is **no** generic `case` or “you own this session” reason. Support who filed twenty open cases would otherwise open Today and see the whole pipeline; that is `/cases?owner=mine`. Engineers find assigned work the same way. Today is **attention** (late, blocked on a human, CI red, review requested), not a workload board (research #5).

A breached case the viewer is involved in is `sla`. An open case with an unanswered `DECISION:` is `decision`. A case whose tracked PR is red is `ci`. Those are enough.

---

## Shape

### Query

```go
type WaitingQuery struct {
    ActorID    string   // empty → empty result
    LookupIDs  []string // reviewerLookupIDs; never nil when ActorID set (at least {ActorID})
    Project    string   // optional exact project filter
    Among      []string // nil = unrestricted (admin / tests); empty slice = match nothing
}
```

`Among` copies `CaseBoardQuery.Among`. Web non-admins pass `filterProjectNames`. A named `Project` that is not in `Among` (when Among is set) returns empty, same as `ListShipBoardAmong`.

### Row

```go
type WaitingReason string // sla | decision | ci | review

type WaitingRow struct {
    Kind       string // "session" | "case" | "review"
    ThreadID   string
    Project    string
    Title      string // Goal / CustomerTitle / review PR title; never a prompt
    CaseKey    string
    URL        string // root-relative session or /prs/... path (inboxSessionPath / inboxPRPath)
    Reasons    []WaitingReason // sorted by rank, unique
    SortRank   int             // min rank among Reasons
    UpdatedAt  string
    // DecisionText is the first unanswered question, truncated (200 runes).
    // Only set when Reasons contains "decision". Page may show it; audit GET
    // pages already render session goals — same residual.
    DecisionText string
    PRNumber     int
    PRURL        string
    Owner        string
    GHOwner, GHRepo string
}
```

No new `Entry` fields. No `Entry.clone` change.

### Join (`internal/bot/waiting.go`)

`ListWaitingOnYou(q WaitingQuery) WaitingBoard` where `WaitingBoard` is `{Rows, Matched, Shown, Project, Cap}`.

Algorithm (one pass + one reviewstore list):

1. If `ActorID` is empty → empty board.
2. Build `allowed` from `Among` (nil = no gate).
3. `for _, listed := range b.sessions.List()`:
   - ACL: `allowed` then `Project` filter. **Before** involvement. Hidden rows are not scored.
   - Skip PR-ask / closed-case / terminal-non-case as above.
   - If not involved, continue.
   - Collect reasons from that entry. SLA: `b.caseSLAAt(e, now)` — **do not** call `CaseSLAFor` from tests (`CaseSLAFor` already exists and uses `time.Now()`). Public `ListWaitingOnYou` wraps `ListWaitingOnYouAt(q, time.Now())`. One definition of breach (`computeCaseSLA`).
   - If reasons non-empty, append one `session` or `case` row (`IsCase()` chooses kind). Kind is display only; it does not add a `case` reason.
4. Pending team reviews via `b.Reviews().ListForReviewerAny(q.LookupIDs, q.Project, reviewstore.StatusPending)` (`Bot.Reviews()` already exists). Skip if the request’s project fails `allowed`. A review row and a session row for the same PR may both appear (different acts: record a team verdict vs open the unit). Dedup review rows by `owner/repo/number`. Do not render `Request.Note` (free text).
5. Sort: `SortRank` asc, then `UpdatedAt` desc, then `ThreadID` for stability.
6. `Matched = len(rows)`; if `> todayKindCap` clip to cap and set `Shown`.

Do not call `ListCaseBoardQuery` as the case source (Owner and SLA filters AND together; pipeline grouping is wasted).

Involvement helper `unitInvolves(e Entry, actorID string) bool` lives next to `caseIsMine` (which is owner/co/engineer only). Watchers are Today-specific; do not silently expand `caseIsMine` — the case board’s `mine` filter would start including watchers, which changes `/cases?owner=mine`.

### Web

| Piece | Where |
|-------|--------|
| `GET /today` | `today.go` `todayPage` — `requireAuth`, global shell |
| `GET /projects/{name}/today` | same handler; `ensureProjectAccess`; `pageData.Project` set so the template drops the Project column (`TestShipPartialScopedLayout` pattern, `scoped=1` on the live-region URL) |
| `GET /partials/today/list` | `live-region` fragment `today_list` |
| `navCounts.Today` | `partialNavCounts` — `len` of waiting rows (pre-cap `Matched`, so the pill equals “how many need you”, not “how many we drew”). Local, no GitHub. |
| Routes | `web.go` `hime.Routes` `"today": "/today"`, `"partial.today.list": "/partials/today/list"` |
| `pageData.IsToday` | `assertNavActive` matches `class="{{if .IsToday}}active{{end}}">Today</a>` |
| Template | `today.tmpl` — `id="page-today"` |

Nav (desktop `.nav-links`): Today **first**, then Projects, then the rest.

Phone tab bar is the only global nav on viewports ≤920px (`.nav-links { display: none }`; `#side-nav { display: contents }`; the brand mark is **not** a link). Dropping Projects from the tab bar would make the launcher unreachable. Five-tab budget, so Sessions moves off the phone bar (it remains in the desktop sidebar):

| Scope | Phone tabs (order) |
|-------|---------------------|
| Global | **Today**, Projects, Ship, Cases, Reviews |
| Workspace | **Today** (`/projects/{p}/today`), Overview, Ship, Cases, Reviews |

Workspace Overview cannot be dropped either: `ws-back` goes to **all projects** (`route "home"`), not to `/projects/{p}`.

`loadNavCounts` must refetch on `sse:history` as well as ship/cases/inbox (`TestNavCountsLiveOnSSE` currently pins the three). Today’s pill moves when a decision is answered (history chrome) even if ship/cases/inbox did not.

JS `navScopeOf()` / `navScopeFromURL`: no new rule. `/today` is global; `/projects/{p}/today` is already covered by `seg[0] == "projects"`.

### Live region

```html
<div class="live-region" id="live-today"
     hx-get="…/partials/today/list?project=…"
     hx-trigger="sse:ship, sse:cases, sse:history, sse:inbox"
     hx-target="this" hx-select="unset"
     hx-swap="innerHTML show:none focus-scroll:false">
```

`appendSessionLiveChrome`: add `|q|{id}|{status}` for each `OpenQuestion` (status empty → `open`). Session page already wants this; answering a decision should refresh more than Today.

### AuthZ

- Pages: `requireAuth` + (scoped) `ensureProjectAccess`. Unauthorized `?project=` on the global page: ignore the filter (same as `/reviews` ignoring unauthorized `?project=` when not in a workspace) — **do not 403 the whole Today**, or a stale bookmark becomes a lockout. Workspace path still 403s (`forbiddenProject`).
- No new capability. Viewers with project membership see their rows. A viewer who owns nothing sees the empty state.
- Token actors (`token:`) are not web session users; ignore.

### Empty states

- Not signed in: existing `requireAuth` redirect to login.
- Signed in, zero rows: “Nothing needs you.” + links to Inbox and Projects.
- Auth off: `UserID` empty (`basePage` early return) → empty board, same sentence. Do not special-case “show everyone”.

### What we will not put on the page

- Inbox items (link to `/inbox` in the header if `inboxUnreadVisible > 0`)
- Running-but-healthy sessions
- Other people’s queues
- Local paths, prompt text, stacks
- A “mark done” control (state is derived; act on the session/PR/case)

---

## Tests (acceptance)

Bot (`internal/bot/waiting_test.go`):

1. Empty `ActorID` → 0 rows (not “all units”).
2. Hidden project (`Among` omits it) is not counted and does not occupy the cap. A hotter hidden SLA case cannot evict a visible decision.
3. `Among: []string{}` → 0 rows.
4. Named `Project` not in `Among` → 0.
5. Owner / co-owner / engineer / watcher each produce a row **when a reason exists**; a stranger does not. An involved open case with no SLA breach, no open question, and no failing PR produces **zero** rows (Today is not `/cases?owner=mine`).
6. `caseIsMine` unchanged: watcher-only is **not** `mine` on the case board.
7. PR-ask skipped. Terminal non-case skipped. Closed case skipped.
8. Answered / dismissed questions skipped; empty status counts.
9. Two reasons on one unit → one row, both chips, `SortRank` is the higher-priority reason.
10. SLA: clock-crossed involved case appears without any store write. Tests call `ListWaitingOnYouAt(q, frozenNow)` (the same `now` `case_sla_test.go` already threads into `computeCaseSLA`). Do not sleep. Do not use `CaseSLAFor` in tests — it calls `time.Now()`.
11. Failing checks on a co-owned session appear even when a second session on the same PR has a different `OwnerID` (the merge-row trap).
12. Cap: 41 involved units → `Matched=41`, `Shown=40`, last row is the 40th by sort, not an arbitrary hidden one.
13. `lastPromptPreview` is never the Title.

Web:

14. `TestPagesRender` includes `/today` → 200, `id="page-today"`. `/projects/proj/today` too.
15. `assertNavActive` for Today on both URLs. Global desktop nav: Today link **before** Projects. Class attribute last, bare label. Global phone `#tab-bar` still contains a Projects tab (launcher must remain reachable ≤920px). Workspace `#tab-bar` still contains Overview.
16. Partial `/partials/today/list` has no `<nav`, no `sse-status`, no htmx script.
17. Scoped live-region URL carries the project (and `scoped=1` if that is how ship hides the column). Global `?project=` does **not** change `data-scope` on `#side-nav`.
18. Containment: member of `public` does not see a `secret` project’s goal/case key in `/today` or the nav pill (pill uses `Matched` after ACL).
19. Nav JSON includes `"today"` even at 0 (`TestNavCounts` inbox pattern).
20. `TestNavCountsLiveOnSSE` now also pins `history` as a local count trigger.
21. Unauthorized `?project=other` on `/today` does not 403 (filter ignored); `/projects/other/today` does.
22. `appendSessionLiveChrome` fingerprint changes when an OpenQuestion status flips (unit test on the helper or via `fpHistory`).

`go test ./internal/bot ./internal/web ./internal/sessionstore ./internal/reviewstore -count=1`

---

## Risks

| Risk | Mitigation |
|------|------------|
| SSE multi-trigger refetches Today too often (dashboard-like churn) | Do **not** listen to `sse:dashboard` (includes elapsed). The four named domains are the ones that change waiting-ness. |
| `fpHistory` grows because of `|q|` on every session | Cheap; already serializes PRs/issues/dossier. Cap is 15 questions per entry. |
| Extra `sessions.List()` clone-walk on every Today partial / nav count | Same cost as `fpShip` / `fpCases` (they already clone the store every tick). Do not listen to `sse:dashboard`. Do not add a fourth per-connection walk in v1. |
| Nav pill ≠ rows because cap | Pill uses `Matched`, page prints the cap. Same as search. |
| WatcherIDs comment still says “Discord snowflakes” | Web watch already stores canonical ids. Involvement uses `==`. No migration. |
| Auth-off empty Today looks broken | Empty copy names signing in / identity; operators who run auth-off still have `/ship` and `/cases`. |
| Decision text on a case is customer wording | Same residual as the session page. Truncate. Do not put it in audit, nav JSON, or Discord. |

---

## Open Questions

None that block implementation. Locked above:

- Route vs replacing `/` → new `/today`.
- Inbox writers → out of this train.
- GitHub wrap → PR 5 skippable.
- New SSE domain → no in v1.

---

## PR Plan

Each PR is independently reviewable and mergeable. **Scrutinize that PR before starting the next** ([contract](#implementation-scrutinize-contract)).

### PR 1 — Join: `ListWaitingOnYou`

- **Title:** `bot: derive Waiting-on-you from sessions and team reviews`
- **Files:** `internal/bot/waiting.go`, `internal/bot/waiting_test.go`. Use existing `caseSLAAt` / `computeCaseSLA` (do not add a second SLA policy). **Do not** change `caseIsMine`. `checksLookFailing` is already in package `bot` (unexported; call it).
- **Depends on:** nothing
- **Does:** Query type, involvement helper, one-pass join, sort, cap, tests 1–13. No HTTP.
- **Scrutinize focus:** visibility-before-rank, empty actor, merge-row CI trap, SLA not stored, `caseIsMine` unchanged.

### PR 2 — Page + nav chrome

- **Title:** `web: add /today and /projects/{p}/today`
- **Files:** `internal/web/today.go`, `internal/web/today_test.go`, `internal/web/web.go` (routes + mux + `pageData.IsToday` + `Waiting bot.WaitingBoard`), `internal/web/templates/today.tmpl`, `internal/web/templates/layout.tmpl` (nav + tab bar), `internal/web/web_test.go` / `nav_counts_test.go` as needed for page id and active class. **Not** nav count JSON yet if that keeps the PR small — prefer it here so the pill ships with the page.
- **Depends on:** PR 1
- **Does:** Handlers, ACL, scoped vs global, empty states, tests 14–18, 21. **Must** wire `navCounts.Today` in this PR (pill without a page, or a page without a pill, is a half-ship). Admin / auth-off: `Among = nil` (same as `listShipBoardVisible`); empty `fixActor` still yields 0 rows. Non-admin: `Among = filterProjectNames(ctx)`.
- **Scrutinize focus:** `navScopeFromURL` **un**changed for `?project=`; `assertNavActive` string; partials have no layout chrome; containment of titles.

### PR 3 — Live region + OpenQuestion fingerprint + nav SSE

- **Title:** `web: live-refresh Today from ship/cases/history/inbox`
- **Files:** `today.tmpl` live-region, `live.go` `appendSessionLiveChrome`, `layout.tmpl` `loadNavCounts` history trigger, tests 19–20, 22.
- **Depends on:** PR 2
- **Does:** Multi-trigger live region; chrome fp includes questions; nav pill refetches on history.
- **Scrutinize focus:** no `sse:dashboard`; `fpInbox` still per-connection and `computeLiveRevs` still host-wide; answering a decision moves history rev; SLA still rides `sse:cases` without a store write.

### PR 4 — Inbox header link + copy

- **Title:** `web: point Inbox at the feed, Today at current needs`
- **Files:** `today.tmpl` (unread inbox count + link), maybe `inbox.tmpl` subcopy so the two pages do not both claim to be “the” needs-you surface.
- **Depends on:** PR 2
- **Does:** If inbox unread > 0, Today header links to `/inbox`. No new writers.
- **Can land in PR 2** if it stays a few lines. Split only if PR 2 is already large.

### PR 5 — GitHub `review-requested` wrap (skippable)

- **Title:** `web: include GitHub review-requested on Today`
- **Files:** `internal/ghpr` (one `gh` call, cached, failure → omit, never 500 the page), `waiting.go` extra reason `gh_review` rank 3.5 (after team review).
- **Depends on:** PR 1–2
- **Does:** Only if `identity.GitHubFor(actor)` is ok. Bound work: ship-board PRs the viewer is involved in, **or** a single `gh` search. Must not walk every repo on every Today load. Nav counts stay local (`remote=1` already exists for issues/errors — do **not** put this on the pill in v1; pill stays derived-local).
- **Skip** without blocking the train.

### After PR 3 (or 4) lands on `main`

Run **full-train `/scrutinize`** on the accumulated diff vs the pre-train tip. Then mark this design **shipped** (the plan is already Accepted).

Do not mark the design green from a per-PR ship verdict alone.

---

## Plan scrutinize (2026-09-06)

Intent: a personal derived queue at `/today`, not a new tracker and not a replacement for `/`.

Traced against: `home` / `navScopeFromURL` / `navScopeOf`, `inbox` + `fpInbox`, `listShipBoardVisible` / `mergeShipRows` / `checksLookFailing`, `ListCaseBoardQuery` / `caseIsMine` / `CaseSLAFor`+`caseSLAAt`, `pendingReviewCount` / `reviewerLookupIDs` / `Bot.Reviews()`, `appendSessionLiveChrome` (no OpenQuestions today), `requireAuth` auth-off passthrough, `basePage` auth-off empty `UserID`, phone `.nav-links { display: none }` and unlinked `.brand`, `ws-back` → `route "home"`.

| Sev | Finding | Resolution |
|-----|---------|------------|
| Blocker | Generic `case` reason listed every involved open case. Support Today would be the case board. | Dropped. Only `sla` / `decision` / `ci` / `review`. Test 5 pins the zero-row case. |
| Major | Phone tab plan dropped Projects. Brand is not a link; `.nav-links` is hidden ≤920px. | Global phone: Today, Projects, Ship, Cases, Reviews. Workspace keeps Overview (`ws-back` is all-projects). |
| Major | “Export `CaseSLAFor`” — already exported and uses `time.Now()`. | `ListWaitingOnYouAt(q, now)` + existing `caseSLAAt`. |
| Major | `mergeShipRows` owner skew if Today filtered the ship board. | Already decided to walk `Entry.PRs`; left as a pinned test (11). |
| Nit | PR 2 “prefer” nav count. | Required in PR 2. Admin `Among=nil` called out. |

Verdict after edits: **ship** (plan). Implementation still runs the [scrutinize contract](#implementation-scrutinize-contract) per PR and on the full train.

---

## Out of scope (this train)

- Replacing `/` with Today
- Inbox writers for CI / SLA / decisions (research #4, #7)
- Sessions list `owner=mine` / load board (research #5)
- Derived standup / Discord digest (research #7 in the OS pass)
- Decision **queue** board (`/ship?blocked=decisions`) — research #8
- Evidence-gated ship, blast-radius claims, review packet
- New SSE domain, Notifier interface (still deferred in web-primary P4)
- Alias-aware `SameActor`
- Discord `/today` command (mention path stays task-shaped; web is the home)
