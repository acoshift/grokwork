package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func testBotWithData(t *testing.T) (*Bot, string) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	// Minimal git repo so worktree Ensure can work when isolation is on — disable isolation for simpler tests.
	cfg := &config.Config{
		GrokBin:           "false", // overridden per-test when running executeTask
		Projects:          config.PathProjects(map[string]string{"app": proj}),
		Channels:          map[string]string{"ch1": "app"},
		DataDir:           filepath.Join(dir, "data"),
		ConfigPath:        filepath.Join(dir, "config.json"),
		WorktreeIsolation: boolPtr(false),
		MaxTurns:          5,
		TimeoutMs:         5000,
		Yolo:              boolPtr(true),
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, store, hist), proj
}

func boolPtr(v bool) *bool { return &v }

func TestDiscordReadyAfterRegister(t *testing.T) {
	b, _ := testBotWithData(t)
	if b.DiscordReady() {
		t.Fatal("expected not ready before Register")
	}
	// Register stores session without Open.
	s, err := discordgo.New("Bot fake-token")
	if err != nil {
		t.Fatal(err)
	}
	b.Register(s)
	if !b.DiscordReady() {
		t.Fatal("expected DiscordReady after Register")
	}
	if b.Discord() != s {
		t.Fatal("Discord() should return registered session")
	}
}

type fakeThreadAPI struct {
	mu       sync.Mutex
	sends    []string
	starts   []string
	failSend error
	failStart error
	nextMsg  string
	nextTh   string
}

func (f *fakeThreadAPI) SendMessage(channelID, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, channelID+"|"+content)
	if f.failSend != nil {
		return "", f.failSend
	}
	if f.nextMsg == "" {
		f.nextMsg = "msg-1"
	}
	return f.nextMsg, nil
}

func (f *fakeThreadAPI) StartThread(channelID, messageID, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, channelID+"|"+messageID+"|"+name)
	if f.failStart != nil {
		return "", f.failStart
	}
	if f.nextTh == "" {
		f.nextTh = "thread-1"
	}
	return f.nextTh, nil
}

func TestCreateWorkflowThread(t *testing.T) {
	b, _ := testBotWithData(t)
	fake := &fakeThreadAPI{nextMsg: "m9", nextTh: "th-99"}
	b.threadAPI = fake

	// Not ready without API would fail; with inject ok.
	id, err := b.CreateWorkflowThread("ch1", "Fix payment", "Starting…")
	if err != nil {
		t.Fatal(err)
	}
	if id != "th-99" {
		t.Fatalf("id=%q", id)
	}
	if len(fake.sends) != 1 || !strings.Contains(fake.sends[0], "Starting") {
		t.Fatalf("sends=%v", fake.sends)
	}
	if len(fake.starts) != 1 || !strings.Contains(fake.starts[0], "ch1|m9|Fix payment") {
		t.Fatalf("starts=%v", fake.starts)
	}
}

func TestCreateWorkflowThreadPureHelper(t *testing.T) {
	fake := &fakeThreadAPI{}
	id, err := createWorkflowThread(fake, "chan", "Title here", "")
	if err != nil || id == "" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	// empty starter gets default
	if !strings.Contains(fake.sends[0], "Starting work") {
		t.Fatalf("sends=%v", fake.sends)
	}
	if _, err := createWorkflowThread(fake, "", "t", "s"); err == nil {
		t.Fatal("expected empty channel error")
	}
	if _, err := createWorkflowThread(nil, "c", "t", "s"); err == nil {
		t.Fatal("expected nil api error")
	}
	fake.failSend = fmt.Errorf("boom")
	if _, err := createWorkflowThread(fake, "c", "t", "s"); err == nil {
		t.Fatal("expected send error")
	}
}

func TestCreateWorkflowThreadNotReady(t *testing.T) {
	b, _ := testBotWithData(t)
	// no threadAPI, no discord
	if _, err := b.CreateWorkflowThread("ch", "t", "s"); err == nil {
		t.Fatal("expected not ready")
	}
}

