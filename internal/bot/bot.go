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
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/runjournal"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const (
	maxMsg           = 1900
	maxFollowupQueue = 5 // pending tasks per thread (not counting the active run)
)

type runJob struct {
	cancel  context.CancelFunc
	start   time.Time
	project string
	// Live phase/activity for web StatusSnapshot (updated by progressLoop).
	mu       sync.Mutex
	activity string
	phases   string
}

type taskItem struct {
	s        *discordgo.Session
	m        *discordgo.MessageCreate // nil for web-origin / Discord-optional runs
	parsed   Parsed
	proj     projectRef
	threadID string
	// Dual-surface fields (optional; Discord handler fills actor/source).
	actor           Actor
	source          string // SourceDiscord | SourceWeb
	attachmentPaths []string
	origin          string
	createdBy       string
	createdByName   string
	discordURL      string
	// Durable resume fields.
	taskID           string
	attempt          int
	referencedPrompt string
	triggerMsgID     string
	// Optional Discord status message posted before the run (early ack).
	// executeTask reuses it as Working instead of sending a second status.
	statusMsgID string
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
	runs     *runjournal.Store

	ready     atomic.Bool
	gateReady atomic.Bool
	stopping  atomic.Bool
	drainWG   sync.WaitGroup
	bootGen   uint64
	hostname  string

	discordMu   sync.RWMutex
	discord     *discordgo.Session // gateway session after Register
	threadAPI   threadAPI          // tests inject; nil → wrap discord
	reconnectMu sync.Mutex         // serializes forced gateway reconnects
}

func New(cfg *config.Config, sessions *sessionstore.Store, hist *history.Store) *Bot {
	b := &Bot{cfg: cfg, sessions: sessions, history: hist}
	if cfg != nil && cfg.DataDir != "" {
		if store, err := runjournal.New(cfg.DataDir); err != nil {
			log.Printf("warn: runjournal: %v", err)
		} else {
			b.runs = store
		}
	}
	if host, err := os.Hostname(); err == nil {
		b.hostname = host
	}
	// Discord-independent background work: do not wait for gateway ready so
	// web-native units still get idle TTL + PR terminal cleanup.
	b.startIdleWorktreeCleanup()
	b.startPRStatusPoller()
	return b
}

// ActiveRun is a thread currently running a Grok job (dashboard).
type ActiveRun struct {
	ThreadID string    `json:"threadId"`
	Project  string    `json:"project"`
	Started  time.Time `json:"started"`
	Elapsed  string    `json:"elapsed"`
	QueueLen int       `json:"queueLen"`
	Activity string    `json:"activity,omitempty"`
	Phases   string    `json:"phases,omitempty"`
}

// StatusSnapshot is a point-in-time view of bot activity for the web dashboard/SSE.
type StatusSnapshot struct {
	ActiveRuns          []ActiveRun `json:"activeRuns"`
	ActiveCount         int         `json:"activeCount"`
	QueuedTotal         int         `json:"queuedTotal"`
	SessionCount        int         `json:"sessionCount"`
	ProjectCount        int         `json:"projectCount"`
	EmptyMemberProjects int         `json:"emptyMemberProjects"`
	Time                time.Time   `json:"time"`
}

