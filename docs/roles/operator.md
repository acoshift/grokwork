# Role: operator

**Capability template** for support-style work that stays on the knowledge path.  
Builtin flags: `investigate`, `draftCustomerReply` only.

**Does not include** `fileEscalation` — operators cannot hand a case to engineering for a code fix unless an admin upgrades their template.

---

## Who this is for

- Support staff who should investigate and draft customer replies  
- Environments where **only eng** (or a smaller investigator set) may escalate  
- Lighter-weight than investigator when escalation must stay controlled  

Still requires project Discord allowlist + mapped channel.

---

## What they can do

- Open cases when investigate caps resolve (`/case`)  
- Run **read-only** investigate / explain work  
- Draft **customer updates** and mark **answered**  
- Close cases they own (owner / co-owner / mod rules)  
- Board, status, brief, claim/hand-off, queue helpers  

---

## What they cannot do

| Action | Outcome |
|--------|---------|
| `/escalate` | **Denied** — need `fileEscalation` or builder-class (`startSessions` / `githubWrites`) |
| Ship / open PR / direct-ship | Coerced to investigate or denied |
| `/start fix` as shipping | Not available |
| Escalate via web case panel | Escalate control hidden/denied without escalate caps |

Compared to **investigator**: same investigate + draft surface, **minus escalate**.

Compared to **builder**: no ship path; freeform stays non-shipping.

---

## Primary Discord commands

Always `@Grok …` first.

| Command | Purpose |
|---------|---------|
| `/case [severity] [ref:ID] <title>` | Open case |
| `/investigate <notes>` | Read-only dig |
| `/answer [draft]` | Phase → answered |
| `/customer-update <text>` | Sanitized customer text |
| `/close [resolution]` | Close (owner/co/mod) |
| `/board cases` | Case list |
| `/status`, `/brief`, `/help` | Continuity |

Case deep-dive: [support-case-guide.md](../support-case-guide.md).

**Do not expect** `/escalate` to succeed for pure operators.

---

## Web UI surfaces

Same pattern as investigator for browse + case intake when web **member+** and `startSessions` feature are on.

- Cases board, new case, session case panel (**investigate / answer / customer-update / close** when ownership allows)  
- Escalate button only if caps allow (operator builtin: **no**)  
- Ship / start fix composers not useful without ship caps  

Web cap resolution uses **user id only** — map operators in `capabilityByUser` for web-first staff.

---

## Typical workflow

1. `@Grok /case medium ref:CS-88 How do I reset 2FA?`  
2. `@Grok /investigate Check docs for self-serve reset path.`  
3. `@Grok /answer` + `/customer-update` with customer-safe steps.  
4. `@Grok /close answered`  

If code must change: ping someone with **investigator** (escalate) or **builder**, or ask an admin to remap the operator to investigator / builder for that project.

---

## Common errors

| Bot says… | What to do |
|-----------|------------|
| “You're not allowed to escalate cases (need fileEscalation or builder caps).” | Expected for operator — ask investigator/eng or admin remap |
| “You're not allowed to open cases…” | Caps missing investigate — check allowlist + template map |
| “You're not allowed to draft customer updates…” | Template missing `draftCustomerReply` (should not happen for builtin operator) |
| “You're not allowed to start fix tasks…” | No ship caps — correct for operator |

---

## How an admin assigns this role

1. Ensure user/role is on the project allowlist.  
2. **Config → project → Capability maps:** set Discord user or role → template **`operator`**.  
3. With Safe Team Mode on, either leave default as investigator and map operators explicitly, **or** set default template to `operator` if the whole project should not escalate by default.  
4. Map eng to `builder` / `approver` / `admin` as needed.

OR-merge: if the user also has a Discord role mapped to `investigator`, they gain `fileEscalation` from that role.
