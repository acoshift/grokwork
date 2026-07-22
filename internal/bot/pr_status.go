package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// prStatusPollInterval is how often open-PR sessions are refreshed via gh.
const prStatusPollInterval = 90 * time.Second

var prStatusPollerOnce sync.Once

// startPRStatusPoller starts the PR poller once. Uses live b.Discord() each cycle
// so web-native cleanup works before (or without) gateway ready.
func (b *Bot) startPRStatusPoller() {
	prStatusPollerOnce.Do(func() {
		log.Printf("bg: starting pr-status poller interval=%s initial_delay=45s", prStatusPollInterval)
		go b.runPRStatusPoller()
	})
}

func (b *Bot) runPRStatusPoller() {
	log.Printf("bg: pr-status poller running (waiting 45s before first poll)")
	// Stagger after idle sweeper so ready is not flooded.
	time.Sleep(45 * time.Second)
	b.runPRStatusPollCycle("initial")

	ticker := time.NewTicker(prStatusPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		b.runPRStatusPollCycle("tick")
	}
}

func (b *Bot) runPRStatusPollCycle(reason string) {
	log.Printf("bg: pr-status poll start reason=%s", reason)
	start := time.Now()
	stats := b.pollPRStatuses(b.Discord())
	log.Printf("bg: pr-status poll done reason=%s sessions=%d with_pr=%d open=%d busy=%d updated=%d elapsed=%s",
		reason, stats.Sessions, stats.WithPR, stats.Open, stats.Busy, stats.Updated,
		time.Since(start).Round(time.Millisecond))
}

// prPollStats summarizes one pollPRStatuses pass.
type prPollStats struct {
	Sessions int
	WithPR   int
	Open     int
	Busy     int
	Updated  int
}

// pollPRStatuses refreshes session PR state (and Discord cards when s != nil).
// s may be nil: terminal cleanup and gh View still run for web-native units.
func (b *Bot) pollPRStatuses(s *discordgo.Session) prPollStats {
	var stats prPollStats
	list := b.sessions.List()
	stats.Sessions = len(list)
	for _, listed := range list {
		e := listed.Entry
		e.NormalizePRs()
		threadID := listed.ThreadID
		if !e.HasAnyPR() {
			continue
		}
		stats.WithPR++
		if b.isThreadBusy(threadID) {
			stats.Busy++
			continue
		}

		// All PRs terminal: eager worktree/session cleanup.
		if e.AllPRsTerminal() {
			if err := b.cleanupWhenAllPRsDone(threadID); err != nil {
				log.Printf("pr-status: terminal cleanup thread=%s: %v", threadID, err)
				continue
			}
			stats.Updated++
			continue
		}
		stats.Open++

		// Prefer a real git worktree; fall back to project/session path so full
		// PR URLs still work when the configured project root is a multi-repo
		// folder without its own .git (gh pr view <url> does not need a repo).
		repoDir := b.prViewCwd(e)

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		for _, pr := range e.PRs {
			if ghpr.IsTerminal(pr.State) {
				continue // keep terminal cards as-is until all done
			}
			sel := prViewSelector(pr)
			if sel == "" {
				log.Printf("pr-status: skip thread=%s: no PR selector (need URL or number)", threadID)
				continue
			}
			info, err := ghpr.View(ctx, repoDir, sel)
			if err != nil {
				log.Printf("pr-status: poll thread=%s pr=%s cwd=%q: %v", threadID, sel, repoDir, err)
				continue
			}
			if err := b.applyPRInfo(s, threadID, info); err != nil {
				log.Printf("pr-status: apply thread=%s: %v", threadID, err)
				continue
			}
			if !ghpr.IsTerminal(info.State) {
				b.maybeHandleCIFailure(s, threadID, info)
			}
			stats.Updated++
		}
		cancel()

		// Re-check after updates: all may now be terminal.
		if e2, ok := b.sessions.Get(threadID); ok {
			e2.NormalizePRs()
			if e2.AllPRsTerminal() && !b.isThreadBusy(threadID) {
				if err := b.cleanupWhenAllPRsDone(threadID); err != nil {
					log.Printf("pr-status: cleanup after poll thread=%s: %v", threadID, err)
				}
			}
		}
	}
	return stats
}

