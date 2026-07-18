package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grok-discord/internal/ghpr"
	"github.com/acoshift/grok-discord/internal/gitworktree"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

// prStatusPollInterval is how often open-PR sessions are refreshed via gh.
const prStatusPollInterval = 90 * time.Second

var prStatusPollerOnce sync.Once

func (b *Bot) startPRStatusPoller(s *discordgo.Session) {
	prStatusPollerOnce.Do(func() {
		go b.runPRStatusPoller(s)
	})
}

func (b *Bot) runPRStatusPoller(s *discordgo.Session) {
	// Stagger after idle sweeper so ready is not flooded.
	time.Sleep(45 * time.Second)
	if n := b.pollPRStatuses(s); n > 0 {
		log.Printf("pr-status: initial poll updated %d", n)
	}

	ticker := time.NewTicker(prStatusPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		if n := b.pollPRStatuses(s); n > 0 {
			log.Printf("pr-status: poll updated %d", n)
		}
	}
}

// pollPRStatuses refreshes cards for sessions with a known PR. Returns update count.
func (b *Bot) pollPRStatuses(s *discordgo.Session) int {
	if s == nil {
		return 0
	}
	updated := 0
	for _, listed := range b.sessions.List() {
		e := listed.Entry
		threadID := listed.ThreadID
		if e.PRNumber <= 0 && e.PRURL == "" {
			continue
		}
		if b.isThreadBusy(threadID) {
			continue
		}
		// Terminal sessions still on disk: finish eager cleanup (e.g. deferred while busy).
		if ghpr.IsTerminal(e.PRState) {
			if err := b.cleanupAfterTerminalPR(threadID, entryPRInfo(e)); err != nil {
				log.Printf("pr-status: terminal cleanup thread=%s: %v", threadID, err)
				continue
			}
			updated++
			continue
		}
		repoDir := prRepoDir(e)
		if repoDir == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		info, err := b.resolvePRInfo(ctx, repoDir, e, "")
		cancel()
		if err != nil {
			log.Printf("pr-status: poll thread=%s: %v", threadID, err)
			continue
		}
		if err := b.applyPRInfo(s, threadID, info); err != nil {
			log.Printf("pr-status: apply thread=%s: %v", threadID, err)
			continue
		}
		// applyPRInfo does not remove worktrees (may run while job still held); do it now if idle.
		if ghpr.IsTerminal(info.State) {
			if err := b.cleanupAfterTerminalPR(threadID, info); err != nil {
				log.Printf("pr-status: cleanup after poll thread=%s: %v", threadID, err)
				continue
			}
		}
		updated++
	}
	return updated
}

// refreshPRAfterTask discovers/updates the PR status card after a Grok run.
func (b *Bot) refreshPRAfterTask(s *discordgo.Session, threadID, repoDir, branch, replyText string) {
	if s == nil || threadID == "" {
		return
	}
	if repoDir == "" {
		if e, ok := b.sessions.Get(threadID); ok {
			repoDir = prRepoDir(e)
		}
	}
	if repoDir == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var prev sessionstore.Entry
	if e, ok := b.sessions.Get(threadID); ok {
		prev = e
		if branch == "" {
			branch = e.WorktreeBranch
		}
	}
	if branch != "" {
		prev.WorktreeBranch = branch
	}

	info, err := b.resolvePRInfo(ctx, repoDir, prev, replyText)
	if err != nil {
		// No PR yet is normal for investigate-only runs.
		if prev.PRNumber == 0 && prev.PRURL == "" {
			log.Printf("pr-status: no PR yet thread=%s: %v", threadID, err)
			return
		}
		log.Printf("pr-status: refresh thread=%s: %v", threadID, err)
		return
	}
	if err := b.applyPRInfo(s, threadID, info); err != nil {
		log.Printf("pr-status: apply after task thread=%s: %v", threadID, err)
	}
}

