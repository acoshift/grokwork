package sessionstore

// SessionKind values on Entry. Empty is ordinary work (Address/Fix reuse).
const (
	// SessionKindPRReview is an agentic PR review started from the PR detail
	// page. It binds the PR so the detail Sessions list can show it, but is
	// excluded from FindByPR (Address CI / Address review reuse).
	SessionKindPRReview = "pr_review"

	// SessionKindImportedPR is a web-native shell created by the background
	// importer for a GitHub PR grokwork did not open. It binds the PR so Ship
	// and Address reuse work, but has no worktree, agent session, or owner
	// until the first run. Auto-fix and auto-label skip these shells.
	SessionKindImportedPR = "imported_pr"

	// SessionKindPRAsk is a throwaway, per-viewer Q&A on a PR detail page.
	// It is not a workflow unit: excluded from /sessions, search, Address
	// reuse, and the PR Sessions table. Lookup is Entry.AskPRKey, not PRs[].
	SessionKindPRAsk = "pr_ask"
)

// IsPRReview reports whether e is an agentic PR-review unit.
func (e Entry) IsPRReview() bool {
	return e.SessionKind == SessionKindPRReview
}

// IsImportedPR reports whether e is an import shell that has not yet run.
func (e Entry) IsImportedPR() bool {
	return e.SessionKind == SessionKindImportedPR
}

// IsPRAsk reports whether e is a throwaway in-page PR ask.
func (e Entry) IsPRAsk() bool {
	return e.SessionKind == SessionKindPRAsk
}
