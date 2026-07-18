package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/gitworktree"
	"github.com/acoshift/grok-discord/internal/grokrun"
	"github.com/acoshift/grok-discord/internal/history"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

const (
	maxMsg           = 1900
	maxFollowupQueue = 5 // pending tasks per thread (not counting the active run)
)

type runJob struct {
	cancel  context.CancelFunc
	start   time.Time
	project string
}

type taskItem struct {
	s        *discordgo.Session
	m        *discordgo.MessageCreate
	parsed   Parsed
	proj     projectRef
	threadID string
}

type threadState struct {
	mu    sync.Mutex
	job   *runJob
	queue []taskItem
}

type Bot struct {
	cfg      *config.Config
	sessions *sessionstore.Store
	history  *history.Store
	states   sync.Map // threadID → *threadState
}

func New(cfg *config.Config, sessions *sessionstore.Store, hist *history.Store) *Bot {
	return &Bot{cfg: cfg, sessions: sessions, history: hist}
}

// ActiveRun is a thread currently running a Grok job (dashboard).
type ActiveRun struct {
	ThreadID string    `json:"threadId"`
	Project  string    `json:"project"`
	Started  time.Time `json:"started"`
	Elapsed  string    `json:"elapsed"`
	QueueLen int       `json:"queueLen"`
}

// StatusSnapshot is a point-in-time view of bot activity for the web dashboard/SSE.
type StatusSnapshot struct {
	ActiveRuns   []ActiveRun `json:"activeRuns"`
	ActiveCount  int         `json:"activeCount"`
	QueuedTotal  int         `json:"queuedTotal"`
	SessionCount int         `json:"sessionCount"`
	ProjectCount int         `json:"projectCount"`
	AllowUsers   int         `json:"allowUsers"`
	AllowRoles   int         `json:"allowRoles"`
	Time         time.Time   `json:"time"`
}

// StatusSnapshot collects active runs and session counts without Discord I/O.
func (b *Bot) StatusSnapshot() StatusSnapshot {
	now := time.Now()
	snap := StatusSnapshot{
		ActiveRuns:   make([]ActiveRun, 0),
		SessionCount: b.sessions.Count(),
		ProjectCount: len(b.cfg.ProjectNames()),
		Time:         now,
	}
	snap.AllowUsers, snap.AllowRoles = b.cfg.AllowlistSizes()

	b.states.Range(func(key, value any) bool {
		threadID, _ := key.(string)
		st, _ := value.(*threadState)
		if st == nil {
			return true
		}
		st.mu.Lock()
		job := st.job
		qlen := len(st.queue)
		st.mu.Unlock()
		if job == nil {
			snap.QueuedTotal += qlen
			return true
		}
		snap.ActiveRuns = append(snap.ActiveRuns, ActiveRun{
			ThreadID: threadID,
			Project:  job.project,
			Started:  job.start,
			Elapsed:  formatElapsed(now.Sub(job.start)),
			QueueLen: qlen,
		})
		snap.QueuedTotal += qlen
		return true
	})
	snap.ActiveCount = len(snap.ActiveRuns)
	// Stable order for UI/tests.
	slices.SortFunc(snap.ActiveRuns, func(a, b ActiveRun) int {
		if a.ThreadID < b.ThreadID {
			return -1
		}
		if a.ThreadID > b.ThreadID {
			return 1
		}
		return 0
	})
	return snap
}

func (b *Bot) stateFor(threadID string) *threadState {
	v, _ := b.states.LoadOrStore(threadID, &threadState{})
	return v.(*threadState)
}

func (b *Bot) claimOrEnqueue(threadID string, job *runJob, item taskItem) (claimed bool, queuePos int, err error) {
	st := b.stateFor(threadID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.job != nil {
		if len(st.queue) >= maxFollowupQueue {
			return false, 0, errQueueFull
		}
		st.queue = append(st.queue, item)
		return false, len(st.queue), nil
	}
	st.job = job
	return true, 0, nil
}

func (b *Bot) finishRun(threadID string) (next taskItem, ok bool) {
	st := b.stateFor(threadID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.queue) == 0 {
		st.job = nil
		return taskItem{}, false
	}
	next = st.queue[0]
	st.queue = st.queue[1:]
	return next, true
}

func (b *Bot) replaceJob(threadID string, job *runJob) {
	st := b.stateFor(threadID)
	st.mu.Lock()
	st.job = job
	st.mu.Unlock()
}

func (b *Bot) queueLen(threadID string) int {
	v, ok := b.states.Load(threadID)
	if !ok {
		return 0
	}
	st := v.(*threadState)
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.queue)
}

func (b *Bot) clearQueue(threadID string) int {
	st := b.stateFor(threadID)
	st.mu.Lock()
	defer st.mu.Unlock()
	n := len(st.queue)
	st.queue = nil
	return n
}