// StatusSnapshot collects active runs and session counts without Discord I/O.
func (b *Bot) StatusSnapshot() StatusSnapshot {
	now := time.Now()
	snap := StatusSnapshot{
		ActiveRuns:          make([]ActiveRun, 0),
		SessionCount:        b.sessions.Count(),
		ProjectCount:        len(b.cfg.ProjectNames()),
		EmptyMemberProjects: b.cfg.EmptyProjectsCount(),
		Time:                now,
	}

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
		job.mu.Lock()
		activity, phases := job.activity, job.phases
		job.mu.Unlock()
		snap.ActiveRuns = append(snap.ActiveRuns, ActiveRun{
			ThreadID: threadID,
			Project:  job.project,
			Started:  job.start,
			Elapsed:  formatElapsed(now.Sub(job.start)),
			QueueLen: qlen,
			Activity: activity,
			Phases:   phases,
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
	return b.claimOrEnqueueInternal(threadID, job, item, false)
}

// claimOrEnqueueInternal claims or enqueues under st.mu and persists the journal (RMW).
// skipReady is true for recovery rehydrate (gate still closed).
func (b *Bot) claimOrEnqueueInternal(threadID string, job *runJob, item taskItem, skipReady bool) (claimed bool, queuePos int, err error) {
	if b != nil && b.stopping.Load() {
		return false, 0, ErrShuttingDown
	}
	if !skipReady && b != nil && !b.Ready() {
		return false, 0, ErrNotReady
	}
	if item.taskID == "" {
		item.taskID = runjournal.NewTaskID()
	}
	if item.attempt <= 0 {
		item.attempt = 1
	}

	st := b.stateFor(threadID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.job != nil {
		if len(st.queue) >= maxFollowupQueue {
			return false, 0, errQueueFull
		}
		st.queue = append(st.queue, item)
		if err := b.saveJournalFromState(threadID, st, item, false); err != nil {
			st.queue = st.queue[:len(st.queue)-1]
			if b.runs != nil {
				b.runs.RemoveTaskFiles(threadID, item.taskID)
			}
			return false, 0, err
		}
		return false, len(st.queue), nil
	}
	st.job = job
	if err := b.saveJournalFromState(threadID, st, item, true); err != nil {
		st.job = nil
		if b.runs != nil {
			b.runs.RemoveTaskFiles(threadID, item.taskID)
		}
		return false, 0, err
	}
	return true, 0, nil
}

func (b *Bot) finishRun(threadID string) (next taskItem, ok bool) {
	st := b.stateFor(threadID)
	st.mu.Lock()
	defer st.mu.Unlock()

	var finishedID string
	if b.runs != nil && b.resumeEnabled() {
		if j, found, err := b.runs.Load(threadID); err == nil && found && j.Active != nil {
			finishedID = j.Active.ID
		}
	}

	if b.stopping.Load() {
		b.checkpointInterruptedLocked(threadID, st)
		st.job = nil
		return taskItem{}, false
	}

	if finishedID != "" && b.runs != nil {
		b.runs.RemoveTaskFiles(threadID, finishedID)
	}

	if len(st.queue) == 0 {
		st.job = nil
		if b.resumeEnabled() {
			b.deleteJournal(threadID)
		}
		return taskItem{}, false
	}
	next = st.queue[0]
	st.queue = st.queue[1:]
	if err := b.saveJournalFromState(threadID, st, next, true); err != nil {
		log.Printf("warn: journal promote thread=%s: %v", threadID, err)
	}
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
	if n > 0 {
		if err := b.saveJournalFromState(threadID, st, taskItem{}, false); err != nil {
			log.Printf("warn: journal clearQueue thread=%s: %v", threadID, err)
		}
	}
	return n
}

// ErrQueueFull is returned when a thread's follow-up queue is at capacity.
var ErrQueueFull = fmt.Errorf("follow-up queue is full (max %d)", maxFollowupQueue)

// errQueueFull is the historical name used inside this package.
var errQueueFull = ErrQueueFull

func (b *Bot) Register(s *discordgo.Session) {
	b.setDiscord(s)
	s.AddHandler(b.onReady)
	s.AddHandler(b.onMessage)
	s.AddHandler(b.onInteraction)
	// MESSAGE CONTENT is a privileged intent (Developer Portal → Bot).
	// Interactions (buttons/modals) arrive without an extra intent.
	s.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("ready: logged in as %s (id=%s)", r.User.String(), r.User.ID)
	names := b.cfg.ProjectNames()
	empty := b.cfg.EmptyProjectsCount()
	log.Printf("ready: projects=%s channels=%d emptyMemberProjects=%d",
		strings.Join(names, ","), b.cfg.ChannelCount(), empty)
	snap := b.cfg.Snapshot()
	for _, ch := range snap.Channels {
		log.Printf("ready: channel %s → %s", ch.ChannelID, ch.Project)
	}
	_ = s.UpdateGameStatus(0, "@Grok <task>")
	// Idle + PR pollers already started in New (Once); re-call is a no-op.
	b.startIdleWorktreeCleanup()
	b.startPRStatusPoller()
	b.startBoardDigest(s)
	b.startGatewayWatch()
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

	if allowed, denyMsg := b.checkMessageAccess(s, m); !allowed {
		log.Printf("deny: user %s(%s) project access: %s", m.Author.String(), m.Author.ID, denyMsg)
		if _, err := s.ChannelMessageSendReply(m.ChannelID, denyMsg, ref(m)); err != nil {
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
		sendChunksReply(s, m.ChannelID, HelpText(), ref(m))
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
		sessionLine := "**session:** (none yet)"
		if e.SessionID != "" {
			sessionLine = "**session:** `" + e.SessionID + "`"
		}
		labelLine := "**label:** " + sessionstore.DisplayLabel(e.EffectiveLabel())
		if e.LabelManual {
			labelLine += " (manual)"
		}
		lines := []string{
			"**project:** " + e.Project,
			sessionLine,
			"**updated:** " + e.UpdatedAt,
			"**state:** " + state,
			labelLine,
		}
		if e.HasOwner() {
			ownerLine := fmt.Sprintf("**owner:** <@%s>", e.OwnerID)
			if e.OwnerName != "" {
				ownerLine = fmt.Sprintf("**owner:** %s (<@%s>)", e.OwnerName, e.OwnerID)
			}
			lines = append(lines, ownerLine)
			if len(e.CoOwnerIDs) > 0 {
				parts := make([]string, 0, len(e.CoOwnerIDs))
				for _, id := range e.CoOwnerIDs {
					parts = append(parts, "<@"+id+">")
				}
				lines = append(lines, "**co-owners:** "+strings.Join(parts, ", "))
			}
		} else {
			lines = append(lines, "**owner:** (none — first `@Grok` task or `/claim`)")
		}
		if e.WorktreeBranch != "" {
			lines = append(lines, "**worktree:** `"+e.WorktreeBranch+"`")
		} else {
			lines = append(lines, "**worktree:** (none — main project cwd)")
		}
		if issLines := sessionstore.FormatIssueStatusLines(e.Issues); len(issLines) > 0 {
			lines = append(lines, issLines...)
		} else {
			lines = append(lines, "**issue:** (none linked)")
		}
		e.NormalizePRs()
		if prLines := ghpr.FormatMultiStatusLines(entryPRInfos(e)); len(prLines) > 0 {
			lines = append(lines, prLines...)
		} else {
			lines = append(lines, "**pr:** (none yet)")
		}
		statusBody := strings.Join(lines, "\n")
		if _, err := s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content:    sanitizeDiscordContent(statusBody),
			Reference:  ref(m),
			Components: actionBarDone(m.ChannelID),
			Flags:      discordgo.MessageFlagsSuppressEmbeds,
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		}); err != nil {
			log.Printf("error: reply status: %v", err)
		}
	case KindCancel:
		b.handleCancel(s, m)
	case KindFixCI:
		b.handleFixCI(s, m)
	case KindClaim:
		b.handleClaim(s, m)
	case KindHandOff:
		b.handleHandOff(s, m)
	case KindBrief:
		b.handleBrief(s, m, parsed)
	case KindLabel:
		b.handleLabel(s, m, parsed)
	case KindBoard:
		b.handleBoard(s, m, parsed)
	case KindLink:
		b.handleLink(s, m, parsed)
	case KindTask:
		log.Printf("task: starting async for msg=%s", m.ID)
		// Immediate typing indicator while we open the thread / claim the queue.
		go func() {
			if err := s.ChannelTyping(m.ChannelID); err != nil {
				log.Printf("warn: typing channel=%s: %v", m.ChannelID, err)
			}
		}()
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
	if e, ok := b.sessions.Get(m.ChannelID); ok && !b.canControlThread(s, m, e) {
		b.denyControl(s, m, e, "cancel")
		return
	}
	who := ""
	if m.Author != nil {
		who = m.Author.String()
	}
	msg, ok := b.cancelCurrentRun(m.ChannelID, who)
	if !ok {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); err != nil {
			log.Printf("error: reply cancel-idle: %v", err)
		}
		return
	}
	if _, err := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); err != nil {
		log.Printf("error: reply cancel: %v", err)
	}
}

// cancelCurrentRun cancels the active job if any. ok is false when idle.
func (b *Bot) cancelCurrentRun(threadID, who string) (msg string, ok bool) {
	job, running := b.getJob(threadID)
	if !running {
		return "No run in progress for this thread.", false
	}
	n := b.queueLen(threadID)
	log.Printf("cancel: thread=%s project=%s elapsed=%s queued=%d user=%s",
		threadID, job.project, formatElapsed(time.Since(job.start)), n, who)
	b.patchJournal(threadID, func(j *runjournal.Journal) {
		if j.Active != nil {
			j.Active.Status = runjournal.StatusCancelling
		}
	})
	job.cancel()
	msg = "Cancelling current run…"
	if n > 0 {
		msg = fmt.Sprintf("Cancelling current run… (%d follow-up%s still queued)", n, plural(n))
	}
	return msg, true
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (b *Bot) resetThread(s *discordgo.Session, m *discordgo.MessageCreate) {
	if e, ok := b.sessions.Get(m.ChannelID); ok && !b.canControlThread(s, m, e) {
		b.denyControl(s, m, e, "reset")
		return
	}
	msg, err := b.resetThreadCore(m.ChannelID)
	if err != nil {
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); sendErr != nil {
			log.Printf("error: reply reset: %v", sendErr)
		}
		return
	}
	if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); sendErr != nil {
		log.Printf("error: reply reset: %v", sendErr)
	}
}

