# Plan: No-PR mode (direct-to-primary)

## Goal

Add a **per-project** setting that lets the bot keep the existing isolation model (one session = one worktree = one managed branch) but **ship by integrating commits onto the project’s primary branch** — no `gh pr create`, no PR status cards, no human merge step.

Target users: solo developers and fast-moving private projects where PR ceremony is pure overhead.

Default remains today’s PR workflow. Opt-in only.

---

## Problem today

Every freeform run injects `remoteWorkPromptPrefix`:

1. Commit on managed branch only (never main)
2. Push branch
3. `gh pr create`
4. Do not merge; include PR URL

Post-run always attempts `refreshPRAfterTask`. Cleanup and “done” labels are PR-terminal-driven. Without a PR, sessions sit until idle TTL (default 30 days).

There is no project-level ship policy. `worktreeIsolation=false` runs on the main checkout but the prompt still demands feature branch + PR.

---

## Non-goals (v1)

- Bot auto-merge of **GitHub PRs** (`gh pr merge`) — still forbidden; human web merge stays as-is for PR-mode projects.
- Force-push to primary.
- Shared checkout of `main` across concurrent sessions (git forbids the same branch in two worktrees).
- Full session `Mode` / `RunPolicy` system from `docs/design-agentic-team-runtime.md` (orthogonal; design for compatibility, do not block on it).
- Changing global `yolo` / `worktreeIsolation` defaults.
- Multi-repo “which primary?” picker beyond today’s single main checkout path.
- Auto-deploy after push.
- CI watcher on **primary** after a direct ship (no PR poller equivalent) — accepted v1 gap; document it.
- Auto-rebase of diverged session branches on non-ff failure.

---

## Key decisions

### K1 — Ship policy is per-project, not global

Field on `ProjectConfig` (not root config). Different projects on one host can mix PR-mode and direct-mode.

**Name / JSON:** `directToPrimary` (`*bool`, nil/false = PR mode, true = No-PR mode).

Accessor: `ProjectDirectToPrimary(name string) bool` (nil → false).  
Setter: `SetProjectDirectToPrimary(name string, enabled bool) error` with lock + `saveLocked`.

Rationale: matches Linear / fetch-interval / members pattern; solo projects opt in without affecting team repos on the same bot.

### K2 — Keep worktree isolation; bot owns integrate-to-primary

```
Session run (unchanged isolation)
  worktree: data/worktrees/<project>/<unitId>
  branch:   grok/discord/<id> or grok/web/<id>
  Grok:     commit on managed branch only; clean tracked tree;
            no PR for this project's repo; no push to primary

Bot post-run (only when session is direct mode + managed branch present)
  1. Gate: Code==0 && !Cancelled && !MaxTurnsReached
           && clean tracked files && commits to ship
  2. Under per-checkout git lock:
       fetch → optional pre-check → git push origin <sessionSHA>:refs/heads/<primary>
       (remote enforces ff; push rejection is canonical non-ff)
  3. Success → stamp session ShippedSHA/ShippedAt/label done;
               completion card; uploads first;
               then Remove worktree IFF queue empty (keep session entry)
  4. Failure → leave worktree; Discord error card; label stays in_progress
```

Rationale:

- Concurrent sessions cannot both check out `main` in separate worktrees.
- Aligns with “bot owns deterministic git; Grok owns judgment.”
- Soft prompt-only “push to main” under yolo is fail-open and races badly.
- SHA push never touches main checkout HEAD (safe if operator is working there).

**Invariant update (docs / CLAUDE.md):**

> Bot never merges **GitHub PRs**. When a session is in direct-to-primary mode, the bot **may fast-forward a managed session branch onto the project primary and push primary** — local integrate + push of an `IsManagedBranch` head only, not PR merge.

### K3 — Push is the safety mechanism; pre-check is UX only

A plain (non-force) `git push origin <sha>:refs/heads/<primary>` is **fast-forward-enforced by the remote**.

Implications:

- Primary stays safe even if local fetch is stale, the lock is skipped, or two hosts race — remote rejects non-ff.
- Per-checkout lock is still worth having for clean errors and less wasted work, not as the sole safety net.
- Local `merge-base --is-ancestor` is **optional**: nicer message + noop detection only. **Push rejection is the canonical non-ff path.** Pre-check pass does not guarantee push success (external push in the gap).
- Worktrees share the object store with the main checkout; push from main repo updates local `origin/<primary>` tracking ref, so subsequent `EnsureWith` / worktree creates see the shipped tip without waiting on idle fetch throttle.

Integrate algorithm (from **main checkout** `ProjectConfig.Path`):