// tryCleanupTerminalPR removes worktree/session when the thread is idle and PR is terminal.
// Call after finishRun releases the job so cleanup is not skipped while a run is "active".
func (b *Bot) tryCleanupTerminalPR(threadID string) {
	e, ok := b.sessions.Get(threadID)
	if !ok || !ghpr.IsTerminal(e.PRState) {
		return
	}
	if b.isThreadBusy(threadID) {
		log.Printf("pr-status: defer terminal cleanup (busy) thread=%s state=%s", threadID, e.PRState)
		return
	}
	if err := b.cleanupAfterTerminalPR(threadID, entryPRInfo(e)); err != nil {
		log.Printf("pr-status: tryCleanup thread=%s: %v", threadID, err)
	}
}

func (b *Bot) resolvePRInfo(ctx context.Context, repoDir string, prev sessionstore.Entry, replyText string) (ghpr.Info, error) {
	// 1) PR URLs in the model reply.
	if replyText != "" {
		if urls := ghpr.ParseGitHubPRURLs(replyText); len(urls) > 0 {
			info, err := ghpr.View(ctx, repoDir, urls[0].URL)
			if err == nil {
				return info, nil
			}
			log.Printf("pr-status: view by URL %s: %v", urls[0].URL, err)
		}
	}

	// 2) Existing session PR.
	if prev.PRURL != "" {
		if info, err := ghpr.View(ctx, repoDir, prev.PRURL); err == nil {
			return info, nil
		} else {
			log.Printf("pr-status: view by stored URL: %v", err)
		}
	}
	if prev.PRNumber > 0 {
		if info, err := ghpr.View(ctx, repoDir, fmt.Sprintf("%d", prev.PRNumber)); err == nil {
			return info, nil
		} else {
			log.Printf("pr-status: view by number %d: %v", prev.PRNumber, err)
		}
	}

	// 3) Branch head (worktree branch).
	branch := strings.TrimSpace(prev.WorktreeBranch)
	if branch != "" {
		return ghpr.ViewByHead(ctx, repoDir, branch)
	}
	return ghpr.Info{}, fmt.Errorf("no PR URL, number, or branch")
}

// applyPRInfo persists PR fields and upserts the Discord card.
// It does not remove worktrees: callers must invoke tryCleanupTerminalPR / cleanupAfterTerminalPR
// only when the thread is idle. (executeTask still holds the job when refresh runs.)
func (b *Bot) applyPRInfo(s *discordgo.Session, threadID string, info ghpr.Info) error {
	if info.Number <= 0 {
		return fmt.Errorf("empty PR info")
	}

	// Ensure a session row exists so the card msg id can be stored.
	e, ok := b.sessions.Get(threadID)
	if !ok {
		e = sessionstore.Entry{}
	}
	prevState := e.PRState
	prevMsg := e.PRStatusMsgID

	e.PRURL = info.URL
	e.PRNumber = info.Number
	e.PRState = info.State
	e.PRTitle = info.Title
	e.PRChecks = info.Checks
	e.PRReview = info.ReviewDecision
	e.PRHeadSHA = info.HeadSHA
	e.PRIsDraft = info.IsDraft

	card := ghpr.FormatCard(info)
	msgID, err := b.upsertPRStatusMessage(s, threadID, prevMsg, card)
	if err != nil {
		log.Printf("pr-status: card thread=%s: %v", threadID, err)
	} else {
		e.PRStatusMsgID = msgID
	}

	if ok {
		if _, _, pErr := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
			ent.PRURL = e.PRURL
			ent.PRNumber = e.PRNumber
			ent.PRState = e.PRState
			ent.PRTitle = e.PRTitle
			ent.PRChecks = e.PRChecks
			ent.PRReview = e.PRReview
			ent.PRHeadSHA = e.PRHeadSHA
			ent.PRIsDraft = e.PRIsDraft
			ent.PRStatusMsgID = e.PRStatusMsgID
		}); pErr != nil {
			return pErr
		}
	} else if e.Project != "" || e.SessionID != "" {
		if sErr := b.sessions.Set(threadID, e); sErr != nil {
			return sErr
		}
	} else {
		// No session yet — still try to show the card once without persisting.
		return nil
	}

	// Announce once when we learn the PR finished (state transition).
	if ghpr.IsTerminal(info.State) && !ghpr.IsTerminal(prevState) {
		note := fmt.Sprintf("PR **#%d** is **%s**. Worktree will be cleaned when this thread is idle.",
			info.Number, ghpr.DisplayState(info))
		if _, sendErr := discordSend(s, threadID, note); sendErr != nil {
			log.Printf("pr-status: announce terminal thread=%s: %v", threadID, sendErr)
		}
	}
	return nil
}