// refreshPRAfterTask discovers/updates PR state after a Grok run.
// Supports multiple PR URLs in the reply plus the worktree branch PR.
// s may be nil (web-native / Discord offline): session bind still runs; cards soft-skip.
func (b *Bot) refreshPRAfterTask(s *discordgo.Session, threadID, repoDir, branch, replyText string) {
	if threadID == "" {
		return
	}
	var prev sessionstore.Entry
	if e, ok := b.sessions.Get(threadID); ok {
		prev = e
		prev.NormalizePRs()
		if branch == "" {
			branch = e.WorktreeBranch
		}
		if repoDir == "" {
			repoDir = b.prViewCwd(e)
		}
	}
	// Branch discovery needs a real git worktree; URL refreshes do not.
	if repoDir == "" && branch == "" && !prev.HasAnyPR() && strings.TrimSpace(replyText) == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	infos := b.discoverPRInfos(ctx, repoDir, prev, branch, replyText)
	if len(infos) == 0 {
		if !prev.HasAnyPR() {
			log.Printf("pr-status: no PR yet thread=%s", threadID)
			return
		}
		// Refresh already-tracked open PRs even if this reply had no URL.
		for _, pr := range prev.PRs {
			if ghpr.IsTerminal(pr.State) {
				continue
			}
			sel := prViewSelector(pr)
			if sel == "" {
				continue
			}
			info, err := ghpr.View(ctx, repoDir, sel)
			if err != nil {
				log.Printf("pr-status: refresh tracked %s cwd=%q: %v", sel, repoDir, err)
				continue
			}
			infos = append(infos, info)
		}
	}
	for _, info := range infos {
		if err := b.applyPRInfo(s, threadID, info); err != nil {
			log.Printf("pr-status: apply after task thread=%s pr=#%d: %v", threadID, info.Number, err)
		}
	}
}

// discoverPRInfos collects PRs from reply URLs and optional worktree branch.
func (b *Bot) discoverPRInfos(ctx context.Context, repoDir string, prev sessionstore.Entry, branch, replyText string) []ghpr.Info {
	seen := map[string]struct{}{}
	var out []ghpr.Info
	add := func(info ghpr.Info) {
		if info.Number <= 0 && info.URL == "" {
			return
		}
		key := strings.ToLower(strings.TrimRight(info.URL, "/"))
		if key == "" {
			key = fmt.Sprintf("%s/%s#%d", info.Owner, info.Repo, info.Number)
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, info)
	}

	// 1) All PR URLs in the model reply (multi-PR / multi-repo).
	if replyText != "" {
		for _, u := range ghpr.ParseGitHubPRURLs(replyText) {
			info, err := ghpr.View(ctx, repoDir, u.URL)
			if err != nil {
				log.Printf("pr-status: view by URL %s: %v", u.URL, err)
				// Still track minimally from the URL so we can retry later.
				add(ghpr.Info{
					Number: u.Number,
					URL:    u.URL,
					Owner:  u.Owner,
					Repo:   u.Repo,
					State:  "OPEN",
				})
				continue
			}
			add(info)
		}
	}

	// 2) Worktree branch PR (primary project repo) — requires a git worktree.
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch = strings.TrimSpace(prev.WorktreeBranch)
	}
	if branch != "" && repoDir != "" && gitworktree.IsRepo(repoDir) {
		if info, err := ghpr.ViewByHead(ctx, repoDir, branch); err == nil {
			add(info)
		}
	}

	// 3) Already-tracked PRs not rediscovered (refresh).
	for _, pr := range prev.PRs {
		sel := prViewSelector(pr)
		if sel == "" {
			continue
		}
		key := strings.ToLower(strings.TrimRight(pr.URL, "/"))
		if key == "" {
			key = pr.PRKey()
		}
		if _, ok := seen[key]; ok {
			continue
		}
		if info, err := ghpr.View(ctx, repoDir, sel); err == nil {
			add(info)
		}
	}
	return out
}

