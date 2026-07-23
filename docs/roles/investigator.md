# Role: investigator

**Capability template** for support and customer success.  
Builtin flags: `investigate`, `draftCustomerReply`, `fileEscalation`.

Under **Safe Team Mode**, unmapped allowlisted users default to this template (unless the project’s default template is changed).

---

## Who this is for

- Customer support / success / CS triage  
- Anyone who should dig into product behavior and write customer-facing answers **without** shipping code  
- Default “safe” persona when Safe Team Mode is on  

You need the project’s Discord allowlist (user or role) and a channel mapped to that project.

---

## What they can do

- Open support **cases** (`/case`) when they have investigate (or escalate/start) caps  
- Run **read-only** investigate / explain-style work (no PR, no push to primary)  
- Freeform messages on cases promote into investigate without shipping  
- Draft sanitized **customer updates** (`/customer-update`)  
- Mark the knowledge path **answered** (`/answer`)  
- **Escalate** a case to engineering (`/escalate` → phase `fixing`)  
- Close a case they **own** (or as co-owner / Discord mod): `/close`  
- Use continuity tools: `/status`, `/brief`, `/board cases`, `/claim`, `/hand-off`, queue helpers  
- Attach logs/screenshots on Discord messages for Grok to read  

Built-in: **no** `startSessions` / `githubWrites` — fix freeform is coerced to investigate policy.

---

## What they cannot do

| Action | Outcome |
|--------|---------|
| Open a PR / push fix branch as ship policy | Blocked / coerced to investigate |
| `/start fix …` shipping run | Denied or coerced (no ship) |
| Direct-ship to primary | Not available |
| Merge GitHub PRs (bot never merges PRs; eng does via gh/UI) | N/A for this role |
| Reopen a closed case in the same thread | Refused — open a **new** `/case` |

Escalate does **not** make the investigator a builder: eng with ship caps continues in the same case thread.

---

## Primary Discord commands

Always mention the bot first: `@Grok …`.

| Command | Purpose |
|---------|---------|
| `/case [severity] [ref:ID] <title>` | Open case (phase `intake`) |
| `/investigate <notes>` | Read-only dig |
| `/answer [draft]` | Knowledge path → phase `answered` |
| `/customer-update <text>` | Set sanitized customer-facing text |
| `/escalate [note]` | Hand to eng → phase `fixing` |
| `/close [answered\|fixed\|…]` | Close case (owner/co/mod) |
| `/board cases` | List open cases by phase |
| `/status`, `/brief`, `/help` | Thread state and help |

Deep case lifecycle detail: [support-case-guide.md](../support-case-guide.md).

Other useful commands: `/claim`, `/hand-off @user`, `/queue`, `/cancel-mine`, `/label` (as allowed).

---

## Web UI surfaces

Requires web OAuth **member+** (or admin) **and** host feature `startSessions` for write CTAs.

| Surface | Typical use |
|---------|-------------|
| Project **Cases** board | Browse case pipeline |
| **New case** (`/projects/{p}/cases/new`) | Web intake (same caps as Discord `/case`) |
| Session page **case panel** | Investigate / answer / customer-update / escalate / close when allowed |
| Sessions / history | Read progress |
| Ship board | Usually read-only for investigators (no ship caps) |

**Note:** Web resolves capabilities with Discord **user id only** (no guild roles). Map investigators by user ID if they primarily work from the web.

Without OAuth (open LAN), write features stay off (fail-closed).

---

## Typical workflow

1. In the support channel:  
   `@Grok /case high ref:CS-1234 Checkout fails after payment on iOS`
2. Inside the thread:  
   `@Grok /investigate Customer on iOS 18, app 4.2.1; Stripe succeeds but order pending.`
3. **No eng needed:**  
   `@Grok /answer …` → `/customer-update …` → `/close answered`
4. **Needs a code fix:**  
   `@Grok /escalate Repro + stack in thread. Suspect webhook.`  
   After eng ships: `/customer-update …` → `/close fixed`

---

## Common errors

| Bot says… | What to do |
|-----------|------------|
| “You're not allowed to use Grok on project …” | Not on project allowlist — ask admin |
| “You're not allowed to open cases…” | Caps lack investigate/escalate/start — check Safe Team mapping |
| “You're not allowed to escalate…” | Need `fileEscalation` or builder-class caps — ask admin |
| “You're not allowed to draft customer updates…” | Need `draftCustomerReply` (or escalate/builder class) |
| “Only the case owner, co-owner, or a mod can close.” | `/claim` or ask owner |
| “This case is **closed**…” | Open a **new** `/case` |
| Silent / no reply | Channel not mapped or allowlist empty |

---

## How an admin assigns this role

1. Project allowlist: Discord user ID and/or role ID.  
2. **Config → project → Safe team mode:** enable; keep default template `investigator` (or set it).  
3. **Capability maps:**  
   - User: Discord snowflake → template `investigator`  
   - Role: Discord role ID → `investigator`  
4. Optional: project **default mode** = `case` for support channels.  
5. Map eng to `builder` **before** enabling Safe Team Mode on mixed channels.

Multiple template hits (user + roles) are **OR-merged** (any true flag wins).