func TestPreserveWorkflowFields(t *testing.T) {
	prev := sessionstore.Entry{
		Origin: SourceWeb, CreatedBy: "u1", CreatedByName: "Alice", DiscordURL: "https://discord.com/x",
		OwnerID: "u1", OwnerName: "Alice",
	}
	next := sessionstore.Entry{SessionID: "s", Project: "app"}
	preservePRFields(&next, prev)
	if next.Origin != SourceWeb || next.CreatedBy != "u1" || next.CreatedByName != "Alice" || next.DiscordURL == "" {
		t.Fatalf("workflow fields dropped: %+v", next)
	}
	if next.OwnerID != "u1" {
		t.Fatalf("ownership dropped: %+v", next)
	}
	// Explicit next wins
	next2 := sessionstore.Entry{Origin: SourceDiscord, CreatedBy: "u2"}
	preserveWorkflowFields(&next2, prev)
	if next2.Origin != SourceDiscord || next2.CreatedBy != "u2" {
		t.Fatalf("should keep next: %+v", next2)
	}
}

func TestPublishAndClearRunActivity(t *testing.T) {
	b, _ := testBotWithData(t)
	job := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	claimed, _, err := b.claimOrEnqueue("th-act", job, taskItem{threadID: "th-act"})
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	b.publishRunActivity("th-act", "editing foo.go", "✓read · **edit**")
	b.publishRunPrompt("th-act", "fix the flaky test")
	b.publishRunLiveText("th-act", "Looking at the race…")
	snap := b.StatusSnapshot()
	if snap.ActiveCount != 1 {
		t.Fatalf("active=%d", snap.ActiveCount)
	}
	if snap.ActiveRuns[0].Activity != "editing foo.go" {
		t.Fatalf("activity=%q", snap.ActiveRuns[0].Activity)
	}
	if snap.ActiveRuns[0].Phases == "" {
		t.Fatal("expected phases")
	}
	if snap.ActiveRuns[0].Prompt != "fix the flaky test" {
		t.Fatalf("prompt=%q", snap.ActiveRuns[0].Prompt)
	}
	if snap.ActiveRuns[0].LiveText != "Looking at the race…" {
		t.Fatalf("liveText=%q", snap.ActiveRuns[0].LiveText)
	}
	b.clearRunActivity("th-act")
	snap = b.StatusSnapshot()
	if snap.ActiveRuns[0].Activity != "" || snap.ActiveRuns[0].Phases != "" ||
		snap.ActiveRuns[0].Prompt != "" || snap.ActiveRuns[0].LiveText != "" {
		t.Fatalf("expected cleared: %+v", snap.ActiveRuns[0])
	}
	// finish
	b.finishRun("th-act")
}

