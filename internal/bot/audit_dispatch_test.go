package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// auditCapsBot is testBotWithData with explicit membership and capability
// templates, which the button and gate tests both need: PathProjects leaves
// allowedUserIds empty, and an empty allowlist refuses every actor before any
// command runs (a different row, or none at all).
//
// u1 is a builder, u2 a member with no useful capability (draftCustomerReply only,
// so neither the freeform gate nor CanShip passes), admin1 a project admin.
func auditCapsBot(t *testing.T) (*Bot, string) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		GrokBin:   "false", // a run that starts must die immediately
		ClaudeBin: "false",
		Projects: config.ProjectsMap{"app": {
			Path:           proj,
			SafeTeamMode:   new(true),
			AllowedUserIDs: []string{"u1", "u2", "admin1"},
			// "replier" is deliberately not a builtin: every builtin except this
			// shape grants Investigate, which passes the freeform gate.
			CapabilityTemplates: map[string]config.Capabilities{
				"replier": {DraftCustomerReply: true},
			},
			CapabilityByUser: map[string]string{
				"u1": "builder", "u2": "replier", "admin1": "admin",
			},
		}},
		Channels:          map[string]string{"ch1": "app"},
		DataDir:           filepath.Join(dir, "data"),
		ConfigPath:        filepath.Join(dir, "config.json"),
		WorktreeIsolation: new(false),
		MaxTurns:          5,
		TimeoutMs:         5000,
		Yolo:              new(true),
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
	t.Cleanup(func() { WaitIdleForTest(b, 10*time.Second) })
	return b, proj
}

// onlyEvent asserts the log holds exactly one row and returns it. Exactness is the
// assertion that matters for a dispatch: a second row means the same run was
// claimed twice, and one of the two is a lie about whether it happened.
func onlyEvent(t *testing.T, b *Bot) audit.Event {
	t.Helper()
	evs, raw := readAuditEvents(t, b)
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 audit event, got %d:\n%s", len(evs), raw)
	}
	return evs[0]
}

// A refused task is a refused privileged operation: the whole point of the log is
// that an operator can see it. Before this, handleTask's capability gate was the
// one Discord denial that left nothing behind.
func TestDiscordTaskCapabilityDenialIsAudited(t *testing.T) {
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)

	b.handleTask(s, auditTestMessage("th-1", "u2", "<@botid> do the thing"), Parsed{
		Kind: KindTask, Prompt: "do the thing",
	})

	ev := onlyEvent(t, b)
	if ev.Action != audit.ActionSessionStart {
		t.Fatalf("Action = %q, want %q", ev.Action, audit.ActionSessionStart)
	}
	if ev.Actor != "u2" {
		t.Fatalf("Actor = %q, want u2", ev.Actor)
	}
	if ev.OK {
		t.Fatal("a refused task must not be recorded as a started run")
	}
	if !strings.Contains(ev.Error, "capability") {
		t.Fatalf("Error = %q, want the capability denial", ev.Error)
	}
	if got := detailString(t, ev, "origin"); got != auditOriginDiscord {
		t.Fatalf("detail.origin = %q, want %q", got, auditOriginDiscord)
	}
	if got := detailString(t, ev, "kind"); got != "task" {
		t.Fatalf("detail.kind = %q, want task", got)
	}
	if got := detailString(t, ev, "threadId"); got != "th-1" {
		t.Fatalf("detail.threadId = %q, want th-1", got)
	}
	// Nothing ran: the row is not merely mislabelled, it describes a refusal.
	if _, busy := b.getJob("th-1"); busy {
		t.Fatal("denied task still claimed the thread")
	}
}

// `/start fix` commits, pushes and can open a PR — a wider blast radius than the
// /fix-ci that was already audited, and parsed a few lines away from it. Its
// refusal and its kind both have to reach the log.
func TestDiscordStartFixDenialIsAuditedWithKind(t *testing.T) {
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)
	// u2 has no CanShip; the thread is not a case, so there is no escalate path.
	b.handleTask(s, auditTestMessage("th-1", "u2", "<@botid> /start fix it"), Parsed{
		Kind: KindStartFix, Prompt: "fix it", Arg: "fix",
	})

	ev := onlyEvent(t, b)
	if ev.OK || ev.Action != audit.ActionSessionStart {
		t.Fatalf("want a refused session.start, got %+v", ev)
	}
	if got := detailString(t, ev, "kind"); got != "fix" {
		t.Fatalf("detail.kind = %q, want fix — an investigate and a fix are not the same refusal", got)
	}
}

