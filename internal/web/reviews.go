package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/reviewstore"
)

// teamReviewRow is one history line for PR detail.
type teamReviewRow struct {
	Review    reviewstore.Review
	Fresh     bool
	StickyCR  bool
	Label     string // machine verdict
	LabelText string // human label for badge
	BadgeClass string
	HeadShort string
}

// reviewRequestRow is one My Reviews table row.
type reviewRequestRow struct {
	Request reviewstore.Request
	PRURL   string
	HeadNow string
	Stale   bool
	State   string
}

// reviewerOption is a Discord user pickable as reviewer.
type reviewerOption struct {
	ID   string
	Name string
}

func (s *Server) reviewsStore() *reviewstore.Store {
	if s == nil || s.bot == nil {
		return nil
	}
	return s.bot.Reviews()
}

func (s *Server) sessionDisplay(ctx *hime.Context) (id, name string) {
	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	if sess == nil {
		return "", ""
	}
	return sess.DiscordUserID, sess.DisplayName
}

func (s *Server) postPRReview(ctx *hime.Context) error {
	store := s.reviewsStore()
	if store == nil {
		return ctx.Status(http.StatusServiceUnavailable).Error("review store unavailable")
	}
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	verdict := reviewstore.NormalizeVerdict(ctx.PostFormValue("verdict"))
	body := strings.TrimSpace(ctx.PostFormValue("body"))
	headSHA := strings.TrimSpace(ctx.PostFormValue("headSha"))
	mirror := ctx.PostFormValue("mirror") == "1" || strings.EqualFold(ctx.PostFormValue("mirror"), "on")

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	owner, repo = ref.Owner, ref.Repo

	id, name := s.sessionDisplay(ctx)
	if id == "" {
		return s.prRedirect(ctx, owner, repo, n, project, "", fmt.Errorf("login required to submit a review"))
	}
	if verdict == "" {
		return s.prRedirect(ctx, owner, repo, n, project, "", fmt.Errorf("invalid verdict"))
	}

	threadID := ""
	if s.bot != nil {
		if threads := s.bot.FindThreadsByPR(owner, repo, n); len(threads) > 0 {
			threadID = threads[0]
		}
	}

	rev := reviewstore.Review{
		Owner:        owner,
		Repo:         repo,
		Number:       n,
		Project:      project,
		ThreadID:     threadID,
		HeadSHA:      headSHA,
		Verdict:      verdict,
		Body:         body,
		ReviewerID:   id,
		ReviewerName: name,
	}
	saved, err := store.SubmitReview(rev)
	detail := map[string]any{
		"owner": owner, "repo": repo, "number": n, "project": project,
		"verdict": string(verdict), "headSha": headSHA, "reviewId": saved.ID,
	}
	if err != nil {
		s.auditAction(ctx, audit.ActionPRReviewSubmit, err, detail)
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}

	mirrorOK := false
	if mirror && s.cfg.FeatureGitHubWrites() {
		commentBody := formatReviewMirrorComment(saved)
		url, mErr := ghpr.CommentPRWithURL(ctx.Context(), s.ghRun(), cwd, owner, repo, n, commentBody)
		if mErr != nil {
			_, _, _ = store.PatchReview(owner, repo, n, saved.ID, func(r *reviewstore.Review) {
				r.GHMirrorErr = mErr.Error()
			})
			detail["mirrorErr"] = mErr.Error()
		} else {
			mirrorOK = true
			now := time.Now().UTC()
			_, _, _ = store.PatchReview(owner, repo, n, saved.ID, func(r *reviewstore.Review) {
				r.GHCommentURL = url
				r.GHMirroredAt = now
				r.GHMirrorErr = ""
			})
			detail["ghCommentUrl"] = url
		}
	}
	detail["mirrored"] = mirrorOK
	s.auditAction(ctx, audit.ActionPRReviewSubmit, nil, detail)

	msg := "Review submitted (" + string(verdict) + ")"
	if headSHA != "" {
		selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
		if live, vErr := ghpr.ViewWith(ctx.Context(), s.ghRun(), cwd, selector); vErr == nil {
			if live.HeadSHA != "" && !strings.EqualFold(live.HeadSHA, headSHA) {
				msg += " — head moved since you loaded the page (review is for the older commit)"
			}
		}
	}
	if mirror && s.cfg.FeatureGitHubWrites() && !mirrorOK {
		msg += " · GitHub mirror failed (local review kept)"
	}
	return s.prRedirect(ctx, owner, repo, n, project, msg, nil)
}

