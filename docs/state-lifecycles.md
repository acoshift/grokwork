# State life-cycles

Current-behavior reference for every user-visible workflow state machine.

If this file and the code disagree, **the code wins** — update this file in the same change. The point of the file is to make that check cheap.

Each section lists: who owns the bits, the canonical values, the transitions, what is stored vs derived, the tests that pin it, and the traps that look like bugs.

Related user-facing guide (support only): [`support-case-guide.md`](support-case-guide.md).

---

## How to check this file

When changing a state machine, walk the matching section and confirm:

1. The constant names still match the source cited under **Canonical**.
2. Every transition still has a call site (or is still “never”).
3. The named tests still exist and still fail if the rule is broken.
4. Traps still describe live behavior, not a wish.

Quick grep for the constants:

```bash
rg 'IssueWorkStateFixing|LabelDone|PhaseClosed|ShipModeDirect|StatusInterrupted' internal --glob '*.go'
```

---

## 1. GitHub issue — tracker state + grokwork overlay

Two independent layers. GitHub owns **open / closed**. grokwork overlays **FIXING** at render time. Nothing about FIXING is written back to GitHub.

```
created (gh issue open)
        │
        ▼
   GitHub OPEN ──────────────────────────────────────────► GitHub CLOSED
        │  Fixes bind + non-terminal session                    ▲
        ▼                                                       │
     FIXING  ── session done/abandoned, /unlink, ───────────────┘
                or GitHub close
                (PR merge with Fixes #N is GitHub auto-close)
```

### Canonical

| Layer | Values | Source |
|---|---|---|
| GitHub `state` | `open` / `OPEN`, `closed` / `CLOSED` (gh JSON is usually upper) | `ghpr.IssueListOpts.State`, `ghpr.IssueInfo.State` |
| Overlay | `WorkState = "FIXING"` or empty | `bot.IssueWorkStateFixing` in `internal/bot/find_issue.go` |
| List filter `?state=` | `active` (default), `open`, `fixing`, `closed`, `all` | `defaultIssueListState` in `internal/web/workflow.go` |

### Who owns what

| Bit | Owner | Stored? |
|---|---|---|
| GitHub open/closed | GitHub | GitHub |
| `TrackedIssue` bind + `Fixes`/`Refs` keyword | grokwork session | `sessions.json` → `Entry.Issues` |
| FIXING badge | grokwork, derived | **never stored** |
| Linked PRs on the issue page | GitHub Development (`closedByPullRequestsReferences`) | GitHub |

Binding is one-way into the session. grokwork does not push issue state to GitHub except:

- **Post comment & close** on the issue page (`githubWrites`; comment body required). There is **no reopen** action in grokwork.
- GitHub’s own auto-close when a merged PR body contains `Fixes owner/repo#N`.

### Overlay rule (FIXING)

Paint FIXING iff **all** of:

1. GitHub state is still open (`strings.EqualFold(state, "open")`). Closed issues never get the overlay.
2. Some session in the same project binds this `owner/repo#N` with keyword **Fixes** (not Refs).
3. That session’s `EffectiveLabel()` is **not** terminal (`done` / `abandoned`).

Code: `Bot.ActiveFixGitHubIssues` → `Server.annotateGitHubIssueWorkState`.

**Refs does not count**, and neither does an unbound mention. Feature-plan (`StartFeaturePlan`) does **not** bind the issue at all (it would otherwise land in the Fix reuse picker). Checklist-item sessions and a `/start plan` unit that filed a GitHub issue bind **Refs**. Chat “about #42” and `/link #42` without `fix` also bind Refs.

### List filters

`state=fixing` is not a GitHub state. The list asks GitHub for `open`, then partitions:

| Filter | GitHub query | Then keep |
|---|---|---|
| **Active** (default) | `open` | all of them (idle Open **and** FIXING) |
| **Open** | `open` | `WorkState != FIXING` |
| **Fixing** | `open` | `WorkState == FIXING` |
| **Closed** | `closed` | as returned (overlay never applies) |
| **All** | `all` | as returned; FIXING still badges matching open rows |

### Transitions that move the overlay

