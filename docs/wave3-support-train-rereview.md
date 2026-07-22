# Wave 3 Support train — re-review (2026-07-22)

| Field | Value |
|-------|-------|
| **Status** | Approved for ship (this goal) |
| **Related** | `docs/design-agentic-team-runtime.md` rev 6; Wave 1 Trust train on `main` |
| **Audience** | Implementers |

## Fit with Wave 1 (confirmed)

| Wave 1 primitive | Wave 3 use |
|------------------|------------|
| `Entry.Mode` | `Mode=case` for entire lifecycle (K17); never switches to `fix` mid-case |
| `BuildRunPolicy` + execute gates | Phase drives AllowPR/AllowDirectShip: non-ship for intake/investigate/answered/closed; ship only fixing/shipping when GithubWrites |
| SafeTeamMode / investigator | Support opens `/case` + investigate without GithubWrites; escalate requires FileEscalation or builder |
| ShipMode (pr/direct) | Still orthogonal (K27): case investigate never integrates; case fixing honors ShipMode |
| Social queue / journal | Unchanged; case runs are normal tasks with Mode/Phase snapshotted |

## Ship scope (this train)

**In:**

1. Case fields on `sessionstore.Entry` (Phase, severity, customer ref/title, dossier, customer update, resolution, escalate timestamps, msg ids) + clamp + preserve
2. K18: `SuggestAutoLabel` / `ApplyAutoLabel` skip when `Mode=case && Phase=closed`; suppress PR-driven needs_review while case not fixing/shipping
3. Discord: `/case`, `/investigate` on case, `/escalate`, `/customer-update`, `/answer`, `/close`, `/board cases`
4. Dossier JSON merge from investigate replies (best-effort)
5. Customer-update sanitizer (paths, worktrees, tokens)
6. Escalation package injected into next fix prompt when Phase=fixing
7. Board filter `cases` / phase grouping

**Cut (explicit):**

- Web cases list / case panels (PR19–20) — Discord board is enough for this ship
- Timeline[] append-only event log (can use history turns)
- Linear create-on-intake (K23 bind-only remains)
- Perfect Discord dossier/customer cards (status lines + session fields; pin polish later)
- Wave 2 IDE-free features

## Risks accepted

- Dossier extract is heuristic (fenced JSON); empty dossier is valid
- Customer card may be text reply rather than a long-lived pin
- Web Support IA deferred; eng ship board unchanged