1. `IsManagedBranch(sessionBranch)` — refuse otherwise.
2. `rev-parse` session HEAD in worktree; require clean **tracked** status (see K7).
3. `git -C mainRepo fetch origin` (best-effort freshness).
4. Resolve primary name (same heuristic as worktree create / base detection).
5. Optional: ancestor pre-check for friendly error / noop.
6. `git -C mainRepo push origin <sessionHEAD>:refs/heads/<primary>` (never `--force`).
7. On push success: return SHAs; caller handles session stamp + optional worktree remove.

### K4 — Thread-sticky ship mode (not enqueue snapshot)

**Do not** snapshot `DirectToPrimary` on every `taskItem` at six enqueue sites (and journal). That forces schema/journal churn and is easy to drop on crash-resume.

Instead:

1. At the top of `executeTask`, resolve effective mode once:
   - If `Entry.ShipMode` is already `"direct"` or `"pr"` → honor it for this thread forever.
   - Else (first run / empty): set from live `ProjectDirectToPrimary(project)` and **stamp** `Entry.ShipMode`.
2. Prompt contract and post-run ship both read the same stamped value.

Why sticky:

- Flipping the project flag mid-thread must not produce mixed state (earlier PR on managed branch, later direct push of same commits → zombie open PR with empty diff; PR-terminal cleanup never fires).
- New threads pick up the new setting.
- Natural seam for future `Mode` / `RunPolicy` (K10).
- Single code site inside `executeTask`; no journal field required for v1.

### K5 — Prompt is mode-specific; not the only control

`remoteWorkPromptPrefix(branch, direct bool)` (or equivalent).

**Direct mode (with managed branch):**

- Isolated worktree; stay on this branch.
- Commit on this branch only; leave tracked files clean.
- Do **not** open a PR **for this project’s repository**; do **not** push to main/master; do **not** `git push origin HEAD:main`.
- Cross-repo PRs elsewhere remain allowed if the task legitimately touches another repo (one sentence in prompt).
- The bot will fast-forward integrate this branch to the project primary after a successful run.
- Summarize commits in the final reply (no PR URL required for this ship).
- Keep `DISCORD_UPLOAD` + filesystem scope rules.

**Direct mode without branch** (`worktreeIsolation` off or non-git project):

- Use the **PR-mode** (or generic) prompt wording; **skip ship entirely**; log why. `DirectShipFF` must never be reachable with empty branch.

**PR mode:** keep current text.

Issue binding in direct mode: `Fixes` / `Refs` belong in **commit messages**, not PR body.

Hard post-run split:

```go
if shipMode == "direct" && wtBranch != "" {
    b.shipDirectAfterTask(...)
} else if shipMode != "direct" {
    b.refreshPRAfterTask(...) // today's path; still runs on non-zero exit except cancel
}
```

### K6 — Session continuity: stamp ship, keep Entry, remove worktree last

**Critical:** do **not** reuse `cleanupWhenAllPRsDone` (it `sessions.Delete`s and is gated on “not busy”). Direct-mode threads are ongoing conversations; deleting the session breaks `prebindSessionID` / grok resume memory on follow-ups.

On ship success:

1. `uploadWorktreeFiles` (needs `runCwd`) — **before** any remove.
2. `postCompletionSummary` (needs `runCwd`) — **before** any remove.
3. Patch session: `ShippedSHA`, `ShippedAt`, `PrimaryBranch`, label `done` via `ApplyAutoLabel(LabelDone)`. **Keep the session entry.**
4. Worktree removal via direct `gitworktree.Remove` (managed branch only) as the **final** step of `executeTask`, **only when the thread queue is empty**.
   - Explicit exception to the “don’t cleanup while job held” PR-mode convention — document in code comment; this path is ship-success + queue-empty only.
   - If queue non-empty: **keep worktree**. Queued task continues on the just-shipped tip (`origin/<primary>` already updated by the push); next ship still ffs if the model committed atop that tip.
5. Next task with missing worktree: existing `resolveRunCwd` / `EnsureWith` recreates at the same path from fresh primary tip. Sequential follow-ups therefore start clean without a new “pre-run catch-up ff” feature.

### K7 — Ship gate (precise)

Ship only when **all** hold:

| Condition | Why |
|-----------|-----|
| `ShipMode == "direct"` | thread sticky |
| `wtBranch != ""` and managed | no-branch path skips ship |
| `!result.Cancelled` | user abort |
| `result.Code == 0` | failed run |
| `!result.MaxTurnsReached` | half-finished work must not land on primary (stricter than today’s PR refresh, which runs on non-zero exit) |
| Clean **tracked** files (no staged/unstaged changes to tracked paths) | untracked scratch must not block ship; scratch is discarded if worktree is removed |
| Session HEAD has commits to ship (ahead of `origin/<primary>`, or push would not be noop) | noop → stamp done-ish, skip push, still may remove clean worktree |

