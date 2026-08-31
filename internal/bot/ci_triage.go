package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/runjournal"
	"github.com/acoshift/grokwork/internal/sessionstore"
	"github.com/acoshift/grokwork/internal/timeline"
)

const ciLogSnippetRunes = 1500

// ciNotice posts a short operator-facing CI note. Web-native units have no Discord
// channel, so the send is skipped rather than attempted and logged as a failure —
// the poller reaches this path every cycle.
func (b *Bot) ciNotice(s *discordgo.Session, threadID, content string) {
	// Recorded for every unit; posted only where there is a thread.
	b.appendTimeline(threadID, timeline.KindNotice, timeline.Notice{Level: "info", Text: content})
	if !b.hasDiscordSurface(threadID) {
		return
	}
	if _, err := discordSend(s, threadID, content); err != nil {
		log.Printf("ci-triage: notice thread=%s: %v", threadID, err)
	}
}

// maybeHandleCIFailure posts a debounced CI digest and optionally queues /fix-ci for one PR.
func (b *Bot) maybeHandleCIFailure(s *discordgo.Session, threadID string, info ghpr.Info) {
	if s == nil || threadID == "" || info.Number <= 0 {
		return
	}
	if ghpr.IsTerminal(info.State) {
		return
	}
	if b.isThreadBusy(threadID) {
		return
	}

	e, ok := b.sessions.Get(threadID)
	if !ok {
		return
	}
	// Import shells have never been a grokwork run. Digest + auto-fix would
	// start an agent on a human PR the moment CI is red.
	if e.IsImportedPR() {
		return
	}
	e.NormalizePRs()
	sel := info.URL
	if sel == "" {
		sel = fmt.Sprintf("%d", info.Number)
	}
	pr, found := e.FindPR(sel)
	if !found {
		pr = trackedFromInfo(info)
	}
	headSHA := strings.TrimSpace(info.HeadSHA)
	if headSHA == "" {
		headSHA = strings.TrimSpace(pr.HeadSHA)
	}
	// Debounce before gh: the poller hits this every 90s for a still-red PR,
	// and ListChecks is a subprocess. One digest per head SHA is the policy;
	// checking SHA first is what makes that policy cheap.
	if headSHA != "" && headSHA == pr.CINotifiedSHA {
		return
	}

	repoDir := prRepoDir(e)
	if repoDir == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	checks, err := ghpr.ListChecks(ctx, repoDir, sel)
	if err != nil {
		log.Printf("ci-triage: list checks thread=%s pr=%s: %v", threadID, sel, err)
		if !strings.Contains(info.Checks, "✗") {
			return
		}
		checks = nil
	}
	failed := ghpr.FailedChecks(checks)
	if len(failed) == 0 && !strings.Contains(info.Checks, "✗") {
		return
	}

	digest := ghpr.FormatCIDigest(info.Number, headSHA, failed)
	if info.Owner != "" && info.Repo != "" {
		digest = strings.Replace(digest, fmt.Sprintf("PR #%d", info.Number),
			fmt.Sprintf("PR %s/%s#%d", info.Owner, info.Repo, info.Number), 1)
	}
	snippet := ""
	branch := pr.HeadRef
	if branch == "" {
		branch = e.WorktreeBranch
	}
	if branch == "" {
		branch = info.HeadRef
	}
	if branch != "" {
		snippet = ghpr.FailedLogSnippet(ctx, repoDir, branch, headSHA, ciLogSnippetRunes)
	}
	if snippet != "" {
		digest += "\n\n**log (tail):**\n```\n" + snippet + "\n```"
		if len([]rune(digest)) > 1900 {
			digest = truncateRunes(digest, 1900)
		}
	}

	// The digest is recorded for every unit before any surface sees it. It used to
	// exist only as a Discord message, so a web-native unit got nothing but a log
	// line — and the web PR dispatch is always web-native, i.e. exactly the
	// sessions started to fix CI.
	b.appendTimeline(threadID, timeline.KindCIDigest, timeline.Notice{Level: "warn", Text: digest})

	// Everything after this must still run even with no surface. Returning early
	// here (which a failed send used to do) left CINotifiedSHA unwritten, so the
	// debounce never engaged and auto-fix was unreachable: the poller would
	// re-derive the same failure every 90s forever.
	if !b.hasDiscordSurface(threadID) {
		log.Printf("ci-triage: web-native unit thread=%s pr=%s sha=%s fails=%d — recorded, no Discord post",
			threadID, sel, shortSHA(headSHA), len(failed))
	} else if _, err := discordSend(s, threadID, digest); err != nil {
		log.Printf("ci-triage: digest thread=%s: %v", threadID, err)
		return
	} else {
		log.Printf("ci-triage: digest posted thread=%s pr=%s sha=%s fails=%d",
			threadID, sel, shortSHA(headSHA), len(failed))
	}

	prKey := pr.PRKey()
	if prKey == "" {
		prKey = strings.ToLower(strings.TrimRight(info.URL, "/"))
	}
	if _, _, pErr := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.NormalizePRs()
		if !ent.PatchPR(prKey, func(p *sessionstore.TrackedPR) {
			if headSHA != "" {
				p.CINotifiedSHA = headSHA
			}
		}) {
			// PR not in list yet — upsert with notify sha.
			tp := trackedFromInfo(info)
			if headSHA != "" {
				tp.CINotifiedSHA = headSHA
			}
			ent.UpsertPR(tp)
		}
	}); pErr != nil {
		log.Printf("ci-triage: patch notified sha thread=%s: %v", threadID, pErr)
	}

	if !b.cfg.AutoFixCIEnabled() {
		return
	}
	maxAttempts := b.cfg.AutoFixCIMaxAttempts()
	// Re-read PR for auto-fix caps.
	if e2, ok := b.sessions.Get(threadID); ok {
		e2.NormalizePRs()
		if p, ok := e2.FindPR(sel); ok {
			pr = p
		}
	}
	if pr.CIAutoFixCount >= maxAttempts {
		log.Printf("ci-triage: auto-fix cap reached thread=%s pr=%s count=%d max=%d",
			threadID, sel, pr.CIAutoFixCount, maxAttempts)
		b.ciNotice(s, threadID, fmt.Sprintf(
			"Auto CI fix disabled for this PR (already tried %d/%d). Use `@Grok /fix-ci` manually.",
			pr.CIAutoFixCount, maxAttempts,
		))
		return
	}
	if headSHA != "" && headSHA == pr.CIAutoFixSHA {
		return
	}

	prompt := buildFixCIPrompt(info, branch, failed, snippet)
	if err := b.queueSystemTask(s, threadID, prompt, "auto-fix-ci"); err != nil {
		log.Printf("ci-triage: auto queue thread=%s: %v", threadID, err)
		b.ciNotice(s, threadID, "Could not queue auto CI fix: "+err.Error())
		return
	}
	if _, _, pErr := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.NormalizePRs()
		if !ent.PatchPR(prKey, func(p *sessionstore.TrackedPR) {
			p.CIAutoFixCount++
			if headSHA != "" {
				p.CIAutoFixSHA = headSHA
			}
		}) {
			tp := trackedFromInfo(info)
			tp.CIAutoFixCount = 1
			tp.CIAutoFixSHA = headSHA
			ent.UpsertPR(tp)
		}
	}); pErr != nil {
		log.Printf("ci-triage: patch auto-fix count thread=%s: %v", threadID, pErr)
	}
	b.ciNotice(s, threadID, fmt.Sprintf(
		"Auto-queued CI fix for PR #%d (%d/%d)…", info.Number, pr.CIAutoFixCount+1, maxAttempts,
	))
}