// tryCleanupTerminalPR removes worktree/session when idle and all tracked PRs are terminal.
func (b *Bot) tryCleanupTerminalPR(threadID string) {
	e, ok := b.sessions.Get(threadID)
	if !ok {
		return
	}
	e.NormalizePRs()
	if !e.AllPRsTerminal() {
		if e.HasOpenPR() {
			return
		}
		// No PRs at all — nothing to do on this path.
		return
	}
	if b.isThreadBusy(threadID) {
		log.Printf("pr-status: defer terminal cleanup (busy) thread=%s", threadID)
		return
	}
	if err := b.cleanupWhenAllPRsDone(threadID); err != nil {
		log.Printf("pr-status: tryCleanup thread=%s: %v", threadID, err)
	}
}

// applyPRInfo upserts one PR into the session and updates its Discord card.
// On poller/task refresh it also posts a short PR event timeline when state
// transitions (approve, changes requested, CI green, merged/closed).
func (b *Bot) applyPRInfo(s *discordgo.Session, threadID string, info ghpr.Info) error {
	if info.Number <= 0 && info.URL == "" {
		return fmt.Errorf("empty PR info")
	}

	e, ok := b.sessions.Get(threadID)
	if !ok {
		e = sessionstore.Entry{}
	}
	e.NormalizePRs()

	pr := trackedFromInfo(info)
	var prevSnap ghpr.Snapshot
	if existing, found := e.FindPR(pr.Selector()); found {
		prevSnap = ghpr.Snapshot{
			State:  existing.State,
			Review: existing.Review,
			Checks: existing.Checks,
		}
		pr.StatusMsgID = existing.StatusMsgID
		pr.CINotifiedSHA = existing.CINotifiedSHA
		pr.CIAutoFixCount = existing.CIAutoFixCount
		pr.CIAutoFixSHA = existing.CIAutoFixSHA
	}

	card := ghpr.FormatCard(info)
	msgID, err := b.upsertPRStatusMessage(s, threadID, pr.StatusMsgID, card)
	if err != nil {
		log.Printf("pr-status: card thread=%s: %v", threadID, err)
	} else {
		pr.StatusMsgID = msgID
	}

	if ok {
		if _, _, pErr := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
			ent.NormalizePRs()
			ent.UpsertPR(pr)
			ent.ApplyAutoLabel(ent.SuggestAutoLabel(false))
		}); pErr != nil {
			return pErr
		}
	} else if e.Project != "" || e.SessionID != "" {
		e.UpsertPR(pr)
		e.ApplyAutoLabel(e.SuggestAutoLabel(false))
		if sErr := b.sessions.Set(threadID, e); sErr != nil {
			return sErr
		}
	} else {
		return nil
	}

	b.syncReviewStoreFromPR(info)
	b.announcePRTimeline(s, threadID, prevSnap, info)
	return nil
}

// syncReviewStoreFromPR stamps head/state on the team review bucket and
// obsoletes pending requests when the PR is terminal.
func (b *Bot) syncReviewStoreFromPR(info ghpr.Info) {
	if b == nil || b.reviews == nil {
		return
	}
	owner, repo := strings.TrimSpace(info.Owner), strings.TrimSpace(info.Repo)
	if (owner == "" || repo == "") && info.URL != "" {
		if parsed := ghpr.ParseGitHubPRURLs(info.URL); len(parsed) > 0 {
			owner, repo = parsed[0].Owner, parsed[0].Repo
		}
	}
	if owner == "" || repo == "" || info.Number <= 0 {
		return
	}
	if ghpr.IsTerminal(info.State) {
		if _, err := b.reviews.ObsoletePendingForPR(owner, repo, info.Number, info.State, info.HeadSHA); err != nil {
			log.Printf("pr-status: obsolete reviews %s/%s#%d: %v", owner, repo, info.Number, err)
		}
		return
	}
	if err := b.reviews.TouchPRHead(owner, repo, info.Number, info.HeadSHA, info.State); err != nil {
		log.Printf("pr-status: touch review head %s/%s#%d: %v", owner, repo, info.Number, err)
	}
}