func formatReviewMirrorComment(r reviewstore.Review) string {
	title := "💬 Comment"
	switch r.Verdict {
	case reviewstore.VerdictApproved:
		title = "✅ Approved"
	case reviewstore.VerdictChangesRequested:
		title = "🔄 Changes requested"
	}
	name := strings.TrimSpace(r.ReviewerName)
	if name == "" {
		name = r.ReviewerID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### %s · Grok Work review\n", title)
	fmt.Fprintf(&b, "**Reviewer:** %s (`discord:%s`)\n", name, r.ReviewerID)
	if r.HeadSHA != "" {
		sha := r.HeadSHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		fmt.Fprintf(&b, "**Commit:** `%s`\n", sha)
	}
	if body := strings.TrimSpace(r.Body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString("\n---\n")
	b.WriteString("_Team process review via Grok Work. Not a GitHub user review — does not satisfy branch protection._\n")
	return b.String()
}

func (s *Server) postPRReviewRequest(ctx *hime.Context) error {
	store := s.reviewsStore()
	if store == nil {
		return ctx.Status(http.StatusServiceUnavailable).Error("review store unavailable")
	}
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	reviewerID := strings.TrimSpace(ctx.PostFormValue("reviewerId"))
	note := strings.TrimSpace(ctx.PostFormValue("note"))
	headSHA := strings.TrimSpace(ctx.PostFormValue("headSha"))

	project, ref, _, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	owner, repo = ref.Owner, ref.Repo

	reqID, reqName := s.sessionDisplay(ctx)
	if reqID == "" {
		return s.prRedirect(ctx, owner, repo, n, project, "", fmt.Errorf("login required"))
	}
	if reviewerID == "" {
		return s.prRedirect(ctx, owner, repo, n, project, "", fmt.Errorf("reviewer required"))
	}
	if !s.canRequestReviewer(project, reviewerID) {
		return s.prRedirect(ctx, owner, repo, n, project, "", fmt.Errorf("reviewer is not eligible (builder-class required)"))
	}
	reviewerName := s.displayNameFor(reviewerID)

	threadID := ""
	if s.bot != nil {
		if threads := s.bot.FindThreadsByPR(owner, repo, n); len(threads) > 0 {
			threadID = threads[0]
		}
	}

	req, err := store.RequestReview(reviewstore.Request{
		Owner:         owner,
		Repo:          repo,
		Number:        n,
		Project:       project,
		ThreadID:      threadID,
		HeadSHA:       headSHA,
		RequesterID:   reqID,
		RequesterName: reqName,
		ReviewerID:    reviewerID,
		ReviewerName:  reviewerName,
		Note:          note,
	})
	detail := map[string]any{
		"owner": owner, "repo": repo, "number": n, "project": project,
		"reviewerId": reviewerID, "requestId": req.ID,
	}
	s.auditAction(ctx, audit.ActionPRReviewRequest, err, detail)
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}

	if threadID != "" && !gitworktree.IsWebUnitID(threadID) && s.bot != nil {
		msg := fmt.Sprintf("<@%s> please review **%s/%s#%d**", reviewerID, owner, repo, n)
		if note != "" {
			msg += "\n> " + note
		}
		msg += "\n" + s.cfg.DiscordPRDisplayURL(owner, repo, n, "")
		s.bot.NotifyThread(threadID, msg)
	}

	return s.prRedirect(ctx, owner, repo, n, project, "Review requested", nil)
}

func (s *Server) postPRReviewCancel(ctx *hime.Context) error {
	store := s.reviewsStore()
	if store == nil {
		return ctx.Status(http.StatusServiceUnavailable).Error("review store unavailable")
	}
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	requestID := strings.TrimSpace(ctx.PostFormValue("requestId"))
	project, ref, _, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	owner, repo = ref.Owner, ref.Repo

	actorID, role := s.sessionIdentity(ctx)
	cancelActor := actorID
	if config.RoleAtLeast(role, config.WebRoleAdmin) {
		cancelActor = "" // store treats empty as admin override
	}
	_, ok, err := store.CancelRequest(owner, repo, n, requestID, cancelActor)
	detail := map[string]any{"owner": owner, "repo": repo, "number": n, "requestId": requestID}
	if err != nil {
		s.auditAction(ctx, audit.ActionPRReviewCancel, err, detail)
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	if !ok {
		return s.prRedirect(ctx, owner, repo, n, project, "", fmt.Errorf("request not found or not pending"))
	}
	s.auditAction(ctx, audit.ActionPRReviewCancel, nil, detail)
	return s.prRedirect(ctx, owner, repo, n, project, "Review request cancelled", nil)
}

func (s *Server) myReviews(ctx *hime.Context) error {
	return s.renderMyReviews(ctx, "")
}

func (s *Server) projectMyReviews(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	return s.renderMyReviews(ctx, project)
}

func (s *Server) renderMyReviews(ctx *hime.Context, projectScope string) error {
	d := s.basePage(ctx)
	d.Title = "My reviews"
	d.IsReviews = true
	if projectScope != "" {
		d.Project = projectScope
		d.Title = projectScope + " · reviews"
	}
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}

	statusFilter := strings.ToLower(strings.TrimSpace(ctx.FormValue("status")))
	if statusFilter == "" {
		statusFilter = reviewstore.StatusPending
	}
	projectFilter := projectScope
	if projectFilter == "" {
		projectFilter = strings.TrimSpace(ctx.FormValue("project"))
	}
	d.ReviewStatusFilter = statusFilter
	d.ReviewProjectFilter = projectFilter

	userID, _ := s.sessionIdentity(ctx)
	store := s.reviewsStore()
	var rows []reviewRequestRow
	pending := 0
	if store != nil && userID != "" {
		// Empty project filter: list across projects, then drop unauthorized ones.
		// Prefer an explicit projectFilter only when the caller may access it.
		listFilter := projectFilter
		if listFilter != "" {
			if err := s.ensureProjectAccess(ctx, listFilter); err != nil {
				if projectScope != "" {
					return forbiddenProject(ctx, err)
				}
				listFilter = "" // ignore unauthorized ?project=
			}
		}
		reqs := store.ListForReviewer(userID, listFilter, statusFilter)
		for _, req := range reqs {
			if req.Project != "" {
				if err := s.ensureProjectAccess(ctx, req.Project); err != nil {
					continue
				}
			} else {
				// No project on the request — only admins see it (same as threads).
				_, role := s.sessionIdentity(ctx)
				if !config.RoleAtLeast(role, config.WebRoleAdmin) {
					continue
				}
			}
			bucket := store.ListForPR(req.Owner, req.Repo, req.Number)
			head := bucket.LastHeadSHA
			rows = append(rows, reviewRequestRow{
				Request: req,
				PRURL:   fmt.Sprintf("https://github.com/%s/%s/pull/%d", req.Owner, req.Repo, req.Number),
				HeadNow: head,
				State:   bucket.LastState,
				Stale:   req.HeadSHA != "" && head != "" && !strings.EqualFold(req.HeadSHA, head),
			})
		}
		// Pending badge: recompute from ACL-visible pending rows (not the raw store count).
		for _, req := range store.ListForReviewer(userID, listFilter, reviewstore.StatusPending) {
			if req.Project != "" {
				if err := s.ensureProjectAccess(ctx, req.Project); err != nil {
					continue
				}
			} else {
				_, role := s.sessionIdentity(ctx)
				if !config.RoleAtLeast(role, config.WebRoleAdmin) {
					continue
				}
			}
			pending++
		}
	}
	d.ReviewRequests = rows
	d.ReviewPendingCount = pending
	return s.viewPage(ctx, "reviews", d)
}

// canRequestReviewer reports whether reviewerID may be assigned a team review.
// Requires project membership (or web admin) and builder-class caps (CanShip).
// Investigators and other non-builder templates are excluded from both the
// request-review dropdown and POST validation.
func (s *Server) canRequestReviewer(project, reviewerID string) bool {
	reviewerID = strings.TrimSpace(reviewerID)
	project = strings.TrimSpace(project)
	if reviewerID == "" || s.cfg == nil {
		return false
	}
	if project != "" {
		return s.eligibleReviewer(project, reviewerID)
	}
	for _, name := range s.cfg.ProjectNames() {
		if s.eligibleReviewer(name, reviewerID) {
			return true
		}
	}
	return false
}

// eligibleReviewer is true when the user can access the project (allowlist or
// web admin) and resolves to builder-class capabilities (StartSessions +
// GithubWrites). Role-mapped caps without a user map entry are not visible
// here (web has no Discord roles for other users) — same as start-session gates.
func (s *Server) eligibleReviewer(project, userID string) bool {
	if !s.reviewerOnProject(project, userID) {
		return false
	}
	return s.cfg.ResolveCapabilities(project, userID, nil).CanShip()
}

func (s *Server) reviewerOnProject(project, userID string) bool {
	for _, id := range s.cfg.WebAuthAdminIDs() {
		if id == userID {
			return true
		}
	}
	return s.cfg.CanAccessProject(project, userID, config.WebRoleMember)
}

func (s *Server) displayNameFor(discordID string) string {
	if s.webUsers == nil {
		return ""
	}
	names := s.webUsers.displayNames()
	return names[discordID]
}

func (s *Server) reviewerOptions(project string) []reviewerOption {
	project = strings.TrimSpace(project)
	ids := map[string]struct{}{}
	if project != "" {
		for _, p := range s.cfg.Snapshot().Projects {
			if !strings.EqualFold(p.Name, project) {
				continue
			}
			for _, id := range p.AllowedUserIDs {
				id = strings.TrimSpace(id)
				if id != "" {
					ids[id] = struct{}{}
				}
			}
		}
	} else {
		for _, name := range s.cfg.ProjectNames() {
			for _, p := range s.cfg.Snapshot().Projects {
				if p.Name != name {
					continue
				}
				for _, id := range p.AllowedUserIDs {
					id = strings.TrimSpace(id)
					if id != "" {
						ids[id] = struct{}{}
					}
				}
			}
		}
	}
	names := map[string]string{}
	if s.webUsers != nil {
		names = s.webUsers.displayNames()
	}
	out := make([]reviewerOption, 0, len(ids))
	for id := range ids {
		// Builder-class only (builder / approver / admin templates via CanShip).
		if project != "" {
			if !s.cfg.ResolveCapabilities(project, id, nil).CanShip() {
				continue
			}
		} else if !s.canRequestReviewer("", id) {
			continue
		}
		name := names[id]
		if name == "" {
			name = id
		}
		out = append(out, reviewerOption{ID: id, Name: name})
	}
	// Stable-ish sort by name.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if strings.ToLower(out[j].Name) < strings.ToLower(out[i].Name) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func buildTeamReviewRows(bucket reviewstore.PRBucket, currentHead string) []teamReviewRow {
	if currentHead == "" {
		currentHead = bucket.LastHeadSHA
	}
	// Newest first for display.
	revs := make([]reviewstore.Review, len(bucket.Reviews))
	copy(revs, bucket.Reviews)
	for i, j := 0, len(revs)-1; i < j; i, j = i+1, j-1 {
		revs[i], revs[j] = revs[j], revs[i]
	}
	effectives := reviewstore.EffectiveReviews(bucket.Reviews, currentHead)
	effByID := map[string]reviewstore.EffectiveReview{}
	for _, er := range effectives {
		effByID[er.ID] = er
	}
	out := make([]teamReviewRow, 0, len(revs))
	for _, r := range revs {
		fresh := reviewstore.IsReviewFresh(r.HeadSHA, currentHead)
		row := teamReviewRow{
			Review:    r,
			Fresh:     fresh,
			Label:     string(r.Verdict),
			LabelText: teamVerdictText(r.Verdict),
			BadgeClass: teamVerdictBadge(r.Verdict),
			HeadShort: shortSHA(r.HeadSHA),
		}
		if er, ok := effByID[r.ID]; ok && er.Verdict == reviewstore.VerdictChangesRequested {
			row.StickyCR = er.Stale
		}
		out = append(out, row)
	}
	return out
}

func teamVerdictText(v reviewstore.Verdict) string {
	switch v {
	case reviewstore.VerdictApproved:
		return "approved"
	case reviewstore.VerdictChangesRequested:
		return "changes requested"
	case reviewstore.VerdictCommented:
		return "comment"
	default:
		return string(v)
	}
}

func teamVerdictBadge(v reviewstore.Verdict) string {
	switch v {
	case reviewstore.VerdictApproved:
		return "status-done"
	case reviewstore.VerdictChangesRequested:
		return "status-warn"
	default:
		return ""
	}
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func teamRollupText(label string) string {
	switch label {
	case reviewstore.RollupChangesRequested:
		return "Changes requested"
	case reviewstore.RollupApproved:
		return "Approved"
	case reviewstore.RollupReviewRequested:
		return "Review requested"
	case reviewstore.RollupStaleApprovals:
		return "Stale approvals"
	case reviewstore.RollupNone, "":
		return "No reviews"
	default:
		return label
	}
}

func teamRollupBadge(label string) string {
	switch label {
	case reviewstore.RollupChangesRequested:
		return "status-warn"
	case reviewstore.RollupApproved:
		return "status-done"
	case reviewstore.RollupReviewRequested:
		return "live"
	case reviewstore.RollupStaleApprovals:
		return "status-warn"
	default:
		return ""
	}
}
