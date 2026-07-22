# Support guide: Discord case commands

How to open and run support cases with Grok Work in Discord.  
You do **not** need Git or an IDE. Everything happens in a Discord thread.

**Audience:** customer support / success (investigators).  
**Prerequisite:** your Discord user (or a role you have) is on the project’s allowlist, and the channel is mapped to a support project. If `@Grok` does nothing, ask an admin.

---

## Mental model (30 seconds)

| Concept | Meaning |
|--------|---------|
| **Case** | One customer problem = one Discord **thread** = one bot session |
| **Phase** | Where the case is in its lifecycle (`intake` → `investigate` → `answered` or `fixing` → `closed`) |
| **Investigate** | Read-only: Grok can search code/logs and explain; it **does not open PRs** or ship code |
| **Escalate** | Hand the case to engineering for a code fix (phase becomes `fixing`) |
| **Answer** | Knowledge path: you have a reply for the customer; no code change needed |

Always mention the bot first: `@Grok …`.

---

## Quick start (happy path)

1. In the support channel, open a case:

   ```text
   @Grok /case high Checkout fails on iOS after payment
   ```

   Optional severity: `low` · `medium` (default) · `high` · `critical`  
   Optional ticket id: `ref:CS-1234`

   Example with both:

   ```text
   @Grok /case high ref:CS-1234 Customer cannot complete checkout on iOS
   ```

2. Grok opens a **thread** (phase **intake**). Work only inside that thread.

3. Investigate (paste customer notes, screenshots, error text):

   ```text
   @Grok /investigate Customer on iOS 18, app 4.2.1. Payment succeeds in Stripe but order stays pending. Screenshot attached.
   ```

   You can also send a normal freeform message after the case is open; it usually promotes the case into investigate and runs the same way.

4. When you know the customer-facing answer (no eng needed):

   ```text
   @Grok /answer Please reinstall the app and update to 4.2.2; known race fixed last week.
   @Grok /customer-update Please reinstall the app and update to 4.2.2. If it still fails, reply with your order id.
   @Grok /close answered
   ```

5. When eng must change code:

   ```text
   @Grok /escalate Repro steps in thread; stack trace in attachment. Suspect order-status webhook.
   ```

   Engineering continues in the **same thread**. When fixed and the customer can be told:

   ```text
   @Grok /customer-update We shipped a fix in 4.2.3; please update and try again.
   @Grok /close fixed
   ```

---

## Command reference

Every command requires `@Grok` first.

### `/case` — open a case

```text
@Grok /case [severity] [ref:ID] <customer-facing title>
```

| Piece | Required | Notes |
|-------|----------|--------|
| severity | no | `low` / `medium` / `high` / `critical` (or sev1–4) |
| `ref:ID` | no | External ticket / CRM id |
| title | **yes** | Short description; becomes the case title |

- Opens a thread if you are in a parent channel.  
- Sets **Mode=case**, phase **intake**.  
- Do **not** run `/case` again in an engineering-only thread; start a new message in the parent channel.  
- Closed cases cannot be reopened with `/case` — open a **new** case.

### `/investigate` — dig without shipping

```text
@Grok /investigate <what you know / what to check>
```

- Read-only analysis (code search, explain, gather evidence).  
- **No pull request, no push to main.**  
- Prefer this over freeform if you want to be explicit.  
- Attach logs/screenshots on the Discord message when possible.

### `/escalate` — hand to engineering

```text
@Grok /escalate [optional note for eng]
```

- Case only; run **inside the case thread**.  
- Phase → **fixing** (Mode stays **case**).  
- Needs escalate permission (`fileEscalation` or builder-class caps). Support “investigator” templates usually include this; if you get “not allowed,” ask admin.  
- Next eng run gets an escalation package (context for the fix).

### `/answer` — knowledge / non-code resolution path

```text
@Grok /answer [optional draft text for the customer]
```

- Phase → **answered**.  
- Optional text is stored as a sanitized customer update.  
- Follow with `/customer-update` if you need to refine the wording, then `/close answered`.

### `/customer-update` — set what you can send the customer

```text
@Grok /customer-update <text safe for the customer>
```

- Or `@Grok /customer-update` alone to **show** the current text.  
- The bot **redacts secrets** (tokens, keys, internal paths, etc.). Prefer plain language; no internal repo paths or stack dumps.  
- Use this as the source of truth for what you paste into email/CRM.

### `/close` — finish the case

```text
@Grok /close [resolution] [optional note]
```

