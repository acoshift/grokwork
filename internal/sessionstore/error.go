package sessionstore

import (
	"fmt"
	"strings"
)

const (
	ErrorProviderGCP     = "gcp"
	ErrorProviderSentry  = "sentry"
	ErrorProviderDeploys = "deploys"
	maxTrackedErrors     = 3
)

// TrackedError is a production error group bound to a session.
// Scalars only — Entry.clone is slices.Clone. Never store a stack.
// LastSeen is RFC3339 string to match Entry's other timestamps.
type TrackedError struct {
	Provider    string `json:"provider"`
	ID          string `json:"id"`
	ShortID     string `json:"shortId,omitempty"`
	Title       string `json:"title,omitempty"`
	URL         string `json:"url,omitempty"`
	Status      string `json:"status,omitempty"`
	Count       int64  `json:"count,omitempty"`
	LastSeen    string `json:"lastSeen,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Location    string `json:"location,omitempty"`
	Resource    string `json:"resource,omitempty"`
}

func (e TrackedError) ErrorKey() string {
	switch strings.ToLower(strings.TrimSpace(e.Provider)) {
	case ErrorProviderDeploys:
		return "deploys:" + e.Location + "/" + e.Resource + "/" + e.ID
	case ErrorProviderSentry:
		if sid := strings.ToUpper(strings.TrimSpace(e.ShortID)); sid != "" {
			return "sentry:" + sid
		}
		return "sentry:" + e.ID
	default:
		return e.Provider + ":" + e.ID
	}
}

func (e TrackedError) DisplayRef() string {
	if sid := strings.TrimSpace(e.ShortID); sid != "" {
		return sid
	}
	id := strings.TrimSpace(e.ID)
	if len(id) > 24 {
		return id[:12] + "…"
	}
	return id
}

// SameError reports whether two tracked errors refer to the same group.
func SameError(a, b TrackedError) bool { return sameError(a, b) }

func sameError(a, b TrackedError) bool {
	ap := strings.ToLower(strings.TrimSpace(a.Provider))
	bp := strings.ToLower(strings.TrimSpace(b.Provider))
	if ap == "" || ap != bp {
		return false
	}
	switch ap {
	case ErrorProviderDeploys:
		return a.ErrorKey() == b.ErrorKey()
	case ErrorProviderSentry:
		if a.ID != "" && a.ID == b.ID {
			return true
		}
		as := strings.ToUpper(strings.TrimSpace(a.ShortID))
		bs := strings.ToUpper(strings.TrimSpace(b.ShortID))
		return as != "" && as == bs
	default:
		return a.ID != "" && a.ID == b.ID
	}
}

// ErrTooManyTrackedErrors is returned when UpsertError would exceed the cap.
var ErrTooManyTrackedErrors = fmt.Errorf("at most %d bound errors", maxTrackedErrors)

// UpsertError inserts or updates a bound error. The 4th distinct error is refused.
func (e *Entry) UpsertError(err TrackedError) error {
	if e == nil {
		return fmt.Errorf("nil entry")
	}
	err.Provider = strings.ToLower(strings.TrimSpace(err.Provider))
	err.ID = strings.TrimSpace(err.ID)
	err.ShortID = strings.TrimSpace(err.ShortID)
	if err.Provider == "" || (err.ID == "" && err.ShortID == "") {
		return fmt.Errorf("error provider and id required")
	}
	for i := range e.Errors {
		if sameError(e.Errors[i], err) {
			prev := e.Errors[i]
			if err.ID == "" {
				err.ID = prev.ID
			}
			if err.ShortID == "" {
				err.ShortID = prev.ShortID
			}
			if err.Title == "" {
				err.Title = prev.Title
			}
			if err.URL == "" {
				err.URL = prev.URL
			}
			if err.Status == "" {
				err.Status = prev.Status
			}
			if err.Fingerprint == "" {
				err.Fingerprint = prev.Fingerprint
			}
			if err.Location == "" {
				err.Location = prev.Location
			}
			if err.Resource == "" {
				err.Resource = prev.Resource
			}
			if err.LastSeen == "" {
				err.LastSeen = prev.LastSeen
			}
			if err.Count == 0 {
				err.Count = prev.Count
			}
			e.Errors[i] = err
			return nil
		}
	}
	if len(e.Errors) >= maxTrackedErrors {
		return ErrTooManyTrackedErrors
	}
	e.Errors = append(e.Errors, err)
	return nil
}

// RemoveError drops a bound error matching query (ErrorKey, id, or shortId).
func (e *Entry) RemoveError(query string) bool {
	if e == nil {
		return false
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	out := e.Errors[:0]
	removed := false
	for _, err := range e.Errors {
		if errorMatchesQuery(err, query) {
			removed = true
			continue
		}
		out = append(out, err)
	}
	if removed {
		e.Errors = out
	}
	return removed
}

// ClearErrors removes all bound errors.
func (e *Entry) ClearErrors() {
	if e == nil {
		return
	}
	e.Errors = nil
}

// FindError looks up a bound error by ErrorKey, id, or shortId.
func (e Entry) FindError(query string) (TrackedError, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return TrackedError{}, false
	}
	for _, err := range e.Errors {
		if errorMatchesQuery(err, query) {
			return err, true
		}
	}
	return TrackedError{}, false
}

func errorMatchesQuery(err TrackedError, query string) bool {
	if err.ErrorKey() == query {
		return true
	}
	if err.ID != "" && err.ID == query {
		return true
	}
	if err.ShortID != "" && strings.EqualFold(err.ShortID, query) {
		return true
	}
	if err.DisplayRef() != "" && strings.EqualFold(err.DisplayRef(), query) {
		return true
	}
	return false
}