var errQueueFull = fmt.Errorf("follow-up queue is full (max %d)", maxFollowupQueue)

func (b *Bot) Register(s *discordgo.Session) {
	s.AddHandler(b.onReady)
	s.AddHandler(b.onMessage)
	// MESSAGE CONTENT is a privileged intent (Developer Portal → Bot).
	s.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("ready: logged in as %s (id=%s)", r.User.String(), r.User.ID)
	names := b.cfg.ProjectNames()
	users, roles := b.cfg.AllowlistSizes()
	log.Printf("ready: projects=%s channels=%d allowUsers=%d allowRoles=%d",
		strings.Join(names, ","), b.cfg.ChannelCount(), users, roles)
	snap := b.cfg.Snapshot()
	for _, ch := range snap.Channels {
		log.Printf("ready: channel %s → %s", ch.ChannelID, ch.Project)
	}
	_ = s.UpdateGameStatus(0, "@Grok <task>")
}

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil {
		return
	}
	if m.Author.Bot {
		return
	}
	if m.GuildID == "" {
		return
	}
	if s.State.User == nil {
		log.Printf("error: message %s from %s but State.User is nil", m.ID, m.Author.ID)
		return
	}
	if !mentionsUser(m, s.State.User.ID) {
		return
	}

	log.Printf("msg: id=%s user=%s(%s) channel=%s guild=%s content=%q mentions=%d",
		m.ID, m.Author.String(), m.Author.ID, m.ChannelID, m.GuildID, truncate(m.Content, 500), len(m.Mentions))

	if m.Content == "" {
		log.Printf("warn: empty content on mention — enable Message Content Intent in Developer Portal")
	}

	if !b.isAllowed(s, m) {
		log.Printf("deny: user %s(%s) not on allowlist", m.Author.String(), m.Author.ID)
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "You're not on the allowlist for this Grok bridge.", ref(m)); err != nil {
			log.Printf("error: reply allowlist deny: %v", err)
		}
		return
	}

	// Prefer full message text (content + embed URLs) so links with query
	// params / #fragments are not dropped when Discord primarily surfaces embeds.
	msgText := ""
	if m.Message != nil {
		msgText = messagePromptText(m.Message)
	} else {
		msgText = messagePromptText(&discordgo.Message{Content: m.Content, Embeds: m.Embeds})
	}
	parsed := ParseMessage(msgText, s.State.User.ID)
	if parsed.Kind == KindEmpty {
		switch {
		case len(m.Attachments) > 0:
			parsed = Parsed{Kind: KindTask, Prompt: "Please review the attached files."}
		case hasMessageReference(m):
			parsed = Parsed{Kind: KindTask, Prompt: "Please review the referenced message."}
		}
	}
	log.Printf("parse: kind=%s prompt=%q attachments=%d reply=%v",
		kindName(parsed.Kind), truncate(parsed.Prompt, 300), len(m.Attachments), hasMessageReference(m))

	switch parsed.Kind {
	case KindEmpty, KindHelp:
		if _, err := s.ChannelMessageSendReply(m.ChannelID, HelpText(), ref(m)); err != nil {
			log.Printf("error: reply help: %v", err)
		}
	case KindProjects:
		parentID := parentChannelID(s, m.ChannelID)
		msg := b.channelProjectHelp(parentID)
		if _, err := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); err != nil {
			log.Printf("error: reply projects: %v", err)
		}
	case KindReset:
		if !isThread(s, m.ChannelID) {
			if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /reset` inside a Grok thread.", ref(m)); err != nil {
				log.Printf("error: reply reset-not-thread: %v", err)
			}
			return
		}
		b.resetThread(s, m)
	case KindStatus:
		if !isThread(s, m.ChannelID) {
			if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /status` inside a Grok thread.", ref(m)); err != nil {
				log.Printf("error: reply status-not-thread: %v", err)
			}
			return
		}
		e, ok := b.sessions.Get(m.ChannelID)
		if !ok {
			if _, err := s.ChannelMessageSendReply(m.ChannelID, "No session for this thread yet.", ref(m)); err != nil {
				log.Printf("error: reply status-empty: %v", err)
			}
			return
		}
		state := "idle"
		if job, busy := b.getJob(m.ChannelID); busy {
			state = "running · " + formatElapsed(time.Since(job.start))
			if n := b.queueLen(m.ChannelID); n > 0 {
				state += fmt.Sprintf(" · %d queued", n)
			}
		}
		lines := []string{
			"**project:** " + e.Project,
			"**session:** `" + e.SessionID + "`",
			"**updated:** " + e.UpdatedAt,
			"**state:** " + state,
		}
		if e.WorktreeBranch != "" {
			lines = append(lines, "**worktree:** `"+e.WorktreeBranch+"`")
		} else {
			lines = append(lines, "**worktree:** (none — main project cwd)")
		}
		if _, err := s.ChannelMessageSendReply(m.ChannelID, strings.Join(lines, "\n"), ref(m)); err != nil {
			log.Printf("error: reply status: %v", err)
		}
	case KindCancel:
		b.handleCancel(s, m)
	case KindTask:
		log.Printf("task: starting async for msg=%s", m.ID)
		go b.handleTask(s, m, parsed)
	}
}