| Resolution | Typical use | Label |
|------------|-------------|--------|
| `answered` | (default) knowledge / config / how-to | done |
| `fixed` | eng shipped a fix | done |
| `duplicate` | same as another case | done |
| `wontfix` | not fixing | abandoned |
| `escalated_external` | handed outside the team | abandoned |

- Only the case **owner**, co-owner, or a Discord **mod** can close (investigators who own the case can close).  
- Closed cases are **frozen** (no more investigate/answer/customer-update). Open a new case if the customer returns.

### `/board cases` — list open cases

```text
@Grok /board cases
```

Lists case sessions for **this channel’s project**, grouped by phase.  
Other board filters (`running`, `queued`, `waiting`, `stale`, labels, `all`) still work for team activity.

---

## Lifecycle (phases)

```text
  /case
    ↓
 intake ──/investigate or freeform──► investigate
    │                                      │
    │                    /answer           │ /escalate
    │                         ↓            ↓
    │                     answered      fixing  (eng implements)
    │                         │            │
    └──────── /close ─────────┴────────────┘
                         closed
```

| Phase | You can… |
|-------|----------|
| **intake** | Investigate, freeform, escalate, answer, close |
| **investigate** | Keep investigating, escalate, answer, close |
| **answered** | Refine `/customer-update`, close; freeform may re-open investigate |
| **fixing** | Eng work in-thread; still set customer update and close when done |
| **closed** | Read-only; start a **new** `/case` if needed |

---

## What support **cannot** do (by design)

With the default **investigator** capability template under Safe Team Mode:

| Action | Result |
|--------|--------|
| Freeform “please fix this in code and open a PR” | Blocked or coerced to investigate-only — **no PR** |
| `/start fix …` without escalate rights | Denied |
| Ship / merge | Not available to support templates |
| Reopen a closed case with `/case` in the same thread | Refused — open a new case |

If you need a code change: **`/escalate`**, then ping eng in the thread.

---

## Tips that save time

1. **Put facts in the first investigate message** — OS/app version, steps, expected vs actual, order/user id (non-secret), timestamps.  
2. **One case per problem** — don’t pile unrelated issues into one thread.  
3. **Use `ref:`** so CRM and Discord stay linked.  
4. **Customer text goes only through `/customer-update`** — never paste raw secrets or internal paths into customer channels.  
5. **`/status`** — owner, phase/label, queue, PR (if eng opened one).  
6. **`/brief`** — sticky summary of goal / done / left (useful after hand-off).  
7. **Queue** — if Grok is busy, follow-ups queue; `/queue`, `/cancel-mine` if needed.  
8. **Attachments** — screenshots and logs on the Discord message are included in the run context.

---

## Escalation checklist (for eng)

When you `/escalate`, leave eng a complete package:

- [ ] Repro steps (or “not reproducible; customer report only”)  
- [ ] Environment (prod/staging, app version, platform)  
- [ ] Relevant IDs (`ref:`, order id, account id — non-secret)  
- [ ] What you already checked (and ruled out)  
- [ ] Desired customer outcome  

Eng continues in the **same thread** (Mode stays **case**). Support can still draft `/customer-update` and `/close fixed` after the fix lands.

---

## Common errors

| Bot says… | What to do |
|-----------|------------|
| “You're not allowed to open cases…” | Ask admin to map your user/role (investigate caps / Safe Team template) |
| “This thread is already a **fix** session…” | `/case` belongs in a **new** thread from the parent channel |
| “This case is **closed**…” | Open a **new** `/case` for follow-up |
| “You're not allowed to escalate…” | Need escalate/fileEscalation or builder mapping — ask admin |
| “Customer update empty after sanitizer” | Text was only secrets/paths; rewrite in plain language |
| Silent / no reply | Confirm channel is mapped and you are on the project allowlist |

---

## Admin note (for ops setting up support)

On the project settings page (**Safe team mode**):

1. Enable **Safe team mode**.  
2. Set **Default mode** to `case` for support channels.  
3. Leave **Default template** as `investigator` (or `operator`).  
4. Map support Discord roles → `investigator` (optional if default is already investigator).  
5. **Map eng roles → `builder` before enabling**, or eng will be demoted to investigator.

---

## Cheat sheet

```text
@Grok /case high ref:CS-99 Title here
@Grok /investigate <notes + context>
@Grok /answer [optional draft]
@Grok /customer-update <safe text for customer>
@Grok /close answered

@Grok /escalate <note for eng>
@Grok /close fixed

@Grok /board cases
@Grok /status
@Grok /help
```