func (b *Bot) handleFixCI(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := discordReply(s, m.ChannelID, "Use `@Grok /fix-ci` inside a Grok thread with an open PR.", ref(m)); err != nil {
			log.Printf("error: reply fix-ci-not-thread: %v", err)
		}
		return
	}
	threadID := m.ChannelID
	e, ok := b.sessions.Get(threadID)
	if !ok {
		if _, err := discordReply(s, threadID, "No PR linked to this thread yet. Run a task that opens a PR first.", ref(m)); err != nil {
			log.Printf("error: reply fix-ci-no-pr: %v", err)
		}
		return
	}
	if e.IsDirectShip() {
		if _, err := discordReply(s, threadID,
			"This thread is in direct-to-primary mode (no PR). `/fix-ci` needs a tracked PR. Start a new task to fix from the current primary tip.",
			ref(m)); err != nil {
			log.Printf("error: reply fix-ci-direct: %v", err)
		}
		return
	}
	e.NormalizePRs()
	if !e.HasAnyPR() {
		if _, err := discordReply(s, threadID, "No PR linked to this thread yet. Run a task that opens a PR first.", ref(m)); err != nil {
			log.Printf("error: reply fix-ci-no-pr: %v", err)
		}
		return
	}

	repoDir := prRepoDir(e)
	if repoDir == "" {
		if _, err := discordReply(s, threadID, "No git worktree/repo for this thread.", ref(m)); err != nil {
			log.Printf("error: reply fix-ci-no-repo: %v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Optional argument: @Grok /fix-ci 42 or owner/repo#42 or URL
	arg := ""
	if m.Content != "" {
		// Strip bot mention and /fix-ci prefix if present (ParseMessage already handled KindFixCI without args).
		// handleFixCI is only KindFixCI with no extra text today; allow follow-up via task path later.
	}
	_ = arg

	// Refresh all open PRs, collect failures.
	type failTarget struct {
		info    ghpr.Info
		failed  []ghpr.Check
		snippet string
		branch  string
	}
	var targets []failTarget
	for _, pr := range e.PRs {
		if ghpr.IsTerminal(pr.State) {
			continue
		}
		sel := pr.Selector()
		if sel == "" {
			continue
		}
		info, err := ghpr.View(ctx, repoDir, sel)
		if err != nil {
			log.Printf("ci-triage: fix-ci view %s: %v", sel, err)
			info = ghpr.Info{
				Number:  pr.Number,
				URL:     pr.URL,
				State:   pr.State,
				HeadSHA: pr.HeadSHA,
				HeadRef: pr.HeadRef,
				Owner:   pr.Owner,
				Repo:    pr.Repo,
				Checks:  pr.Checks,
			}
		} else {
			_ = b.applyPRInfo(s, threadID, info)
		}
		checks, err := ghpr.ListChecks(ctx, repoDir, sel)
		if err != nil {
			if !strings.Contains(info.Checks, "✗") {
				continue
			}
		}
		failed := ghpr.FailedChecks(checks)
		if len(failed) == 0 && !strings.Contains(info.Checks, "✗") {
			continue
		}
		branch := info.HeadRef
		if branch == "" {
			branch = pr.HeadRef
		}
		if branch == "" {
			branch = e.WorktreeBranch
		}
		snippet := ""
		if branch != "" {
			snippet = ghpr.FailedLogSnippet(ctx, repoDir, branch, info.HeadSHA, ciLogSnippetRunes)
		}
		targets = append(targets, failTarget{info: info, failed: failed, snippet: snippet, branch: branch})
	}

	if len(targets) == 0 {
		if _, err := discordReply(s, threadID, "No failing checks right now (or checks still pending). Nothing to fix.", ref(m)); err != nil {
			log.Printf("error: reply fix-ci-clean: %v", err)
		}
		return
	}

	// Build one prompt covering all failing PRs (or a single PR).
	var bld strings.Builder
	if len(targets) == 1 {
		bld.WriteString(buildFixCIPrompt(targets[0].info, targets[0].branch, targets[0].failed, targets[0].snippet))
	} else {
		bld.WriteString(fmt.Sprintf("CI failed on %d pull requests linked to this Discord thread.\n", len(targets)))
		bld.WriteString("Fix each failure with a minimal change on the correct branch/repo, push, and update each PR (do not merge).\n\n")
		for i, t := range targets {
			fmt.Fprintf(&bld, "--- PR %d of %d ---\n", i+1, len(targets))
			bld.WriteString(buildFixCIPrompt(t.info, t.branch, t.failed, t.snippet))
			bld.WriteString("\n\n")
		}
	}

	// Audited as a session start (same action web's Address CI writes), because that
	// is what it is: a model run this actor put on the thread's branch. The row is
	// written by handleTaskOrigin, not here: handleTask carries the only capability
	// gate on a Discord run, so an append before the call would assert a run the
	// gate can still refuse — and record the refusal nowhere.
	go b.handleTaskOrigin(s, m, Parsed{Kind: KindTask, Prompt: strings.TrimSpace(bld.String())},
		auditOriginFixCI, map[string]any{"prs": len(targets)})
}

func buildFixCIPrompt(info ghpr.Info, branch string, failed []ghpr.Check, logSnippet string) string {
	var b strings.Builder
	label := fmt.Sprintf("#%d", info.Number)
	if info.Owner != "" && info.Repo != "" {
		label = fmt.Sprintf("%s/%s#%d", info.Owner, info.Repo, info.Number)
	}
	fmt.Fprintf(&b, "CI failed on pull request %s", label)
	if info.HeadSHA != "" {
		fmt.Fprintf(&b, " (head %s)", shortSHA(info.HeadSHA))
	}
	b.WriteString(".\n")
	if info.URL != "" {
		fmt.Fprintf(&b, "PR URL: %s\n", info.URL)
	}
	if branch != "" {
		fmt.Fprintf(&b, "Stay on branch %s when fixing this PR. Do not switch to main/master.\n", branch)
	}
	b.WriteString("Failed checks:\n")
	if len(failed) == 0 {
		b.WriteString("- (see gh pr checks)\n")
	} else {
		for _, c := range failed {
			name := strings.TrimSpace(c.Name)
			if name == "" {
				name = "(unnamed)"
			}
			if c.Link != "" {
				fmt.Fprintf(&b, "- %s (%s)\n", name, c.Link)
			} else {
				fmt.Fprintf(&b, "- %s\n", name)
			}
		}
	}
	b.WriteString("\nTasks:\n")
	b.WriteString("1. Inspect failures with `gh pr checks` and `gh run view --log-failed` as needed.\n")
	b.WriteString("2. Apply a minimal fix for the CI failures only.\n")
	b.WriteString("3. Run the relevant tests/build commands locally when practical.\n")
	b.WriteString("4. Commit, push to the PR branch, and update the existing PR (do not open a duplicate; do not merge).\n")
	b.WriteString("5. Summarize what failed and what you changed.\n")
	if logSnippet != "" {
		b.WriteString("\nFailed log tail (may be truncated):\n```\n")
		b.WriteString(logSnippet)
		b.WriteString("\n```\n")
	}
	return strings.TrimSpace(b.String())
}

// queueSystemTask enqueues a Grok task on an existing thread without a user message.
func (b *Bot) queueSystemTask(s *discordgo.Session, threadID, prompt, label string) error {
	if s == nil || threadID == "" || strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("missing session, thread, or prompt")
	}
	e, ok := b.sessions.Get(threadID)
	if !ok {
		return fmt.Errorf("no session for thread")
	}
	projName := e.Project
	if projName == "" {
		return fmt.Errorf("session has no project")
	}
	cwd, ok := b.cfg.ProjectPath(projName)
	if !ok || cwd == "" {
		return fmt.Errorf("project %q not in config", projName)
	}
	if e.MainCwd != "" {
		cwd = e.MainCwd
	}

	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        fmt.Sprintf("%s-%d", label, time.Now().UnixNano()),
			ChannelID: threadID,
			Author:    &discordgo.User{ID: "0", Username: label},
			Content:   prompt,
		},
	}
	parsed := Parsed{Kind: KindTask, Prompt: prompt}
	item := taskItem{
		s: s, m: m, parsed: parsed, proj: projectRef{Name: projName, Cwd: cwd}, threadID: threadID,
		actor: Actor{ID: "0", DisplayName: label}, source: SourceDiscord, origin: SourceDiscord,
		createdBy: "0", createdByName: label,
		taskID: runjournal.NewTaskID(), attempt: 1, triggerMsgID: m.ID,
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := &runJob{cancel: cancel, start: time.Now(), project: projName}
	claimed, queuePos, err := b.claimOrEnqueue(threadID, job, item)
	if err != nil {
		cancel()
		return err
	}
	if !claimed {
		cancel()
		log.Printf("ci-triage: system task queued pos=%d thread=%s label=%s", queuePos, threadID, label)
		return nil
	}
	b.drainWG.Add(1)
	go b.drainTaskQueue(ctx, cancel, item, job)
	return nil
}

