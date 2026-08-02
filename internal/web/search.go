package web

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Search — one box over everything the instance has accumulated.
//
// A case key is minted to be quotable: WEBAPP-14 goes into a commit message, a
// PR title, another team's chat. Until this page there was nowhere to paste it
// back into, and the same was true of a PR number, a goal half-remembered from
// last month, or a short sha. Every record was addressable and none of it was
// findable.
//
// Two rules shape the implementation and neither is negotiable.
//
// Visibility is applied BEFORE ranking, never to the ranked list. Ranking first
// and dropping hidden rows afterwards leaks in two ways that no amount of
// careful rendering fixes: the count is drawn from rows the viewer may not see,
// and a hidden project's better match silently evicts one of the viewer's own
// from a capped list. A search box that can be made to disclose another
// project's case titles — even as an absence — is a read primitive over the
// whole store, which is exactly the containment rule that keeps related-case
// links inside a single project (see bot.RelatedCaseLinks).
//
// Work per query is bounded and the bound is stated on the page. sessions.json
// is already resident, so cases, sessions, tracked PRs and tracked issues cost
// one pass over it; each kind then returns at most searchKindCap rows and says
// how many matched. Commits are the one kind that needs the disk, and they are
// read as a fixed window of the newest searchCommitScan commits from one
// repository — deliberately not `git log --grep`, whose walk is bounded by the
// number of *matches* asked for and therefore unbounded for a rare term.

const (
	// searchKindCap is how many rows one kind may return. The page prints the
	// number and the pre-cap total, because a silently truncated list reads as
	// "that is all there is" and sends people back to grepping the data dir.
	searchKindCap = 20
	// searchCommitScan is the window of most-recent commits one query reads per
	// repository — the same range the commits browser paginates over, so the
	// scope of the answer is one the user can already navigate.
	searchCommitScan = 300
	// searchCommitTimeout bounds every git call one query makes — the log, and
	// the repo discovery that has to happen before it. A repository being
	// re-packed must not hold an HTTP worker open.
	searchCommitTimeout = 8 * time.Second
	// searchNoProjectNote explains an empty Commits section. Phrased as a reason
	// rather than an instruction, because the template reads "not searched — ".
	searchNoProjectNote = "no project selected (commits live in per-project checkouts, so a scan needs one)"
	// searchMaxQuery clamps the needle, in runes. Nothing useful is 200
	// characters long, and every field comparison below is O(len(query)).
	searchMaxQuery = 200
)

// Result kinds. Also the group key and the row badge.
const (
	searchKindCase    = "case"
	searchKindSession = "session"
	searchKindPR      = "pr"
	searchKindIssue   = "issue"
	searchKindCommit  = "commit"
)

// Match strength. Ranking is deliberately crude — an identity match (a case
// key, a thread id, a full sha, "#42") beats a title prefix beats a substring
// anywhere — because the alternative is a relevance model nobody can predict,
// over a corpus small enough that recency breaks most ties correctly.
const (
	scoreExact  = 100
	scorePrefix = 60
	scoreSub    = 25
	scoreWeak   = 10
)

// searchHit is one row on the results page.
type searchHit struct {
	Kind    string
	Title   string
	Detail  string // one muted line under the title
	Mono    string // the quotable id, rendered monospace (key, #N, sha, thread)
	Project string
	Badge   string
	Href    string
	// External is true when Href leaves the app — a tracked PR whose owner and
	// repo were never learned has only its github.com URL to offer.
	External bool

	score int
	when  string // recency tiebreak (RFC3339); empty sorts last
	id    string // final tiebreak, so equal rows keep one order between renders
}

// searchGroup is one kind's rows plus what the cap hid.
type searchGroup struct {
	Kind   string
	Label  string
	Hits   []searchHit
	Total  int // matches before searchKindCap
	Capped bool
}

// searchResults is the whole answer, including what was *not* searched: a
// commit scan needs a project, and an empty Commits group would otherwise read
// as "no commit says that" when the truth is "no commit was read".
type searchResults struct {
	Query    string
	Project  string
	Projects []string // project filter options, already visibility-filtered

	Groups []searchGroup
	Shown  int // rows rendered
	Total  int // rows matched, before per-kind caps

	KindCap    int
	CommitScan int
	// CommitsSearched records that a log was actually read; CommitRepo/CommitRef
	// name what was read, and CommitNote says why nothing was when it was not.
	CommitsSearched bool
	CommitRepo      string
	CommitRef       string
	CommitNote      string
}