// announcePRTimeline posts discrete PR lifecycle events when the poller (or
// post-task refresh) detects a transition. Quiet on first seed except terminal.
// Posts as a Discord rich embed (color-coded by event kind).
func (b *Bot) announcePRTimeline(s *discordgo.Session, threadID string, prev ghpr.Snapshot, info ghpr.Info) {
	if s == nil || threadID == "" || gitworktree.IsWebUnitID(threadID) {
		return
	}
	events := ghpr.DiffTimeline(prev, ghpr.SnapshotFromInfo(info))
	if len(events) == 0 {
		return
	}
	emb, ok := ghpr.FormatTimelineEmbed(info, events)
	if !ok {
		return
	}
	if ghpr.HasTerminalTimeline(events) {
		if e2, ok := b.sessions.Get(threadID); ok {
			e2.NormalizePRs()
			if e2.AllPRsTerminal() {
				emb.Description += "\n\nWorktree will be cleaned when this thread is idle (all PRs finished)."
			} else if n := len(e2.OpenPRs()); n > 0 {
				emb.Description += fmt.Sprintf("\n\n%d other PR(s) still open on this thread.", n)
			}
		}
	}
	if _, sendErr := discordSendEmbed(s, threadID, &discordgo.MessageEmbed{
		Title:       emb.Title,
		Description: emb.Description,
		URL:         emb.URL,
		Color:       emb.Color,
	}); sendErr != nil {
		// Embed Links may be missing; fall back to plain text with same body.
		log.Printf("pr-status: timeline embed thread=%s: %v — text fallback", threadID, sendErr)
		plain := emb.Title
		if emb.Description != "" {
			plain += "\n" + emb.Description
		}
		if emb.URL != "" && !strings.Contains(plain, emb.URL) {
			plain += "\n" + emb.URL
		}
		if _, textErr := discordSend(s, threadID, plain); textErr != nil {
			log.Printf("pr-status: timeline thread=%s: %v", threadID, textErr)
			return
		}
	}
	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, string(ev.Kind))
	}
	log.Printf("pr-status: timeline thread=%s pr=#%d events=%s", threadID, info.Number, strings.Join(kinds, ","))
}

func trackedFromInfo(info ghpr.Info) sessionstore.TrackedPR {
	pr := sessionstore.TrackedPR{
		URL:     info.URL,
		Number:  info.Number,
		State:   info.State,
		Title:   info.Title,
		Checks:  info.Checks,
		Review:  info.ReviewDecision,
		HeadSHA: info.HeadSHA,
		HeadRef: info.HeadRef,
		IsDraft: info.IsDraft,
		Owner:   info.Owner,
		Repo:    info.Repo,
	}
	pr.FillOwnerRepoFromURL()
	return pr
}

func (b *Bot) upsertPRStatusMessage(s *discordgo.Session, threadID, msgID, content string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return msgID, fmt.Errorf("empty card content")
	}
	// Web-native units are not Discord channel snowflakes — never post cards there.
	if gitworktree.IsWebUnitID(threadID) {
		return msgID, nil
	}
	if s == nil {
		return msgID, fmt.Errorf("discord session nil")
	}
	if msgID != "" {
		if err := discordEdit(s, threadID, msgID, content); err == nil {
			return msgID, nil
		} else {
			log.Printf("pr-status: edit card %s: %v — posting new", msgID, err)
		}
	}
	msg, err := discordSend(s, threadID, content)
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