| Event | GitHub | Overlay |
|---|---|---|
| Create issue (web `/issues/new` or `gh`) | open | empty (Open) |
| Fix / bulk Fix / `@Grok fix #N` / `/link fix #N` / close-intent wording | unchanged | **FIXING** once the session exists and is non-terminal |
| Agent run in progress | unchanged | FIXING (session `in_progress`) |
| Agent opens a PR, emits `SESSION_DONE:` | unchanged (still open) | **drops to Open** — session is now terminal `done` |
| Session labeled `needs_review` / `in_progress` / `blocked` / `open` | unchanged | FIXING stays |
| `/unlink`, `/link clear`, session reset | unchanged | drops if no other active Fixes bind remains |
| PR merges with `Fixes #N` | **closed** (GitHub) | none (closed never overlays) |
| Issue page “Post comment & close” | **closed** | none |
| Close on GitHub | **closed** | none |
| Reopen on GitHub | **open** | FIXING again only if a non-terminal Fixes session is still bound |

### Trap: Fix → open PR → badge back to Open

This is **current behavior**, not a lost bind.

1. Fix prompt: implement, push, open/update a PR, **do not merge**, put `Fixes owner/repo#N` in the PR body.
2. Remote-work prompt: when the unit of work is finished, emit `SESSION_DONE:`.
3. Host applies that as **manual** `done` (`SetLabelManual`) *before* PR bind can auto-label.
4. Auto-label would want `needs_review` for a non-draft open PR, and would even revive a *non-manual* terminal — but `LabelManual` is sticky, so it does not.
5. Overlay sees a terminal session → not FIXING. GitHub is still open → badge **Open**.
6. Default **Active** filter still shows the row (Active = every GitHub-open issue). The **Fixing** filter does not.

The bind and the PR remain on the session. Issue detail still lists the (now done) session under Sessions. Related PRs still show.

Direct-to-primary Fix is different: the bot stamps `Label=done` after a successful ship (`LabelManual=false`) and GitHub close depends on commit-message `Fixes` (no PR). Same overlay drop.

### Tests

- `TestActiveFixGitHubIssues` — Fixes counts; Refs and terminal do not (`internal/bot/find_issue_test.go`).
- `TestIssuesListShowsFixingWorkState` — Active default, open/fixing partition, overlay on detail (`internal/web/workflow_test.go`).

---

## 2. Linear issue — tracker state + overlay

Linear owns the workflow **state name** (Todo, In Progress, Done, …). grokwork overlays **FIXING** the same way as GitHub, with one difference: the overlay is **not** gated on “still open”. A Done Linear issue can still show FIXING if a live Fixes session is bound.

### Canonical

- Native: `linear.Issue.State` (Linear workflow state name).
- Overlay: `WorkState = "FIXING"` via `annotateLinearIssueWorkState` / `ActiveFixLinearIssues`.
- Bind: `TrackedIssue{Provider: linear, Identifier: ENG-123, Keyword: Fixes|Refs}`.

### Transitions grokwork will not make

The agent is told not to call Linear’s HTTP API / `issueUpdate`. State movement, if any, is Linear’s own GitHub integration when a PR title/body carries `Fixes ENG-123`.

The list is “recent issues for the team”, not an open/closed partition (`ListTeamIssues`).

### Tests

- `TestActiveFixLinearIssues`
- `TestLinearListShowsFixingWorkState`

---

## 3. ClickUp task — tracker status + overlay

ClickUp owns the list **status**. grokwork overlays **FIXING** without gating on ClickUp status (same as Linear). `Fixes DEV-42` in a PR/commit is a convention only — ClickUp does not auto-close from GitHub keywords.

List fetch uses `include_closed=true`. Overlay: `annotateClickUpTaskWorkState` / `ActiveFixClickUpIssues`.

---

## 4. Session label — team workflow on the unit

One Discord thread / web unit = one session. The **label** is the unit’s lifecycle. Empty stores as open.

```
open → in_progress → needs_review → done
                 ↘ blocked ↗
any non-closed ──────────────► abandoned
```

### Canonical

`internal/sessionstore/label.go`:

| Label | Terminal? | Meaning |
|---|---|---|
| `open` | no | no real work yet (also the empty-label default) |
| `in_progress` | no | run started, or draft-only PR, or worktree/session exists |
| `blocked` | no | waiting on a human in a stuck sense (cases: **answered** stamps this) |
| `needs_review` | no | non-draft open PR |
| `done` | **yes** | finished; FIXING overlay drops |
| `abandoned` | **yes** | won’t do; FIXING overlay drops |

Aliases (`ParseLabel`): `wip`/`working` → in_progress; `review`/`ready` → needs_review; `complete`/`merged`/`shipped` → done; `close`/`closed`/`wontfix` → **abandoned** (not done).

### Stored vs auto

