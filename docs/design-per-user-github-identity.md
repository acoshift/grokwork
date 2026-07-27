# Per-user GitHub identity for commits, comments, and PRs

| Field | Value |
|-------|-------|
| **Status** | Research / decision plan. **Tier A has since shipped** — but *not* via this doc's Discord→GitHub map: the login now comes from a link the person proved by signing in with it (`internal/identity`, `/account`), and the admin-maintained `discordUserGitHub` map that shipped first has been removed. Tier B remains unimplemented. |
| **Date** | 2026-07-21 |
| **Repo** | `github.com/acoshift/grokwork` |
| **Audience** | Operators and engineers familiar with this codebase |
| **Related** | `TODO.md` (Safe team mode attribution; `/review` Discord→GitHub map), `docs/design-full-workflow-web-ui.md`, `README.md` (host `gh` auth) |

---

## Overview

**Question:** Can Discord users have commits, PR comments, and opened PRs attributed to *their* GitHub account instead of the single account logged into the grokwork host?

**Verdict:** **Possible, but not with a small patch.** Today grokwork is deliberately a **single host GitHub identity**: whatever `gh auth login` / `GH_TOKEN` / git credentials the machine has is who commits, pushes, opens PRs, and comments. There is no Discord→GitHub credential path.

True “act as Alice’s GitHub account” requires **Alice’s credentials on every write path** (Grok’s `git`/`gh` *and* bot/web `gh pr comment` etc.). That is a product + secrets redesign, not a config flag.

**What is impossible without those credentials:** GitHub will never attribute a PR author, PR comment, or API write to a human account that never authorized the host. Setting only `GIT_AUTHOR_NAME`/`EMAIL` can make *commit metadata* look like Alice; push/PR/comment still show the host bot unless a token for Alice is used.

This doc freezes the research into two tiers: **Tier A (attribution only)** as the recommended first ship, and **Tier B (true per-user write identity)** as a later multi-phase option.

---

## Background & current model

```
Discord user A / B / C
        │
        ▼
   grokwork process  ── env = os.Environ() ──►  one host GH user
        │                                          (gh + git push)
        ├─ grokrun (Grok yolo: git commit/push, gh pr create)
        ├─ ghpr.* (poll, web comment/close/merge, commit-review issues)
        └─ gitworktree (branch isolation only — not credential isolation)
```

| Concern | Today |
|--------|--------|
| Who opens PRs | Grok via `gh pr create` under host `gh` |
| Who commits | Grok `git commit` under host `user.name`/`email` (no production `GIT_AUTHOR_*`) |
| Who comments (web) | Host `gh` via `ghpr.CommentPR` |
| Discord people model | Allowlist + owner/co-owner (snowflakes only) |
| Discord→GitHub map | **None** (TODO mentions map only for `/review @user` mentions) |

Key code:

- `internal/grokrun/run.go` — `cmd.Env = os.Environ()` (no per-actor overlay)
- `internal/ghpr/ghpr.go` — `execRunner` has no token/env channel
- `internal/bot/bot.go` `remoteWorkPromptPrefix` — instructs commit/push/`gh pr create`
- `TODO.md` already records the intended light path: *“PR/commit attribution: prompter + thread URL in PR body / commit trailer; **host remains pusher only**”*

Documented operator requirement (`README.md`): host needs `gh auth login` or `GH_TOKEN` and push access to project remotes.

---

## What “user’s own GitHub account” actually means

| Action | What GitHub attributes | Needs user’s token? |
|--------|------------------------|---------------------|
| Commit **author** field | Author name/email (links to GH if email matches noreply) | No (env/`--author` enough) |
| Commit **pusher** | Account that authenticated `git push` | Yes (or host keeps push) |
| **PR author** | Account that called create-PR API / `gh pr create` | Yes |
| **PR/issue comment** | Account that posted the comment | Yes |
| Verified / signed commits | Signer key | Usually no (impractical per-user on shared host) |
| “Requested review from @alice” | Login string only | No (username map enough) |

The feature splits cleanly into two product levels.

---

## Tier A — Attribution only (recommended first)

**Goal:** Humans and audit trails can see *who asked* without storing GitHub secrets.

**Does not** make GitHub show Alice as PR author. Host bot still opens/pushes.

| Piece | Behavior |
|-------|----------|
| Discord→GitHub **login** map | Config or web: `discordUserId → githubLogin` (and optional display name/email) |
| Commit trailers | Inject into remote-work prompt (and optionally enforce via wrapper later): `Co-authored-by: Name <id+login@users.noreply.github.com>` and/or prompter trailer + thread URL |
| `GIT_AUTHOR_*` | Optional: set author to mapped noreply email so GitHub linkifies commits when email matches; **committer** can stay host bot |
| PR body footer | Always append: prompter Discord, mapped `@githubLogin`, thread/jump URL, session id |
| Web comments | Still host bot; body prefix `On behalf of @login (Discord …):` if map exists |
| `/review @user` | Unblocks planned review-request map (TODO P1) |