// The successful half: a run that really was enqueued is recorded once, with the
// affordance that started it. Without this, only refusals would be visible.
func TestDiscordTaskStartIsAuditedOnce(t *testing.T) {
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)

	b.handleTask(s, auditTestMessage("th-1", "u1", "<@botid> do the thing"), Parsed{
		Kind: KindTask, Prompt: "do the thing",
	})
	WaitIdleForTest(b, 10*time.Second)

	ev := onlyEvent(t, b)
	if ev.Action != audit.ActionSessionStart || !ev.OK || ev.Error != "" {
		t.Fatalf("want one successful session.start, got %+v", ev)
	}
	if ev.Actor != "u1" {
		t.Fatalf("Actor = %q, want u1", ev.Actor)
	}
	if got := detailString(t, ev, "origin"); got != auditOriginDiscord {
		t.Fatalf("detail.origin = %q, want %q", got, auditOriginDiscord)
	}
	if got := detailString(t, ev, "project"); got != "app" {
		t.Fatalf("detail.project = %q, want app", got)
	}
	// The prompt is the user's (or a customer's) prose and never gets written.
	_, raw := readAuditEvents(t, b)
	if strings.Contains(raw, "do the thing") {
		t.Fatalf("audit log leaked the prompt:\n%s", raw)
	}
}

// /fix-ci used to append ok=true *before* handleTask, whose gate could then refuse
// the run — a row asserting a model run that never started, and no row for the
// refusal that did happen. The origin still has to survive the move.
func TestDiscordFixCIOriginAuditedAtTheGateNotBeforeIt(t *testing.T) {
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)

	b.handleTaskOrigin(s, auditTestMessage("th-1", "u2", "<@botid> /fix-ci"), Parsed{
		Kind: KindTask, Prompt: "CI failed on pull request acme/app#7",
	}, auditOriginFixCI, map[string]any{"prs": 2})

	ev := onlyEvent(t, b)
	if ev.OK {
		t.Fatal("the gate refused the run; the row must not claim one started")
	}
	if got := detailString(t, ev, "origin"); got != auditOriginFixCI {
		t.Fatalf("detail.origin = %q, want %q", got, auditOriginFixCI)
	}
	if got, ok := ev.Detail["prs"]; !ok || got.(float64) != 2 {
		t.Fatalf("detail.prs = %v, want the failing-PR count", ev.Detail["prs"])
	}
}

// auditInteraction builds a component click by u on a thread.
func auditInteraction(customID, channelID, userID string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:        "ix-1",
		Type:      discordgo.InteractionMessageComponent,
		ChannelID: channelID,
		GuildID:   "g1",
		Member:    &discordgo.Member{User: &discordgo.User{ID: userID, Username: "user" + userID}},
		Data: discordgo.MessageComponentInteractionData{
			CustomID:      customID,
			ComponentType: discordgo.ButtonComponent,
		},
	}}
}

// Every completion card renders a Reset button, so this — not `@Grok /reset` — is
// where most resets come from. "Who reset that thread" has to be answerable for the
// affordance the bot itself puts in front of people.
func TestDiscordResetButtonWritesAuditEvent(t *testing.T) {
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)
	if err := b.sessions.Set("th-1", sessionstore.Entry{
		Project: "app", OwnerID: "u1", OwnerName: "user u1",
	}); err != nil {
		t.Fatal(err)
	}

	b.onInteraction(s, auditInteraction(actionCustomID(actionResetOK, "th-1"), "th-1", "u1"))

	ev := onlyEvent(t, b)
	if ev.Action != audit.ActionSessionReset {
		t.Fatalf("Action = %q, want %q", ev.Action, audit.ActionSessionReset)
	}
	if ev.Actor != "u1" || !ev.OK || ev.Error != "" {
		t.Fatalf("want a successful reset by u1, got %+v", ev)
	}
	if got := detailString(t, ev, "via"); got != "button" {
		t.Fatalf("detail.via = %q, want button — the row has to say which surface", got)
	}
	if got := detailString(t, ev, "threadId"); got != "th-1" {
		t.Fatalf("detail.threadId = %q, want th-1", got)
	}
	if got := detailString(t, ev, "project"); got != "app" {
		t.Fatalf("detail.project = %q, want app", got)
	}
	// The reset really happened, so the row is a record and not a claim.
	if e, ok := b.sessions.Get("th-1"); !ok || e.EffectiveLabel() != sessionstore.LabelAbandoned {
		t.Fatalf("reset did not abandon the session: %+v", e)
	}
}