- `Entry.Label` + `LabelManual` are stored.
- Auto-label (`SuggestAutoLabel` / `ApplyAutoLabel`) is derived from PR state + running flag, then written.
- **Manual is fully sticky**, including against done/abandoned. `/label auto` (or web equivalent) clears the lock and re-applies the suggestion.
- Closed **cases** freeze auto-label even without `LabelManual` (`IsCaseClosed`).

### Auto-label suggestions (when not manual, not a closed case)

| Condition | Suggests |
|---|---|
| Run starting, current `open` | `in_progress` (`ApplyAutoLabelOnRunStart`) |
| Direct-ship session, current terminal, run starting | `in_progress` (revival; PR-mode terminals do not revive this way) |
| Has a non-draft open PR | `needs_review` |
| Open PRs are all drafts | `in_progress` |
| All tracked PRs terminal | **keep current** — merged/closed PRs do **not** auto-stamp done |
| Session/worktree exists, no PR | `in_progress` |
| Nothing yet | `open` |

Will not demote `needs_review` → `in_progress` just because a follow-up run starts.

Will revive terminal → `needs_review` / draft `in_progress` **only** when an open PR appears **and** the label is not manual. `SESSION_DONE` is manual, so an open PR after a Fix run does **not** revive.

### What actually writes a terminal label

| Writer | Label | Manual? |
|---|---|---|
| Agent `SESSION_DONE:` | `done` | **yes** |
| Agent `SESSION_ABANDON:` / SoftAbandon | `abandoned` | **yes** |
| `/label done` / web Mark as done | `done` | **yes** |
| `/label abandoned` / Reset unit | `abandoned` | **yes** |
| Direct-to-primary successful ship | `done` | **no** (`LabelManual=false`) |
| Case `/close` resolution `answered`/`fixed`/`duplicate` | `done` | as stored |
| Case `/close` resolution `wontfix`/`escalated_external` | `abandoned` | as stored |

**PR merge does not mark the session done.** Cleanup may free the worktree; the session entry stays (PR links + final label). Idle TTL also keeps sessions that still have PRs, terminal labels, or are cases.

### Tests

- `internal/sessionstore/label_test.go` — parse, auto, manual stickiness, run-start revival.
- `internal/sessionstore/case_label_test.go` — case close freeze; no `needs_review` from stale PR in intake/investigate.
- `internal/bot/session_markers_test.go` — `SESSION_DONE` / `SESSION_ABANDON` parse + apply.

---

## 5. Session activity — derived board bucket

Not stored. `/board` (and similar) classifies a row into exactly one bucket, **priority first match**:

`running → queued → waiting → stale → active` (or `done` / `abandoned` if the label is terminal).

Source: `classifyActivity` in `internal/bot/board.go`.

| Bucket | When |
|---|---|
| `running` | an agent run is in flight |
| `queued` | follow-up queue length > 0 |
| `waiting` | `waitingOnHumanReason` ≠ "" (`blocked`, `needs_review`, PR `CHANGES_REQUESTED`, CI failing) |
| `stale` | idle whole days ≥ `boardStaleDays` (default 3) |
| `active` | none of the above, and not terminal |
| `done` / `abandoned` | terminal label (takes priority over running/queued) |

Terminal labels win even if a run is somehow still marked busy — the classifier returns done/abandoned first.

---

## 6. Case phase — support lifecycle

Orthogonal to session label. Only on `Mode=case`. Phase drives RunPolicy ship gates: only `fixing` and `shipping` may open PRs / direct-ship.

```
        /case  (or web intake)
              │
              ▼
           intake
              │  /investigate or freeform
              ▼
         investigate ── /answer ──► answered
              │                      │  freeform
              │                      └──► investigate
              │
              ├── /escalate or /start fix ──► fixing
              │                                 │  first open PR
              │                                 ▼
              │                              shipping
              │
              └── /close (from any open phase) ──► closed
                                                     │  /reopen [investigate|fixing]
                                                     └──► investigate or fixing
```

### Canonical

`internal/sessionstore/store.go` + display order `bot.CasePhaseOrder`:

| Phase | Plain (board) | Ship? |
|---|---|---|
| `intake` | New case | no |
| `investigate` | Looking into it | no |
| `answered` | Answer ready | no |
| `fixing` | With engineering | **yes** |
| `shipping` | Fix in review | **yes** |
| `closed` | Resolved | no |

Unknown/empty phase buckets to **intake** on the board (`normalizeCasePhase`) so a bad value cannot fall off the board.

