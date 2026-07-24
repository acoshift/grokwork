package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/runjournal"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Task sources for dual-surface runs.
const (
	SourceDiscord = "discord"
	SourceWeb     = "web"
)

// Actor is who started a task (Discord user or web identity).
type Actor struct {
	ID          string // Discord snowflake preferred
	DisplayName string
}

// String returns a stable display form.
func (a Actor) String() string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.ID
}

// ActorFromUser builds an Actor from a Discord user.
func ActorFromUser(u *discordgo.User) Actor {
	if u == nil {
		return Actor{}
	}
	return Actor{ID: u.ID, DisplayName: u.String()}
}

// Discord returns the gateway session held after Register (may be nil before Open).
func (b *Bot) Discord() *discordgo.Session {
	if b == nil {
		return nil
	}
	b.discordMu.RLock()
	defer b.discordMu.RUnlock()
	return b.discord
}

// DiscordReady reports whether a gateway session is stored (Open may still be in progress).
func (b *Bot) DiscordReady() bool {
	return b.Discord() != nil
}

func (b *Bot) setDiscord(s *discordgo.Session) {
	if b == nil {
		return
	}
	b.discordMu.Lock()
	b.discord = s
	b.discordMu.Unlock()
}

// threadAPI creates Discord threads for web-started work units (testable).
type threadAPI interface {
	SendMessage(channelID, content string) (messageID string, err error)
	StartThread(channelID, messageID, name string) (threadID string, err error)
}

type discordThreadAPI struct {
	s *discordgo.Session
}

func (d discordThreadAPI) SendMessage(channelID, content string) (string, error) {
	msg, err := d.s.ChannelMessageSend(channelID, content)
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

func (d discordThreadAPI) StartThread(channelID, messageID, name string) (string, error) {
	th, err := d.s.MessageThreadStartComplex(channelID, messageID, &discordgo.ThreadStart{
		Name:                name,
		AutoArchiveDuration: 1440,
	})
	if err != nil {
		return "", err
	}
	return th.ID, nil
}

// CreateWorkflowThread opens a public thread without a user parent message.
// Posts a bot starter message, then starts a thread on it.
func (b *Bot) CreateWorkflowThread(channelID, title, starterContent string) (threadID string, err error) {
	if b == nil {
		return "", fmt.Errorf("bot is nil")
	}
	api := b.threadAPI
	if api == nil {
		s := b.Discord()
		if s == nil {
			return "", fmt.Errorf("discord not ready")
		}
		api = discordThreadAPI{s: s}
	}
	return createWorkflowThread(api, channelID, title, starterContent)
}

func createWorkflowThread(api threadAPI, channelID, title, starterContent string) (string, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return "", fmt.Errorf("empty channel id")
	}
	if api == nil {
		return "", fmt.Errorf("discord thread API not available")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Grok task"
	}
	if len(title) > 100 {
		title = title[:97] + "…"
	}
	starterContent = strings.TrimSpace(starterContent)
	if starterContent == "" {
		starterContent = "Starting work…"
	}
	msgID, err := api.SendMessage(channelID, starterContent)
	if err != nil {
		return "", fmt.Errorf("starter message: %w", err)
	}
	tid, err := api.StartThread(channelID, msgID, title)
	if err != nil {
		return "", fmt.Errorf("start thread: %w", err)
	}
	return tid, nil
}

// StartTaskOpts starts a Grok run on an existing thread (Discord or web-created).
// Presentation is optional: nil DG → no Discord posts (still runs Grok + history).
type StartTaskOpts struct {
	ThreadID        string
	Proj            projectRef
	Prompt          string
	Actor           Actor
	Source          string // SourceDiscord | SourceWeb
	DG              *discordgo.Session
	AttachmentPaths []string
	Origin          string // session Origin field
	CreatedBy       string
	CreatedByName   string
	DiscordURL      string
	// Kind selects the task Kind for policy snapshotting (zero value → KindTask).
	// Web starts map their mode select onto KindStartInvestigate/KindStartExplain
	// exactly as Discord "/start investigate|explain" does.
	Kind Kind
}

