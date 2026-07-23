# Role: admin (capability template)

**Project capability template** named `admin` — **not** the same as Discord “Server Admin” and **not** automatically the same as **web admin**.

Builtin flags (full set):

| Flag | |
|------|--|
| `investigate` | ✓ |
| `draftCustomerReply` | ✓ |
| `fileEscalation` | ✓ |
| `startSessions` | ✓ |
| `githubWrites` | ✓ |
| `approve` | ✓ |
| `merge` | ✓ |
| `adminProject` | ✓ |

This is the richest **project-scoped** Discord capability bundle.

---

## Who this is for

- Project owners who both ship and own process (cases, approvals, full flags)  
- A small set of people who should never hit capability denials on that project  
- **Not** a substitute for host **web admin** (config.json / Safe Team UI still needs `WebRoleAdmin`)  

---

## What they can do

- Everything **approver** and **builder** can do (ship, investigate, cases)  
- Explicit **`merge`** and **`adminProject`** flags for future/gates that check them  
- Today’s bot gates that honor `AdminProject` include approve-class checks (e.g. checkpoint restore alongside `Approve` / `CanShip`)  
- Open cases, escalate, draft, close (with ownership/mod rules for close)  
- Thread control when also owner/co/mod for cancel/reset  

**Important product invariant:** the Discord bot **does not merge GitHub PRs** (`gh pr merge`). Merging in the product sense is:

- Human on GitHub, or  
- Web UI merge POST when host feature `merge` is on and the user is web **member+**  

The capability flag `merge` on this template marks project-level intent; do not assume `@Grok` will merge a PR.

---

## What they cannot do (alone)

| Gap | How to get it |
|-----|----------------|
| Edit global/project config in web UI | **Web role admin** (`webAuth.adminDiscordIds`) |
| Web write features when flags are off | Turn on `webAuth.features.*` (admin config) |
| Act on projects without allowlist membership | Still need AccessAllowed (admin web role sees all projects in UI; Discord still uses channel map + allowlist) |
| Bypass Safe Team maps for others | Admins configure maps; their own Discord actions still resolve via maps + this template if assigned |

---

## Primary Discord commands

Full surface of builder + investigator (see those docs). Live list: `@Grok /help`.

Highlights:

- Freeform fix / `/start fix|investigate|explain`  
- Case lifecycle: `/case`, `/investigate`, `/escalate`, `/answer`, `/customer-update`, `/close`  
- Ship hygiene: `/fix-ci`, `/address`, `/sync`, `/checkpoint`, `/verify`, `/comments`  
- Continuity: `/status`, `/brief`, `/board`, `/label`, `/link`, `/review`  

---

## Web UI surfaces

Depends on **web role**, not this template name:

| Need | Requirement |
|------|-------------|
| Browse all projects | Web **admin** |
| Config / Safe Team / capability maps | Web **admin** + `/config` routes |
| Start sessions / cases / merge buttons | Web **member+** + feature flags; case open also needs project caps on user id |

A person can be capability-template `admin` on Discord and still be web **viewer** if only listed in viewer IDs—fix those lists deliberately.

---

## Typical workflow

1. Admin maps Safe Team templates for support (`investigator`) and eng (`builder`).  
2. Uses Discord as a full eng + case actor on critical threads.  
3. Uses web **as web admin** to adjust allowlists, Safe Team Mode, and host feature flags.  

---

## Common errors

| Situation | Fix |
|-----------|-----|
| “forbidden: admin required” on `/config` | Discord template admin ≠ web admin — add to `webAuth.adminDiscordIds` |
| Still demoted to investigator | User not mapped to template `admin` (or higher OR-merge); Safe Team default applies |
| Merge button missing | Enable `webAuth.features.merge` and ensure web member+ |
| “You're not allowed to use Grok…” | Not on project allowlist for Discord path |

---

## How an admin assigns this role

1. Project allowlist the Discord user/role.  
2. **Capability maps:** user or role → template **`admin`**.  
3. Separately, for host config: put Discord user id in **`webAuth.adminDiscordIds`** (or bootstrap env).  
4. Prefer few capability-template admins; use `builder` / `approver` for day-to-day eng.

---

## Related roles

| Name | Meaning |
|------|---------|
| Capability **admin** (this doc) | Project Discord/web-start capability flags |
| [Web admin](./web-admin.md) | OAuth role for config UI and all-project visibility |
| Discord server Administrator | Platform permission for `/cancel`/`/reset` as “mod”; not a grokwork template |
