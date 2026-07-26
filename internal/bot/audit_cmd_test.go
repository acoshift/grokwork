package bot

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// auditStubTransport answers every Discord REST call with a minimal 200 so the
// real command handlers can run end to end without touching the network.
//
// One body serves every call: the fields a Message needs and the fields a Channel
// needs do not collide, and parentChannelID resolves a thread's parent over REST
// (not from State), so parent_id has to be here or every handler that resolves a
// project from the channel map bails out before doing anything.
type auditStubTransport struct{}

func (auditStubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	const body = `{"id":"stub-1","channel_id":"th-1","parent_id":"ch1","guild_id":"g1","type":11}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// auditTestSession is a gateway-less session whose State already knows one text
// channel (ch1, mapped to project app) and two threads under it.
func auditTestSession(t *testing.T) *discordgo.Session {
	t.Helper()
	s, err := discordgo.New("Bot fake-token")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: auditStubTransport{}}
	s.State.User = &discordgo.User{ID: "botid", Bot: true}
	if err := s.State.GuildAdd(&discordgo.Guild{ID: "g1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.State.ChannelAdd(&discordgo.Channel{
		ID: "ch1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText,
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"th-1", "th-2"} {
		if err := s.State.ChannelAdd(&discordgo.Channel{
			ID: id, GuildID: "g1", ParentID: "ch1", Type: discordgo.ChannelTypeGuildPublicThread,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func auditTestMessage(channelID, userID, content string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:        "msg-" + userID,
		ChannelID: channelID,
		GuildID:   "g1",
		Author:    &discordgo.User{ID: userID, Username: "user" + userID},
		Content:   content,
		Mentions:  []*discordgo.User{{ID: "botid"}},
	}}
}

// readAuditEvents parses every day file, so a run straddling UTC midnight cannot
// make an assertion silently pass on an empty slice.
func readAuditEvents(t *testing.T, b *Bot) ([]audit.Event, string) {
	t.Helper()
	if b.audit == nil {
		t.Fatal("bot has no audit logger — New() should have built one from cfg.DataDir")
	}
	files, err := filepath.Glob(filepath.Join(b.audit.Dir(), "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var evs []audit.Event
	var raw strings.Builder
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(body)
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ev audit.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("audit line %q: %v", line, err)
			}
			evs = append(evs, ev)
		}
	}
	return evs, raw.String()
}

func detailString(t *testing.T, ev audit.Event, key string) string {
	t.Helper()
	v, ok := ev.Detail[key]
	if !ok {
		t.Fatalf("audit %s: detail has no %q: %v", ev.Action, key, ev.Detail)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("audit %s: detail[%q] = %T, want string", ev.Action, key, v)
	}
	return s
}

// A Discord /reset is recorded with the actor and the thread it acted on — the
// question "who reset that thread" had no answer at all before this.
func TestDiscordResetWritesAuditEvent(t *testing.T) {
	b, _ := testBotWithData(t)
	s := auditTestSession(t)
	if err := b.sessions.Set("th-1", sessionstore.Entry{
		Project: "app", OwnerID: "u1", OwnerName: "user u1",
	}); err != nil {
		t.Fatal(err)
	}

	b.resetThread(s, auditTestMessage("th-1", "u1", "<@botid> /reset"))

	evs, _ := readAuditEvents(t, b)
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 audit event, got %d: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Action != audit.ActionSessionReset {
		t.Fatalf("Action = %q, want %q", ev.Action, audit.ActionSessionReset)
	}
	if ev.Actor != "u1" {
		t.Fatalf("Actor = %q, want the Discord user id u1", ev.Actor)
	}
	if !ev.OK || ev.Error != "" {
		t.Fatalf("want a successful event, got ok=%v err=%q", ev.OK, ev.Error)
	}
	if got := detailString(t, ev, "threadId"); got != "th-1" {
		t.Fatalf("detail.threadId = %q, want th-1", got)
	}
	if got := detailString(t, ev, "project"); got != "app" {
		t.Fatalf("detail.project = %q, want app", got)
	}
	if got := detailString(t, ev, "source"); got != SourceDiscord {
		t.Fatalf("detail.source = %q, want %q", got, SourceDiscord)
	}
	if ev.Time.IsZero() {
		t.Fatal("event has no timestamp")
	}
	// The reset really happened — the audit row is not a claim about nothing.
	if e, ok := b.sessions.Get("th-1"); !ok || e.EffectiveLabel() != sessionstore.LabelAbandoned {
		t.Fatalf("reset did not abandon the session: %+v", e)
	}
}

// A refused command is the row an operator actually goes looking for, so a denial
// is recorded with OK=false and the reason — not dropped as "nothing happened".
func TestDiscordCancelDeniedWritesDenialEvent(t *testing.T) {
	b, _ := testBotWithData(t)
	s := auditTestSession(t)
	// Owned by u1; u2 is neither owner, co-owner, nor a project admin.
	if err := b.sessions.Set("th-1", sessionstore.Entry{
		Project: "app", OwnerID: "u1", OwnerName: "user u1",
	}); err != nil {
		t.Fatal(err)
	}

	b.handleCancel(s, auditTestMessage("th-1", "u2", "<@botid> /cancel"))

	evs, _ := readAuditEvents(t, b)
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 audit event, got %d: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Action != audit.ActionSessionCancel {
		t.Fatalf("Action = %q, want %q", ev.Action, audit.ActionSessionCancel)
	}
	if ev.Actor != "u2" {
		t.Fatalf("Actor = %q, want the denied user u2", ev.Actor)
	}
	if ev.OK {
		t.Fatal("a denied /cancel must not be recorded as ok")
	}
	if !strings.Contains(ev.Error, "forbidden") {
		t.Fatalf("Error = %q, want the denial reason", ev.Error)
	}
	if got := detailString(t, ev, "threadId"); got != "th-1" {
		t.Fatalf("detail.threadId = %q, want th-1", got)
	}
}

// An idle /cancel by someone allowed to run it is a different row from a denial:
// both are OK=false, so the reason has to distinguish them.
func TestDiscordCancelIdleRecordsReasonNotDenial(t *testing.T) {
	b, _ := testBotWithData(t)
	s := auditTestSession(t)
	if err := b.sessions.Set("th-1", sessionstore.Entry{
		Project: "app", OwnerID: "u1", OwnerName: "user u1",
	}); err != nil {
		t.Fatal(err)
	}

	b.handleCancel(s, auditTestMessage("th-1", "u1", "<@botid> /cancel"))

	evs, _ := readAuditEvents(t, b)
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 audit event, got %d: %+v", len(evs), evs)
	}
	if evs[0].OK {
		t.Fatal("cancelling an idle thread cancelled nothing; must not be ok")
	}
	if strings.Contains(evs[0].Error, "forbidden") {
		t.Fatalf("idle cancel recorded as a denial: %q", evs[0].Error)
	}
	if !strings.Contains(evs[0].Error, "No run in progress") {
		t.Fatalf("Error = %q, want the idle reason", evs[0].Error)
	}
}

// A non-member is refused before the command is even parsed, so without this row
// their /reset attempt would leave no trace anywhere.
func TestDiscordAccessDenyIsAudited(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		// Membership is explicit here (PathProjects leaves it empty, which is a
		// different refusal: "project has no members").
		Projects:   config.ProjectsMap{"app": {Path: proj, AllowedUserIDs: []string{"u1"}}},
		Channels:   map[string]string{"ch1": "app"},
		DataDir:    filepath.Join(dir, "data"),
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(cfg, store, hist)
	s := auditTestSession(t)

	b.onMessage(s, auditTestMessage("th-1", "intruder", "<@botid> /reset"))

	evs, _ := readAuditEvents(t, b)
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 audit event, got %d: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Action != audit.ActionAccessDeny {
		t.Fatalf("Action = %q, want %q", ev.Action, audit.ActionAccessDeny)
	}
	if ev.Actor != "intruder" || ev.OK {
		t.Fatalf("Actor = %q ok = %v, want the refused user and ok=false", ev.Actor, ev.OK)
	}
	if got := detailString(t, ev, "project"); got != "app" {
		t.Fatalf("detail.project = %q, want app", got)
	}
}

// Nothing the audit log keeps may carry a local filesystem path or the text of a
// command — a path is private infrastructure and command text can be a customer's
// words. Subprocess stderr is the dangerous case: git names the directory it
// could not enter, and nobody controls that wording.
func TestDiscordAuditCarriesNoPathsOrPromptText(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.ProjectsMap{"app": {
			Path:           proj,
			AllowedUserIDs: []string{"u1"},
			VerifyCommands: []config.VerifyCommand{{
				Name: "pwd", Command: "pwd && echo /Users/leaked/from/verify", TimeoutMs: 20_000,
			}},
		}},
		Channels:   map[string]string{"ch1": "app"},
		DataDir:    filepath.Join(dir, "data"),
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(cfg, store, hist)
	s := auditTestSession(t)

	const marker = "CUSTOMER-PROSE-MARKER-9f3"
	// A worktree cwd that does not exist, so git fails with the absolute path in
	// its stderr — the exact shape that would leak through an error string.
	missing := filepath.Join(dir, "gone", "worktree")
	if err := b.sessions.Set("th-1", sessionstore.Entry{
		Project: "app", OwnerID: "u1", OwnerName: "user u1",
		Mode: ModeCase, Phase: sessionstore.PhaseInvestigate, CaseKey: "APP-1",
		Cwd: missing, WorktreeBranch: "grokwork/th-1",
	}); err != nil {
		t.Fatal(err)
	}

	m := func(content string) *discordgo.MessageCreate {
		return auditTestMessage("th-1", "u1", content)
	}
	b.handleCheckpoint(s, m("<@botid> /checkpoint "+marker), Parsed{
		Kind: KindCheckpoint, Prompt: "/checkpoint " + marker,
	})
	b.handleCustomerUpdate(s, m("<@botid> /customer-update x"), Parsed{
		Kind: KindCustomerUpdate, Prompt: "/customer-update " + marker + " see /Users/victim/app/log.txt",
	})
	b.handleLink(s, m("<@botid> /link #42"), Parsed{Kind: KindLink, Prompt: "/link #42"})
	b.handleVerify(s, m("<@botid> /verify"), Parsed{Kind: KindVerify, Prompt: "/verify"})
	b.handleCase(s, m("<@botid> /case high "+marker), Parsed{
		Kind: KindCase, Prompt: "/case high " + marker,
	})

	evs, raw := readAuditEvents(t, b)
	if len(evs) < 5 {
		t.Fatalf("expected an event per command, got %d: %+v", len(evs), evs)
	}
	for _, forbidden := range []string{
		marker,           // command text / customer prose
		dir,              // the test's temp root — covers cwd and worktree paths
		missing,          // the cwd git will have named in its stderr
		"/Users/",        // any absolute home path, wherever it came from
		"data/worktrees", // managed worktree paths are paths too
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("audit log leaked %q:\n%s", forbidden, raw)
		}
	}
	// The checkpoint really did fail on that path, so the scrubber (not luck) is
	// what kept it out.
	var sawScrubbedCheckpoint bool
	for _, ev := range evs {
		if ev.Action == audit.ActionGitCheckpoint && !ev.OK && strings.Contains(ev.Error, "[path]") {
			sawScrubbedCheckpoint = true
		}
	}
	if !sawScrubbedCheckpoint {
		t.Fatalf("expected a failed git.checkpoint whose error had a path scrubbed: %+v", evs)
	}
}

// Case actions are the workflow both surfaces drive, so a Discord /escalate must
// land under the same action name the web rail writes — one reader, one query.
func TestDiscordEscalateAuditMatchesWebActionName(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	on := true
	cfg := &config.Config{
		Projects: config.ProjectsMap{"app": {
			Path:           proj,
			SafeTeamMode:   &on,
			AllowedUserIDs: []string{"eng1", "ops1"},
			// operator = Investigate only: no fileEscalation, no builder caps.
			CapabilityByUser: map[string]string{"eng1": "builder", "ops1": "operator"},
		}},
		Channels:   map[string]string{"ch1": "app"},
		DataDir:    filepath.Join(dir, "data"),
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(cfg, store, hist)
	s := auditTestSession(t)
	if err := b.sessions.Set("th-1", sessionstore.Entry{
		Project: "app", OwnerID: "ops1", CaseKey: "APP-7",
		Mode: ModeCase, Phase: sessionstore.PhaseInvestigate,
	}); err != nil {
		t.Fatal(err)
	}

	parsed := Parsed{Kind: KindEscalate, Prompt: "/escalate please own"}
	b.handleEscalate(s, auditTestMessage("th-1", "ops1", "<@botid> /escalate"), parsed)
	b.handleEscalate(s, auditTestMessage("th-1", "eng1", "<@botid> /escalate"), parsed)

	evs, _ := readAuditEvents(t, b)
	if len(evs) != 2 {
		t.Fatalf("want a denial and a success, got %d: %+v", len(evs), evs)
	}
	denied, ok2 := evs[0], evs[1]
	for _, ev := range evs {
		if ev.Action != audit.ActionCaseEscalate {
			t.Fatalf("Action = %q, want %q", ev.Action, audit.ActionCaseEscalate)
		}
		if got := detailString(t, ev, "caseKey"); got != "APP-7" {
			t.Fatalf("detail.caseKey = %q, want APP-7", got)
		}
	}
	if denied.OK || denied.Actor != "ops1" || !strings.Contains(denied.Error, "capability") {
		t.Fatalf("operator escalate should be a capability denial: %+v", denied)
	}
	if !ok2.OK || ok2.Actor != "eng1" {
		t.Fatalf("builder escalate should succeed: %+v", ok2)
	}
	if got := detailString(t, ok2, "engineerId"); got != "eng1" {
		t.Fatalf("detail.engineerId = %q, want eng1", got)
	}
	// phase is the phase the command found, not the one it left behind.
	if got := detailString(t, ok2, "phase"); got != sessionstore.PhaseInvestigate {
		t.Fatalf("detail.phase = %q, want the pre-escalation phase", got)
	}
}

func TestScrubAuditPaths(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"git rev-parse: fatal: cannot change to '/var/folders/x/y': nope",
			"git rev-parse: fatal: cannot change to '[path]': nope"},
		{"not a git repository:/Users/acoshift/Projects/app", "not a git repository:[path]"},
		{"removed data/worktrees/app/th-1", "removed [path]"},
		{`C:\Users\bob\app failed`, "[path] failed"},
		// Branch names and shas look nothing like paths and must survive.
		{"grokwork/th-1 at 0f9c1ab", "grokwork/th-1 at 0f9c1ab"},
	}
	for _, c := range cases {
		if got := scrubAuditPaths(c.in); got != c.want {
			t.Errorf("scrubAuditPaths(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