func (b *Bot) upsertPRStatusMessage(s *discordgo.Session, threadID, msgID, content string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return msgID, fmt.Errorf("empty card content")
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

func (b *Bot) cleanupAfterTerminalPR(threadID string, info ghpr.Info) error {
	if b.isThreadBusy(threadID) {
		return fmt.Errorf("thread busy")
	}
	e, ok := b.sessions.Get(threadID)
	if !ok {
		return nil
	}
	mainCwd := e.MainCwd
	if mainCwd == "" {
		mainCwd = e.Cwd
	}
	branch := e.WorktreeBranch
	if branch == "" {
		branch = gitworktree.BranchName(threadID)
	}
	path := ""
	if e.WorktreeBranch != "" && e.Cwd != "" && e.Cwd != mainCwd {
		path = e.Cwd
	}
	if path == "" && mainCwd != "" && e.Project != "" {
		path = gitworktree.WorktreePath(b.cfg.DataDir, e.Project, threadID)
	}

	if mainCwd != "" && gitworktree.IsRepo(mainCwd) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		// Prefer full CleanupIfPRDone (also deletes remote branch) when possible.
		cleaned, state, err := gitworktree.CleanupIfPRDone(ctx, mainCwd, b.cfg.DataDir, e.Project, threadID)
		if err != nil {
			log.Printf("pr-status: CleanupIfPRDone thread=%s: %v — trying Remove", threadID, err)
			if rmErr := gitworktree.Remove(ctx, mainCwd, path, branch); rmErr != nil {
				log.Printf("pr-status: Remove thread=%s: %v", threadID, rmErr)
			}
		} else if cleaned {
			log.Printf("pr-status: cleaned worktree after PR %s thread=%s", state, threadID)
		} else {
			// PR known terminal to us but gh list might disagree (URL-only); still remove local tree.
			if rmErr := gitworktree.Remove(ctx, mainCwd, path, branch); rmErr != nil {
				log.Printf("pr-status: Remove (fallback) thread=%s: %v", threadID, rmErr)
			} else {
				log.Printf("pr-status: removed worktree (fallback) thread=%s pr=%d %s", threadID, info.Number, info.State)
			}
		}
		cancel()
	}

	if delErr := b.sessions.Delete(threadID); delErr != nil {
		return delErr
	}
	log.Printf("pr-status: session deleted after PR %s #%d thread=%s", info.State, info.Number, threadID)
	return nil
}

func prRepoDir(e sessionstore.Entry) string {
	// Prefer worktree (has the branch); fall back to main checkout.
	if e.Cwd != "" && gitworktree.IsRepo(e.Cwd) {
		return e.Cwd
	}
	if e.MainCwd != "" && gitworktree.IsRepo(e.MainCwd) {
		return e.MainCwd
	}
	return ""
}

func entryPRInfo(e sessionstore.Entry) ghpr.Info {
	return ghpr.Info{
		Number:         e.PRNumber,
		URL:            e.PRURL,
		Title:          e.PRTitle,
		State:          e.PRState,
		IsDraft:        e.PRIsDraft,
		ReviewDecision: e.PRReview,
		HeadSHA:        e.PRHeadSHA,
		Checks:         e.PRChecks,
	}
}

// preservePRFields copies PR card fields from prev onto next (session Set overwrites whole entry).
func preservePRFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	next.PRURL = prev.PRURL
	next.PRNumber = prev.PRNumber
	next.PRState = prev.PRState
	next.PRTitle = prev.PRTitle
	next.PRChecks = prev.PRChecks
	next.PRReview = prev.PRReview
	next.PRHeadSHA = prev.PRHeadSHA
	next.PRIsDraft = prev.PRIsDraft
	next.PRStatusMsgID = prev.PRStatusMsgID
}
