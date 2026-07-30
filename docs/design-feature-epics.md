# Feature epics: a GitHub issue as a multi-session requirement

Status: phase 0 shipping (issue creation); phases 1–3 designed, not started.

## Problem

A feature requirement is bigger than one agent session. Today the unit of work
is fixed — one Discord thread = one git worktree = one branch = one session —
and that invariant is load-bearing (ownership, queueing, cleanup, ship policy
all hang off it). What is missing is the layer *above* the unit: write a
requirement once, break it into scoped sub-tasks, run one session per sub-task,
and see progress in one place.

## Design principles

1. **GitHub stays the source of truth for the requirement.** The issue holds
   the text; grokwork stores only bindings, exactly as it already does for PRs
   (`Entry.Issues` / `TrackedIssue`). No epic store, no second copy of the
   requirement — a second copy is the thing that drifts. The issue number is
   the quotable id; nothing CaseKey-shaped is minted.
2. **The reverse index is derived, never stored.** "Which sessions serve issue
   #42" is a scan of `sessions.json` (the `FindByPR` shape; search already
   folds rows by `IssueKey` for the same reason). A stored index is wrong the
   moment a session is reset out from under it.
3. **The bot does deterministic `gh` edits; the agent does judgment.** Breaking
   a requirement into sub-tasks is judgment (an agent session). Creating an
   issue from a form, appending a session link to a checklist line, or checking
   a box when a PR merges is deterministic (bot), and therefore auditable.
4. **A feature never changes the unit.** No cross-session branch stacking, no
   shared worktrees. A sub-task that depends on another's work starts after
   that PR merges; worktrees come from main-checkout HEAD, so sequencing falls
   out of the existing model for free.

## Phase 0 — issue creation (shipping now)

The web UI can list, view, comment on, close, and dispatch a fix for issues,
but a human cannot *create* one; only the commit-review agent files issues.
Phase 0 adds `/projects/{name}/issues/new`:

- **Kind selector: Feature / Bug**, mapped to labels (`feature` / `bug`) on
  the created issue — labels rather than separate routes, because a GitHub
  issue is one kind of record and a label is what lists and the future epic
  hub filter on. `ghpr.CreateIssue` already retries label-less when a label
  does not exist in the repo, so an unlabeled repo degrades to a plain issue
  instead of an error.
- Kind also scaffolds the body placeholder: a feature asks for motivation /
  scope / acceptance criteria; a bug asks for repro / expected / actual.
- **A customer-reported problem is a case, not an issue.** Cases carry
  severity, SLA and the customer ref; issues do not. The bug form says so and
  links to `/projects/{name}/cases/new` rather than blurring the two records.
- **Gates:** deployment-wide `requireFeature("githubWrites")` +
  `requireMember`, plus the per-project `GithubWrites` capability checked in
  the handler (the `postPRGitHubReview` pattern — the route gate is
  deployment-wide, the capability is per project, and hiding the form is not a
  gate). The form renders read-only without the capability, like `case_new`.
- **Attribution:** the issue is filed by the host `gh` account, so the body is
  the only place the human is named — same "On behalf of @login" prefix as PR
  and issue comments, resolved from the proven identity link and omitted when
  there is none.
- **Audit:** `audit.ActionIssueCreate` with project/owner/repo/number/kind.
  Title and body never enter the log (they can carry customer data), matching
  the no-content rule every other action follows.
- **Multi-repo:** owner/repo resolve through `resolveCatalogRepoAccess`, the
  same containment boundary every other repo-scoped write uses.
- Image attachments in issue bodies are out of scope (GitHub's upload CDN has
  no clean `gh` path); revisit after the epic phases.

## Phase 1 — the issue page becomes the feature hub

- `bot.FindByIssue(owner, repo, n)` mirroring `FindByPR`.
- Issue detail gains a **Sessions** section: every unit tracking the issue,
  with phase, label, owner and PR states. Rows carry the `?back=` provenance
  crumb (already allowlisted in `backlink.go`). Visibility is the viewer's
  project ACL, applied before rendering — the same containment rule search and
  related-case links enforce.
- A **Start session from this issue** dispatch on the issue rail: web-native
  unit (the stream lands on the page the user is redirected to, the
  commit-review reasoning), prompt pre-filled with issue title/body/URL, the
  issue bound at creation, goal stamped from the title. Gate: `startSessions`;
  the model field sits behind `requireCanSelectModel` like the other dispatch
  cards.

## Phase 2 — agent-assisted breakdown

- **Plan this feature** rail action: an investigate-mode session whose prompt
  contract is to read the issue and the repo, then write a GitHub tasklist
  (`- [ ]` markdown) back onto the issue via `gh` — GitHub renders tasklists
  natively, so the breakdown is visible to people who never open grokwork.
- The bot **parses the tasklist deterministically** (plain markdown scan, no
  model call — the `completion.go` stance) and renders the items on the issue
  page, each with its own Start session pre-filled with that item's scope.
- **The item ↔ session mapping lives in GitHub:** when a session starts from
  an item, the bot appends the session link to that checklist line (one small
  `gh` edit). Durable, human-visible, and it survives grokwork state loss.
  Issue-body edits are last-write-wins; each edit stays single-line to keep
  the collision surface small.
- The parse must ignore checkboxes inside code fences, and an unparseable
  body degrades to "no breakdown", never an error page.

## Phase 3 — deterministic progress

- When every tracked PR of an item's session reaches terminal-merged, the bot
  checks the box (`- [x]`) via `gh` — audited under its own action constant,
  gated on the project `GithubWrites` capability like every host-credential
  write.
- The issue page shows a checked/total progress strip derived from the parse
  on render — never stored. A stored progress flag is wrong the minute reality
  moves with no writer running (the case-SLA rule).

## Explicitly out of scope

- Cross-session branch stacking or dependency ordering (violates the unit
  invariant; sequencing via merge order is enough).
- Auto-spawning every sub-task session at once (concurrency caps and human
  review of the breakdown make deliberate starts the right default).
- A Discord hub view (auto-binding from prompt text already works in Discord;
  the hub is web-first).