Dirty tracked tree or gate fail → no push; completion card shows leftover work; worktree kept.

### K8 — Labels: revive terminal labels on direct-mode run start

Today `ApplyAutoLabelOnRunStart` only promotes `open → in_progress`. After a direct ship marks `done`, the next run in the same thread is stuck: `ApplyAutoLabel` will not revive a terminal label without an open PR, and direct sessions never have one.

**Fix:** on run start, if session is direct mode (or `ShipMode=="direct"` / no tracked PRs path), promote `done`/`abandoned` → `in_progress` unless `LabelManual`.

Ship → done: `ApplyAutoLabel(LabelDone)` already applies (even over manual); no need to special-case `SuggestAutoLabel` for that transition.

### K9 — Command surface adjustments

| Command / feature | Direct mode behavior |
|-------------------|----------------------|
| Freeform task | Direct prompt + post-run ship |
| `/fix-ci` | Reject: no tracked PR; start a new task to fix from primary tip |
| `/address` (PR comments) | Reject or no-op — PR-centric |
| Fix-from-issue (GitHub/Linear) | Allowed: commit on session branch; bot ships; keywords in **commit message** |
| Commit review | Unchanged intent; ship only if optional fixes committed and gate passes |
| Ship board | Project badge “direct”; empty open-PR list expected |
| PR status poller | Already skips sessions with no PRs — no change |
| `CleanupIfPRDoneWith` in `resolveRunCwd` | **Skip** when session is direct (dead `gh pr list` every task) |
| CI on primary after ship | No watcher in v1 (document) |

### K10 — Concurrency lock key

Key the integrate mutex by **resolved main-checkout path** (`filepath.Abs` + `EvalSymlinks`, same idea as `fetchKey` in `gitworktree/fetch.go`), **not** project name — two project names can share one path.

### K11 — Web UI

Project settings (`/config/projects/{name}`):

- Section **Ship workflow**
- Checkbox: “Direct to primary (no pull requests)”
- Help text: worktrees stay isolated; successful runs fast-forward to the default branch and push. No PR review. For solo / trusted projects only. Existing threads keep their ship mode until reset/new thread.
- Admin-only (existing project config gate)
- Persist via `SetProjectDirectToPrimary`
- Optional badge on project home / ship board when enabled

### K12 — Compatibility with future modes

When `Mode` / `AllowPR` land later:

- Thread `ShipMode=direct` + eng fix freeform → integrate path, not PR
- Investigate still no ship
- Do not invent session Mode in this change set

---

## Detailed design

### Config shape

```go
// ProjectConfig
// DirectToPrimary, when true, new sessions stamp ShipMode=direct and
// ship via ff push to primary (no PR). nil/false = PR mode.
DirectToPrimary *bool `json:"directToPrimary,omitempty"`
```

Marshal/clone/`ProjectItem` / example docs updated. Tests for default false and round-trip.

### Session store

```go
// On Entry:
ShipMode      string // "" | "pr" | "direct" — sticky for the thread
ShippedSHA    string
ShippedAt     string // RFC3339
PrimaryBranch string
```

Preserve across `Set` rebuilds (same preserve helper chain as PRs/ownership/brief).

### Primary branch resolution

Reuse existing detection; centralize if needed:

```go
func ResolvePrimaryBranch(ctx, repoDir) (name string, remoteRef string, err error)
// e.g. "main", "origin/main"
```

### DirectShipFF

```go
type DirectShipResult struct {
    PrimaryBranch string
    FromSHA       string // origin/primary before push (best-effort)
    ToSHA         string // session head pushed
    Noop          bool
}

// DirectShipFF push-rejects non-ff; never force.
// Tests must include: pre-check would pass but concurrent push loses (simulate remote rejection).
func DirectShipFF(ctx, mainRepo, worktreePath, sessionBranch, primary string) (DirectShipResult, error)
```

### Post-run ordering in executeTask (direct success path)

```
uploadWorktreeFiles
shipDirectAfterTask (lock + push + session stamp + label done)
postCompletionSummary (include primary SHA / ship result)
refreshBriefCard
if queue empty: gitworktree.Remove(worktree)  // keep session entry
```

### Failure UX (Discord)

Non-ff / push rejected:

```
Could not ship to <primary> (non-fast-forward or push rejected).
Commits remain on <managed branch> in this thread's worktree.
Primary may have advanced (another session or human push).
Use @Grok /reset then re-run the task so a fresh worktree starts from the current primary tip.
(Web: Prune worktree, then re-run.)
```

Do **not** claim “re-run will auto-rebase” — v1 keeps the diverged worktree on re-run and would fail the same way until reset/prune.

### Tests