// The button path is gated on the same ownership rule as /cancel, so its refusal is
// the same row — otherwise a denial is visible only when the user typed it.
func TestDiscordCancelButtonDeniedIsAudited(t *testing.T) {
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)
	// Owned by u1; u2 is neither owner, co-owner, nor a project admin.
	if err := b.sessions.Set("th-1", sessionstore.Entry{
		Project: "app", OwnerID: "u1", OwnerName: "user u1",
	}); err != nil {
		t.Fatal(err)
	}

	b.onInteraction(s, auditInteraction(actionCustomID(actionCancel, "th-1"), "th-1", "u2"))

	ev := onlyEvent(t, b)
	if ev.Action != audit.ActionSessionCancel {
		t.Fatalf("Action = %q, want %q", ev.Action, audit.ActionSessionCancel)
	}
	if ev.Actor != "u2" || ev.OK {
		t.Fatalf("want a denial by u2, got %+v", ev)
	}
	if !strings.Contains(ev.Error, "forbidden") {
		t.Fatalf("Error = %q, want the denial reason", ev.Error)
	}
	if got := detailString(t, ev, "via"); got != "button" {
		t.Fatalf("detail.via = %q, want button", got)
	}
}

// A Reset click by someone without control is turned away at the confirm *prompt*
// (they never see Yes), so that is where the refusal has to be recorded.
func TestDiscordResetButtonPromptDenialIsAudited(t *testing.T) {
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)
	if err := b.sessions.Set("th-1", sessionstore.Entry{
		Project: "app", OwnerID: "u1", OwnerName: "user u1",
	}); err != nil {
		t.Fatal(err)
	}

	b.onInteraction(s, auditInteraction(actionCustomID(actionReset, "th-1"), "th-1", "u2"))

	ev := onlyEvent(t, b)
	if ev.Action != audit.ActionSessionReset || ev.Actor != "u2" || ev.OK {
		t.Fatalf("want a reset denial by u2, got %+v", ev)
	}
	// The session survived, so the row must not read as a reset that happened.
	if e, ok := b.sessions.Get("th-1"); !ok || e.EffectiveLabel() == sessionstore.LabelAbandoned {
		t.Fatalf("denied reset still abandoned the session: %+v", e)
	}
}