// drainTaskQueue runs the active task and any follow-ups (same loop as handleTask after claim).
// Caller must b.drainWG.Add(1) before go/call; Done is deferred here.
func (b *Bot) drainTaskQueue(ctx context.Context, cancel context.CancelFunc, item taskItem, job *runJob) {
	defer b.drainWG.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("error: panic in drainTaskQueue thread=%s: %v", item.threadID, r)
		}
	}()
	for {
		if b.stopping.Load() {
			// Still finish the cancelled execute so journal checkpoint runs via finishRun.
		}
		b.executeTask(ctx, item, job)
		cancel()

		next, ok := b.finishRun(item.threadID)
		if !ok {
			b.tryCleanupTerminalPR(item.threadID)
			return
		}
		if b.stopping.Load() {
			// finishRun should not promote while stopping; belt-and-suspenders.
			return
		}
		nextCtx, nextCancel := context.WithCancel(context.Background())
		nextJob := &runJob{
			cancel:  nextCancel,
			start:   time.Now(),
			project: next.proj.Name,
			// A promoted queued item is now an active run and must count
			// against its actor's cap the same as a freshly claimed one.
			actorID: config.NormalizeActorID(runActorID(next)),
		}
		b.replaceJob(next.threadID, nextJob)
		nextTag := next.source
		if next.m != nil {
			nextTag = next.m.ID
		} else if next.actor.ID != "" {
			nextTag = next.actor.ID
		}
		log.Printf("task: draining queue thread=%s next=%s remaining=%d",
			next.threadID, nextTag, b.queueLen(next.threadID))
		// Discord-optional: web queue items have m/s nil; fall back to live gateway when present.
		s := next.s
		if s == nil {
			s = b.Discord()
		}
		// When the queued item already has a status card (Discord early-ack → Queued),
		// executeTask upgrades it to Working; skip the extra line.
		if s != nil && next.statusMsgID == "" {
			if _, sendErr := s.ChannelMessageSend(next.threadID, "Starting queued follow-up…"); sendErr != nil {
				log.Printf("error: reply queue-start: %v", sendErr)
			}
		}
		item = next
		job = nextJob
		ctx = nextCtx
		cancel = nextCancel
	}
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