// cleanupWhenAllPRsDone removes worktree + session after every tracked PR is terminal.
func (b *Bot) cleanupWhenAllPRsDone(threadID string) error {
	if b.isThreadBusy(threadID) {
		return fmt.Errorf("thread busy")
	}
	e, ok := b.sessions.Get(threadID)
	if !ok {
		return nil
	}
	e.NormalizePRs()
	if e.HasOpenPR() {
		return fmt.Errorf("open PRs remain")
	}

	mainCwd := e.MainCwd
	if mainCwd == "" {
		mainCwd = e.Cwd
	}
	branch := e.WorktreeBranch
	if branch == "" {
		branch = gitworktree.BranchNameForUnit(threadID)
	}
	path, onDisk := gitworktree.ResolveSessionWorktreePath(b.cfg.DataDir, e.Project, threadID, e.Cwd, mainCwd)
	if onDisk && e.Cwd != "" && e.Cwd != path {
		b.healSessionWorktreeCwd(threadID, path)
	}

	if mainCwd != "" && gitworktree.IsRepo(mainCwd) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		opts := gitworktree.EnsureOpts{BranchPrefix: gitworktree.PrefixForUnitID(threadID)}
		if p := gitworktree.PrefixFromBranch(branch); p != "" {
			opts.BranchPrefix = p
		}
		cleaned, state, err := gitworktree.CleanupIfPRDoneWith(ctx, mainCwd, b.cfg.DataDir, e.Project, threadID, opts)
		if err != nil {
			log.Printf("pr-status: CleanupIfPRDone thread=%s: %v — trying Remove", threadID, err)
			if rmErr := gitworktree.Remove(ctx, mainCwd, path, branch); rmErr != nil {
				log.Printf("pr-status: Remove thread=%s: %v", threadID, rmErr)
			}
		} else if cleaned {
			log.Printf("pr-status: cleaned worktree after PR %s thread=%s", state, threadID)
		} else {
			if rmErr := gitworktree.Remove(ctx, mainCwd, path, branch); rmErr != nil {
				log.Printf("pr-status: Remove (fallback) thread=%s: %v", threadID, rmErr)
			} else {
				log.Printf("pr-status: removed worktree (fallback) thread=%s prs=%d", threadID, len(e.PRs))
			}
		}
		cancel()
	}

	if delErr := b.sessions.Delete(threadID); delErr != nil {
		return delErr
	}
	log.Printf("pr-status: session deleted after all PRs terminal thread=%s count=%d", threadID, len(e.PRs))
	return nil
}

func prRepoDir(e sessionstore.Entry) string {
	if e.Cwd != "" && gitworktree.IsRepo(e.Cwd) {
		return e.Cwd
	}
	if e.MainCwd != "" && gitworktree.IsRepo(e.MainCwd) {
		return e.MainCwd
	}
	return ""
}

// prViewCwd is the working directory for `gh pr view/checks`.
// Prefer a real git worktree (branch/number selectors). When the project root
// is a multi-repo folder without .git, fall back to that path (or empty) so
// full PR URL selectors still work — `gh pr view <url>` does not need a repo.
func (b *Bot) prViewCwd(e sessionstore.Entry) string {
	// Live session cwd first.
	if d := prRepoDir(e); d != "" {
		return d
	}
	// Heal stale absolute cwd after dataDir rename (…/grok-discord/data/… → current dataDir).
	if b != nil && b.cfg != nil && e.Project != "" {
		unitID := unitIDFromSession(e)
		if unitID != "" {
			path, onDisk := gitworktree.ResolveSessionWorktreePath(b.cfg.DataDir, e.Project, unitID, e.Cwd, e.MainCwd)
			if onDisk && gitworktree.IsRepo(path) {
				return path
			}
		}
		if p, ok := b.cfg.ProjectPath(e.Project); ok {
			if dirExists(p) {
				return p
			}
		}
	}
	for _, p := range []string{e.Cwd, e.MainCwd} {
		if dirExists(p) {
			return p
		}
	}
	return ""
}