// resetThreadCore clears session + worktree. msg is always set; err is non-nil on busy/failure.
func (b *Bot) resetThreadCore(threadID string) (msg string, err error) {
	if _, busy := b.getJob(threadID); busy {
		return "A run is in progress — Cancel first, then Reset.", fmt.Errorf("busy")
	}
	if n := b.clearQueue(threadID); n > 0 {
		log.Printf("reset: cleared %d queued follow-up(s) thread=%s", n, threadID)
	}

	if e, ok := b.sessions.Get(threadID); ok {
		mainCwd := e.MainCwd
		if mainCwd == "" {
			mainCwd = e.Cwd
		}
		branch := e.WorktreeBranch
		path, _ := gitworktree.ResolveSessionWorktreePath(b.cfg.DataDir, e.Project, threadID, e.Cwd, mainCwd)
		if branch == "" {
			branch = gitworktree.BranchNameForUnit(threadID)
		}
		if mainCwd != "" && (path != "" || branch != "") {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if rmErr := gitworktree.Remove(ctx, mainCwd, path, branch); rmErr != nil {
				log.Printf("warn: worktree cleanup on reset thread=%s: %v", threadID, rmErr)
			} else {
				log.Printf("reset: removed worktree thread=%s branch=%s", threadID, branch)
			}
			cancel()
		}
	}

	if delErr := b.sessions.Delete(threadID); delErr != nil {
		log.Printf("error: session delete: %v", delErr)
		return "Could not clear session: " + delErr.Error(), delErr
	}
	return "Session cleared for this thread (worktree removed if any).", nil
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

	opts := b.ensureOptsForUnit(threadID)
	if cleaned, state, cErr := gitworktree.CleanupIfPRDoneWith(ctx, proj.Cwd, b.cfg.DataDir, proj.Name, threadID, opts); cErr != nil {
		log.Printf("warn: worktree PR cleanup check thread=%s: %v", threadID, cErr)
	} else if cleaned {
		log.Printf("task: cleaned worktree after PR %s thread=%s", state, threadID)
		if delErr := b.sessions.Delete(threadID); delErr != nil {
			log.Printf("warn: session delete after PR cleanup thread=%s: %v", threadID, delErr)
		}
	}

	if e, ok := b.sessions.Get(threadID); ok && e.WorktreeBranch != "" {
		path, onDisk := gitworktree.ResolveSessionWorktreePath(b.cfg.DataDir, e.Project, threadID, e.Cwd, proj.Cwd)
		if onDisk && path != "" && path != proj.Cwd && gitworktree.IsRepo(path) {
			if e.Cwd != path {
				b.healSessionWorktreeCwd(threadID, path)
			}
			log.Printf("task: reuse session worktree branch=%s cwd=%s", e.WorktreeBranch, path)
			return path, e.WorktreeBranch, nil
		}
	}
	tree, err := gitworktree.EnsureWith(ctx, proj.Cwd, b.cfg.DataDir, proj.Name, threadID, opts)
	if err != nil {
		return "", "", err
	}
	return tree.Path, tree.Branch, nil
}