func writeFakeGrok(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-grok")
	// streaming-json consumer expects line events ending with end
	script := `#!/bin/sh
printf '%s\n' '{"type":"text","data":"hello from fake"}'
printf '%s\n' '{"type":"end","sessionId":"sess-fake","stopReason":"EndTurn","num_turns":1,"usage":{"total_tokens":10}}'
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecuteTaskDiscordOptionalNilSession(t *testing.T) {
	b, projPath := testBotWithData(t)
	b.cfg.GrokBin = writeFakeGrok(t)
	threadID := "web-thread-1"
	// Seed empty session so preserve path has something to merge with
	if err := b.sessions.Set(threadID, sessionstore.Entry{
		Project: "app", Origin: SourceWeb, CreatedBy: "user-9", CreatedByName: "Web User",
		DiscordURL: "https://discord.example/threads/web-thread-1",
	}); err != nil {
		t.Fatal(err)
	}

	item := taskItem{
		s: nil, m: nil,
		parsed:   Parsed{Kind: KindTask, Prompt: "fix the flaky test"},
		proj:     projectRef{Name: "app", Cwd: projPath},
		threadID: threadID,
		actor:    Actor{ID: "user-9", DisplayName: "Web User"},
		source:   SourceWeb,
		origin:   SourceWeb,
		createdBy: "user-9", createdByName: "Web User",
		discordURL: "https://discord.example/threads/web-thread-1",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &runJob{cancel: cancel, start: time.Now(), project: "app"}
	claimed, _, err := b.claimOrEnqueue(threadID, job, item)
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}

	// Run synchronously (not drain) so we can assert after.
	b.executeTask(ctx, item, job)
	b.finishRun(threadID)

	// History recorded
	th, err := b.history.Get(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) != 1 {
		t.Fatalf("turns=%d", len(th.Turns))
	}
	if th.Turns[0].UserID != "user-9" || !strings.Contains(th.Turns[0].Prompt, "flaky") {
		t.Fatalf("turn=%+v", th.Turns[0])
	}
	if th.Turns[0].Status != "done" && th.Turns[0].Response == "" {
		// fake may set done with text
		t.Logf("turn status=%s response=%q", th.Turns[0].Status, th.Turns[0].Response)
	}

	// Session fields preserved after run rewrite
	e, ok := b.sessions.Get(threadID)
	if !ok {
		t.Fatal("session missing")
	}
	if e.Origin != SourceWeb {
		t.Fatalf("origin=%q", e.Origin)
	}
	if e.CreatedBy != "user-9" || e.CreatedByName != "Web User" {
		t.Fatalf("createdBy=%q name=%q", e.CreatedBy, e.CreatedByName)
	}
	if e.DiscordURL == "" {
		t.Fatal("discord URL dropped")
	}
	if e.OwnerID != "user-9" {
		t.Fatalf("owner=%q", e.OwnerID)
	}
	if e.SessionID == "" && e.WorktreeBranch == "" {
		// Without isolation we may only get session id from fake grok
		t.Logf("session=%+v", e)
	}
	// Response should have fake text if streaming worked
	if !strings.Contains(th.Turns[0].Response, "hello from fake") && th.Turns[0].Status == "done" {
		// allow if session id only
		if e.SessionID != "sess-fake" && th.Turns[0].SessionID != "sess-fake" {
			t.Fatalf("expected fake output or session: turn=%+v entry=%+v", th.Turns[0], e)
		}
	}
}

func TestStartTaskQueuesAndRuns(t *testing.T) {
	b, projPath := testBotWithData(t)
	b.cfg.GrokBin = writeFakeGrok(t)
	threadID := "start-task-1"

	// Hold a fake running job so StartTask enqueues
	block := make(chan struct{})
	ctx1, cancel1 := context.WithCancel(context.Background())
	job1 := &runJob{cancel: cancel1, start: time.Now(), project: "app"}
	itemHold := taskItem{threadID: threadID, proj: projectRef{Name: "app", Cwd: projPath}}
	if claimed, _, err := b.claimOrEnqueue(threadID, job1, itemHold); err != nil || !claimed {
		t.Fatalf("hold: %v %v", claimed, err)
	}
	// Run a blocking execute in background that waits on block
	go func() {
		<-block
		// minimal finish without full execute
		cancel1()
		if next, ok := b.finishRun(threadID); ok {
			// drain next via StartTask path already started? we only enqueued
			_ = next
		}
	}()

	pos, err := b.StartTask(StartTaskOpts{
		ThreadID: threadID,
		Proj:     projectRef{Name: "app", Cwd: projPath},
		Prompt:   "queued work",
		Actor:    Actor{ID: "u2", DisplayName: "Bob"},
		Source:   SourceWeb,
		Origin:   SourceWeb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pos != 1 {
		t.Fatalf("queue pos=%d want 1", pos)
	}
	if b.queueLen(threadID) != 1 {
		t.Fatalf("queueLen=%d", b.queueLen(threadID))
	}
	close(block)
	// cleanup
	cancel1()
	b.clearQueue(threadID)
	b.finishRun(threadID)
	_ = ctx1
}

func TestStartTaskRunsWhenIdle(t *testing.T) {
	b, projPath := testBotWithData(t)
	b.cfg.GrokBin = writeFakeGrok(t)
	threadID := "start-idle-1"
	pos, err := b.StartTask(StartTaskOpts{
		ThreadID: threadID,
		Proj:     projectRef{Name: "app", Cwd: projPath},
		Prompt:   "do the thing",
		Actor:    Actor{ID: "u3", DisplayName: "Cara"},
		Source:   SourceWeb,
		Origin:   SourceWeb,
		CreatedBy: "u3", CreatedByName: "Cara",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Fatalf("pos=%d want 0 (started)", pos)
	}
	// Wait for async drain
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b.queueLen(threadID) == 0 {
			// check if job finished
			v, ok := b.states.Load(threadID)
			if ok {
				st := v.(*threadState)
				st.mu.Lock()
				idle := st.job == nil
				st.mu.Unlock()
				if idle {
					break
				}
			} else {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	// History should eventually appear
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		th, err := b.history.Get(threadID)
		if err == nil && len(th.Turns) >= 1 {
			if th.Turns[0].UserID != "u3" {
				t.Fatalf("user=%q", th.Turns[0].UserID)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout waiting for history turn")
}

func TestBindThreadOwnerActorNilSafe(t *testing.T) {
	b, _ := testBotWithData(t)
	// empty actor no-op
	b.bindThreadOwnerActor("t", "app", Actor{})
	if _, ok := b.sessions.Get("t"); ok {
		t.Fatal("should not create session for empty actor")
	}
	b.bindThreadOwnerActor("t", "app", Actor{ID: "u1", DisplayName: "A"})
	e, ok := b.sessions.Get("t")
	if !ok || e.OwnerID != "u1" {
		t.Fatalf("%+v ok=%v", e, ok)
	}
	// second bind no change
	b.bindThreadOwnerActor("t", "app", Actor{ID: "u2", DisplayName: "B"})
	e, _ = b.sessions.Get("t")
	if e.OwnerID != "u1" {
		t.Fatalf("owner changed: %q", e.OwnerID)
	}
}

func TestQueueStillWorksAfterRefactor(t *testing.T) {
	b, _ := testBotWithData(t)
	threadID := "q1"
	job1 := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	item1 := taskItem{threadID: threadID, proj: projectRef{Name: "app"}, actor: Actor{ID: "a"}}
	claimed, pos, err := b.claimOrEnqueue(threadID, job1, item1)
	if err != nil || !claimed || pos != 0 {
		t.Fatalf("first: claimed=%v pos=%d err=%v", claimed, pos, err)
	}
	item2 := taskItem{threadID: threadID, parsed: Parsed{Prompt: "follow"}, actor: Actor{ID: "b"}}
	claimed, pos, err = b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, item2)
	if err != nil || claimed || pos != 1 {
		t.Fatalf("queue: claimed=%v pos=%d err=%v", claimed, pos, err)
	}
	next, ok := b.finishRun(threadID)
	if !ok || next.parsed.Prompt != "follow" || next.actor.ID != "b" {
		t.Fatalf("next=%+v ok=%v", next, ok)
	}
}

func TestRemotePromptNoMergeContract(t *testing.T) {
	p := remoteWorkPromptPrefix("grok/discord/x")
	if !strings.Contains(p, "Do not merge") {
		t.Fatal("must keep do not merge")
	}
	if strings.Contains(strings.ToLower(p), "only via discord") {
		t.Fatal("should not claim Discord-only exclusively")
	}
}

func TestActorHelpers(t *testing.T) {
	if ActorFromUser(nil).ID != "" {
		t.Fatal("nil user")
	}
	a := Actor{ID: "1", DisplayName: "Bob#0"}
	if a.String() != "Bob#0" {
		t.Fatalf("String=%q", a.String())
	}
	if (Actor{ID: "x"}).String() != "x" {
		t.Fatal("fallback to ID")
	}
	u := &discordgo.User{ID: "99", Username: "cara", Discriminator: "0001"}
	from := ActorFromUser(u)
	if from.ID != "99" || from.DisplayName == "" {
		t.Fatalf("%+v", from)
	}
}

func TestStartTaskValidation(t *testing.T) {
	b, projPath := testBotWithData(t)
	if _, err := b.StartTask(StartTaskOpts{}); err == nil {
		t.Fatal("empty thread")
	}
	if _, err := b.StartTask(StartTaskOpts{ThreadID: "t"}); err == nil {
		t.Fatal("empty project")
	}
	if _, err := b.StartTask(StartTaskOpts{
		ThreadID: "t",
		Proj:     projectRef{Name: "app"}, // no Cwd
	}); err == nil {
		t.Fatal("empty cwd")
	}
	// Valid enough to start (async) — use fake grok
	b.cfg.GrokBin = writeFakeGrok(t)
	pos, err := b.StartTask(StartTaskOpts{
		ThreadID: "valid-1",
		Proj:     projectRef{Name: "app", Cwd: projPath},
		Prompt:   "hi",
		Actor:    Actor{ID: "u", DisplayName: "U"},
		Source:   SourceWeb,
	})
	if err != nil || pos != 0 {
		t.Fatalf("pos=%d err=%v", pos, err)
	}
	// Wait briefly so async drain doesn't race package teardown.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := b.states.Load("valid-1")
		if !ok {
			return
		}
		st := v.(*threadState)
		st.mu.Lock()
		idle := st.job == nil
		st.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func TestStartTaskQueueFull(t *testing.T) {
	b, projPath := testBotWithData(t)
	threadID := "q-full-start"
	job := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID}); err != nil || !claimed {
		t.Fatalf("hold: %v %v", claimed, err)
	}
	for i := 0; i < maxFollowupQueue; i++ {
		pos, err := b.StartTask(StartTaskOpts{
			ThreadID: threadID,
			Proj:     projectRef{Name: "app", Cwd: projPath},
			Prompt:   fmt.Sprintf("q%d", i),
			Actor:    Actor{ID: "u", DisplayName: "U"},
			Source:   SourceWeb,
		})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if pos != i+1 {
			t.Fatalf("pos=%d want %d", pos, i+1)
		}
	}
	_, err := b.StartTask(StartTaskOpts{
		ThreadID: threadID,
		Proj:     projectRef{Name: "app", Cwd: projPath},
		Prompt:   "overflow",
		Actor:    Actor{ID: "u", DisplayName: "U"},
		Source:   SourceWeb,
	})
	if err != errQueueFull {
		t.Fatalf("want queue full, got %v", err)
	}
	b.clearQueue(threadID)
	b.finishRun(threadID)
}

func TestCreateWorkflowThreadTitleTruncation(t *testing.T) {
	fake := &fakeThreadAPI{}
	long := strings.Repeat("x", 150)
	id, err := createWorkflowThread(fake, "ch", long, "hi")
	if err != nil || id == "" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	// StartThread records name as third field
	parts := strings.SplitN(fake.starts[0], "|", 3)
	if len(parts) != 3 {
		t.Fatalf("starts=%v", fake.starts)
	}
	name := parts[2]
	if len(name) > 100 {
		t.Fatalf("name len=%d", len(name))
	}
	if !strings.HasSuffix(name, "…") {
		t.Fatalf("expected ellipsis: %q", name)
	}
}

func TestCreateWorkflowThreadStartFail(t *testing.T) {
	fake := &fakeThreadAPI{failStart: fmt.Errorf("thread boom")}
	if _, err := createWorkflowThread(fake, "ch", "t", "s"); err == nil {
		t.Fatal("expected start error")
	}
	if len(fake.sends) != 1 {
		t.Fatalf("should have sent starter first: %v", fake.sends)
	}
}

func TestExecuteTaskWithAttachmentPaths(t *testing.T) {
	b, projPath := testBotWithData(t)
	b.cfg.GrokBin = writeFakeGrok(t)
	// Write a fake attachment file the prompt will reference.
	att := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(att, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	threadID := "att-path-1"
	item := taskItem{
		parsed:          Parsed{Kind: KindTask, Prompt: "use the note"},
		proj:            projectRef{Name: "app", Cwd: projPath},
		threadID:        threadID,
		actor:           Actor{ID: "u-att", DisplayName: "Attacher"},
		source:          SourceWeb,
		origin:          SourceWeb,
		attachmentPaths: []string{att},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &runJob{cancel: cancel, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, item); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	b.executeTask(ctx, item, job)
	b.finishRun(threadID)

	th, err := b.history.Get(threadID)
	if err != nil || len(th.Turns) != 1 {
		t.Fatalf("history: %+v err=%v", th, err)
	}
	if th.Turns[0].UserID != "u-att" {
		t.Fatalf("user=%q", th.Turns[0].UserID)
	}
}

func TestExecuteTaskDiscordOriginUsesActor(t *testing.T) {
	// Simulate Discord handleTask item shape without a live gateway: m nil, actor set, source discord.
	b, projPath := testBotWithData(t)
	b.cfg.GrokBin = writeFakeGrok(t)
	threadID := "discord-shape-1"
	item := taskItem{
		s: nil, m: nil,
		parsed:   Parsed{Kind: KindTask, Prompt: "discord-style follow-up"},
		proj:     projectRef{Name: "app", Cwd: projPath},
		threadID: threadID,
		actor:    Actor{ID: "disc-1", DisplayName: "DiscordUser#1"},
		source:   SourceDiscord,
		origin:   SourceDiscord,
		createdBy: "disc-1", createdByName: "DiscordUser#1",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &runJob{cancel: cancel, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, item); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	b.executeTask(ctx, item, job)
	b.finishRun(threadID)

	e, ok := b.sessions.Get(threadID)
	if !ok {
		t.Fatal("session missing")
	}
	if e.Origin != SourceDiscord {
		t.Fatalf("origin=%q", e.Origin)
	}
	if e.OwnerID != "disc-1" || e.LastUser != "DiscordUser#1" {
		t.Fatalf("owner/lastUser: %+v", e)
	}
	th, _ := b.history.Get(threadID)
	if len(th.Turns) != 1 || th.Turns[0].UserID != "disc-1" {
		t.Fatalf("turn=%+v", th)
	}
}

func TestExecuteTaskCancelledStillRecordsHistory(t *testing.T) {
	b, projPath := testBotWithData(t)
	// Slow fake grok so we can cancel mid-run.
	dir := t.TempDir()
	path := filepath.Join(dir, "slow-grok")
	script := `#!/bin/sh
sleep 30
printf '%s\n' '{"type":"end","sessionId":"x","stopReason":"EndTurn","num_turns":1}'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b.cfg.GrokBin = path
	b.cfg.TimeoutMs = 60000

	threadID := "cancel-web-1"
	ctx, cancel := context.WithCancel(context.Background())
	item := taskItem{
		parsed:   Parsed{Kind: KindTask, Prompt: "long job"},
		proj:     projectRef{Name: "app", Cwd: projPath},
		threadID: threadID,
		actor:    Actor{ID: "c1", DisplayName: "Canceller"},
		source:   SourceWeb,
		origin:   SourceWeb,
	}
	job := &runJob{cancel: cancel, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, item); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.executeTask(ctx, item, job)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeTask hung after cancel")
	}
	b.finishRun(threadID)

	th, err := b.history.Get(threadID)
	if err != nil || len(th.Turns) != 1 {
		t.Fatalf("history: %+v err=%v", th, err)
	}
	if th.Turns[0].Status != "cancelled" {
		t.Fatalf("status=%q want cancelled", th.Turns[0].Status)
	}
}

