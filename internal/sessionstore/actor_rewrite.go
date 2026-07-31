package sessionstore

import "slices"

// RewriteActor points every actor-id field the store keeps at to, wherever
// same reports the stored id denotes that same actor. It returns how many
// fields changed.
//
// This is the sessionstore half of absorbing a linked login (see
// internal/identity): once two logins are one account, the alias is never
// minted again, so a unit still owned by it has an owner who can no longer
// cancel, claim or find it.
//
// same is supplied by the caller — actor-id spelling rules live in
// internal/config, which this package deliberately does not depend on. It must
// be pure: it is called while the store lock is held, so reaching back into the
// store from inside it deadlocks.
//
// Two deliberate differences from doing this with Patch, which is otherwise the
// only way to mutate an entry:
//
//   - One save for the whole rewrite, not one per entry. Absorbing touches an
//     unbounded number of units at once, and each Patch is a separate atomic
//     file replacement.
//   - UpdatedAt is not touched (Patch no longer stamps it either). Turn time is
//     only written by TouchTurn / Entry.StampTurn — linking a login is not a turn.
//
// Lists collapse rather than duplicate: a co-owner or watcher list that already
// names the account keeps one entry, not two.
func (s *Store) RewriteActor(same func(actorID string) bool, to string) (int, error) {
	if s == nil || same == nil || to == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	rewriteOne := func(field *string) {
		if *field == "" || !same(*field) {
			return
		}
		*field = to
		n++
	}
	rewriteList := func(ids []string) []string {
		if len(ids) == 0 {
			return ids
		}
		hit := false
		for _, id := range ids {
			if same(id) {
				hit = true
				break
			}
		}
		if !hit {
			return ids
		}
		already := slices.Contains(ids, to)
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			if !same(id) {
				out = append(out, id)
				continue
			}
			n++
			if already {
				continue
			}
			out = append(out, to)
			already = true
		}
		return out
	}

	for threadID, stored := range s.entries {
		before := n
		// Mutate a detached copy: an in-place edit that a failed save leaves
		// behind is a store whose memory and disk disagree.
		e := stored.clone()
		rewriteOne(&e.OwnerID)
		rewriteOne(&e.CreatedBy)
		rewriteOne(&e.ReporterID)
		rewriteOne(&e.EngineerID)
		rewriteOne(&e.EscalatedBy)
		rewriteOne(&e.ResolvedBy)
		rewriteOne(&e.ReopenedBy)
		e.CoOwnerIDs = rewriteList(e.CoOwnerIDs)
		e.WatcherIDs = rewriteList(e.WatcherIDs)
		for i := range e.Checkpoints {
			rewriteOne(&e.Checkpoints[i].CreatedBy)
		}
		// An owner rewritten into a co-owner they already were would be listed
		// twice; SetOwner's own invariant is that the two never overlap.
		if e.OwnerID != "" {
			if next := removeID(e.CoOwnerIDs, e.OwnerID); len(next) != len(e.CoOwnerIDs) {
				e.CoOwnerIDs = next
			}
		}
		if n == before {
			continue
		}
		s.entries[threadID] = e
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.save(); err != nil {
		return 0, err
	}
	return n, nil
}