// ensureOptsForUnit picks managed branch prefix from session WorktreeBranch or unit id form.
func (b *Bot) ensureOptsForUnit(unitID string) gitworktree.EnsureOpts {
	if b != nil && b.sessions != nil {
		if e, ok := b.sessions.Get(unitID); ok {
			if p := gitworktree.PrefixFromBranch(e.WorktreeBranch); p != "" {
				return gitworktree.EnsureOpts{BranchPrefix: p}
			}
		}
	}
	return gitworktree.EnsureOpts{BranchPrefix: gitworktree.PrefixForUnitID(unitID)}
}

func remoteWorkPromptPrefix(branch string) string {
	lines := []string{
		"You are working on a shared workflow unit (Discord thread and/or web session) on a remote machine — not a local interactive session.",
	}
	if branch != "" {
		lines = append(lines,
			"Isolated git worktree for this workflow unit / thread.",
			"Branch: "+branch,
			"Stay in this worktree; do not switch to the main checkout.",
			"When you make code changes you MUST:",
			"1. Commit on this branch only (never commit to main/master).",
			"2. Push the branch to the remote (`git push -u origin HEAD`).",
			"3. Open a pull request with `gh pr create` (or push to update an existing PR for this branch).",
			"",
			"Uploading files to Discord: only files inside THIS worktree can be attached.",
			"If the user wants a build artifact, report, APK, Excel, etc. shared back, write the file under the worktree, then end your reply with:",
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
		"Include the PR URL in your final reply.",
		"",
	)
	return strings.Join(lines, "\n")
}

// checkMessageAccess resolves the channel's project and checks membership.
func (b *Bot) checkMessageAccess(s *discordgo.Session, m *discordgo.MessageCreate) (bool, string) {
	if m == nil || m.Author == nil {
		return false, "You're not allowed to use Grok."
	}
	parent := parentChannelID(s, m.ChannelID)
	project, ok := b.cfg.ChannelProject(parent)
	if !ok || project == "" {
		return false, "This channel is not mapped to a project."
	}
	if !b.cfg.ProjectHasAllowlist(project) {
		return false, fmt.Sprintf(
			"Project **%s** has no members configured. An admin must add members in the web config.",
			project,
		)
	}
	if b.isAllowedUser(s, m.GuildID, m.Author.ID, project, m.Member) {
		return true, ""
	}
	return false, fmt.Sprintf("You're not allowed to use Grok on project **%s**.", project)
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
	// Local title first so we can open the thread + early-ack without waiting on Grok.
	username := ""
	if m.Author != nil {
		username = m.Author.Username
	}
	title := threadNameFromPrompt(titlePrompt, username)
	needTitle := !isThread(s, m.ChannelID) || shouldRetitleThread(s, m.ChannelID)
	// Prefix Discord thread title with bound/parsed issue numbers (#42 …).
	titleIssues := sessionstore.ParseIssueRefs(titlePrompt)
	if e, ok := b.sessions.Get(m.ChannelID); ok && e.HasIssues() {
		titleIssues = append(titleIssues, e.Issues...)
	}
	title = prefixThreadTitleWithIssues(title, titleIssues)

	threadID, err := b.ensureThread(s, m, title)
	if err != nil {
		log.Printf("error: ensure thread: %v", err)
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, "Could not open thread: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply ensure-thread: %v", sendErr)
		}
		return
	}
	log.Printf("task: thread=%s title=%q", threadID, title)

	// Non-blocking title improve: rename the thread later if Grok returns a better name.
	if needTitle && b.cfg.SummarizeTitleEnabled() {
		go b.improveThreadTitle(s, threadID, titlePrompt, username, proj.Cwd, titleIssues)
	}

	// Early ack so the user sees activity before materialize / worktree / Grok.
	statusMsgID := ""
	if status, ackErr := discordSendComponents(s, threadID,
		startingStatus(proj.Name),
		actionBarRunning(threadID),
	); ackErr != nil {
		log.Printf("warn: early ack thread=%s: %v", threadID, ackErr)
		// REST failure can mean a half-dead gateway session; poke the watchdog path.
		b.maybeForceReconnectOnDiscordErr(ackErr)
	} else {
		statusMsgID = status.ID
		log.Printf("task: early-ack status=%s thread=%s", statusMsgID, threadID)
	}
	_ = s.ChannelTyping(threadID)

	// Phase A: materialize attachments / referenced prompt outside st.mu (K11).
	taskID := runjournal.NewTaskID()
	var related *discordgo.Message
	if hasMessageReference(m) {
		refMsg, refErr := resolveReferencedMessage(s, m)
		if refErr != nil {
			log.Printf("warn: referenced message (materialize): %v", refErr)
		} else {
			related = refMsg
		}
	}
	matCtx, matCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	paths, refPrompt, matErr := b.materializeTaskFiles(matCtx, threadID, taskID, m, nil, related)
	matCancel()
	if matErr != nil {
		log.Printf("error: materialize thread=%s: %v", threadID, matErr)
		msg := "Could not save attachments for this task: " + matErr.Error()
		b.postOrEditThreadStatus(s, threadID, statusMsgID, msg, actionBarDone(threadID))
		return
	}

	item := taskItem{
		s: s, m: m, parsed: parsed, proj: proj, threadID: threadID,
		actor:            ActorFromUser(m.Author),
		source:           SourceDiscord,
		origin:           SourceDiscord,
		taskID:           taskID,
		attempt:          1,
		attachmentPaths:  paths,
		referencedPrompt: refPrompt,
		triggerMsgID:     m.ID,
		statusMsgID:      statusMsgID,
	}
	if m.Author != nil {
		item.createdBy = m.Author.ID
		item.createdByName = m.Author.String()
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &runJob{cancel: cancel, start: time.Now(), project: proj.Name}
	claimed, queuePos, qerr := b.claimOrEnqueue(threadID, job, item)
	if qerr != nil {
		cancel()
		if b.runs != nil {
			b.runs.RemoveTaskFiles(threadID, taskID)
		}
		var msg string
		switch {
		case qerr == ErrNotReady:
			log.Printf("task: not ready thread=%s", threadID)
			msg = "Bot is starting up; try again in a moment."
		case qerr == errQueueFull:
			log.Printf("task: queue full thread=%s", threadID)
			msg = fmt.Sprintf(
				"Follow-up queue is full (max %d). Wait for a run to finish, or `@Grok /cancel`.", maxFollowupQueue,
			)
		default:
			log.Printf("task: claim failed thread=%s: %v", threadID, qerr)
			msg = "Could not queue task (durable state failed). Try again."
		}
		b.postOrEditThreadStatus(s, threadID, statusMsgID, msg, actionBarDone(threadID))
		return
	}
	if !claimed {
		cancel()
		log.Printf("task: queued pos=%d thread=%s msg=%s", queuePos, threadID, m.ID)
		b.postOrEditThreadStatus(s, threadID, statusMsgID, fmt.Sprintf(
			"Queued (#%d). Will run after the current task finishes.", queuePos,
		), actionBarDone(threadID))
		return
	}

	// Persist early status id so resume / progress can find it.
	if statusMsgID != "" {
		b.patchJournal(threadID, func(j *runjournal.Journal) {
			if j.Active != nil {
				j.Active.StatusMsgID = statusMsgID
			}
		})
	}

	b.drainWG.Add(1)
	b.drainTaskQueue(ctx, cancel, item, job)
}