// searchPage answers GET /search.
//
// The shell stays global and ?project= is a data filter, the same contract
// /ship and /sessions use for a cross-project lead view: navScopeFromURL has no
// rule for /search, so nothing here can put the sidebar into workspace mode.
func (s *Server) searchPage(ctx *hime.Context) error {
	q := normalizeSearchQuery(ctx.FormValue("q"))
	project := strings.TrimSpace(ctx.FormValue("project"))
	allowed := s.visibleProjectSet(ctx)

	// An exact case key is a *reference*, not a query — the same reference
	// /c/{key} resolves — so it goes straight to the case instead of rendering
	// one row that has to be clicked. Resolution reuses FindCaseByKey, so the
	// canonical-spelling rules ("webapp-14", " WEBAPP-14 ") cannot drift from
	// the ones the rest of the app applies.
	if key := sessionstore.NormalizeCaseKey(q); key != "" {
		if threadID, ent, ok := s.bot.FindCaseByKey(key); ok && searchVisible(allowed, ent.Project) {
			return ctx.Redirect(sessionSearchHref(threadID, ent.Project, searchBackHref(q, project)))
		}
		// A case that does not exist and a case this viewer may not open fall
		// through identically, to ordinary (empty) results. Answering the second
		// with a redirect — or with anything else the first does not do — would
		// turn the key space into a probe for which projects and cases exist,
		// which is precisely what caseByKey refuses to be.
	}

	d := s.basePage(ctx)
	d.Title = "Search"
	d.Search = s.runSearch(ctx, q, project, allowed)
	return s.viewPage(ctx, "search", d)
}

// normalizeSearchQuery trims and clamps the needle.
func normalizeSearchQuery(raw string) string {
	q := strings.TrimSpace(raw)
	// Runes, not bytes: slicing mid-rune would put invalid UTF-8 into every
	// comparison below and into the query echoed back on the page.
	if utf8.RuneCountInString(q) > searchMaxQuery {
		q = strings.TrimSpace(string([]rune(q)[:searchMaxQuery]))
	}
	return q
}

// searchVisible reports whether a record's project is one this viewer may open.
// A nil set is the unrestricted admin (or auth-off) case. A record with no
// project at all is admin-only, matching filterThreadsVisible — a session whose
// project cannot be determined cannot be authorized either.
func searchVisible(allowed map[string]struct{}, project string) bool {
	if allowed == nil {
		return true
	}
	project = strings.TrimSpace(project)
	if project == "" {
		return false
	}
	_, ok := allowed[project]
	return ok
}

// runSearch scans the in-memory session store once and, when the search is
// scoped to a project, one bounded window of that project's git log.
func (s *Server) runSearch(ctx *hime.Context, q, project string, allowed map[string]struct{}) searchResults {
	res := searchResults{
		Query:      q,
		Project:    project,
		Projects:   s.filterProjectNames(ctx),
		KindCap:    searchKindCap,
		CommitScan: searchCommitScan,
	}
	if q == "" {
		return res
	}
	needle := strings.ToLower(q)
	back := searchBackHref(q, project)

	var cases, sessions, prs, issues []searchHit
	for _, listed := range s.sessions.List() {
		e := listed.Entry
		// ACL before anything else: a hidden record is never scored, so it can
		// neither be counted nor push a visible row out of a capped group.
		if !searchVisible(allowed, e.Project) {
			continue
		}
		if project != "" && !strings.EqualFold(e.Project, project) {
			continue
		}
		// Legacy single-PR entries carry their PR only in the mirrored fields.
		e.NormalizePRs()

		if e.IsCase() {
			if h, ok := caseSearchHit(listed.ThreadID, e, needle, back); ok {
				cases = append(cases, h)
			}
		} else if h, ok := sessionSearchHit(listed.ThreadID, e, needle, back); ok {
			sessions = append(sessions, h)
		}
		prs = append(prs, prSearchHits(e, needle)...)
		issues = append(issues, issueSearchHits(e, needle)...)
	}

	commits := s.searchCommits(ctx, project, needle, allowed, &res)

	for _, g := range []searchGroup{
		newSearchGroup(searchKindCase, "Cases", cases),
		newSearchGroup(searchKindSession, "Sessions", sessions),
		// One PR or issue is legitimately tracked by several units — Address
		// reuses a work session bound to the PR; agent reviews also bind (for
		// the detail Sessions list) but stay out of Address reuse — every copy
		// points at the same page, so the rows are folded.
		// Folding also keeps the cap honest: twenty rows naming ten PRs is not
		// twenty answers.
		newSearchGroup(searchKindPR, "Pull requests", dedupeSearchHits(prs)),
		newSearchGroup(searchKindIssue, "Issues", dedupeSearchHits(issues)),
		newSearchGroup(searchKindCommit, "Commits", commits),
	} {
		if g.Total == 0 {
			continue
		}
		res.Groups = append(res.Groups, g)
		res.Total += g.Total
		res.Shown += len(g.Hits)
	}
	return res
}