| Area | Cases |
|------|--------|
| config | default false; set true; JSON round-trip; clone |
| prompt | direct vs PR strings; no-branch direct falls back |
| DirectShipFF | ff success; remote non-ff rejection; dirty **tracked** tree; untracked-only still ships; non-managed branch refused; noop |
| execute path | ship when ShipMode direct; PR refresh skipped; stamp sticky; no session delete after ship |
| labels | done after ship; revive done→in_progress on next run start (direct) |
| queue | ship + non-empty queue keeps worktree; empty queue removes |
| web | project config form + POST |
| /fix-ci | rejects when direct / no PR |
| resolveRunCwd | skips CleanupIfPRDone when direct |

### Docs

- `README.md` — mode, safety, no primary CI watcher, sticky threads
- `CLAUDE.md` — invariant amendment
- `config.example.json` field only if we keep comments in README (JSON has no comments)

---

## Implementation plan (ordered)

### PR1 — Config + web toggle

- `ProjectConfig.DirectToPrimary`, accessors, marshal/clone, tests
- Project settings UI section + POST handler + redirect
- README note (behavior not wired yet)

### PR2 — Direct ship git primitive + prompt

- `ResolvePrimaryBranch` if needed
- `DirectShipFF` + temp-repo tests (including push-rejection as canonical non-ff)
- `remoteWorkPromptPrefix(branch, direct)` + issue-binding variant
- Prompt tests

### PR3 — Bot wiring (all behavioral gaps)

- Thread-sticky `ShipMode` stamp at execute
- Ship gate (exit 0, not cancelled, not max-turns, clean tracked)
- `shipDirectAfterTask` under path-keyed lock
- Skip `refreshPRAfterTask` / `CleanupIfPRDone` when direct
- Post-run order: upload → ship → completion → remove iff queue empty; **keep session**
- Label revival on run start for direct
- Command guards (`/fix-ci`, `/address`)
- Bot tests

### PR4 — Polish

- Project home / ship board “direct” badge
- Idle cleanup regression
- Docs + CLAUDE invariant
- Note: no primary CI watcher in v1

Can squash for a solo repo if preferred; ordering keeps each step reviewable.

---

## Risks and mitigations

| Risk | Mitigation |
|------|------------|
| Concurrent ships race | Path-keyed mutex + remote ff enforcement on push |
| Model pushes to main under yolo | Prompt forbids; bot ships session HEAD (idempotent if already there) |
| Main checkout dirty / wrong branch | SHA push to `refs/heads/<primary>` never checks out primary |
| Protected branch rejects push | Surface error; mode requires bot identity can push primary |
| Flag enabled on team repo by mistake | Explicit checkbox + warning; default off; sticky only for **new** threads after disable still need care — document |
| Session delete loses memory | Never delete session on direct ship (K6) |
| Stuck `done` label | Revive on run start (K8) |
| Diverged worktree re-run loops | Failure UX points at `/reset`; no fake rebase claim |
| Untracked scratch blocks ship | Clean check = tracked only; scratch discarded on worktree remove |

---

## Advisor review summary

Consulted Fable (`/advisor`, effort xhigh). **Verdict: proceed with architecture; fix four concrete gaps before coding.**

| Finding | Disposition |
|---------|-------------|
| Architecture (bot-owned ff SHA push, keep worktrees) is correct | Adopted |
| Framing: remote push is the real safety; pre-check is UX | Adopted (K3) |
| Gap 1: don’t delete session; remove worktree last; only if queue empty | Adopted (K6) |
| Gap 2: label revival done→in_progress on direct run start | Adopted (K8) |
| Gap 3: thread-sticky ShipMode, not enqueue snapshot | Adopted (K4) |
| Gap 4: ship gate (max-turns, no-branch, tracked-clean) | Adopted (K7) |
| Lock by abs path not project name | Adopted (K10) |
| Skip CleanupIfPRDone when direct | Adopted (K9) |
| Failure UX must say `/reset`, not fake rebase | Adopted |
| No primary CI watcher in v1 | Documented non-goal |
| Multi-repo: “no PR” scoped to this project’s repo | Adopted (K5) |

---

## Success criteria

1. With `directToPrimary: true`, a new Discord `@Grok` task that commits code advances primary on origin with **no** PR created.
2. Concurrent sessions cannot corrupt primary (remote rejects non-ff; lock serializes cleanly).
3. PR-mode projects unchanged when flag absent/false.
4. Successful ship **keeps** the session entry; removes worktree only when queue empty; follow-up tasks resume Grok memory and recreate worktree from primary tip.
5. Second task on a shipped thread leaves `done` and becomes `in_progress` again (unless LabelManual).
6. Web UI can toggle the setting; config survives restart.
7. Tests cover ff, push-rejection non-ff, tracked-vs-untracked clean, sticky mode, label revival, no session delete.