// StartTask claims the thread queue and runs the task (async drain).
// Returns queue position (0 = started now) or error (queue full, empty thread, etc.).
func (b *Bot) StartTask(opts StartTaskOpts) (queuePos int, err error) {
	if b == nil {
		return 0, fmt.Errorf("bot is nil")
	}
	threadID := strings.TrimSpace(opts.ThreadID)
	if threadID == "" {
		return 0, fmt.Errorf("empty thread id")
	}
	if strings.TrimSpace(opts.Proj.Name) == "" || strings.TrimSpace(opts.Proj.Cwd) == "" {
		return 0, fmt.Errorf("project required")
	}
	src := strings.TrimSpace(opts.Source)
	if src == "" {
		src = SourceDiscord
	}
	kind := opts.Kind
	if kind == KindEmpty {
		kind = KindTask
	}
	taskID := runjournal.NewTaskID()
	matCtx, matCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	paths, _, matErr := b.materializeTaskFiles(matCtx, threadID, taskID, nil, opts.AttachmentPaths, nil)
	matCancel()
	if matErr != nil {
		return 0, fmt.Errorf("materialize attachments: %w", matErr)
	}

	item := taskItem{
		s:               opts.DG,
		m:               nil,
		parsed:          Parsed{Kind: kind, Prompt: opts.Prompt},
		proj:            opts.Proj,
		threadID:        threadID,
		actor:           opts.Actor,
		source:          src,
		attachmentPaths: paths,
		origin:          opts.Origin,
		createdBy:       opts.CreatedBy,
		createdByName:   opts.CreatedByName,
		discordURL:      opts.DiscordURL,
		taskID:          taskID,
		attempt:         1,
		authorID:        opts.Actor.ID,
		authorName:      opts.Actor.DisplayName,
	}
	if item.origin == "" {
		item.origin = src
	}
	if item.createdBy == "" {
		item.createdBy = opts.Actor.ID
		item.createdByName = opts.Actor.DisplayName
	}
	// Web path: refuse closed cases before enqueue (executeTask also checks live).
	if b.sessions != nil {
		if e, ok := b.sessions.Get(threadID); ok && e.IsCaseClosed() {
			return 0, fmt.Errorf("case is closed — use /reopen first")
		}
	}
	// Defense in depth: explicit KindStartFix requires CanShip (web /start fix parity).
	if kind == KindStartFix {
		if err := b.requireCanStartFix(opts.Proj.Name, opts.Actor.ID, nil); err != nil {
			if b.runs != nil {
				b.runs.RemoveTaskFiles(threadID, taskID)
			}
			return 0, err
		}
	}
	b.snapshotPolicyOntoItem(&item, opts.Proj.Name, nil)

	ctx, cancel := context.WithCancel(context.Background())
	job := &runJob{cancel: cancel, start: time.Now(), project: opts.Proj.Name}
	claimed, pos, qerr := b.claimOrEnqueue(threadID, job, item)
	if qerr != nil {
		cancel()
		if b.runs != nil {
			b.runs.RemoveTaskFiles(threadID, taskID)
		}
		return 0, qerr
	}
	if !claimed {
		cancel()
		return pos, nil
	}
	b.drainWG.Add(1)
	go b.drainTaskQueue(ctx, cancel, item, job)
	return 0, nil
}

// publishRunActivity stores phase/activity on the live run job for StatusSnapshot.
func (b *Bot) publishRunActivity(threadID string, activity, phases string) {
	if b == nil || threadID == "" {
		return
	}
	v, ok := b.states.Load(threadID)
	if !ok {
		return
	}
	st := v.(*threadState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.job == nil {
		return
	}
	st.job.mu.Lock()
	st.job.activity = activity
	st.job.phases = phases
	st.job.mu.Unlock()
}

// publishRunPrompt stores the user-facing prompt for the in-flight turn (web session view).
func (b *Bot) publishRunPrompt(threadID, prompt string) {
	if b == nil || threadID == "" {
		return
	}
	v, ok := b.states.Load(threadID)
	if !ok {
		return
	}
	st := v.(*threadState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.job == nil {
		return
	}
	st.job.mu.Lock()
	st.job.prompt = prompt
	st.job.mu.Unlock()
}

// publishRunLiveText stores the accumulating assistant reply for web session streaming.
// Hot path (every text delta): release st.mu before writing job fields so StatusSnapshot
// and queue ops are not serialized behind stream updates.
func (b *Bot) publishRunLiveText(threadID, text string) {
	if b == nil || threadID == "" {
		return
	}
	v, ok := b.states.Load(threadID)
	if !ok {
		return
	}
	st := v.(*threadState)
	st.mu.Lock()
	job := st.job
	st.mu.Unlock()
	if job == nil {
		return
	}
	job.mu.Lock()
	job.liveText = text
	job.mu.Unlock()
}

func (b *Bot) clearRunActivity(threadID string) {
	if b == nil || threadID == "" {
		return
	}
	v, ok := b.states.Load(threadID)
	if !ok {
		return
	}
	st := v.(*threadState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.job == nil {
		return
	}
	st.job.mu.Lock()
	st.job.activity = ""
	st.job.phases = ""
	st.job.prompt = ""
	st.job.liveText = ""
	st.job.mu.Unlock()
}

// noopMessenger discards Discord presentation (Discord-optional runs).
type noopMessenger struct{}

func (noopMessenger) Send(_, _ string) (string, error) { return "noop", nil }
func (noopMessenger) Edit(_, _, _ string) error        { return nil }
func (noopMessenger) Typing(_ string) error            { return nil }

// preserveWorkflowFields copies dual-surface metadata onto next when session Set rebuilds the entry.
func preserveWorkflowFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if next.Origin == "" {
		next.Origin = prev.Origin
	}
	if next.CreatedBy == "" {
		next.CreatedBy = prev.CreatedBy
		next.CreatedByName = prev.CreatedByName
	}
	if next.DiscordURL == "" {
		next.DiscordURL = prev.DiscordURL
	}
}