// newSearchGroup ranks one kind and applies the cap, remembering the pre-cap
// total so the page can say what it is not showing.
func newSearchGroup(kind, label string, hits []searchHit) searchGroup {
	sortSearchHits(hits)
	g := searchGroup{Kind: kind, Label: label, Total: len(hits), Hits: hits}
	if len(hits) > searchKindCap {
		g.Hits = hits[:searchKindCap]
		g.Capped = true
	}
	return g
}

// dedupeSearchHits folds rows that name the same thing, keeping the strongest
// match and, at equal strength, the one whose unit was touched most recently.
// Input order is otherwise preserved so ranking stays the sorter's job.
// A hit with no id is never folded — an unidentified row is not a duplicate of
// anything, and dropping it would hide it.
func dedupeSearchHits(hits []searchHit) []searchHit {
	if len(hits) < 2 {
		return hits
	}
	at := make(map[string]int, len(hits))
	out := make([]searchHit, 0, len(hits))
	for _, h := range hits {
		if h.id == "" {
			out = append(out, h)
			continue
		}
		i, seen := at[h.id]
		if !seen {
			at[h.id] = len(out)
			out = append(out, h)
			continue
		}
		if h.score > out[i].score || (h.score == out[i].score && h.when > out[i].when) {
			out[i] = h
		}
	}
	return out
}