func (b *Bot) getJob(threadID string) (*runJob, bool) {
	v, ok := b.states.Load(threadID)
	if !ok {
		return nil, false
	}
	st := v.(*threadState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.job == nil {
		return nil, false
	}
	return st.job, true
}

func (b *Bot) handleCancel(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /cancel` inside a Grok thread that is running.", ref(m)); err != nil {
			log.Printf("error: reply cancel-not-thread: %v", err)
		}
		return
	}
	job, ok := b.getJob(m.ChannelID)
	if !ok {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "No run in progress for this thread.", ref(m)); err != nil {
			log.Printf("error: reply cancel-idle: %v", err)
		}
		return
	}
	n := b.queueLen(m.ChannelID)
	log.Printf("cancel: thread=%s project=%s elapsed=%s queued=%d user=%s",
		m.ChannelID, job.project, formatElapsed(time.Since(job.start)), n, m.Author.String())
	job.cancel()
	msg := "Cancelling current run…"
	if n > 0 {
		msg = fmt.Sprintf("Cancelling current run… (%d follow-up%s still queued)", n, plural(n))
	}
	if _, err := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); err != nil {
		log.Printf("error: reply cancel: %v", err)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (b *Bot) resetThread(s *discordgo.Session, m *discordgo.MessageCreate) {
	if _, busy := b.getJob(m.ChannelID); busy {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "A run is in progress — `@Grok /cancel` first, then `/reset`.", ref(m)); err != nil {
			log.Printf("error: reply reset-busy: %v", err)
		}
		return
	}
	if n := b.clearQueue(m.ChannelID); n > 0 {
		log.Printf("reset: cleared %d queued follow-up(s) thread=%s", n, m.ChannelID)
	}

	if e, ok := b.sessions.Get(m.ChannelID); ok {
		mainCwd := e.MainCwd
		if mainCwd == "" {
			mainCwd = e.Cwd
		}
		branch := e.WorktreeBranch
		path := ""
		if e.WorktreeBranch != "" && e.Cwd != "" && e.Cwd != mainCwd {
			path = e.Cwd
		}
		if path == "" && mainCwd != "" {
			path = gitworktree.WorktreePath(b.cfg.DataDir, e.Project, m.ChannelID)
		}
		if branch == "" {
			branch = gitworktree.BranchName(m.ChannelID)
		}
		if mainCwd != "" && (path != "" || branch != "") {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := gitworktree.Remove(ctx, mainCwd, path, branch); err != nil {
				log.Printf("warn: worktree cleanup on reset thread=%s: %v", m.ChannelID, err)
			} else {
				log.Printf("reset: removed worktree thread=%s branch=%s", m.ChannelID, branch)
			}
			cancel()
		}
	}

	if err := b.sessions.Delete(m.ChannelID); err != nil {
		log.Printf("error: session delete: %v", err)
	}
	if _, err := s.ChannelMessageSendReply(m.ChannelID, "Session cleared for this thread (worktree removed if any).", ref(m)); err != nil {
		log.Printf("error: reply reset: %v", err)
	}
}

func (b *Bot) resolveRunCwd(ctx context.Context, proj projectRef, threadID string) (cwd, branch string, err error) {
	cwd = proj.Cwd
	if !b.cfg.WorktreeIsolationEnabled() {
		return cwd, "", nil
	}
	if !gitworktree.IsRepo(proj.Cwd) {
		log.Printf("task: project %s is not a git repo — using main cwd", proj.Name)
		return cwd, "", nil
	}

	if cleaned, state, cErr := gitworktree.CleanupIfPRDone(ctx, proj.Cwd, b.cfg.DataDir, proj.Name, threadID); cErr != nil {
		log.Printf("warn: worktree PR cleanup check thread=%s: %v", threadID, cErr)
	} else if cleaned {
		log.Printf("task: cleaned worktree after PR %s thread=%s", state, threadID)
		if delErr := b.sessions.Delete(threadID); delErr != nil {
			log.Printf("warn: session delete after PR cleanup thread=%s: %v", threadID, delErr)
		}
	}

	if e, ok := b.sessions.Get(threadID); ok && e.WorktreeBranch != "" && e.Cwd != "" && e.Cwd != proj.Cwd {
		if st, statErr := os.Stat(e.Cwd); statErr == nil && st.IsDir() && gitworktree.IsRepo(e.Cwd) {
			log.Printf("task: reuse session worktree branch=%s", e.WorktreeBranch)
			return e.Cwd, e.WorktreeBranch, nil
		}
	}
	tree, err := gitworktree.Ensure(ctx, proj.Cwd, b.cfg.DataDir, proj.Name, threadID)
	if err != nil {
		return "", "", err
	}
	return tree.Path, tree.Branch, nil
}

func remoteWorkPromptPrefix(branch string) string {
	lines := []string{
		"You are working remotely via Discord on a shared machine — not a local interactive session.",
	}
	if branch != "" {
		lines = append(lines,
			"Isolated git worktree for this Discord thread.",
			"Branch: "+branch,
			"Stay in this worktree; do not switch to the main checkout.",
			"When you make code changes you MUST:",
			"1. Commit on this branch only (never commit to main/master).",
			"2. Push the branch to the remote (`git push -u origin HEAD`).",
			"3. Open a pull request with `gh pr create` (or push to update an existing PR for this branch).",
			"",
			"Uploading files to Discord: only files inside THIS worktree can be attached.",
			"If the user wants a build artifact, report, APK, Excel, etc. on Discord, write the file under the worktree, then end your reply with:",
			"DISCORD_UPLOAD:",
			"path/relative/to/worktree/file.apk",
			"(one path per line; relative paths preferred; max 10 files, 25 MiB each).",
			"Do not list paths outside this worktree — they will be rejected.",
		)
	} else {
		lines = append(lines,
			"When you make code changes in a git repository you MUST:",
			"1. Create or use a feature branch (never commit directly to main/master).",
			"2. Commit on that branch.",
			"3. Push the branch and open a pull request with `gh pr create` (or update an existing PR).",
			"If this is not a git repository, skip PR steps and say so.",
			"",
			"File upload to Discord is only available for threads with an isolated git worktree.",
			"Do not promise Discord attachments when there is no worktree.",
		)
	}
	lines = append(lines,
		"Do not leave work as local-only commits. Do not merge the PR.",
		"Include the PR URL in your final reply to Discord.",
		"",
	)
	return strings.Join(lines, "\n")
}

func (b *Bot) isAllowed(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if !b.cfg.HasAllowlist() {
		return false
	}
	if b.cfg.UserAllowed(m.Author.ID) {
		return true
	}
	_, roleCount := b.cfg.AllowlistSizes()
	if roleCount == 0 {
		return false
	}
	member := m.Member
	if member == nil {
		var err error
		member, err = s.GuildMember(m.GuildID, m.Author.ID)
		if err != nil {
			return false
		}
	}
	for _, roleID := range member.Roles {
		if b.cfg.RoleAllowed(roleID) {
			return true
		}
	}
	return false
}

type projectRef struct {
	Name string
	Cwd  string
}

func (b *Bot) resolveProject(channelID string) (projectRef, error) {
	mapped, ok := b.cfg.ChannelProject(channelID)
	if !ok {
		return projectRef{}, fmt.Errorf("this channel is not mapped to a project (admin: set `channels.%s` in config)", channelID)
	}
	cwd, ok := b.cfg.ProjectPath(mapped)
	if !ok || cwd == "" {
		return projectRef{}, fmt.Errorf("channel maps to project `%s`, but that project is missing from config.projects", mapped)
	}
	return projectRef{Name: mapped, Cwd: cwd}, nil
}

func (b *Bot) channelProjectHelp(channelID string) string {
	proj, err := b.resolveProject(channelID)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("This channel → **%s**", proj.Name)
}

func parentChannelID(s *discordgo.Session, channelID string) string {
	if !isThread(s, channelID) {
		return channelID
	}
	ch, err := s.Channel(channelID)
	if err == nil && ch.ParentID != "" {
		return ch.ParentID
	}
	return channelID
}

func (b *Bot) handleTask(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("error: panic in handleTask msg=%s: %v", m.ID, r)
		}
	}()

	parentID := parentChannelID(s, m.ChannelID)
	log.Printf("task: msg=%s channel=%s parent=%s prompt=%q",
		m.ID, m.ChannelID, parentID, truncate(parsed.Prompt, 300))

	proj, err := b.resolveProject(parentID)
	if err != nil {
		log.Printf("error: resolve project parent=%s: %v", parentID, err)
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply resolve-project: %v", sendErr)
		}
		return
	}
	log.Printf("task: project=%s cwd=%s", proj.Name, proj.Cwd)

	titlePrompt := parsed.Prompt
	if titlePrompt == "" || titlePrompt == "Please review the attached files." || titlePrompt == "Please review the referenced message." {
		switch {
		case len(m.Attachments) > 0:
			titlePrompt = "attachments: " + m.Attachments[0].Filename
		case m.ReferencedMessage != nil && len(m.ReferencedMessage.Attachments) > 0:
			titlePrompt = "attachments: " + m.ReferencedMessage.Attachments[0].Filename
		case m.ReferencedMessage != nil:
			if t := messagePromptText(m.ReferencedMessage); t != "" {
				titlePrompt = t
			}
		}
	}
	title := threadNameFromPrompt(titlePrompt, m.Author.Username)
	needTitle := !isThread(s, m.ChannelID) || shouldRetitleThread(s, m.ChannelID)
	if needTitle && b.cfg.SummarizeTitleEnabled() {
		log.Printf("task: summarizing title via grok…")
		sumCtx, cancel := context.WithTimeout(context.Background(), time.Duration(b.cfg.SummarizeTimeoutMs)*time.Millisecond)
		if t, ok := grokrun.SummarizeTitle(sumCtx, b.cfg.GrokBin, b.cfg.Model, titlePrompt, proj.Cwd, time.Duration(b.cfg.SummarizeTimeoutMs)*time.Millisecond); ok {
			title = threadNameFromPrompt(t, m.Author.Username)
			log.Printf("task: grok title=%q", title)
		} else {
			log.Printf("task: summarize failed, using local title=%q", title)
		}
		cancel()
	}

	threadID, err := b.ensureThread(s, m, title)
	if err != nil {
		log.Printf("error: ensure thread: %v", err)
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, "Could not open thread: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply ensure-thread: %v", sendErr)
		}
		return
	}
	log.Printf("task: thread=%s title=%q", threadID, title)

	item := taskItem{s: s, m: m, parsed: parsed, proj: proj, threadID: threadID}
	ctx, cancel := context.WithCancel(context.Background())
	job := &runJob{cancel: cancel, start: time.Now(), project: proj.Name}
	claimed, queuePos, qerr := b.claimOrEnqueue(threadID, job, item)
	if qerr != nil {
		cancel()
		log.Printf("task: queue full thread=%s", threadID)
		if _, sendErr := s.ChannelMessageSend(threadID, fmt.Sprintf(
			"Follow-up queue is full (max %d). Wait for a run to finish, or `@Grok /cancel`.", maxFollowupQueue,
		)); sendErr != nil {
			log.Printf("error: reply queue-full: %v", sendErr)
		}
		return
	}
	if !claimed {
		cancel()
		log.Printf("task: queued pos=%d thread=%s msg=%s", queuePos, threadID, m.ID)
		if _, sendErr := s.ChannelMessageSend(threadID, fmt.Sprintf(
			"Queued (#%d). Will run after the current task finishes.", queuePos,
		)); sendErr != nil {
			log.Printf("error: reply queued: %v", sendErr)
		}
		return
	}

	for {
		b.executeTask(ctx, item, job)
		cancel()

		next, ok := b.finishRun(item.threadID)
		if !ok {
			return
		}
		nextCtx, nextCancel := context.WithCancel(context.Background())
		nextJob := &runJob{cancel: nextCancel, start: time.Now(), project: next.proj.Name}
		b.replaceJob(next.threadID, nextJob)
		log.Printf("task: draining queue thread=%s nextMsg=%s remaining=%d",
			next.threadID, next.m.ID, b.queueLen(next.threadID))
		if _, sendErr := next.s.ChannelMessageSend(next.threadID, "Starting queued follow-up…"); sendErr != nil {
			log.Printf("error: reply queue-start: %v", sendErr)
		}
		item = next
		job = nextJob
		ctx = nextCtx
		cancel = nextCancel
	}
}

func (b *Bot) executeTask(ctx context.Context, item taskItem, job *runJob) {
	s, m, parsed, proj, threadID := item.s, item.m, item.parsed, item.proj, item.threadID

	status, err := s.ChannelMessageSend(threadID, workingStatus(proj.Name, 0, ""))
	if err != nil {
		log.Printf("error: status message thread=%s: %v", threadID, err)
		return
	}

	var thoughts thoughtTracker
	streamer := newStreamPoster(s, threadID)

	stopProgress := make(chan struct{})
	var progressWG sync.WaitGroup
	progressWG.Add(1)
	go func() {
		defer progressWG.Done()
		b.progressLoop(s, threadID, status.ID, proj.Name, job.start, &thoughts, stopProgress)
	}()

	runCwd, wtBranch, wtErr := b.resolveRunCwd(ctx, proj, threadID)
	if wtErr != nil {
		streamer.Stop()
		close(stopProgress)
		progressWG.Wait()
		log.Printf("error: worktree thread=%s: %v", threadID, wtErr)
		if _, editErr := s.ChannelMessageEdit(threadID, status.ID, "Failed · worktree"); editErr != nil {
			log.Printf("error: edit status: %v", editErr)
		}
		sendChunks(s, threadID, "Could not create git worktree: "+wtErr.Error())
		return
	}
	if wtBranch != "" {
		log.Printf("task: worktree branch=%s cwd=%s", wtBranch, runCwd)
	} else {
		log.Printf("task: no worktree isolation cwd=%s", runCwd)
	}

	prompt := parsed.Prompt

	var related *discordgo.Message
	if hasMessageReference(m) {
		refMsg, refErr := resolveReferencedMessage(s, m)
		if refErr != nil {
			log.Printf("warn: referenced message: %v", refErr)
		} else if refMsg != nil {
			related = refMsg
			prompt = promptWithReferenced(prompt, related)
			log.Printf("task: included referenced message id=%s attachments=%d contentLen=%d",
				related.ID, len(related.Attachments), len(related.Content))
		} else {
			log.Printf("warn: referenced message %s missing or deleted", m.MessageReference.MessageID)
		}
	}

	attachments := collectAttachments(m.Attachments, related)
	if len(attachments) > 0 {
		attDir := filepath.Join(b.cfg.DataDir, "attachments", m.ID)
		defer func() {
			if rmErr := os.RemoveAll(attDir); rmErr != nil {
				log.Printf("warn: cleanup attachments %s: %v", attDir, rmErr)
			}
		}()
		log.Printf("task: downloading %d attachment(s) → %s", len(attachments), attDir)
		files, dlErr := downloadAttachments(ctx, attachments, attDir)
		if dlErr != nil {
			streamer.Stop()
			close(stopProgress)
			progressWG.Wait()
			log.Printf("error: attachments: %v", dlErr)
			msg := "Could not download attachments: " + dlErr.Error()
			if _, editErr := s.ChannelMessageEdit(threadID, status.ID, "Failed · attachments"); editErr != nil {
				log.Printf("error: edit status: %v", editErr)
			}
			sendChunks(s, threadID, msg)
			return
		}
		prompt = promptWithAttachments(prompt, files)
		log.Printf("task: saved %d attachment(s)", len(files))
	}
	// Normalize Discord link markup and keep query/# fragments explicit for the model.
	prompt = enrichPromptWithLinks(prompt)
	if urls := extractURLs(prompt); len(urls) > 0 {
		log.Printf("task: urls=%v", urls)
	}
	prompt = remoteWorkPromptPrefix(wtBranch) + prompt

	var sessionID string
	if e, ok := b.sessions.Get(threadID); ok {
		sessionID = e.SessionID
		log.Printf("task: resume session=%s", sessionID)
	}

	log.Printf("task: running grok bin=%s yolo=%v maxTurns=%d timeout=%s cwd=%s stream=true",
		b.cfg.GrokBin, b.cfg.YoloEnabled(), b.cfg.MaxTurns, time.Duration(b.cfg.TimeoutMs)*time.Millisecond, runCwd)

	result := grokrun.Run(ctx, grokrun.Options{
		GrokBin:   b.cfg.GrokBin,
		Prompt:    prompt,
		Cwd:       runCwd,
		SessionID: sessionID,
		Yolo:      b.cfg.YoloEnabled(),
		Model:     b.cfg.Model,
		MaxTurns:  b.cfg.MaxTurns,
		Timeout:   time.Duration(b.cfg.TimeoutMs) * time.Millisecond,
		ExtraArgs: b.cfg.ExtraArgs,
		OnTextDelta: func(delta string) {
			streamer.OnDelta(delta)
		},
		OnThought: func(delta string) {
			thoughts.OnDelta(delta)
		},
		OnActivity: func(line string) {
			thoughts.OnActivity(line)
		},
	})
	streamer.Flush()

	close(stopProgress)
	progressWG.Wait()

	elapsed := time.Since(job.start)
	log.Printf("task: grok done elapsed=%s code=%d cancelled=%v session=%s textLen=%d stderrLen=%d ctx=%s text=%q",
		elapsed.Round(time.Millisecond),
		result.Code,
		result.Cancelled,
		result.SessionID,
		len(result.Text),
		len(result.Stderr),
		result.ContextSummary(),
		truncate(result.Text, 400),
	)
	if result.Stderr != "" {
		log.Printf("task: grok stderr=%q", truncate(result.Stderr, 2000))
	}
	if result.Code != 0 && !result.Cancelled {
		log.Printf("error: grok exit code=%d", result.Code)
	}

	// Keep session/worktree on failure so follow-ups can resume.
	if result.SessionID != "" || wtBranch != "" {
		sid := result.SessionID
		if sid == "" {
			if e, ok := b.sessions.Get(threadID); ok {
				sid = e.SessionID
			}
		}
		if err := b.sessions.Set(threadID, sessionstore.Entry{
			SessionID:      sid,
			Project:        proj.Name,
			Cwd:            runCwd,
			MainCwd:        proj.Cwd,
			WorktreeBranch: wtBranch,
			LastUser:       m.Author.String(),
		}); err != nil {
			log.Printf("error: session save: %v", err)
		}
	}

	header := fmt.Sprintf("Done · **%s** · %s", proj.Name, formatElapsed(elapsed))
	switch {
	case result.Cancelled:
		header = fmt.Sprintf("Cancelled · **%s** · %s", proj.Name, formatElapsed(elapsed))
	case result.Code != 0:
		header = fmt.Sprintf("Finished with exit **%d** · **%s** · %s", result.Code, proj.Name, formatElapsed(elapsed))
	}
	if wtBranch != "" {
		header += " · worktree"
	}
	if n := b.queueLen(threadID); n > 0 {
		header += fmt.Sprintf(" · %d queued", n)
	}
	if ctxSum := result.ContextSummary(); ctxSum != "" {
		header += " · ctx " + ctxSum
	}
	if _, err := s.ChannelMessageEdit(threadID, status.ID, header); err != nil {
		log.Printf("error: edit status: %v", err)
	}

	var fullyStreamed bool
	if result.Cancelled {
		streamer.Abort("cancelled")
		fullyStreamed = streamer.Text() != "" && streamer.Unposted() == ""
	} else {
		fullyStreamed = streamer.Finish()
	}
	if !fullyStreamed {
		rem := streamer.Unposted()
		if rem == "" {
			rem = result.Text
		}
		sendChunks(s, threadID, rem)
	}

	if result.Stderr != "" && os.Getenv("GROK_DISCORD_DEBUG") != "" {
		errText := result.Stderr
		if len(errText) > 1500 {
			errText = errText[:1500]
		}
		sendChunks(s, threadID, "stderr:\n```\n"+errText+"\n```")
	}

	// Attach files requested via DISCORD_UPLOAD: markers — worktree only.
	if wtBranch != "" && !result.Cancelled {
		uploadText := result.Text
		if uploadText == "" {
			uploadText = streamer.Text()
		}
		uploadWorktreeFiles(s, threadID, runCwd, uploadText)
	}

	b.recordTurn(threadID, m, proj.Name, parsed.Prompt, result, elapsed)
	log.Printf("task: finished msg=%s thread=%s", m.ID, threadID)
}

func (b *Bot) recordTurn(threadID string, m *discordgo.MessageCreate, project, userPrompt string, result grokrun.Result, elapsed time.Duration) {
	if b.history == nil {
		return
	}
	status := "done"
	switch {
	case result.Cancelled:
		status = "cancelled"
	case result.Code != 0:
		status = "error"
	}
	user, userID := "", ""
	if m != nil && m.Author != nil {
		user = m.Author.String()
		userID = m.Author.ID
	}
	msgID := ""
	if m != nil {
		msgID = m.ID
	}
	// Prefer streamer/result text; keep a hard cap so history files stay manageable.
	response := result.Text
	const maxResponse = 200_000
	if len(response) > maxResponse {
		response = response[:maxResponse] + "\n…(truncated)"
	}
	prompt := userPrompt
	if prompt == "" {
		prompt = "(empty prompt)"
	}
	if err := b.history.Append(threadID, history.Turn{
		User:      user,
		UserID:    userID,
		Prompt:    prompt,
		Response:  response,
		Status:    status,
		ExitCode:  result.Code,
		Elapsed:   formatElapsed(elapsed),
		Project:   project,
		SessionID: result.SessionID,
		MessageID: msgID,
	}); err != nil {
		log.Printf("error: history append thread=%s: %v", threadID, err)
	}
}

func (b *Bot) progressLoop(s *discordgo.Session, threadID, msgID, project string, start time.Time, thoughts *thoughtTracker, stop <-chan struct{}) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			activity := ""
			if thoughts != nil {
				activity = thoughts.Latest()
			}
			text := workingStatus(project, time.Since(start), activity)
			if _, err := s.ChannelMessageEdit(threadID, msgID, text); err != nil {
				log.Printf("warn: progress edit thread=%s: %v", threadID, err)
			}
		}
	}
}

func workingStatus(project string, elapsed time.Duration, activity string) string {
	var b strings.Builder
	if elapsed < time.Second {
		fmt.Fprintf(&b, "Working in **%s**… · `@Grok /cancel` to stop", project)
	} else {
		fmt.Fprintf(&b, "Working in **%s**… · %s elapsed · `@Grok /cancel` to stop",
			project, formatElapsed(elapsed))
	}
	activity = strings.TrimSpace(activity)
	if activity != "" {
		fmt.Fprintf(&b, "\n_%s_", activity)
	}
	return b.String()
}

func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

func (b *Bot) ensureThread(s *discordgo.Session, m *discordgo.MessageCreate, name string) (string, error) {
	name = threadNameFromPrompt(name, m.Author.Username)

	if isThread(s, m.ChannelID) {
		if shouldRetitleThread(s, m.ChannelID) {
			if _, err := s.ChannelEdit(m.ChannelID, &discordgo.ChannelEdit{Name: name}); err != nil {
				log.Printf("warn: rename thread %s: %v", m.ChannelID, err)
			} else {
				log.Printf("task: renamed thread %s → %q", m.ChannelID, name)
			}
		}
		return m.ChannelID, nil
	}

	th, err := s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
		Name:                name,
		AutoArchiveDuration: 1440,
	})
	if err != nil {
		return "", fmt.Errorf("MessageThreadStartComplex: %w", err)
	}
	log.Printf("task: created thread %s name=%q", th.ID, name)
	return th.ID, nil
}

func threadNameFromPrompt(prompt, username string) string {
	summary := strings.Join(strings.Fields(prompt), " ")
	summary = strings.TrimSpace(summary)
	for _, p := range []string{"please ", "can you ", "could you ", "hey ", "hi "} {
		if len(summary) > len(p) && strings.EqualFold(summary[:len(p)], p) {
			summary = strings.TrimSpace(summary[len(p):])
		}
	}
	if summary == "" {
		summary = "task from " + username
	}

	const max = 100
	if len(summary) <= max {
		return summary
	}
	cut := strings.LastIndex(summary[:max-1], " ")
	if cut < max/3 {
		cut = max - 1
	}
	return strings.TrimSpace(summary[:cut]) + "…"
}

func shouldRetitleThread(s *discordgo.Session, channelID string) bool {
	ch, err := s.State.Channel(channelID)
	if err != nil {
		ch, err = s.Channel(channelID)
		if err != nil {
			return false
		}
	}
	name := strings.ToLower(strings.TrimSpace(ch.Name))
	return name == "" ||
		strings.HasPrefix(name, "grok:") ||
		strings.HasPrefix(name, "task from ")
}

func kindName(k Kind) string {
	switch k {
	case KindEmpty:
		return "empty"
	case KindHelp:
		return "help"
	case KindProjects:
		return "projects"
	case KindReset:
		return "reset"
	case KindStatus:
		return "status"
	case KindCancel:
		return "cancel"
	case KindTask:
		return "task"
	default:
		return fmt.Sprintf("kind(%d)", k)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func mentionsUser(m *discordgo.MessageCreate, userID string) bool {
	for _, u := range m.Mentions {
		if u.ID == userID {
			return true
		}
	}
	return strings.Contains(m.Content, "<@"+userID+">") || strings.Contains(m.Content, "<@!"+userID+">")
}

func isThread(s *discordgo.Session, channelID string) bool {
	ch, err := s.State.Channel(channelID)
	if err != nil {
		ch, err = s.Channel(channelID)
		if err != nil {
			return false
		}
	}
	return ch.Type == discordgo.ChannelTypeGuildPublicThread ||
		ch.Type == discordgo.ChannelTypeGuildPrivateThread ||
		ch.Type == discordgo.ChannelTypeGuildNewsThread
}

func ref(m *discordgo.MessageCreate) *discordgo.MessageReference {
	return &discordgo.MessageReference{
		MessageID: m.ID,
		ChannelID: m.ChannelID,
		GuildID:   m.GuildID,
	}
}

func sendChunks(s *discordgo.Session, channelID, text string) {
	parts := splitMessage(text)
	log.Printf("reply: channel=%s parts=%d totalLen=%d", channelID, len(parts), len(text))
	for i, p := range parts {
		content := p
		if len(parts) > 1 {
			content = fmt.Sprintf("(%d/%d)\n%s", i+1, len(parts), p)
		}
		if _, err := discordSend(s, channelID, content); err != nil {
			log.Printf("error: send chunk %d/%d channel=%s: %v", i+1, len(parts), channelID, err)
			// Surface a short error so the thread is not left silent.
			if _, err2 := discordSend(s, channelID,
				fmt.Sprintf("Failed to post reply chunk %d/%d: %v", i+1, len(parts), err),
			); err2 != nil {
				log.Printf("error: send failure notice: %v", err2)
			}
		}
	}
}

// sanitizeDiscordContent strips bytes Discord rejects (NUL) while keeping
// #, ?, &, and other characters used in issue refs and query strings.
func sanitizeDiscordContent(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\x00", "")
	if strings.TrimSpace(s) == "" {
		return "(empty response)"
	}
	return s
}

func splitMessage(text string) []string {
	if text == "" {
		return []string{"(empty response)"}
	}
	if len(text) <= maxMsg {
		return []string{text}
	}
	var parts []string
	rest := text
	for len(rest) > maxMsg {
		cut := strings.LastIndex(rest[:maxMsg], "\n")
		if cut < maxMsg/2 {
			cut = maxMsg
		}
		parts = append(parts, rest[:cut])
		rest = strings.TrimLeft(rest[cut:], "\n")
	}
	if rest != "" {
		parts = append(parts, rest)
	}
	return parts
}