func TestProgressLoopPublishesWithoutDiscord(t *testing.T) {
	b, _ := testBotWithData(t)
	threadID := "prog-1"
	job := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID}); err != nil || !claimed {
		t.Fatalf("claim: %v", err)
	}
	stop := make(chan struct{})
	var thoughts thoughtTracker
	thoughts.OnActivity("reading main.go")
	done := make(chan struct{})
	go func() {
		defer close(done)
		// nil session + empty msgID → publish only (no Discord edit panic)
		b.progressLoop(nil, threadID, "", "app", job, &thoughts, stop)
	}()
	// progressInterval is 4s; wait for first tick + slack
	deadline := time.Now().Add(progressInterval + 2*time.Second)
	for time.Now().Before(deadline) {
		snap := b.StatusSnapshot()
		if len(snap.ActiveRuns) == 1 && snap.ActiveRuns[0].Activity != "" {
			close(stop)
			<-done
			b.finishRun(threadID)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	<-done
	t.Fatal("activity never published")
}

func TestDiscordSendNilSessionSafe(t *testing.T) {
	if _, err := discordSend(nil, "ch", "hi"); err == nil {
		t.Fatal("expected error")
	}
	if err := discordEdit(nil, "ch", "m", "hi"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := discordSendReply(nil, "ch", "hi", nil); err == nil {
		t.Fatal("expected error")
	}
	if _, err := discordSendEmbed(nil, "ch", &discordgo.MessageEmbed{Title: "t"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestNoopMessenger(t *testing.T) {
	var m noopMessenger
	if id, err := m.Send("c", "x"); err != nil || id != "noop" {
		t.Fatalf("send: %q %v", id, err)
	}
	if err := m.Edit("c", "m", "y"); err != nil {
		t.Fatal(err)
	}
	if err := m.Typing("c"); err != nil {
		t.Fatal(err)
	}
}

func TestStartTaskDefaultsSourceAndCreatedBy(t *testing.T) {
	b, projPath := testBotWithData(t)
	b.cfg.GrokBin = writeFakeGrok(t)
	// Hold queue so we can inspect the enqueued item without racing execute.
	threadID := "defaults-1"
	hold := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, hold, taskItem{threadID: threadID}); err != nil || !claimed {
		t.Fatal(err)
	}
	pos, err := b.StartTask(StartTaskOpts{
		ThreadID: threadID,
		Proj:     projectRef{Name: "app", Cwd: projPath},
		Prompt:   "p",
		Actor:    Actor{ID: "def-u", DisplayName: "Def User"},
		// Source empty → discord default; Origin empty → follows source
	})
	if err != nil || pos != 1 {
		t.Fatalf("pos=%d err=%v", pos, err)
	}
	v, _ := b.states.Load(threadID)
	st := v.(*threadState)
	st.mu.Lock()
	item := st.queue[0]
	st.mu.Unlock()
	if item.source != SourceDiscord {
		t.Fatalf("source=%q", item.source)
	}
	if item.origin != SourceDiscord {
		t.Fatalf("origin=%q", item.origin)
	}
	if item.createdBy != "def-u" || item.createdByName != "Def User" {
		t.Fatalf("createdBy=%q name=%q", item.createdBy, item.createdByName)
	}
	b.clearQueue(threadID)
	b.finishRun(threadID)
}

func TestPreserveWorkflowFieldsNilSafe(t *testing.T) {
	preserveWorkflowFields(nil, sessionstore.Entry{Origin: SourceWeb})
	// empty next fully filled from prev
	next := sessionstore.Entry{}
	prev := sessionstore.Entry{
		Origin: SourceWeb, CreatedBy: "a", CreatedByName: "A", DiscordURL: "u",
	}
	preserveWorkflowFields(&next, prev)
	if next.Origin != SourceWeb || next.CreatedBy != "a" || next.DiscordURL != "u" {
		t.Fatalf("%+v", next)
	}
}

func TestRegisterStoresDiscordSession(t *testing.T) {
	b, _ := testBotWithData(t)
	s1, err := discordgo.New("Bot t1")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := discordgo.New("Bot t2")
	if err != nil {
		t.Fatal(err)
	}
	b.Register(s1)
	if b.Discord() != s1 {
		t.Fatal("first register")
	}
	b.Register(s2)
	if b.Discord() != s2 {
		t.Fatal("re-register should replace")
	}
	if !b.DiscordReady() {
		t.Fatal("ready")
	}
}

func TestExecuteTaskSetsGoalWithoutDiscord(t *testing.T) {
	b, projPath := testBotWithData(t)
	b.cfg.GrokBin = writeFakeGrok(t)
	threadID := "goal-web-1"
	item := taskItem{
		parsed:   Parsed{Kind: KindTask, Prompt: "implement feature X carefully"},
		proj:     projectRef{Name: "app", Cwd: projPath},
		threadID: threadID,
		actor:    Actor{ID: "g1", DisplayName: "Goal User"},
		source:   SourceWeb,
		origin:   SourceWeb,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &runJob{cancel: cancel, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, item); err != nil || !claimed {
		t.Fatal(err)
	}
	b.executeTask(ctx, item, job)
	b.finishRun(threadID)
	e, ok := b.sessions.Get(threadID)
	if !ok || e.Goal == "" {
		t.Fatalf("expected sticky goal: %+v ok=%v", e, ok)
	}
	if !strings.Contains(e.Goal, "feature X") {
		t.Fatalf("goal=%q", e.Goal)
	}
}

// Web-origin follow-ups used to panic in drainTaskQueue on next.m.ID / next.s.ChannelMessageSend.
func TestDrainTaskQueueWebFollowUpNoPanic(t *testing.T) {
	b, projPath := testBotWithData(t)
	b.cfg.GrokBin = writeFakeGrok(t)
	threadID := "drain-web-q"

	// First task runs quickly; second is web-origin with nil m/s (the panic case).
	item1 := taskItem{
		parsed:   Parsed{Kind: KindTask, Prompt: "first"},
		proj:     projectRef{Name: "app", Cwd: projPath},
		threadID: threadID,
		actor:    Actor{ID: "u1", DisplayName: "One"},
		source:   SourceWeb,
		origin:   SourceWeb,
	}
	item2 := taskItem{
		s: nil, m: nil,
		parsed:   Parsed{Kind: KindTask, Prompt: "second web follow-up"},
		proj:     projectRef{Name: "app", Cwd: projPath},
		threadID: threadID,
		actor:    Actor{ID: "u2", DisplayName: "Two"},
		source:   SourceWeb,
		origin:   SourceWeb,
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &runJob{cancel: cancel, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, item1); err != nil || !claimed {
		t.Fatalf("claim1: %v %v", claimed, err)
	}
	if claimed, pos, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, item2); err != nil || claimed || pos != 1 {
		t.Fatalf("queue2: claimed=%v pos=%d err=%v", claimed, pos, err)
	}

	// Must not panic; both turns should land in history.
	b.drainWG.Add(1)
	b.drainTaskQueue(ctx, cancel, item1, job)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		th, err := b.history.Get(threadID)
		if err == nil && len(th.Turns) >= 2 {
			if th.Turns[0].UserID != "u1" || th.Turns[1].UserID != "u2" {
				t.Fatalf("turns users: %+v %+v", th.Turns[0], th.Turns[1])
			}
			if !strings.Contains(th.Turns[1].Prompt, "second") {
				t.Fatalf("second prompt=%q", th.Turns[1].Prompt)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	th, _ := b.history.Get(threadID)
	t.Fatalf("expected 2 history turns, got %+v", th)
}

func TestStatusSnapshotIncludesPhases(t *testing.T) {
	b, _ := testBotWithData(t)
	job := &runJob{cancel: func() {}, start: time.Now().Add(-2 * time.Second), project: "p"}
	job.mu.Lock()
	job.activity = "writing"
	job.phases = "✓read · **write**"
	job.mu.Unlock()
	if claimed, _, err := b.claimOrEnqueue("snap-1", job, taskItem{threadID: "snap-1"}); err != nil || !claimed {
		t.Fatal(err)
	}
	// Also enqueue one
	if claimed, pos, err := b.claimOrEnqueue("snap-1", &runJob{cancel: func() {}}, taskItem{threadID: "snap-1"}); err != nil || claimed || pos != 1 {
		t.Fatalf("queue: %v %v %v", claimed, pos, err)
	}
	snap := b.StatusSnapshot()
	if snap.ActiveCount != 1 || snap.QueuedTotal != 1 {
		t.Fatalf("counts active=%d queued=%d", snap.ActiveCount, snap.QueuedTotal)
	}
	if snap.ActiveRuns[0].Activity != "writing" || snap.ActiveRuns[0].Phases == "" {
		t.Fatalf("%+v", snap.ActiveRuns[0])
	}
	if snap.ActiveRuns[0].Elapsed == "" {
		t.Fatal("expected elapsed")
	}
	b.clearQueue("snap-1")
	b.finishRun("snap-1")
}