// sortSearchHits orders by match strength, then recency, then id.
func sortSearchHits(hits []searchHit) {
	slices.SortStableFunc(hits, func(a, b searchHit) int {
		if a.score != b.score {
			return b.score - a.score
		}
		switch {
		case a.when == b.when:
		case a.when == "":
			return 1
		case b.when == "":
			return -1
		case a.when > b.when:
			return -1
		default:
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
}

// fieldScore scores one field against an already-lowercased needle.
func fieldScore(field, needle string) int {
	f := strings.ToLower(strings.TrimSpace(field))
	if f == "" || needle == "" {
		return 0
	}
	switch {
	case f == needle:
		return scoreExact
	case strings.HasPrefix(f, needle):
		return scorePrefix
	case strings.Contains(f, needle):
		return scoreSub
	}
	return 0
}

// bestFieldScore is the strongest match across fields of one weight class.
func bestFieldScore(needle string, fields ...string) int {
	best := 0
	for _, f := range fields {
		if sc := fieldScore(f, needle); sc > best {
			best = sc
		}
	}
	return best
}

// weakContains is the fallback class: the needle appears somewhere in a field
// nobody searches *for* (an author email, a branch name). Enough to surface the
// row, never enough to outrank an identity match.
func weakContains(needle string, fields ...string) bool {
	for _, f := range fields {
		if f == "" {
			continue
		}
		if strings.Contains(strings.ToLower(f), needle) {
			return true
		}
	}
	return false
}

// ── Session store hits ───────────────────────────────────────────────────

// caseSearchHit matches a case on its two quotable ids and its title.
//
// CaseKey and CustomerRef are deliberately both searched and deliberately not
// the same thing: the key is ours to mint, the customer ref is theirs (ZD-4821)
// and is what support has in front of them when they come looking.
func caseSearchHit(threadID string, e sessionstore.Entry, needle, back string) (searchHit, bool) {
	title := caseSearchTitle(e)
	score := bestFieldScore(needle, e.CaseKey, e.CustomerRef, threadID)
	if sc := bestFieldScore(needle, title, e.Goal); sc > score {
		score = sc
	}
	if score == 0 && weakContains(needle, e.CustomerUpdate, e.ReporterName, e.EngineerName, e.OwnerName) {
		score = scoreWeak
	}
	if score == 0 {
		return searchHit{}, false
	}
	detail := []string{}
	if p := e.CasePhase(); p != "" {
		detail = append(detail, p)
	}
	if sev := strings.TrimSpace(e.Severity); sev != "" {
		detail = append(detail, sev)
	}
	if ref := strings.TrimSpace(e.CustomerRef); ref != "" {
		detail = append(detail, "ref "+ref)
	}
	mono := strings.TrimSpace(e.CaseKey)
	if mono == "" {
		mono = threadID
	}
	return searchHit{
		Kind:    searchKindCase,
		Title:   title,
		Detail:  strings.Join(detail, " · "),
		Mono:    mono,
		Project: e.Project,
		Badge:   "case",
		Href:    sessionSearchHref(threadID, e.Project, back),
		score:   score,
		when:    e.UpdatedAt,
		id:      threadID,
	}, true
}

// caseSearchTitle mirrors the board's title rule: the external-safe title, else
// the goal, else a placeholder — a case with no title still has to be clickable.
func caseSearchTitle(e sessionstore.Entry) string {
	if t := strings.TrimSpace(e.CustomerTitle); t != "" {
		return t
	}
	if g := strings.TrimSpace(e.Goal); g != "" {
		return g
	}
	return "(untitled case)"
}

// sessionSearchHit matches a non-case work unit on goal, label and thread id.
func sessionSearchHit(threadID string, e sessionstore.Entry, needle, back string) (searchHit, bool) {
	score := bestFieldScore(needle, threadID, e.Label)
	if sc := fieldScore(e.Goal, needle); sc > score {
		score = sc
	}
	if score == 0 && weakContains(needle, e.OwnerName, e.LastUser, e.WorktreeBranch, e.Mode) {
		score = scoreWeak
	}
	if score == 0 {
		return searchHit{}, false
	}
	title := strings.TrimSpace(e.Goal)
	if title == "" {
		title = threadID
	}
	detail := []string{}
	if lab := strings.TrimSpace(e.Label); lab != "" {
		detail = append(detail, lab)
	}
	if owner := strings.TrimSpace(e.OwnerName); owner != "" {
		detail = append(detail, owner)
	}
	return searchHit{
		Kind:    searchKindSession,
		Title:   title,
		Detail:  strings.Join(detail, " · "),
		Mono:    threadID,
		Project: e.Project,
		Badge:   "session",
		Href:    sessionSearchHref(threadID, e.Project, back),
		score:   score,
		when:    e.UpdatedAt,
		id:      threadID,
	}, true
}

// prSearchHits matches the PRs tracked on one unit. "#42", "42", the title, and
// owner/repo all resolve, because all four are how a PR gets referred to.
func prSearchHits(e sessionstore.Entry, needle string) []searchHit {
	var out []searchHit
	for _, pr := range e.PRs {
		num := ""
		if pr.Number > 0 {
			num = strconv.Itoa(pr.Number)
		}
		score := bestFieldScore(needle, num, "#"+num, pr.RepoSlug())
		if num != "" && pr.RepoSlug() != "" {
			if sc := fieldScore(pr.RepoSlug()+"#"+num, needle); sc > score {
				score = sc
			}
		}
		if sc := fieldScore(pr.Title, needle); sc > score {
			score = sc
		}
		if score == 0 && weakContains(needle, pr.URL, pr.HeadRef, pr.HeadSHA) {
			score = scoreWeak
		}
		if score == 0 {
			continue
		}
		title := strings.TrimSpace(pr.Title)
		if title == "" {
			title = "PR #" + num
		}
		detail := []string{}
		if slug := pr.RepoSlug(); slug != "" {
			detail = append(detail, slug)
		}
		if st := strings.TrimSpace(pr.State); st != "" {
			detail = append(detail, st)
		}
		hit := searchHit{
			Kind:    searchKindPR,
			Title:   title,
			Detail:  strings.Join(detail, " · "),
			Mono:    "#" + num,
			Project: e.Project,
			Badge:   "PR",
			score:   score,
			when:    e.UpdatedAt,
			id:      pr.PRKey(),
		}
		switch {
		case pr.Owner != "" && pr.Repo != "" && pr.Number > 0:
			hit.Href = prSearchHref(pr.Owner, pr.Repo, pr.Number, e.Project)
		case strings.TrimSpace(pr.URL) != "":
			hit.Href, hit.External = pr.URL, true
		default:
			// Nothing to open: a number with no repo and no URL is not a link.
			continue
		}
		out = append(out, hit)
	}
	return out
}

// issueSearchHits matches tracked GitHub issues and Linear tickets. Linear
// identifiers (ENG-123) are quotable the way case keys are, so they match on
// equality first.
func issueSearchHits(e sessionstore.Entry, needle string) []searchHit {
	var out []searchHit
	for _, iss := range e.Issues {
		num := ""
		if iss.Number > 0 {
			num = strconv.Itoa(iss.Number)
		}
		slug := ""
		if iss.Owner != "" && iss.Repo != "" {
			slug = iss.Owner + "/" + iss.Repo
		}
		score := bestFieldScore(needle, iss.Identifier, num, "#"+num, slug)
		if num != "" && slug != "" {
			if sc := fieldScore(slug+"#"+num, needle); sc > score {
				score = sc
			}
		}
		if sc := fieldScore(iss.Title, needle); sc > score {
			score = sc
		}
		if score == 0 && weakContains(needle, iss.URL, iss.TeamKey, iss.State) {
			score = scoreWeak
		}
		if score == 0 {
			continue
		}
		title := strings.TrimSpace(iss.Title)
		detail := []string{}
		if slug != "" {
			detail = append(detail, slug)
		}
		if st := strings.TrimSpace(iss.State); st != "" {
			detail = append(detail, st)
		}
		hit := searchHit{
			Kind:    searchKindIssue,
			Project: e.Project,
			Detail:  strings.Join(detail, " · "),
			score:   score,
			when:    e.UpdatedAt,
			id:      iss.IssueKey(),
		}
		if iss.IsLinear() {
			hit.Badge = "Linear"
			hit.Mono = strings.TrimSpace(iss.Identifier)
			if title == "" {
				title = hit.Mono
			}
			if e.Project != "" && hit.Mono != "" {
				hit.Href = "/projects/" + url.PathEscape(e.Project) + "/linear/" + url.PathEscape(hit.Mono)
			} else if u := strings.TrimSpace(iss.URL); u != "" {
				hit.Href, hit.External = u, true
			}
		} else {
			hit.Badge = "issue"
			hit.Mono = "#" + num
			if title == "" {
				title = "Issue #" + num
			}
			switch {
			case e.Project != "" && iss.Number > 0:
				hit.Href = issueSearchHref(e.Project, iss.Number, iss.Owner, iss.Repo)
			case strings.TrimSpace(iss.URL) != "":
				hit.Href, hit.External = iss.URL, true
			}
		}
		if hit.Href == "" {
			continue
		}
		hit.Title = title
		out = append(out, hit)
	}
	return out
}

// ── Commits ──────────────────────────────────────────────────────────────

// searchCommits reads a bounded window of one project's log and matches in Go.
//
// It runs only for a project-scoped search, and that is a design choice rather
// than an omission: every project is a separate checkout, so a global commit
// search costs one git invocation per project and grows with the deployment.
// The page says so instead of quietly returning nothing.
//
// `git log --grep` is deliberately not used. Its -n limit counts *matches*, so
// git keeps walking until it has found that many — a term that appears nowhere
// walks the entire history, which is the one shape of query a search box gets
// asked constantly. A fixed window costs the same every time.
func (s *Server) searchCommits(ctx *hime.Context, project, needle string, allowed map[string]struct{}, res *searchResults) []searchHit {
	// Both arms answer identically on purpose: a project the viewer cannot open
	// must be indistinguishable from no project at all, or the filter reports
	// which project names exist.
	if project == "" || !searchVisible(allowed, project) {
		res.CommitNote = searchNoProjectNote
		return nil
	}
	root, err := s.projectPath(project)
	if err != nil {
		res.CommitNote = err.Error()
		return nil
	}
	// The deadline covers *every* git call, not only the log: repo discovery and
	// ResolveLocalRepo also shell out (a remote read per child directory on a
	// multi-repo layout), and a bound that skips them is not a bound.
	cctx, cancel := context.WithTimeout(ctx.Context(), searchCommitTimeout)
	defer cancel()
	// The catalog is the authorization boundary for ?owner=/?repo= (see
	// resolveCatalogRepo): a repo the viewer may read is one this project
	// declares or discovers. So a discovery *failure* is reported rather than
	// swallowed — dropping the error would silently empty the catalog, and an
	// empty catalog is exactly the state in which the picker stops checking
	// anything.
	catalog, catErr := s.cfg.ProjectRepoCatalogWith(cctx, project, nil)
	if catErr != nil {
		res.CommitNote = catErr.Error()
		return nil
	}
	// With no catalog there is nothing to authorize a submitted repo name
	// against, so the scan is the project root or nothing. ResolveLocalRepo
	// refuses a traversal outright, but "inside some project" is not the rule
	// here — the rule is "inside the project this viewer named".
	var active config.GitHubRepoRef
	if len(catalog) > 0 {
		picked, pickErr := config.ResolveRepoPicker(catalog,
			strings.TrimSpace(ctx.FormValue("owner")), strings.TrimSpace(ctx.FormValue("repo")))
		if pickErr != nil {
			res.CommitNote = pickErr.Error()
			return nil
		}
		active = picked
	}
	repoPath, pathErr := gitworktree.ResolveLocalRepo(cctx, root, active.Owner, active.Repo)
	if pathErr != nil {
		res.CommitNote = pathErr.Error()
		return nil
	}
	// Primary tip, not local HEAD — the same base the commits browser lists and
	// a worktree is cut from, so a stale checkout does not silently narrow the
	// window to someone else's branch.
	ref := gitworktree.PrimaryStartRef(cctx, repoPath)
	list, logErr := ghpr.ListCommitsWith(cctx, s.ghRun(), repoPath, ghpr.CommitListOpts{
		Ref:   ref,
		Limit: searchCommitScan,
	})
	if logErr != nil {
		res.CommitNote = logErr.Error()
		return nil
	}
	res.CommitsSearched = true
	res.CommitRef = ref
	if active.Owner != "" && active.Repo != "" {
		res.CommitRepo = active.Owner + "/" + active.Repo
	}

	out := make([]searchHit, 0, len(list))
	for _, c := range list {
		score := commitScore(c, needle)
		if score == 0 {
			continue
		}
		when := ""
		if !c.AuthorDate.IsZero() {
			when = c.AuthorDate.UTC().Format(time.RFC3339)
		}
		detail := []string{}
		if a := strings.TrimSpace(c.AuthorName); a != "" {
			detail = append(detail, a)
		}
		if when != "" {
			detail = append(detail, c.AuthorDate.UTC().Format("2006-01-02"))
		}
		out = append(out, searchHit{
			Kind:    searchKindCommit,
			Title:   c.Subject,
			Detail:  strings.Join(detail, " · "),
			Mono:    c.ShortSHA,
			Project: project,
			Badge:   "commit",
			Href:    commitSearchHref(project, c.SHA, active.Owner, active.Repo),
			score:   score,
			when:    when,
			id:      c.SHA,
		})
	}
	return out
}

// commitScore matches a sha (full or a prefix long enough to mean it), the
// subject, or the author.
func commitScore(c ghpr.CommitSummary, needle string) int {
	sha := strings.ToLower(strings.TrimSpace(c.SHA))
	if sha != "" && len(needle) >= 4 && strings.HasPrefix(sha, needle) {
		return scoreExact
	}
	if sc := fieldScore(c.Subject, needle); sc > 0 {
		return sc
	}
	if weakContains(needle, c.AuthorName, c.AuthorEmail) {
		return scoreWeak
	}
	return 0
}

// ── Hrefs ────────────────────────────────────────────────────────────────

// searchBackHref is the provenance this page stamps on the session links it
// renders, so a case opened from a search returns to the search — see
// backlink.go, whose allowlist has to carry "search" for the crumb to resolve.
func searchBackHref(q, project string) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if project != "" {
		v.Set("project", project)
	}
	if len(v) == 0 {
		return "/search"
	}
	return "/search?" + v.Encode()
}

func sessionSearchHref(threadID, project, back string) string {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	if back != "" {
		v.Set("back", back)
	}
	out := "/sessions/" + url.PathEscape(threadID)
	if len(v) == 0 {
		return out
	}
	return out + "?" + v.Encode()
}

func prSearchHref(owner, repo string, number int, project string) string {
	out := "/prs/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/" + strconv.Itoa(number)
	if project != "" {
		out += "?project=" + url.QueryEscape(project)
	}
	return out
}

func issueSearchHref(project string, number int, owner, repo string) string {
	out := "/projects/" + url.PathEscape(project) + "/issues/" + strconv.Itoa(number)
	v := url.Values{}
	if owner != "" {
		v.Set("owner", owner)
	}
	if repo != "" {
		v.Set("repo", repo)
	}
	if len(v) == 0 {
		return out
	}
	return out + "?" + v.Encode()
}

func commitSearchHref(project, sha, owner, repo string) string {
	out := "/projects/" + url.PathEscape(project) + "/commits/" + url.PathEscape(sha)
	v := url.Values{}
	if owner != "" {
		v.Set("owner", owner)
	}
	if repo != "" {
		v.Set("repo", repo)
	}
	if len(v) == 0 {
		return out
	}
	return out + "?" + v.Encode()
}