**Pros:** Small surface, no PAT vault, matches design principle “host remains pusher only”, works with one machine bot that already has org write access.

**Cons:** GitHub UI still shows bot as PR author/commenter; branch protection / CODEOWNERS “author” rules still see the bot.

**Fits:** Safe team mode slice C in `TODO.md` (attribution with host as pusher only).

---

## Tier B — True per-user write identity (possible; large)

**Goal:** `gh pr create`, `gh pr comment`, and optionally `git push` run as the Discord user’s GitHub account.

### B1 — Per-user tokens (PAT or GitHub App user-to-server)

1. **Link flow (web or Discord)**
   - Prefer **GitHub App user-to-server OAuth** (refreshable, app-scoped, audit logs show “via app”) over long-lived classic PATs.
   - Fallback: user pastes fine-scoped PAT (repo contents + PRs + issues) into encrypted store — weaker ops story.

2. **Secret storage**
   - New store under `data/` (0600), never `config.json` (already holds Discord token; do not grow that blast radius).
   - Encrypt at rest with a host key (`GROK_WORK_SECRETS_KEY` or similar).
   - Map: Discord snowflake → `{ githubLogin, tokenRef, scopes, expiresAt }`.

3. **Credential injection**
   - Extend `grokrun.Options` with `Env []string` or `ExtraEnv map[string]string`.
   - Per run: set `GH_TOKEN=<user token>`, strip any host `GH_TOKEN`/`GITHUB_TOKEN` from inherited env for that child only.
   - Set `GIT_AUTHOR_NAME` / `GIT_AUTHOR_EMAIL` (and preferably `GIT_COMMITTER_*`) from linked profile.
   - For `git push` over HTTPS: either same token via `GIT_ASKPASS` / temporary `http.extraHeader`, or keep **host** as pusher (hybrid: user authors PR API, host pushes branch — inconsistent; prefer one identity for both).

4. **`ghpr.Runner` rewrite**
   - Change runner (or wrap) to accept env / token context so web comment/close and bot poller paths can choose:
     - **User token** for user-initiated writes (comment, open PR if bot ever creates).
     - **Host token** for read-only poll (`pr view`, checks) and system cleanup.

5. **Actor selection rules**
   - Default: thread **owner’s** linked GitHub for runs on that thread (stable PR author across queue).
   - Or: **prompter** of this run (PR author jumps when different people queue — messy).
   - Recommend: **owner** for push/PR; comment “on behalf of” prompter if different.
   - Fail closed: no linked account → refuse ship steps or fall back to host with loud Discord warning (config tri-state: `requireUserGitHub: true|false`).

6. **Prompt contract**
   - Remote-work prefix: “You already have this user’s `gh` auth via `GH_TOKEN`; do not `gh auth login`; do not print tokens.”
   - Still commit only on thread branch; do not merge.

7. **Safety (hard requirements if Tier B ships)**
   - **Env filter** for Grok children (already TODO P0): deny host cloud keys; allow only curated allowlist + the injected `GH_TOKEN`.
   - Never log tokens (history, stream, audit redaction).
   - Token never written into worktree or commit.
   - Concurrent runs: each process gets its own env; no global `GH_TOKEN` mutation.
   - Scope minimum: contents R/W, pull_requests R/W, issues R/W (if comments/issues); no org admin.
   - Users must already have write access to the target repos (bot cannot grant what GitHub denies).

### B2 — GitHub App installation only (not “user’s account”)

Installation tokens act as `AppName[bot]`, not as Alice. Good for a dedicated bot identity; **does not** satisfy “user’s own GitHub account.” Keep as ops improvement for the **host** identity, orthogonal to per-user.

### B3 — Impersonation without OAuth

**Impossible** on real GitHub.com for PR author / comments. No API “create PR as user X” with only the bot token.

---

## Feasibility summary

| Desired outcome | Feasible? | Path |
|-----------------|-----------|------|
| PR body / commit trailer names the Discord prompter | Yes | Tier A |
| Commits linked to user’s GitHub profile (email match) | Yes | Tier A + noreply email |
| `/review @discord` → request GitHub reviewer | Yes | Login map only |
| PR **author** = user’s login | Yes, hard | Tier B tokens |
| Comments as user | Yes, hard | Tier B tokens |
| Push as user | Yes, hard | Tier B + push auth |
| Verified commits as user | Effectively no | Needs their signing keys on host |
| Spoof user without their auth | **No** | — |

