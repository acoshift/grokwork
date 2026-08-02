---
name: scrutinize-before-ship
description: Mandatory pre-ship review gate for this repository. Load and run the scrutinize skill on the full change before any commit intended for main. Use when finishing implementation, about to commit, push, open a PR, or ship code on this repo — and whenever CLAUDE.md says scrutinize-then-ship.
---

# Scrutinize before ship

This repo forbids shipping without an outsider-style pass.

## When this applies

Any time you have code changes you intend to commit/push to `main` (or hand to a bot that ships them).

## Steps

1. Ensure tests/build for the change are green (or document why they cannot run).
2. **Load the in-repo `scrutinize` skill** (`.grok/skills/scrutinize/SKILL.md`, slash `/scrutinize`) and run its full workflow on the **complete** change vs the primary tip — not only a diff summary:
   - Intent (including simpler alternatives)
   - End-to-end path trace
   - Verify claims, edges, tests
   - Report with evidence
3. Fix every **blocker** and **major** finding. Re-run scrutinize if the fix is non-trivial.
4. Ship only with verdict **`ship`** (or after fixes reached `ship`).
5. Then commit and integrate per CLAUDE.md Workflow (worktree → main). Do not leave long-lived feature branches for routine work.

## Output

Include in your final message:

```
SCRUTINIZE_VERDICT: ship|fix-then-ship|rework|reject
```

plus 2–5 lines of evidence (paths traced or top findings). A bare LGTM is not a review.

## Rules

- Never skip this to save turns.
- Self-review still must follow the scrutinize skill procedure; do not invent a lighter checklist.
- The full procedure is vendored at `.grok/skills/scrutinize/SKILL.md` in this repository. If the skill is not in the skill list, **read that file** and follow it.