// A decision-card click queues a model run exactly like a typed follow-up, so it
// is a session.start too — the option labels are model-authored prose and only the
// question id may be recorded.
func TestDiscordDecisionClickAuditsSessionStart(t *testing.T) {
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)
	if err := b.sessions.Set("th-1", sessionstore.Entry{
		Project: "app", OwnerID: "u1", OwnerName: "user u1",
		OpenQuestions: []sessionstore.OpenQuestion{{
			ID: "q1", Text: "Bump the timeout?", Status: "open",
			Options: []string{"CUSTOMER-OPTION-PROSE", "No"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	b.onInteraction(s, auditInteraction(decisionPrefix+"th-1:q1:0", "th-1", "u1"))
	WaitIdleForTest(b, 10*time.Second)

	ev := onlyEvent(t, b)
	if ev.Action != audit.ActionSessionStart || ev.Actor != "u1" || !ev.OK {
		t.Fatalf("want a successful session.start by u1, got %+v", ev)
	}
	if got := detailString(t, ev, "origin"); got != auditOriginDecision {
		t.Fatalf("detail.origin = %q, want %q", got, auditOriginDecision)
	}
	if got := detailString(t, ev, "questionId"); got != "q1" {
		t.Fatalf("detail.questionId = %q, want q1", got)
	}
	_, raw := readAuditEvents(t, b)
	if strings.Contains(raw, "CUSTOMER-OPTION-PROSE") {
		t.Fatalf("audit log leaked the option label:\n%s", raw)
	}
}

// writeFakeGH puts a `gh` on PATH that answers only the unresolved-review-threads
// GraphQL query. Everything else fails, which is what the caller expects of the
// best-effort PR-detail read.
func writeFakeGH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	const body = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[` +
		`{"isResolved":false,"isOutdated":false,"path":"a.go","line":3,"comments":{"nodes":[` +
		`{"body":"tighten this","url":"https://github.com/acme/app/pull/7","author":{"login":"rev1"}}]}}]}}}}}`
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"api\" ] && [ \"$2\" = \"graphql\" ]; then\n" +
		"  cat <<'JSON'\n" + body + "\nJSON\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"fake gh: unsupported: $*\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// `/address` dispatches an agent at a real PR: it edits the branch, commits and
// pushes. Its web twin (internal/web/address.go) has always written session.start
// with kind=address_review; in Discord it wrote nothing, so the same operation was
// auditable in the browser and invisible in chat.
func TestDiscordAddressAuditsDispatch(t *testing.T) {
	writeFakeGH(t)
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)
	e := sessionstore.Entry{Project: "app", OwnerID: "u1", OwnerName: "user u1"}
	e.UpsertPR(sessionstore.TrackedPR{
		Owner: "acme", Repo: "app", Number: 7, State: "OPEN",
		URL: "https://github.com/acme/app/pull/7",
	})
	if err := b.sessions.Set("th-1", e); err != nil {
		t.Fatal(err)
	}

	b.handleAddress(s, auditTestMessage("th-1", "u1", "<@botid> /address"), Parsed{Kind: KindAddress})
	WaitIdleForTest(b, 10*time.Second)

	ev := onlyEvent(t, b)
	if ev.Action != audit.ActionSessionStart || !ev.OK || ev.Error != "" {
		t.Fatalf("want one successful session.start, got %+v", ev)
	}
	if ev.Actor != "u1" {
		t.Fatalf("Actor = %q, want u1", ev.Actor)
	}
	if got := detailString(t, ev, "kind"); got != "address_review" {
		t.Fatalf("detail.kind = %q, want the web rail's address_review", got)
	}
	if got := detailString(t, ev, "origin"); got != auditOriginAddress {
		t.Fatalf("detail.origin = %q, want %q", got, auditOriginAddress)
	}
	if got := detailString(t, ev, "pr"); got != "acme/app#7" {
		t.Fatalf("detail.pr = %q, want acme/app#7", got)
	}
	// Reviewers' prose stays out; the counts are the record.
	_, raw := readAuditEvents(t, b)
	if strings.Contains(raw, "tighten this") {
		t.Fatalf("audit log leaked a review comment body:\n%s", raw)
	}
}

// The /address refusal is a refused privileged operation like any other, and the
// commit that audits this surface claims every one of them appends ok=false.
func TestDiscordAddressDenialIsAudited(t *testing.T) {
	b, _ := auditCapsBot(t)
	s := auditTestSession(t)
	e := sessionstore.Entry{Project: "app", OwnerID: "u1", OwnerName: "user u1"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 7, State: "OPEN"})
	if err := b.sessions.Set("th-1", e); err != nil {
		t.Fatal(err)
	}

	// u2 is a member but neither builder-class nor in control of this thread.
	b.handleAddress(s, auditTestMessage("th-1", "u2", "<@botid> /address"), Parsed{Kind: KindAddress})

	ev := onlyEvent(t, b)
	if ev.Action != audit.ActionSessionStart || ev.Actor != "u2" || ev.OK {
		t.Fatalf("want a refused session.start by u2, got %+v", ev)
	}
	if !strings.Contains(ev.Error, "capability") {
		t.Fatalf("Error = %q, want the capability denial", ev.Error)
	}
	if got := detailString(t, ev, "origin"); got != auditOriginAddress {
		t.Fatalf("detail.origin = %q, want %q", got, auditOriginAddress)
	}
}