---

## Product decision (recommended)

1. **Ship Tier A** as Safe team mode slice C (already on the roadmap). Covers auditability and team readability with low risk.
2. **Treat Tier B as optional later** only if the team needs GitHub CODEOWNERS / “I opened this PR” / personal contribution graphs to show the human. Budget it as a multi-PR design (OAuth app, secrets store, runner env, env filter, link UX), not a weekend change.
3. Do **not** half-implement Tier B by only setting `GIT_AUTHOR_*` and calling it “user identity” — that misleads on push/PR/comment.

---

## Implementation sketch (if building)

### Phase 0 — Decision gate

Confirm with operator:

- Is the goal **attribution** (who asked) or **true GitHub actor** (who GitHub thinks opened the PR)?
- Must every allowlisted Discord user have repo write on all mapped projects?
- Is a public HTTPS callback acceptable for GitHub App OAuth, or is the host private-only (then device flow / PAT paste)?

### Phase 1 — Tier A (concrete)

| Area | Change |
|------|--------|
| Config | `githubUsers: { "<discordId>": { "login": "alice", "name": "Alice", "email": "…@users.noreply.github.com" } }` or per-project overlay; web UI edit on user/project members |
| Session / actor | Resolve map from thread owner or prompter when assembling run |
| Prompt | Extend `remoteWorkPromptPrefix` / issue binding block with attribution footer requirements + Co-authored-by line |
| Optional env | `grokrun.Options.ExtraEnv` for `GIT_AUTHOR_NAME`/`EMAIL` only (no token yet) |
| Web writes | Prefix comment bodies when map hit |
| Docs | README: host remains ship identity; map is attribution + review mentions |

Tests: prompt contains trailer; unmapped user still works; map resolution prefers configured login.

### Phase 2 — Prerequisites for Tier B

| Area | Change |
|------|--------|
| Env filter | Grok child: allowlist env (PATH, HOME limited, XAI/grok keys as needed, injected GH only) — TODO P0 |
| Runner | `ghpr` + `grokrun` accept per-call env |
| Secrets package | `internal/ghauth` or `internal/usercreds`: encrypt, load by Discord ID, refresh App tokens |
| Link UX | Web: “Connect GitHub” OAuth; Discord: deep link to web |
| Policy | `requireUserGitHub` tri-state; clear Discord errors when missing |

### Phase 3 — Tier B write paths

| Path | Identity |
|------|----------|
| Grok `git`/`gh` in task run | User token (owner) |
| Web PR comment | Commenter’s linked token |
| Web close | Same |
| Web merge | **Stay host or admin-only host** (do not merge as arbitrary user; branch protection + bot design say human authority, bot never sneaky-merge) |
| PR poller / checks | Host read token |
| Worktree branch delete push | Host (cleanup), not user |
| Commit-review issue create | Host bot (system), not prompter |

### Phase 4 — Hardening

- Redact `gho_`/`ghu_`/`github_pat_` in stream/history
- Audit log: discordId, githubLogin, action, no raw token
- Token revoke on unlink; expire handling
- Document org policy: users need write; SSO-authorized PATs if org enforces SAML

---

## Out of scope / non-goals

- Replacing host bot for **read-only** fleet ops (poller, ship board) with N user tokens
- Storing tokens in Discord messages or worktrees
- Auto-merge as the user
- Multi-tenant SaaS GitHub marketplace listing
- Signing commits with each user’s GPG/SSH key

---

## Risk notes (Tier B)

| Risk | Mitigation |
|------|------------|
| Grok prints or exfiltrates `GH_TOKEN` under yolo | Env filter + prompt ban + stream redaction; prefer short-lived App user tokens |
| User loses org access; pushes fail | Surface gh stderr; fall back policy explicit |
| Mixed queue authors, one PR author | Bind identity to thread owner |
| Secrets on disk beside Discord token | Separate encrypted store; 0600; not in git |
| Concurrent threads overwrite global gh auth | Never `gh auth switch` globally; only per-process `GH_TOKEN` |

---

## Recommendation

- **Not impossible** for full per-user PR/comment/commit identity.
- **Not worth doing first** relative to Tier A + env filter; the codebase and `TODO.md` already chose “host remains pusher only.”
- If the only pain is “we can’t tell who asked for this PR,” ship **Tier A**.
- If the pain is “GitHub must show *my* face as PR author / my token for compliance,” accept **Tier B** as a deliberate multi-phase project with OAuth + secrets + runner env + env filter.

This document is the decision artifact only; no implementation is implied by filing it under `docs/`.