// unitIDFromSession extracts the worktree unit id from branch or cwd basename.
func unitIDFromSession(e sessionstore.Entry) string {
	if p := gitworktree.PrefixFromBranch(e.WorktreeBranch); p != "" {
		return strings.TrimPrefix(e.WorktreeBranch, p)
	}
	if e.Cwd != "" && e.MainCwd != "" && e.Cwd != e.MainCwd {
		base := filepath.Base(filepath.Clean(e.Cwd))
		if base != "" && base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return ""
}

// prViewSelector prefers a full GitHub URL so gh works outside a git worktree.
func prViewSelector(pr sessionstore.TrackedPR) string {
	pr.FillOwnerRepoFromURL()
	if u := strings.TrimSpace(pr.URL); u != "" {
		return u
	}
	if pr.Owner != "" && pr.Repo != "" && pr.Number > 0 {
		return fmt.Sprintf("https://github.com/%s/%s/pull/%d", pr.Owner, pr.Repo, pr.Number)
	}
	return pr.Selector()
}

func dirExists(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func entryPRInfos(e sessionstore.Entry) []ghpr.Info {
	e.NormalizePRs()
	out := make([]ghpr.Info, 0, len(e.PRs))
	for _, p := range e.PRs {
		out = append(out, ghpr.Info{
			Number:         p.Number,
			URL:            p.URL,
			Title:          p.Title,
			State:          p.State,
			IsDraft:        p.IsDraft,
			ReviewDecision: p.Review,
			HeadSHA:        p.HeadSHA,
			HeadRef:        p.HeadRef,
			Checks:         p.Checks,
			Owner:          p.Owner,
			Repo:           p.Repo,
		})
	}
	return out
}

// preservePRFields copies PR card fields from prev onto next (session Set overwrites whole entry).
func preservePRFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	prev.NormalizePRs()
	if len(prev.PRs) > 0 {
		next.PRs = append([]sessionstore.TrackedPR(nil), prev.PRs...)
	}
	next.PRURL = prev.PRURL
	next.PRNumber = prev.PRNumber
	next.PRState = prev.PRState
	next.PRTitle = prev.PRTitle
	next.PRChecks = prev.PRChecks
	next.PRReview = prev.PRReview
	next.PRHeadSHA = prev.PRHeadSHA
	next.PRIsDraft = prev.PRIsDraft
	next.PRStatusMsgID = prev.PRStatusMsgID
	next.CINotifiedSHA = prev.CINotifiedSHA
	next.CIAutoFixCount = prev.CIAutoFixCount
	next.CIAutoFixSHA = prev.CIAutoFixSHA
	preserveOwnershipFields(next, prev)
	preserveBriefFields(next, prev)
	preserveLabelFields(next, prev)
	preserveIssueFields(next, prev)
	preserveWorkflowFields(next, prev)
	preserveShipFields(next, prev)
	preserveModeFields(next, prev)
}

// preserveShipFields copies direct-to-primary ship metadata across session Set rebuilds.
func preserveShipFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if next.ShipMode == "" {
		next.ShipMode = prev.ShipMode
	}
	if next.ShippedSHA == "" {
		next.ShippedSHA = prev.ShippedSHA
	}
	if next.ShippedAt == "" {
		next.ShippedAt = prev.ShippedAt
	}
	if next.PrimaryBranch == "" {
		next.PrimaryBranch = prev.PrimaryBranch
	}
}

// preserveModeFields copies Wave-1 session Mode and Wave-3 case fields across Set rebuilds.
func preserveModeFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if next.Mode == "" {
		next.Mode = prev.Mode
	}
	if next.Phase == "" {
		next.Phase = prev.Phase
	}
	if next.Severity == "" {
		next.Severity = prev.Severity
	}
	if next.CustomerTitle == "" {
		next.CustomerTitle = prev.CustomerTitle
	}
	if next.CustomerRef == "" {
		next.CustomerRef = prev.CustomerRef
	}
	if next.ReporterID == "" {
		next.ReporterID = prev.ReporterID
	}
	if next.ReporterName == "" {
		next.ReporterName = prev.ReporterName
	}
	if next.IntakeSource == "" {
		next.IntakeSource = prev.IntakeSource
	}
	if next.CaseMsgID == "" {
		next.CaseMsgID = prev.CaseMsgID
	}
	if next.DossierMsgID == "" {
		next.DossierMsgID = prev.DossierMsgID
	}
	if next.CustomerUpdateMsgID == "" {
		next.CustomerUpdateMsgID = prev.CustomerUpdateMsgID
	}
	if next.Dossier == nil && prev.Dossier != nil {
		cp := *prev.Dossier
		next.Dossier = &cp
	}
	if next.CustomerUpdate == "" {
		next.CustomerUpdate = prev.CustomerUpdate
	}
	if next.Resolution == "" {
		next.Resolution = prev.Resolution
	}
	if next.ResolutionNote == "" {
		next.ResolutionNote = prev.ResolutionNote
	}
	if next.ResolvedAt == "" {
		next.ResolvedAt = prev.ResolvedAt
	}
	if next.ResolvedBy == "" {
		next.ResolvedBy = prev.ResolvedBy
	}
	if next.EscalatedAt == "" {
		next.EscalatedAt = prev.EscalatedAt
	}
	if next.EscalatedBy == "" {
		next.EscalatedBy = prev.EscalatedBy
	}
}