// improveThreadTitle runs SummarizeTitle off the critical path and renames the thread if useful.
func (b *Bot) improveThreadTitle(s *discordgo.Session, threadID, titlePrompt, username, cwd string, issues []sessionstore.TrackedIssue) {
	if b == nil || s == nil || threadID == "" || b.cfg == nil {
		return
	}
	if b.stopping.Load() {
		return
	}
	timeout := time.Duration(b.cfg.SummarizeTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	log.Printf("task: summarizing title async thread=%s…", threadID)
	sumCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t, ok := grokrun.SummarizeTitle(sumCtx, b.cfg.GrokBin, b.cfg.Model, titlePrompt, cwd, timeout)
	if !ok {
		log.Printf("task: async summarize failed thread=%s (keeping local title)", threadID)
		return
	}
	if b.stopping.Load() {
		return
	}
	name := prefixThreadTitleWithIssues(threadNameFromPrompt(t, username), issues)
	if name == "" {
		return
	}
	if _, err := s.ChannelEdit(threadID, &discordgo.ChannelEdit{Name: name}); err != nil {
		log.Printf("warn: async retitle thread=%s: %v", threadID, err)
		return
	}
	log.Printf("task: async retitle thread=%s → %q", threadID, name)
}

// startingStatus is the early-ack line before worktree / Grok exec.
func startingStatus(project string) string {
	return fmt.Sprintf("Starting · **%s**…", project)
}

// postOrEditThreadStatus edits an existing status message when possible; otherwise sends a new one.
// Returns the status message id (may be empty on total failure).
func (b *Bot) postOrEditThreadStatus(s *discordgo.Session, threadID, msgID, content string, components []discordgo.MessageComponent) string {
	if s == nil || threadID == "" {
		return msgID
	}
	if msgID != "" {
		if err := discordEditComponents(s, threadID, msgID, content, components, true); err == nil {
			return msgID
		} else {
			log.Printf("warn: status edit thread=%s msg=%s: %v", threadID, msgID, err)
			b.maybeForceReconnectOnDiscordErr(err)
		}
	}
	msg, err := discordSendComponents(s, threadID, content, components)
	if err != nil {
		log.Printf("warn: status send thread=%s: %v", threadID, err)
		b.maybeForceReconnectOnDiscordErr(err)
		return msgID
	}
	return msg.ID
}

func (b *Bot) executeTask(ctx context.Context, item taskItem, job *runJob) {
	s, m, parsed, proj, threadID := item.s, item.m, item.parsed, item.proj, item.threadID
	actor := item.actor
	if actor.ID == "" && m != nil && m.Author != nil {
		actor = ActorFromUser(m.Author)
	}
	// Prefer live gateway session when item.s is nil (web path).
	if s == nil {
		s = b.Discord()
	}
	present := s != nil // Discord presentation available

	// Bind owner before the run so /cancel is gated for multi-person threads
	// even on the first task (session used to be written only after grok exits).
	b.bindThreadOwnerActor(threadID, proj.Name, actor)
	// open → in_progress on first real work (manual labels stay sticky).
	b.applyAutoLabelOnRunStart(threadID, proj.Name, actor)

	var thoughts thoughtTracker
	var statusID string
	if present {
		workHeader := workingStatus(proj.Name, 0, "", formatPhaseChips([phaseCount]bool{}, -1))
		// Prefer upgrading the early-ack message so the thread stays one status card.
		if item.statusMsgID != "" {
			if err := discordEditComponents(s, threadID, item.statusMsgID, workHeader, actionBarRunning(threadID), true); err != nil {
				log.Printf("warn: status upgrade thread=%s msg=%s: %v", threadID, item.statusMsgID, err)
				b.maybeForceReconnectOnDiscordErr(err)
			} else {
				statusID = item.statusMsgID
			}
		}
		if statusID == "" {
			status, err := discordSendComponents(s, threadID, workHeader, actionBarRunning(threadID))
			if err != nil {
				log.Printf("error: status message thread=%s: %v", threadID, err)
				b.maybeForceReconnectOnDiscordErr(err)
				// Soft-degrade: continue without live Discord status (still run Grok).
				present = false
			} else {
				statusID = status.ID
			}
		}
		if statusID != "" {
			b.patchJournal(threadID, func(j *runjournal.Journal) {
				if j.Active != nil {
					j.Active.StatusMsgID = statusID
				}
			})
		}
	}

	var streamer *streamPoster
	if present {
		streamer = newStreamPoster(s, threadID)
	} else {
		streamer = newStreamPosterWith(noopMessenger{}, threadID)
	}

	stopProgress := make(chan struct{})
	var progressWG sync.WaitGroup
	progressWG.Add(1)
	go func() {
		defer progressWG.Done()
		b.progressLoop(s, threadID, statusID, proj.Name, job, &thoughts, stopProgress)
	}()
	defer b.clearRunActivity(threadID)

	runCwd, wtBranch, wtErr := b.resolveRunCwd(ctx, proj, threadID)
	if wtErr != nil {
		streamer.Stop()
		close(stopProgress)
		progressWG.Wait()
		log.Printf("error: worktree thread=%s: %v", threadID, wtErr)
		if present && statusID != "" {
			if editErr := discordEditComponents(s, threadID, statusID, "Failed · worktree", actionBarDone(threadID), true); editErr != nil {
				log.Printf("error: edit status: %v", editErr)
			}
			sendChunks(s, threadID, "Could not create git worktree: "+wtErr.Error())
		}
		return
	}
	if wtBranch != "" {
		log.Printf("task: worktree branch=%s cwd=%s", wtBranch, runCwd)
	} else {
		log.Printf("task: no worktree isolation cwd=%s", runCwd)
	}
	b.patchJournal(threadID, func(j *runjournal.Journal) {
		j.WorktreeCwd = runCwd
		j.Branch = wtBranch
	})

	prompt := parsed.Prompt

	// Single-apply ReferencedPrompt: never resolve live if journal already has it.
	var related *discordgo.Message
	if item.referencedPrompt != "" {
		if prompt != "" {
			prompt = strings.TrimSpace(prompt) + "\n\n" + item.referencedPrompt
		} else {
			prompt = item.referencedPrompt
		}
		log.Printf("task: applied durable referenced prompt len=%d", len(item.referencedPrompt))
	} else if present && m != nil && hasMessageReference(m) {
		refMsg, refErr := resolveReferencedMessage(s, m)
		if refErr != nil {
			log.Printf("warn: referenced message: %v", refErr)
		} else if refMsg != nil {
			related = refMsg
			prompt = promptWithReferenced(prompt, related)
			log.Printf("task: included referenced message id=%s attachments=%d contentLen=%d",
				related.ID, len(related.Attachments), len(related.Content))
		} else if m.MessageReference != nil {
			log.Printf("warn: referenced message %s missing or deleted", m.MessageReference.MessageID)
		}
	}

	// Prefer durable paths (web / materialize); fail closed if listed but missing.
	if len(item.attachmentPaths) > 0 {
		var files []savedAttachment
		for _, p := range item.attachmentPaths {
			if _, stErr := os.Stat(p); stErr != nil {
				streamer.Stop()
				close(stopProgress)
				progressWG.Wait()
				log.Printf("error: durable attachment missing %s: %v", p, stErr)
				msg := "Attachments were lost before the run could start. Please re-send the task with files attached."
				if present && statusID != "" {
					if editErr := discordEditComponents(s, threadID, statusID, "Failed · attachments", actionBarDone(threadID), true); editErr != nil {
						log.Printf("error: edit status: %v", editErr)
					}
				}
				if present {
					sendChunks(s, threadID, msg)
				}
				return
			}
			files = append(files, savedAttachment{Path: p, Filename: filepath.Base(p)})
		}
		prompt = promptWithAttachments(prompt, files)
		log.Printf("task: using %d pre-attached path(s)", len(files))
	} else if present && m != nil {
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
				if statusID != "" {
					if editErr := discordEditComponents(s, threadID, statusID, "Failed · attachments", actionBarDone(threadID), true); editErr != nil {
						log.Printf("error: edit status: %v", editErr)
					}
				}
				sendChunks(s, threadID, msg)
				return
			}
			prompt = promptWithAttachments(prompt, files)
			log.Printf("task: saved %d attachment(s)", len(files))
		}
	}

	if item.attempt > 1 {
		prompt = interruptionPromptNote + prompt
	}

	// Normalize Discord link markup and keep query/# fragments explicit for the model.
	prompt = enrichPromptWithLinks(prompt)
	if urls := extractURLs(prompt); len(urls) > 0 {
		log.Printf("task: urls=%v", urls)
	}

	// Auto-bind GitHub (+ Linear when project-enabled) issues from the user prompt.
	var issueLines []sessionstore.TrackedIssue
	projName := proj.Name
	if e, ok := b.sessions.Get(threadID); ok {
		owner, repo := defaultIssueRepo(e)
		b.bindIssuesFromText(threadID, parsed.Prompt, owner, repo)
		b.bindLinearIssuesFromText(threadID, projName, parsed.Prompt)
		if related != nil {
			if refText := messagePromptText(related); refText != "" {
				b.bindIssuesFromText(threadID, refText, owner, repo)
				b.bindLinearIssuesFromText(threadID, projName, refText)
			}
		}
		if item.referencedPrompt != "" {
			b.bindIssuesFromText(threadID, item.referencedPrompt, owner, repo)
			b.bindLinearIssuesFromText(threadID, projName, item.referencedPrompt)
		}
	} else {
		b.bindIssuesFromText(threadID, parsed.Prompt, "", "")
		b.bindLinearIssuesFromText(threadID, projName, parsed.Prompt)
	}
	if e, ok := b.sessions.Get(threadID); ok {
		issueLines = e.Issues
	}

	prompt = remoteWorkPromptPrefix(wtBranch) + issueBindingPrompt(issueLines) + prompt

	sessionID, forceNew := b.prebindSessionID(threadID, proj.Name)
	if sessionID != "" {
		log.Printf("task: session=%s forceNew=%v attempt=%d", sessionID, forceNew, item.attempt)
	}

	maxTurns := b.cfg.MaxTurnsValue()
	timeout := time.Duration(b.cfg.TimeoutMsValue()) * time.Millisecond
	log.Printf("task: running grok bin=%s yolo=%v maxTurns=%d timeout=%s cwd=%s stream=true",
		b.cfg.GrokBin, b.cfg.YoloEnabled(), maxTurns, timeout, runCwd)

	result := grokrun.Run(ctx, grokrun.Options{
		GrokBin:         b.cfg.GrokBin,
		Prompt:          prompt,
		Cwd:             runCwd,
		SessionID:       sessionID,
		ForceNewSession: forceNew,
		Yolo:            b.cfg.YoloEnabled(),
		Model:           b.cfg.Model,
		MaxTurns:        maxTurns,
		Timeout:         timeout,
		ExtraArgs:       b.cfg.ExtraArgs,
		OnTextDelta: func(delta string) {
			streamer.OnDelta(delta)
		},
		OnThought: func(delta string) {
			thoughts.OnDelta(delta)
		},
		OnActivity: func(line string) {
			thoughts.OnActivity(line)
		},
		OnStartPID: func(pid int) {
			b.patchJournal(threadID, func(j *runjournal.Journal) {
				j.GrokPID = pid
			})
		},
	})
	streamer.Flush()
	b.patchJournal(threadID, func(j *runjournal.Journal) {
		j.GrokPID = 0
	})

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
		var prev sessionstore.Entry
		if e, ok := b.sessions.Get(threadID); ok {
			prev = e
			if sid == "" {
				sid = e.SessionID
			}
		}
		lastUser := actor.String()
		entry := sessionstore.Entry{
			SessionID:      sid,
			Project:        proj.Name,
			Cwd:            runCwd,
			MainCwd:        proj.Cwd,
			WorktreeBranch: wtBranch,
			LastUser:       lastUser,
			Origin:         item.origin,
			CreatedBy:      item.createdBy,
			CreatedByName:  item.createdByName,
			DiscordURL:     item.discordURL,
		}
		if entry.Origin == "" && item.source != "" {
			entry.Origin = item.source
		}
		if entry.CreatedBy == "" && actor.ID != "" {
			entry.CreatedBy = actor.ID
			entry.CreatedByName = actor.DisplayName
		}
		preservePRFields(&entry, prev)
		// Prefer latest goal/brief msg id if /brief raced in while this run finished.
		// (preserveBriefFields only fills empties; here we overwrite with the live store.)
		if fresh, ok := b.sessions.Get(threadID); ok {
			if fresh.Goal != "" {
				entry.Goal = fresh.Goal
			}
			if fresh.BriefMsgID != "" {
				entry.BriefMsgID = fresh.BriefMsgID
			}
		}
		if actor.ID != "" {
			ensureSessionOwner(&entry, actor.ID, actor.String())
		}
		// Keep lifecycle aligned with session/worktree even when no PR is discovered yet.
		entry.ApplyAutoLabel(entry.SuggestAutoLabel(false))
		if err := b.sessions.Set(threadID, entry); err != nil {
			log.Printf("error: session save: %v", err)
		}
	}

	header := fmt.Sprintf("Done · **%s** · %s", proj.Name, formatElapsed(elapsed))
	switch {
	case result.Cancelled:
		header = fmt.Sprintf("Cancelled · **%s** · %s", proj.Name, formatElapsed(elapsed))
	case result.MaxTurnsReached:
		header = fmt.Sprintf("Stopped · max turns reached · **%s** · %s", proj.Name, formatElapsed(elapsed))
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
	if present && statusID != "" {
		if err := discordEditComponents(s, threadID, statusID, header, actionBarDone(threadID), true); err != nil {
			log.Printf("error: edit status: %v", err)
		}
	}

	var fullyStreamed bool
	if result.Cancelled {
		streamer.Abort("cancelled")
		fullyStreamed = streamer.Text() != "" && streamer.Unposted() == ""
	} else {
		fullyStreamed = streamer.Finish()
	}
	if present {
		if !fullyStreamed {
			rem := streamer.Unposted()
			if rem == "" {
				rem = result.Text
			}
			sendChunks(s, threadID, rem)
		} else if result.MaxTurnsReached && !strings.Contains(streamer.Text(), "Reached max turns") {
			// Stream finished before the notice was injected (e.g. stderr-only detection).
			sendChunks(s, threadID, grokrun.MaxTurnsUserMessage)
		}

		if result.Stderr != "" && config.EnvWork("DEBUG") != "" {
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
	}

	histM := m
	if histM == nil && item.triggerMsgID != "" {
		histM = &discordgo.MessageCreate{Message: &discordgo.Message{ID: item.triggerMsgID}}
	}
	b.recordTurnActor(threadID, actor, histM, proj.Name, parsed.Prompt, result, elapsed)

	if !result.Cancelled {
		replyText := result.Text
		if replyText == "" {
			replyText = streamer.Text()
		}
		repoDir := runCwd
		if repoDir == "" {
			repoDir = proj.Cwd
		}
		// Bind/discover PRs even when Discord is absent (web-native / soft-degrade).
		b.refreshPRAfterTask(s, threadID, repoDir, wtBranch, replyText)
		b.ensureThreadGoal(threadID, parsed.Prompt)
		// Completion card + brief pin are Discord-only presentation.
		if present {
			b.postCompletionSummary(s, threadID, proj.Name, runCwd, wtBranch, elapsed, result.Code, result.Cancelled)
			if _, err := b.refreshBriefCard(s, threadID, runCwd); err != nil {
				log.Printf("brief: post-task refresh thread=%s: %v", threadID, err)
			}
		}
	}

	msgTag := ""
	if m != nil {
		msgTag = m.ID
	} else {
		msgTag = item.source
	}
	log.Printf("task: finished msg=%s thread=%s source=%s present=%v", msgTag, threadID, item.source, present)
}

func (b *Bot) recordTurn(threadID string, m *discordgo.MessageCreate, project, userPrompt string, result grokrun.Result, elapsed time.Duration) {
	actor := Actor{}
	if m != nil {
		actor = ActorFromUser(m.Author)
	}
	b.recordTurnActor(threadID, actor, m, project, userPrompt, result, elapsed)
}

func (b *Bot) recordTurnActor(threadID string, actor Actor, m *discordgo.MessageCreate, project, userPrompt string, result grokrun.Result, elapsed time.Duration) {
	if b.history == nil {
		return
	}
	status := "done"
	errMsg := ""
	switch {
	case result.Cancelled:
		status = "cancelled"
		errMsg = "Cancelled"
	case result.MaxTurnsReached:
		status = "error"
		errMsg = "Reached max turns before a final reply"
	case result.Code != 0:
		status = "error"
		errMsg = historyErrorFromResult(result)
	}
	user, userID := actor.String(), actor.ID
	if user == "" && m != nil && m.Author != nil {
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
		Error:     errMsg,
		Elapsed:   formatElapsed(elapsed),
		Project:   project,
		SessionID: result.SessionID,
		MessageID: msgID,
	}); err != nil {
		log.Printf("error: history append thread=%s: %v", threadID, err)
	}
}