### Transitions

| From | Event | To | Also |
|---|---|---|---|
| (new) | `/case` / web intake | `intake` | Mode=case, severity, CaseKey minted once |
| open case | second `/case` in the same thread | **`intake` again** | re-file title/severity; CaseKey and `OpenedAt` kept; refused only when **closed** or the thread is already a non-case mode |
| `intake` / `answered` / empty | freeform task or `/investigate` | `investigate` | label `open`/`blocked` → `in_progress` |
| any open | `/answer` | `answered` | label **blocked**; stamps `AnsweredAt` / first-response |
| any open | `/escalate` or builder `/start fix` | `fixing` | Mode stays **case** (never becomes `Mode=fix`); engineer assign/clear rules in `EscalateCase` |
| `fixing` | first open PR after a ship run | `shipping` | |
| any open | `/close` | `closed` | resolution + `ResolvedAt`; label done or abandoned |
| `closed` | `/reopen` / `/reopen fixing` | `investigate` or `fixing` | clears resolution; new SLA round; **not** `/case` on the same thread |

Closed is frozen: no investigate / answer / customer-update until reopen. `/case` on a closed thread is refused so it cannot clobber.

Escalate never silently happens: `/start fix` on a case without escalate caps leaves the phase unchanged and the run stays non-ship.

### Resolution (only meaningful when closed)

`answered` | `fixed` | `duplicate` | `wontfix` | `escalated_external`. Unknown text is stored as `answered` plus a note. `wontfix` / `escalated_external` → session label abandoned; the others → done.

### SLA clocks (derived, never stored)

Targets live on the project (`projects.<name>.sla.<severity>.*`). The case stores timestamps only. Breach is computed at render (`computeCaseSLA`).

- Round start = `ReopenedAt` if set, else intake/`OpenedAt`. Pre-stamp cases never breach.
- First-response clock stops at `FirstResponseAt` (only `/customer-update` and `/answer`, and their web equivalents).
- Resolution clock pauses **while** phase is `answered`, frozen at `AnsweredAt`. Leaving `answered` re-inherits the wait (over-report, not a ledger).
- Closed stops both.

### Tests

- `internal/web/case_actions_test.go`, `internal/bot/case_lifecycle_test.go`
- SLA: `internal/sessionstore/sla_test.go`, `internal/bot/case_sla_test.go`, `internal/web/cases_sla_test.go`

---

## 7. Pull request — GitHub state on a tracked session

GitHub owns PR lifecycle. grokwork mirrors it onto `Entry.PRs[]` (source of truth; legacy single-PR fields are copies).

### Canonical

`ghpr.Info.State` / `TrackedPR.State`: `OPEN`, `MERGED`, `CLOSED`. Draft is `IsDraft` on an OPEN PR (`DisplayState` shows `DRAFT`).

`ghpr.IsTerminal`: `MERGED` or `CLOSED`.

### How a session learns about a PR

Post-run `refreshPRAfterTask` (reply URLs + worktree branch) and the ~90s poller. Investigate/explain runs warn only — they do not bind.

The bot **never** merges a GitHub PR. Web merge is a human `githubWrites`/`merge` action.

### Effect on other machines

| PR becomes | Session label (auto, if not manual) | Case phase | Issue overlay |
|---|---|---|---|
| OPEN draft | `in_progress` | fixing stays fixing until a non-draft/open bind promotes shipping | FIXING if session non-terminal + Fixes |
| OPEN ready | `needs_review` | `fixing` → `shipping` on first open PR | FIXING if session non-terminal + Fixes |
| MERGED / CLOSED | **unchanged** (does not auto-done) | unchanged | FIXING drops only if the session is then labeled terminal, or GitHub closed the issue via `Fixes` |

When **all** tracked PRs are terminal, the worktree may be removed; the session entry is kept.

### Tests

- `TestIsTerminal` (`internal/ghpr/ghpr_test.go`)
- `internal/bot/pr_status_test.go` — preserve fields, session kept after terminal cleanup

---

## 8. Team review verdict — grokwork-local, not GitHub

Three different “reviews” on a PR. Do not collapse them.

| Route | What it is | Counts for branch protection? |
|---|---|---|
| `POST …/reviews` | grokwork team verdict (`reviewstore`) | **no** |
| `POST …/agent-review` | agent posts one `gh pr comment` | no |
| `POST …/github-reviews` | `gh pr review` as the gh user | **yes** |

