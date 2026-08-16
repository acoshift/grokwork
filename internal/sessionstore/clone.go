package sessionstore

import "slices"

// clone returns a copy of e that shares no mutable state with the original.
//
// Every Store read path (Get, Patch, FindByCaseKey, List) used to hand back a
// bare struct copy of the stored Entry. A struct copy duplicates the scalar
// fields but shares every slice's backing array and every pointer field's
// target, so a returned Entry stayed wired to the one inside the store. That
// caused two distinct problems:
//
//   - A data race. A caller reading e.Issues while a concurrent Patch appended
//     to the stored entry's Issues touched the same backing array with no lock
//     held — Store.mu is released before the caller ever looks at the value.
//     Observed under -race as a web handler rendering a session against a
//     running task's upsertIssue.
//   - A silent write-through. CLAUDE.md is explicit that mutation goes through
//     Patch, but a caller doing e.PRs[0].State = … or e.Dossier.Summary = …
//     mutated store state directly and invisibly, skipping both the lock and
//     the save. The pointer fields made this especially easy: Dossier alone
//     carries five slices behind one shared pointer.
//
// Copying on read is the cheap fix and keeps Patch as the only writer. The
// entries here are small (a handful of short slices), and reads are per-request
// or per-poll rather than in a tight loop, so the extra allocation is not worth
// trading away the invariant.
//
// Nested element types are flat by design — TrackedPR, TrackedIssue,
// TrackedError, CheckpointMeta, LastVerify and DiscordRef hold only scalars, so slices.Clone
// fully detaches them. The two exceptions carry slices of their own and need
// per-element work: OpenQuestion.Options and every slice on Dossier. Adding a
// slice or pointer field to any of those types means extending this function —
// TestEntryCloneDetachesEveryField fails if a new mutable field is missed.
func (e Entry) clone() Entry {
	out := e

	out.CoOwnerIDs = slices.Clone(e.CoOwnerIDs)
	out.Issues = slices.Clone(e.Issues)
	out.Errors = slices.Clone(e.Errors)
	out.PRs = slices.Clone(e.PRs)
	out.RelatedCases = slices.Clone(e.RelatedCases)
	out.Checkpoints = slices.Clone(e.Checkpoints)
	out.WatcherIDs = slices.Clone(e.WatcherIDs)

	// OpenQuestion carries its own Options slice, so a shallow element copy
	// would still share it.
	if e.OpenQuestions != nil {
		qs := make([]OpenQuestion, len(e.OpenQuestions))
		for i, q := range e.OpenQuestions {
			q.Options = slices.Clone(q.Options)
			qs[i] = q
		}
		out.OpenQuestions = qs
	}

	// Pointer fields: give the copy its own target, or a caller mutating
	// through the pointer writes straight into the store.
	if e.Discord != nil {
		d := *e.Discord // flat
		out.Discord = &d
	}
	if e.LastVerify != nil {
		v := *e.LastVerify // flat
		out.LastVerify = &v
	}
	if e.Dossier != nil {
		d := *e.Dossier
		d.ReproSteps = slices.Clone(e.Dossier.ReproSteps)
		d.Evidence = slices.Clone(e.Dossier.Evidence)
		d.Hypotheses = slices.Clone(e.Dossier.Hypotheses)
		d.KnownBugHits = slices.Clone(e.Dossier.KnownBugHits)
		d.NextActions = slices.Clone(e.Dossier.NextActions)
		out.Dossier = &d
	}

	return out
}
