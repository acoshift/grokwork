# Grok Work roles

Operator-facing guide to **who can do what** in Grok Work (`grokwork`).

There are two separate permission systems:

| Layer | Where it applies | What it controls |
|-------|------------------|------------------|
| **Capability templates** (Safe Team Mode) | Discord bot (and web case/start checks that re-resolve caps) | Investigate vs ship, escalate, customer draft, approve, project-admin flags |
| **Web roles** | Private web UI when Discord OAuth is enabled | Login, project visibility, config access, host-level write feature flags |

Membership still comes first: an actor must be on the project’s allowlist — either directly (`allowedUserIds`) or as a member of one of its `teams` — before capability templates apply. A project with no direct members and no team that names anyone is fail-closed. Discord-role authorization (`allowedRoleIds` / `capabilityByRole`) has been removed; a team’s `capabilities` field names the template its members get.

---

## Capability templates (Discord / project membership)

| Role | Audience | One-line purpose | Doc |
|------|----------|------------------|-----|
| **investigator** | Support / CS | Open cases, investigate read-only, draft customer replies, escalate to eng | [investigator.md](./investigator.md) |
| **operator** | Support / triage | Investigate and draft customer replies; **cannot escalate** | [operator.md](./operator.md) |
| **builder** | Engineering | Investigate + start fix sessions with GitHub writes (PR / direct ship when project allows) | [builder.md](./builder.md) |
| **approver** | Senior eng / tech lead | Builder-class ship + case draft/escalate + approve-class gates | [approver.md](./approver.md) |
| **admin** | Project admin | Full project capability set (ship, merge flag, approve, adminProject) | [admin.md](./admin.md) |

Built-in flag matrix (source: `internal/config/capabilities.go`):

| Flag | investigator | operator | builder | approver | admin |
|------|:------------:|:--------:|:-------:|:--------:|:-----:|
| `investigate` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `draftCustomerReply` | ✓ | ✓ | | ✓ | ✓ |
| `fileEscalation` | ✓ | | | ✓ | ✓ |
| `safeOps` | | | | | |
| `startSessions` | | | ✓ | ✓ | ✓ |
| `githubWrites` | | | ✓ | ✓ | ✓ |
| `approve` | | | | ✓ | ✓ |
| `merge` | | | | | ✓ |
| `adminProject` | | | | | ✓ |

`RequestChange` remains reserved (no command gates yet).

**Ship rule:** PR/direct ship requires **both** `startSessions` and `githubWrites` (`CanShip()`). Missing `githubWrites` coerces the run to investigate-only (never half-fix).

**Investigate shell:** diagnostic host shell (`psql`, logs, …) on investigate runs is granted when `fileEscalation`, `safeOps`, or `CanShip()` is true (`CanInvestigateShell()`). Builtin **investigator** and upper roles get shell; **operator** stays file-only. The investigate prompt requires **investigation-only** use (read/diagnose; no writes, DDL, restarts, or code “fixes”).

---

## Unmapped users and Safe Team Mode

| Safe Team Mode | Unmapped allowlisted user | Effective template |
|----------------|---------------------------|--------------------|
| **On** | No `capabilityByUser` hit and no team naming a template | `safeTeamDefaultTemplate` (**default `investigator`**) |
| **Off** / unset | Same | Builtin **`builder`** (legacy eng-only deploys) |

Details and rollout warnings: [safe-team-unmapped.md](./safe-team-unmapped.md).

**Rollout:** put engineers on a team whose `capabilities` is `builder` (or higher) **before** enabling Safe Team Mode, or engineers are demoted immediately—including already-queued tasks.

Configure under web: **Config → project → Safe team mode**, **Teams**, and **Capability maps**.

---

## Web roles (OAuth)

| Role | Audience | One-line purpose | Doc |
|------|----------|------------------|-----|
| **viewer** | Read-only stakeholders | Browse UI; no write POSTs | [web-viewer.md](./web-viewer.md) |
| **member** | Day-to-day operators | Writes when host feature flags are on | [web-member.md](./web-member.md) |
| **admin** | Host operators | Config UI + all member capabilities | [web-admin.md](./web-admin.md) |

Web write features (host-level, fail-closed when OAuth is off):

- `startSessions` — start tasks, case intake, session controls, case phase POSTs  
- `githubWrites` — issue/PR comments and close  
- `merge` — merge PR from web  
- `prReviews` — team PR review actions  

Resolution order for an actor id: **admin list → member list → viewer list → membership of any project (`allowedUserIds` or a `teams.<key>.members` list) → deny**. `webAuth.adminDiscordIds` (and the member/viewer lists) accept namespaced actor ids; a bare snowflake still means Discord.

Login providers: Discord, plus Google and GitHub when `webAuth.providers.google` / `webAuth.providers.github` carry a client id and secret (the secret may come from `GOOGLE_CLIENT_SECRET` / `GITHUB_CLIENT_SECRET` instead). A provider missing either half renders no button **and** its `/auth/<provider>` routes refuse. Non-Discord logins are spelled `google:<sub>` and `github:<numeric id>` — the provider's immutable subject, never an email or a login handle, and one namespace per provider so the same number arriving from two issuers is two different actors. Those are the spellings to put in an allowlist or a team; a denied login names its own actor id in the error message so it can be copied verbatim.

Web and Discord capability checks resolve from the **actor id only** — there is no guild-role lookup on either path, so a team membership means the same thing in chat and in the web UI.

---

## Related docs

- [Support case guide](../support-case-guide.md) — Discord case command detail for support  
- Design background: `docs/design-agentic-team-runtime.md` (code wins if design and code disagree)

---

## Mental model for leadership

Grok Work ties **one Discord thread = one worktree = one session**. Safe Team Mode splits **support** (cases, investigate, customer draft) from **engineering** (fix sessions that can open PRs or direct-ship). The web UI adds a second axis—viewer / member / admin—so private-network ops can browse safely while host feature flags gate destructive GitHub actions. Capabilities fail closed; unmapped users under Safe Team Mode become investigators by default, not builders.