// historyErrorFromResult picks a short failure reason for the history detail page.
func historyErrorFromResult(result grokrun.Result) string {
	if result.MaxTurnsReached || strings.Contains(result.Text, "Reached max turns") {
		return "Reached max turns before a final reply"
	}
	text := strings.TrimSpace(result.Text)
	if text != "" {
		first := text
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			first = strings.TrimSpace(text[:i])
		}
		lower := strings.ToLower(first)
		if strings.HasPrefix(lower, "timed out") ||
			strings.HasPrefix(lower, "failed ") ||
			strings.HasPrefix(lower, "error:") {
			if len(first) > 240 {
				first = first[:240] + "…"
			}
			return first
		}
	}
	if result.Code != 0 {
		return fmt.Sprintf("Grok exited with code %d", result.Code)
	}
	return "Run failed"
}

func (b *Bot) progressLoop(s *discordgo.Session, threadID, msgID, project string, job *runJob, thoughts *thoughtTracker, stop <-chan struct{}) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	start := time.Now()
	if job != nil {
		start = job.start
	}
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			activity, phases := "", formatPhaseChips([phaseCount]bool{}, -1)
			if thoughts != nil {
				activity, phases = thoughts.Progress()
			}
			// Always publish for web StatusSnapshot / SSE chips.
			b.publishRunActivity(threadID, activity, phases)
			if s == nil || msgID == "" || msgID == "noop" {
				continue
			}
			text := workingStatus(project, time.Since(start), activity, phases)
			if _, err := s.ChannelMessageEdit(threadID, msgID, text); err != nil {
				log.Printf("warn: progress edit thread=%s: %v", threadID, err)
			}
		}
	}
}

