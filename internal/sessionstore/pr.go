package sessionstore

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// TrackedPR is one GitHub pull request linked to a Discord thread.
type TrackedPR struct {
	URL     string `json:"url,omitempty"`
	Number  int    `json:"number,omitempty"`
	State   string `json:"state,omitempty"` // OPEN, MERGED, CLOSED
	Title   string `json:"title,omitempty"`
	Checks  string `json:"checks,omitempty"`
	Review  string `json:"review,omitempty"`
	HeadSHA string `json:"headSha,omitempty"`
	HeadRef string `json:"headRef,omitempty"`
	IsDraft bool   `json:"isDraft,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Repo    string `json:"repo,omitempty"`
	// Discord status card message for this PR.
	StatusMsgID string `json:"statusMsgId,omitempty"`

	// CI triage (per PR).
	CINotifiedSHA  string `json:"ciNotifiedSha,omitempty"`
	CIAutoFixCount int    `json:"ciAutoFixCount,omitempty"`
	CIAutoFixSHA   string `json:"ciAutoFixSha,omitempty"`
}

var githubPRURLRE = regexp.MustCompile(`(?i)https?://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/(\d+)`)

// PRKey returns a stable identity for matching tracked PRs.
func (p TrackedPR) PRKey() string {
	if u := strings.TrimSpace(p.URL); u != "" {
		return strings.ToLower(strings.TrimRight(u, "/"))
	}
	if p.Owner != "" && p.Repo != "" && p.Number > 0 {
		return strings.ToLower(fmt.Sprintf("https://github.com/%s/%s/pull/%d", p.Owner, p.Repo, p.Number))
	}
	if p.Number > 0 {
		return fmt.Sprintf("#%d", p.Number)
	}
	return ""
}

// RepoSlug returns owner/repo when known.
func (p TrackedPR) RepoSlug() string {
	if p.Owner != "" && p.Repo != "" {
		return p.Owner + "/" + p.Repo
	}
	return ""
}

// Selector is the best gh argument for this PR (prefer full URL).
func (p TrackedPR) Selector() string {
	if u := strings.TrimSpace(p.URL); u != "" {
		return u
	}
	if p.Owner != "" && p.Repo != "" && p.Number > 0 {
		return fmt.Sprintf("https://github.com/%s/%s/pull/%d", p.Owner, p.Repo, p.Number)
	}
	if p.Number > 0 {
		return strconv.Itoa(p.Number)
	}
	return ""
}

// FillOwnerRepoFromURL parses owner/repo/number from URL when missing.
func (p *TrackedPR) FillOwnerRepoFromURL() {
	if p == nil {
		return
	}
	m := githubPRURLRE.FindStringSubmatch(p.URL)
	if len(m) < 4 {
		return
	}
	if p.Owner == "" {
		p.Owner = m[1]
	}
	if p.Repo == "" {
		p.Repo = m[2]
	}
	if p.Number <= 0 {
		if n, err := strconv.Atoi(m[3]); err == nil {
			p.Number = n
		}
	}
	if p.URL == "" && p.Owner != "" && p.Repo != "" && p.Number > 0 {
		p.URL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", p.Owner, p.Repo, p.Number)
	}
}

// NormalizePRs migrates legacy single-PR fields into PRs and keeps legacy in sync.
func (e *Entry) NormalizePRs() {
	if e == nil {
		return
	}
	if len(e.PRs) == 0 && (e.PRNumber > 0 || e.PRURL != "") {
		pr := TrackedPR{
			URL:            e.PRURL,
			Number:         e.PRNumber,
			State:          e.PRState,
			Title:          e.PRTitle,
			Checks:         e.PRChecks,
			Review:         e.PRReview,
			HeadSHA:        e.PRHeadSHA,
			IsDraft:        e.PRIsDraft,
			StatusMsgID:    e.PRStatusMsgID,
			CINotifiedSHA:  e.CINotifiedSHA,
			CIAutoFixCount: e.CIAutoFixCount,
			CIAutoFixSHA:   e.CIAutoFixSHA,
		}
		pr.FillOwnerRepoFromURL()
		e.PRs = []TrackedPR{pr}
	}
	for i := range e.PRs {
		e.PRs[i].FillOwnerRepoFromURL()
	}
	e.syncLegacyFromPRs()
}

// UpsertPR inserts or updates a tracked PR (match by URL / owner/repo#n).
func (e *Entry) UpsertPR(pr TrackedPR) {
	if e == nil {
		return
	}
	pr.FillOwnerRepoFromURL()
	key := pr.PRKey()
	if key == "" {
		return
	}
	e.NormalizePRs()
	for i := range e.PRs {
		if e.PRs[i].PRKey() == key || samePR(e.PRs[i], pr) {
			prev := e.PRs[i]
			if pr.StatusMsgID == "" {
				pr.StatusMsgID = prev.StatusMsgID
			}
			if pr.CINotifiedSHA == "" {
				pr.CINotifiedSHA = prev.CINotifiedSHA
			}
			if pr.CIAutoFixSHA == "" {
				pr.CIAutoFixSHA = prev.CIAutoFixSHA
			}
			// Preserve auto-fix count unless the caller increased it.
			if pr.CIAutoFixCount < prev.CIAutoFixCount {
				pr.CIAutoFixCount = prev.CIAutoFixCount
			}
			e.PRs[i] = pr
			e.syncLegacyFromPRs()
			return
		}
	}
	e.PRs = append(e.PRs, pr)
	e.syncLegacyFromPRs()
}

func samePR(a, b TrackedPR) bool {
	if a.Number > 0 && a.Number == b.Number {
		if a.Owner != "" && b.Owner != "" && a.Repo != "" && b.Repo != "" {
			return strings.EqualFold(a.Owner, b.Owner) && strings.EqualFold(a.Repo, b.Repo)
		}
		// Same number only: treat as same when one side lacks owner (legacy).
		if a.Owner == "" || b.Owner == "" {
			return true
		}
	}
	return false
}

// HasAnyPR reports whether the session tracks at least one PR.
func (e Entry) HasAnyPR() bool {
	e.NormalizePRs()
	return len(e.PRs) > 0
}

// HasOpenPR reports whether any tracked PR is still open.
func (e Entry) HasOpenPR() bool {
	for _, p := range e.OpenPRs() {
		_ = p
		return true
	}
	return false
}

// OpenPRs returns non-terminal tracked PRs.
func (e Entry) OpenPRs() []TrackedPR {
	e.NormalizePRs()
	var out []TrackedPR
	for _, p := range e.PRs {
		if !isTerminalState(p.State) {
			out = append(out, p)
		}
	}
	return out
}

// AllPRsTerminal is true when there is at least one PR and none are open.
func (e Entry) AllPRsTerminal() bool {
	e.NormalizePRs()
	if len(e.PRs) == 0 {
		return false
	}
	for _, p := range e.PRs {
		if !isTerminalState(p.State) {
			return false
		}
	}
	return true
}

// PrimaryPR returns the preferred PR for single-slot displays (first open, else last).
func (e Entry) PrimaryPR() (TrackedPR, bool) {
	e.NormalizePRs()
	if len(e.PRs) == 0 {
		return TrackedPR{}, false
	}
	for _, p := range e.PRs {
		if !isTerminalState(p.State) {
			return p, true
		}
	}
	return e.PRs[len(e.PRs)-1], true
}

// FindPR looks up a tracked PR by URL, number, or owner/repo#n text.
func (e Entry) FindPR(query string) (TrackedPR, bool) {
	e.NormalizePRs()
	query = strings.TrimSpace(query)
	if query == "" {
		return TrackedPR{}, false
	}
	qKey := strings.ToLower(strings.TrimRight(query, "/"))
	for _, p := range e.PRs {
		if p.PRKey() == qKey || strings.EqualFold(p.URL, query) {
			return p, true
		}
		if m := githubPRURLRE.FindStringSubmatch(query); len(m) >= 4 {
			n, _ := strconv.Atoi(m[3])
			if n == p.Number && strings.EqualFold(m[1], p.Owner) && strings.EqualFold(m[2], p.Repo) {
				return p, true
			}
		}
		// Bare #42 or 42
		if num, err := strconv.Atoi(strings.TrimPrefix(query, "#")); err == nil && num == p.Number {
			// Prefer exact if only one match
			matches := 0
			var last TrackedPR
			for _, q := range e.PRs {
				if q.Number == num {
					matches++
					last = q
				}
			}
			if matches == 1 {
				return last, true
			}
		}
		// owner/repo#42
		if i := strings.LastIndex(query, "#"); i > 0 {
			slug := query[:i]
			num, err := strconv.Atoi(query[i+1:])
			if err == nil && num == p.Number && strings.EqualFold(slug, p.RepoSlug()) {
				return p, true
			}
		}
	}
	// Second pass bare number when unique.
	if num, err := strconv.Atoi(strings.TrimPrefix(query, "#")); err == nil {
		var found []TrackedPR
		for _, p := range e.PRs {
			if p.Number == num {
				found = append(found, p)
			}
		}
		if len(found) == 1 {
			return found[0], true
		}
	}
	return TrackedPR{}, false
}

// PatchPR updates one PR in place by key.
func (e *Entry) PatchPR(key string, fn func(*TrackedPR)) bool {
	if e == nil {
		return false
	}
	e.NormalizePRs()
	key = strings.ToLower(strings.TrimRight(strings.TrimSpace(key), "/"))
	for i := range e.PRs {
		if e.PRs[i].PRKey() == key {
			fn(&e.PRs[i])
			e.PRs[i].FillOwnerRepoFromURL()
			e.syncLegacyFromPRs()
			return true
		}
	}
	return false
}

func (e *Entry) syncLegacyFromPRs() {
	if e == nil {
		return
	}
	if len(e.PRs) == 0 {
		e.PRURL = ""
		e.PRNumber = 0
		e.PRState = ""
		e.PRTitle = ""
		e.PRChecks = ""
		e.PRReview = ""
		e.PRHeadSHA = ""
		e.PRIsDraft = false
		e.PRStatusMsgID = ""
		return
	}
	// Primary = first open, else last.
	p := e.PRs[len(e.PRs)-1]
	for _, x := range e.PRs {
		if !isTerminalState(x.State) {
			p = x
			break
		}
	}
	e.PRURL = p.URL
	e.PRNumber = p.Number
	e.PRState = p.State
	e.PRTitle = p.Title
	e.PRChecks = p.Checks
	e.PRReview = p.Review
	e.PRHeadSHA = p.HeadSHA
	e.PRIsDraft = p.IsDraft
	e.PRStatusMsgID = p.StatusMsgID
	e.CINotifiedSHA = p.CINotifiedSHA
	e.CIAutoFixCount = p.CIAutoFixCount
	e.CIAutoFixSHA = p.CIAutoFixSHA
}

func isTerminalState(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "MERGED", "CLOSED":
		return true
	default:
		return false
	}
}