Team verdicts (`internal/reviewstore`): `approved` | `changes_requested` | `commented`. Unrecognized values do **not** default — `NormalizeVerdict` returns empty. `changes_requested` requires a body.

Head-SHA scoped: a new PR head obsoletes pending team reviews. This is not a session or issue state.

---

## 9. Session mode and ship mode

Orthogonal to label and (for cases) to phase.

### Mode (`Entry.Mode`)

`""` (legacy fix default) | `investigate` | `explain` | `fix` | `plan` | `case`

First writer wins (`ensureSessionMode`). A case stays `Mode=case` through escalate/ship — it does not become `Mode=fix`. Plan on a unit already in another mode is refused (`ErrPlanModeConflict`).

### Ship mode (`Entry.ShipMode`)

`""` | `pr` | `direct`

Stamped on first run from project `directToPrimary`. Sticky. Direct: agent commits on the managed branch, bot fast-forwards primary, no PR. Direct successful ship writes `Label=done` (not manual). Direct run-start may revive a terminal session back to `in_progress`.

---

## 10. Agent run (journal) — in-flight task, not the session

One session can have many runs. The journal is crash recovery for the **current** run + FIFO queue (max 5), not the unit’s team label.

`internal/runjournal`:

| Status | Terminal? |
|---|---|
| `pending` | no (queued) |
| `running` | no |
| `cancelling` | no |
| `done` | yes |
| `failed` | yes |
| `interrupted` | recovered after restart; agent runs **are** re-driven |
| `blocked_orphan` | process group still alive after restart; do not signal (pid may be recycled) |

This is what `/board` “running” / “queued” reads (live `Bot.states` + journal), not `Entry.Label`.

---

## 11. Deploy run — separate from agent sessions

Deploys are **not** sessions. One run = one service × environment × commit. Never auto-resumed (shell steps are not idempotent).

`internal/deploy.Status`:

| Status | Terminal? | Notes |
|---|---|---|
| `pending` | no | queued behind a lane |
| `running` | no | |
| `cancelling` | no | stop requested |
| `succeeded` | yes | |
| `failed` | yes | |
| `cancelled` | yes | |
| `interrupted` | yes | was `running` at process death; **not** re-driven |
| `skipped` | step-level | |
| `blocked` | yes for scheduling | process tree outlived the restart; human must look |

A succeeded run may later be marked `SupersededBy` when a newer success lands in the same lane. The board reads that flag; it does not re-derive “newest success” from mtime.

---

## Cross-machine map (the usual confusion)

These are **not** the same bit, even when they share a word.

| You see | Machine | “fixing” means |
|---|---|---|
| Issues list **FIXING** | §1 overlay | a non-terminal session binds this ticket with **Fixes** |
| Issues filter **Open** | §1 overlay partition | GitHub-open **and** not FIXING |
| Case phase **fixing** | §6 | engineering has the case (may not have a GitHub issue at all) |
| Session label **in_progress** | §4 | the unit is being worked |
| Session label **done** | §4 | the **unit** is finished — the GitHub issue may still be open |
| PR **OPEN** | §7 | waiting on review/merge |
| Board **waiting** | §5 | human should act (review, CI, blocked) |

Typical Fix-button path, PR mode:

1. Issue GitHub **open**, overlay **FIXING**, session `in_progress`, activity `running`.
2. Agent opens PR, emits `SESSION_DONE`.
3. Session **done** (manual). Overlay **Open**. PR **OPEN**. Issue GitHub **open**. Activity `done`.
4. Human merges PR with `Fixes #N`.
5. GitHub issue **closed**. Session still **done**. Overlay none.

That drop from FIXING → Open at step 3 is the trap in §1.

---

## If you change a machine

| Change | Also update |
|---|---|
| Overlay rule (terminal? Refs? closed GitHub?) | this §1–3; `find_issue_test.go`; `workflow_test.go` |
| Default issues filter | `defaultIssueListState`; issues.tmpl options; `TestIssuesListShowsFixingWorkState` |
| `SESSION_DONE` vs auto-label vs open PR | this §1 trap + §4; `session_markers_test.go`; `label_test.go` |
| Case phase set | this §6; `CasePhaseOrder` / `CasePhasePlain`; support-case-guide.md |
| PR merge auto-done (today: never) | this §4 and §7; `SuggestAutoLabel` comment; pr_status cleanup tests |
| Deploy statuses | this §11; `deploy.Status.Terminal`; recover.go `StatusBlocked` |
