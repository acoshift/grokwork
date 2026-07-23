# Role: approver

**Capability template** for senior engineers and tech leads.  
Builtin flags:

- From support-class: `investigate`, `draftCustomerReply`, `fileEscalation`  
- From builder-class: `startSessions`, `githubWrites`  
- Plus: **`approve`**

Does **not** include builtin `merge` or `adminProject` (those are on **admin**).

---

## Who this is for

- Tech leads who ship **and** handle support-style case actions end-to-end  
- People who may restore checkpoints / use approve-class gates beyond plain ownership  
- Mixed CS+eng hybrids that need escalate + draft + ship without full project-admin flags  

---

## What they can do

Everything a **builder** can do for ship/PR/direct (when project allows), **plus**:

- Full case lifecycle without relying only on builder-class side doors: draft customer replies and **file escalation** flags are explicit  
- **`approve` capability** — used in bot gates such as checkpoint restore when the actor is not thread controller: `Approve || AdminProject || CanShip()` (builders already pass via CanShip; pure approve is for non-ship templates if customized)  
- Investigate, open cases, answer, customer-update, escalate, close (ownership rules still apply for close)  

---

## What they cannot do

| Action | Notes |
|--------|-------|
| Builtin `merge` capability | Admin template only; bot never runs `gh pr merge` |
| Web config / host admin | Requires **web admin** role |
| Close cases they do not own | Still need owner, co-owner, or Discord mod |
| Bypass project allowlist | Membership is separate from capability templates |

---

## Primary Discord commands

Same eng set as [builder.md](./builder.md), plus full case set as [investigator.md](./investigator.md):

| Area | Commands |
|------|----------|
| Ship | freeform, `/start fix`, `/fix-ci`, `/address`, `/sync`, `/checkpoint`, `/verify` |
| Cases | `/case`, `/investigate`, `/escalate`, `/answer`, `/customer-update`, `/close`, `/board cases` |
| Continuity | `/status`, `/brief`, `/label`, `/link`, `/review`, ownership/queue |

`@Grok /help` for the live list.

---

## Web UI surfaces

Same as builder when web **member+** and features are enabled:

- Start task, ship board, issues/Linear fix, session controls  
- Case board + intake + session case panel (escalate/draft/investigate CTAs when caps allow)  
- GitHub write / merge / PR review features when host flags are on  

Prefer `capabilityByUser` → `approver` for web capability resolution (roles not applied on pure web starts).

---

## Typical workflow

**Lead on a support-originated bug**

1. Support opened and investigated; or approver opens `/case` themselves.  
2. `@Grok /escalate` with eng package (or already `fixing`).  
3. `@Grok /start fix …` implement and open PR.  
4. Drive review loop (`/address`, `/fix-ci`).  
5. `@Grok /customer-update` safe release note → `/close fixed`.  

**Pure eng work** — same as builder freeform fix.

---

## Common errors

Same denial strings as builder and investigator. Additionally:

| Situation | Meaning |
|-----------|---------|
| Expected approve but blocked | Gate may require ownership still (e.g. `/close`, cancel/reset) |
| Web cannot open case | Need `startSessions` feature + member+ **and** investigate/escalate/start caps on user map |
| Custom template missing Approve | Builtin approver has it; project overlay templates can strip flags |

---

## How an admin assigns this role

1. Allowlist the user/role on the project.  
2. **Capability maps:** Discord user or role → **`approver`**.  
3. Safe Team Mode on; do not leave leads unmapped (default investigator loses ship).  
4. For web: user id on project allowlist + feature flags as needed.
