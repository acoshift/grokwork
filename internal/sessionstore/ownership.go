package sessionstore

import "slices"

// HasOwner reports whether the session has a thread owner set.
func (e Entry) HasOwner() bool {
	return e.OwnerID != ""
}

// IsOwner reports whether userID is the primary thread owner.
func (e Entry) IsOwner(userID string) bool {
	return userID != "" && e.OwnerID == userID
}

// IsCoOwner reports whether userID is a co-owner.
func (e Entry) IsCoOwner(userID string) bool {
	if userID == "" {
		return false
	}
	return slices.Contains(e.CoOwnerIDs, userID)
}

// CanControl reports whether userID may cancel/reset (owner or co-owner).
// Unowned sessions return false — callers decide soft-open policy for that case.
func (e Entry) CanControl(userID string) bool {
	return e.IsOwner(userID) || e.IsCoOwner(userID)
}

// SetOwner sets the primary owner and clears them from co-owners if present.
func (e *Entry) SetOwner(userID, displayName string) {
	if e == nil || userID == "" {
		return
	}
	e.OwnerID = userID
	e.OwnerName = displayName
	e.CoOwnerIDs = removeID(e.CoOwnerIDs, userID)
}

// AddCoOwner adds a co-owner (no-op if empty, already owner, or already listed).
func (e *Entry) AddCoOwner(userID string) {
	if e == nil || userID == "" || e.OwnerID == userID {
		return
	}
	if slices.Contains(e.CoOwnerIDs, userID) {
		return
	}
	e.CoOwnerIDs = append(e.CoOwnerIDs, userID)
}

// HandOff transfers ownership to newOwnerID; previous owner becomes a co-owner.
func (e *Entry) HandOff(newOwnerID, newOwnerName string) {
	if e == nil || newOwnerID == "" {
		return
	}
	prevID := e.OwnerID
	e.SetOwner(newOwnerID, newOwnerName)
	if prevID != "" && prevID != newOwnerID {
		e.AddCoOwner(prevID)
	}
}

func removeID(ids []string, want string) []string {
	if len(ids) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != want {
			out = append(out, id)
		}
	}
	return out
}