func workingStatus(project string, elapsed time.Duration, activity, phases string) string {
	var b strings.Builder
	if elapsed < time.Second {
		fmt.Fprintf(&b, "Working in **%s**… · Cancel button or `@Grok /cancel`", project)
	} else {
		fmt.Fprintf(&b, "Working in **%s**… · %s elapsed · Cancel button or `@Grok /cancel`",
			project, formatElapsed(elapsed))
	}
	phases = strings.TrimSpace(phases)
	if phases != "" {
		fmt.Fprintf(&b, "\n%s", phases)
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
	case KindFixCI:
		return "fix-ci"
	case KindClaim:
		return "claim"
	case KindHandOff:
		return "hand-off"
	case KindBrief:
		return "brief"
	case KindLabel:
		return "label"
	case KindBoard:
		return "board"
	case KindLink:
		return "link"
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
	sendChunksReply(s, channelID, text, nil)
}

// sendChunksReply posts text in Discord-safe chunks (≤ maxMsg). When reference is
// non-nil, the first chunk is a reply to that message; remaining chunks are plain
// channel messages so the full body can exceed Discord's 2000-char content limit.
func sendChunksReply(s *discordgo.Session, channelID, text string, reference *discordgo.MessageReference) {
	parts := splitMessage(text)
	log.Printf("reply: channel=%s parts=%d totalLen=%d", channelID, len(parts), len(text))
	for i, p := range parts {
		content := p
		if len(parts) > 1 {
			content = fmt.Sprintf("(%d/%d)\n%s", i+1, len(parts), p)
		}
		var err error
		if i == 0 && reference != nil {
			_, err = discordSendReply(s, channelID, content, reference)
		} else {
			_, err = discordSend(s, channelID, content)
		}
		if err != nil {
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
