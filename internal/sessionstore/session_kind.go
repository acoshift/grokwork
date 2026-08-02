package sessionstore

// SessionKind values on Entry. Empty is ordinary work (Address/Fix reuse).
const (
	// SessionKindPRReview is an agentic PR review started from the PR detail
	// page. It binds the PR so the detail Sessions list can show it, but is
	// excluded from FindByPR (Address CI / Address review reuse).
	SessionKindPRReview = "pr_review"
)

// IsPRReview reports whether e is an agentic PR-review unit.
func (e Entry) IsPRReview() bool {
	return e.SessionKind == SessionKindPRReview
}
