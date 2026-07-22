package bot

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

func TestWebTaskKindMapping(t *testing.T) {
	cases := map[string]Kind{
		"":              KindTask,
		"fix":           KindStartFix,
		"FIX":           KindStartFix,
		" investigate ": KindStartInvestigate,
		"investigate":   KindStartInvestigate,
		"explain":       KindStartExplain,
		"nonsense":      KindTask,
	}
	for in, want := range cases {
		if got := webTaskKind(in); got != want {
			t.Fatalf("webTaskKind(%q)=%d want %d", in, got, want)
		}
	}
}

func TestStartWebTaskDiscordCreate(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextMsg: "m1", nextTh: "th-webtask"}
	SetThreadAPIForTest(b, fake)

	res, err := b.StartWebTask(StartWebTaskOpts{
		Project: "app",
		Prompt:  "add rate limiting to the login endpoint",
		Actor:   Actor{ID: "u1", DisplayName: "Alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.ThreadID != "th-webtask" {
		t.Fatalf("%+v", res)
	}
	if len(fake.starts) != 1 || len(fake.sends) != 1 {
		t.Fatalf("starts=%v sends=%v", fake.starts, fake.sends)
	}
	if !strings.Contains(fake.sends[0], "task") || !strings.Contains(fake.sends[0], "Alice") || !strings.Contains(fake.sends[0], "(web)") {
		t.Fatalf("starter=%v", fake.sends)
	}
	e, ok := b.sessions.Get("th-webtask")
	if !ok {
		t.Fatal("session missing")
	}
	// Owner stamp is critical: the creator must be able to cancel/reset.
	if e.OwnerID != "u1" {
		t.Fatalf("owner=%q want u1 (%+v)", e.OwnerID, e)
	}
	if e.Origin != SourceWeb || e.CreatedBy != "u1" {
		t.Fatalf("%+v", e)
	}
	if e.DiscordURL == "" || !strings.Contains(e.DiscordURL, "guild-1") {
		t.Fatalf("discordURL=%q", e.DiscordURL)
	}
	if !strings.Contains(e.Goal, "rate limiting") {
		t.Fatalf("goal=%q", e.Goal)
	}
	waitHistory(t, b, "th-webtask", 1)
}

func TestStartWebTaskThreadCreateFailFallsBackWebNative(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	SetThreadAPIForTest(b, &fakeThreadAPI{failStart: fmt.Errorf("discord api outage")})

	res, err := b.StartWebTask(StartWebTaskOpts{
		Project: "app",
		Prompt:  "investigate flaky test",
		Actor:   Actor{ID: "u2", DisplayName: "Bob"},
	})
	if err != nil {
		t.Fatalf("expected web-native fallback, got %v", err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("want web-native unit, got %+v", res)
	}
	if res.DiscordURL != "" {
		t.Fatalf("web-native should not have Discord URL: %+v", res)
	}
	// Broken promise: the start page advertised a Discord destination but the
	// thread create failed, so the session page must surface discord=offline.
	if !res.DiscordOffline {
		t.Fatalf("thread-create failure fallback must flag DiscordOffline: %+v", res)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.OwnerID != "u2" {
		t.Fatalf("owner not stamped on fallback: %+v", e)
	}
	if !strings.HasPrefix(e.WorktreeBranch, gitworktree.WebBranchPrefix) {
		t.Fatalf("branch=%q want web prefix", e.WorktreeBranch)
	}
	waitHistory(t, b, res.ThreadID, 1)
}

func TestStartWebTaskNoChannelMappedFallsBackWebNative(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	// Gateway available (threadAPI) but the project has no mapped channel.
	// Unlike StartCommitReview, a freeform web start must still succeed web-native.
	SetThreadAPIForTest(b, &fakeThreadAPI{nextTh: "should-not-create"})
	b.cfg.Channels = map[string]string{}
	pc := b.cfg.Projects["app"]
	pc.DiscordChannelID = ""
	b.cfg.Projects["app"] = pc

	res, err := b.StartWebTask(StartWebTaskOpts{
		Project: "app",
		Prompt:  "write a design doc",
		Title:   "design doc",
		Actor:   Actor{ID: "u3", DisplayName: "Cara"},
	})
	if err != nil {
		t.Fatalf("no-channel web start should succeed web-native: %v", err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("want web-native unit, got %+v", res)
	}
	if res.DiscordURL != "" {
		t.Fatalf("web-native should not have Discord URL: %+v", res)
	}
	// No mapped channel: the page already showed "web-native", so no broken promise.
	if res.DiscordOffline {
		t.Fatalf("no-channel fallback must not flag DiscordOffline: %+v", res)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.OwnerID != "u3" {
		t.Fatalf("owner not stamped: %+v", e)
	}
	// The optional Title becomes the session goal (not just a would-be thread name).
	if e.Goal != "design doc" {
		t.Fatalf("title should become goal: goal=%q", e.Goal)
	}
	waitHistory(t, b, res.ThreadID, 1)
}

func TestStartWebTaskEmptyPrompt(t *testing.T) {
	b, _ := testFixBot(t)
	_, err := b.StartWebTask(StartWebTaskOpts{
		Project: "app",
		Prompt:  "   ",
		Actor:   Actor{ID: "u", DisplayName: "U"},
	})
	if !errors.Is(err, ErrEmptyPrompt) {
		t.Fatalf("err=%v want ErrEmptyPrompt", err)
	}
}

// A web-originated investigate task must never open a PR — same policy path as
// Discord "/start investigate" (mode select → KindStartInvestigate → snapshot).
func TestStartWebTaskInvestigateNonShip(t *testing.T) {
	yolo := true
	cfg := &config.Config{
		Yolo: &yolo,
		Projects: config.ProjectsMap{
			"app": {
				Path:           t.TempDir(),
				AllowedUserIDs: []string{"builder1"},
			},
		},
	}
	b := &Bot{cfg: cfg}
	item := taskItem{
		parsed:   Parsed{Kind: webTaskKind("investigate"), Prompt: "why is checkout slow"},
		threadID: "th-web-inv",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "builder1", DisplayName: "Builder"},
	}
	b.snapshotPolicyOntoItem(&item, "app", nil)

	if item.snapMode != ModeInvestigate {
		t.Fatalf("snapMode=%q want %q", item.snapMode, ModeInvestigate)
	}
	if item.snapAllowPR || item.snapAllowDirect {
		t.Fatalf("investigate must not ship: allowPR=%v allowDirect=%v", item.snapAllowPR, item.snapAllowDirect)
	}
	pol := b.resolveRunPolicy("th-web-inv", "app", item, "pr", item.actor)
	if pol.AllowPR || pol.AllowDirectShip || pol.AllowDirectIntegrate {
		t.Fatalf("resolve must not ship: %+v", pol)
	}
}

// Sanity: the shared bind renamed cleanly and commit review still owner-stamps.
func TestBindWebStartedSessionStampsOwner(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.bindWebStartedSession("bind-1", "app", "goal text", Actor{ID: "o1", DisplayName: "Owner"}, "https://x/guild-1/bind-1", true); err != nil {
		t.Fatal(err)
	}
	e, ok := b.sessions.Get("bind-1")
	if !ok {
		t.Fatal("missing")
	}
	if e.OwnerID != "o1" || e.Origin != SourceWeb || e.Goal != "goal text" || e.DiscordURL == "" {
		t.Fatalf("%+v", e)
	}
}
