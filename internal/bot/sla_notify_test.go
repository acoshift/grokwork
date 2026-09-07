package bot

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/inbox"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const slaOwnerSnowflake = "123456789012345678"

func slaStamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func newSLABot(t *testing.T) (*Bot, *sessionstore.Store, *inbox.Store) {
	t.Helper()
	dir := t.TempDir()
	sessions, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.ProjectsMap{"alpha": {
			Path: filepath.Join(dir, "alpha"),
			SLA: map[string]config.SLATarget{
				"critical": {FirstResponseMinutes: new(60), ResolutionMinutes: new(240)},
				"high":     {FirstResponseMinutes: new(60), ResolutionMinutes: new(60)},
			},
		}},
		DataDir: dir,
	}
	return &Bot{cfg: cfg, sessions: sessions, inbox: ib}, sessions, ib
}

func mustSetCase(t *testing.T, store *sessionstore.Store, id string, e sessionstore.Entry) {
	t.Helper()
	if e.Project == "" {
		e.Project = "alpha"
	}
	if e.Mode == "" {
		e.Mode = ModeCase
	}
	if err := store.Set(id, e); err != nil {
		t.Fatal(err)
	}
}

func inboxKinds(t *testing.T, store *inbox.Store, actor string) []inbox.Item {
	t.Helper()
	items, err := store.List(actor)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestSweepSLAInboxFirstResponseBreach(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mustSetCase(t, sessions, "c-late", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		CaseKey: "WEBAPP-14", CustomerTitle: "Checkout fails",
		OwnerID: "u-owner", EngineerID: "u-eng",
		CoOwnerIDs: []string{"u-co"}, WatcherIDs: []string{"u-watch"},
		OpenedAt:  slaStamp(now.Add(-90 * time.Minute)),
		UpdatedAt: "2026-07-27T09:00:00Z",
	})

	n := b.sweepSLAInboxAt(now, nil)
	if n != 4 {
		t.Fatalf("appended = %d want 4 (owner, eng, co, watch)", n)
	}
	wantURL := inboxSessionPath("c-late", "alpha")
	for _, actor := range []string{"u-owner", "u-eng", "u-co", "u-watch"} {
		items := inboxKinds(t, ib, actor)
		if len(items) != 1 {
			t.Fatalf("%s items = %d want 1", actor, len(items))
		}
		it := items[0]
		if it.Kind != inbox.KindSLABreached {
			t.Errorf("%s kind = %q", actor, it.Kind)
		}
		if !strings.Contains(it.Subject, "first response") {
			t.Errorf("%s subject %q missing first response", actor, it.Subject)
		}
		if strings.Contains(it.Subject, "resolution") {
			t.Errorf("%s subject %q should not name resolution", actor, it.Subject)
		}
		if !strings.Contains(it.Subject, "WEBAPP-14") {
			t.Errorf("%s subject %q missing case key", actor, it.Subject)
		}
		if it.URL != wantURL {
			t.Errorf("%s url = %q want %q", actor, it.URL, wantURL)
		}
		if it.UnitID != "c-late" || it.Project != "alpha" {
			t.Errorf("%s unit/project = %s/%s", actor, it.UnitID, it.Project)
		}
		if it.Body != "Checkout fails" {
			t.Errorf("%s body = %q", actor, it.Body)
		}
	}
	if got := inboxKinds(t, ib, "stranger"); len(got) != 0 {
		t.Fatalf("stranger got %d items", len(got))
	}

	if n = b.sweepSLAInboxAt(now, nil); n != 0 {
		t.Fatalf("second sweep appended %d", n)
	}

	e, ok := sessions.Get("c-late")
	if !ok {
		t.Fatal("missing case")
	}
	if e.UpdatedAt != "2026-07-27T09:00:00Z" {
		t.Fatalf("UpdatedAt changed: %q", e.UpdatedAt)
	}
	if e.SLAAlertedFirstResponse == "" {
		t.Fatal("first-response watermark not set")
	}
	if e.SLAAlertedResolution != "" {
		t.Fatalf("resolution watermark set: %q", e.SLAAlertedResolution)
	}
}

func TestSweepSLAInboxRestartDoesNotRedeliver(t *testing.T) {
	b, sessions, _ := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mustSetCase(t, sessions, "c-late", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OwnerID: "u-owner", OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	if n := b.sweepSLAInboxAt(now, nil); n != 1 {
		t.Fatalf("first sweep = %d", n)
	}

	sessions2, err := sessionstore.New(b.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	ib2, err := inbox.New(b.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	b2 := &Bot{cfg: b.cfg, sessions: sessions2, inbox: ib2}
	if n := b2.sweepSLAInboxAt(now, nil); n != 0 {
		t.Fatalf("restart sweep = %d", n)
	}
	if got := inboxKinds(t, ib2, "u-owner"); len(got) != 1 {
		t.Fatalf("restart items = %d", len(got))
	}
}

func TestSweepSLAInboxResolutionWhileFirstResponseStillBreached(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-5 * time.Hour)
	mustSetCase(t, sessions, "c-late", sessionstore.Entry{
		Phase: sessionstore.PhaseFixing, Severity: "critical",
		OwnerID: "u-owner", OpenedAt: slaStamp(opened),
		SLAAlertedFirstResponse: slaStamp(opened),
	})
	n := b.sweepSLAInboxAt(now, nil)
	if n != 1 {
		t.Fatalf("appended = %d want 1", n)
	}
	items := inboxKinds(t, ib, "u-owner")
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	if !strings.Contains(items[0].Subject, "resolution") {
		t.Errorf("subject = %q want resolution", items[0].Subject)
	}
	if strings.Contains(items[0].Subject, "first response") {
		t.Errorf("subject = %q should not name first response", items[0].Subject)
	}
}

func TestSweepSLAInboxBothClocksOneDiscord(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mustSetCase(t, sessions, "c-late", sessionstore.Entry{
		Phase: sessionstore.PhaseFixing, Severity: "high",
		CaseKey: "WEBAPP-9", OwnerID: slaOwnerSnowflake,
		OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	type sent struct {
		thread string
		body   string
		users  []string
	}
	var got []sent
	n := b.sweepSLAInboxAt(now, func(threadID, content string, userIDs []string) error {
		got = append(got, sent{threadID, content, slices.Clone(userIDs)})
		return nil
	})
	if n != 2 {
		t.Fatalf("appended = %d want 2", n)
	}
	items := inboxKinds(t, ib, slaOwnerSnowflake)
	if len(items) != 2 {
		t.Fatalf("items = %d want 2", len(items))
	}
	subjects := items[0].Subject + "\n" + items[1].Subject
	if !strings.Contains(subjects, "first response") || !strings.Contains(subjects, "resolution") {
		t.Fatalf("subjects = %q", subjects)
	}
	if len(got) != 1 {
		t.Fatalf("discord sends = %d want 1", len(got))
	}
	if got[0].thread != "c-late" {
		t.Errorf("thread = %q", got[0].thread)
	}
	if !strings.Contains(got[0].body, "<@"+slaOwnerSnowflake+">") {
		t.Errorf("content missing mention: %q", got[0].body)
	}
	if !strings.Contains(got[0].body, "first response") || !strings.Contains(got[0].body, "resolution") {
		t.Errorf("discord body = %q", got[0].body)
	}
	if !slices.Equal(got[0].users, []string{slaOwnerSnowflake}) {
		t.Errorf("Users = %v", got[0].users)
	}
	e, _ := sessions.Get("c-late")
	if e.SLAAlertedFirstResponse == "" || e.SLAAlertedResolution == "" {
		t.Fatalf("watermarks: fr=%q res=%q", e.SLAAlertedFirstResponse, e.SLAAlertedResolution)
	}
}

func TestSweepSLAInboxReopenNewRound(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mustSetCase(t, sessions, "c-late", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OwnerID: "u-owner", OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	if n := b.sweepSLAInboxAt(now, nil); n != 1 {
		t.Fatalf("first = %d", n)
	}
	reopened := now
	if _, _, err := sessions.Patch("c-late", func(e *sessionstore.Entry) {
		e.ReopenedAt = slaStamp(reopened)
		sessionstore.ResetCaseSLARound(e)
		e.Phase = sessionstore.PhaseInvestigate
	}); err != nil {
		t.Fatal(err)
	}
	if n := b.sweepSLAInboxAt(now, nil); n != 0 {
		t.Fatalf("just reopened still inside target: appended %d", n)
	}
	if n := b.sweepSLAInboxAt(now.Add(90*time.Minute), nil); n != 1 {
		t.Fatalf("new round breach = %d", n)
	}
	if got := inboxKinds(t, ib, "u-owner"); len(got) != 2 {
		t.Fatalf("items after reopen = %d want 2", len(got))
	}
}

func TestSweepSLAInboxSkipsClosedNoTargetNoRoundNotCase(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	old := slaStamp(now.Add(-5 * time.Hour))
	mustSetCase(t, sessions, "c-closed", sessionstore.Entry{
		Phase: sessionstore.PhaseClosed, Severity: "critical",
		OwnerID: "u-owner", OpenedAt: old,
	})
	mustSetCase(t, sessions, "c-low", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "low",
		OwnerID: "u-owner", OpenedAt: old,
	})
	mustSetCase(t, sessions, "c-legacy", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OwnerID: "u-owner",
	})
	if err := sessions.Set("eng-sess", sessionstore.Entry{
		Project: "alpha", OwnerID: "u-owner", OpenedAt: old, Severity: "critical",
	}); err != nil {
		t.Fatal(err)
	}
	if n := b.sweepSLAInboxAt(now, nil); n != 0 {
		t.Fatalf("appended = %d want 0", n)
	}
	if got := inboxKinds(t, ib, "u-owner"); len(got) != 0 {
		t.Fatalf("items = %d", len(got))
	}
}

func TestSweepSLAInboxWatcherAndEmptyInvolvement(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mustSetCase(t, sessions, "c-watch", sessionstore.Entry{
		Phase: sessionstore.PhaseFixing, Severity: "critical",
		OwnerID: "u-other", WatcherIDs: []string{"u-watch"},
		OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	mustSetCase(t, sessions, "c-nobody", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	n := b.sweepSLAInboxAt(now, nil)
	if n != 2 {
		t.Fatalf("appended = %d want 2 (owner+watcher on c-watch)", n)
	}
	if got := inboxKinds(t, ib, "u-watch"); len(got) != 1 {
		t.Fatalf("watcher items = %d", len(got))
	}
	if got := inboxKinds(t, ib, "allowlisted"); len(got) != 0 {
		t.Fatalf("stranger allowlisted got %d", len(got))
	}
	nobody, _ := sessions.Get("c-nobody")
	if nobody.SLAAlertedFirstResponse != "" {
		t.Fatalf("empty involvement stamped watermark %q", nobody.SLAAlertedFirstResponse)
	}

	if _, _, err := sessions.Patch("c-nobody", func(e *sessionstore.Entry) {
		e.OwnerID = "u-claimed"
	}); err != nil {
		t.Fatal(err)
	}
	if n = b.sweepSLAInboxAt(now, nil); n != 1 {
		t.Fatalf("after claim appended = %d", n)
	}
	if got := inboxKinds(t, ib, "u-claimed"); len(got) != 1 {
		t.Fatalf("claimed items = %d", len(got))
	}
}

func TestSweepSLAInboxNilInboxWritesNoReceipt(t *testing.T) {
	b, sessions, _ := newSLABot(t)
	b.inbox = nil
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mustSetCase(t, sessions, "c-late", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OwnerID: "u-owner", OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	if n := b.sweepSLAInboxAt(now, nil); n != 0 {
		t.Fatalf("appended = %d", n)
	}
	e, _ := sessions.Get("c-late")
	if e.SLAAlertedFirstResponse != "" {
		t.Fatalf("watermark = %q", e.SLAAlertedFirstResponse)
	}
}

func TestSweepSLAInboxPartialAppendStillReceipts(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mustSetCase(t, sessions, "c-late", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OwnerID: "u-owner", CoOwnerIDs: []string{"bad id!"},
		OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	if n := b.sweepSLAInboxAt(now, nil); n != 1 {
		t.Fatalf("appended = %d want 1 (good owner only)", n)
	}
	e, _ := sessions.Get("c-late")
	if e.SLAAlertedFirstResponse == "" {
		t.Fatal("partial success must stamp")
	}
	if got := inboxKinds(t, ib, "u-owner"); len(got) != 1 {
		t.Fatalf("owner items = %d", len(got))
	}

	mustSetCase(t, sessions, "c-allbad", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OwnerID: "bad id!", OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	if n := b.sweepSLAInboxAt(now, nil); n != 0 {
		t.Fatalf("all-fail appended = %d", n)
	}
	bad, _ := sessions.Get("c-allbad")
	if bad.SLAAlertedFirstResponse != "" {
		t.Fatalf("all-fail watermark = %q", bad.SLAAlertedFirstResponse)
	}
}

func TestSweepSLAInboxDiscordUsersSnowflakesOnly(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mustSetCase(t, sessions, "c-late", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OwnerID: slaOwnerSnowflake, CoOwnerIDs: []string{"google:alice"},
		OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	type sent struct {
		body  string
		users []string
	}
	var got []sent
	send := func(threadID, content string, userIDs []string) error {
		got = append(got, sent{content, slices.Clone(userIDs)})
		return nil
	}
	if n := b.sweepSLAInboxAt(now, send); n != 2 {
		t.Fatalf("appended = %d", n)
	}
	if len(got) != 1 {
		t.Fatalf("sends = %d", len(got))
	}
	if slices.Contains(got[0].users, "google:alice") {
		t.Fatalf("Users contains namespaced id: %v", got[0].users)
	}
	if !slices.Equal(got[0].users, []string{slaOwnerSnowflake}) {
		t.Errorf("Users = %v", got[0].users)
	}
	if !strings.Contains(got[0].body, "google:alice") {
		t.Errorf("content dropped unmapped id: %q", got[0].body)
	}
	if !strings.Contains(got[0].body, "<@"+slaOwnerSnowflake+">") {
		t.Errorf("content missing snowflake mention: %q", got[0].body)
	}

	// send nil: Discord unit still inbox-only.
	mustSetCase(t, sessions, "c-nil-send", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OwnerID: slaOwnerSnowflake, OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	got = nil
	if n := b.sweepSLAInboxAt(now, nil); n != 1 {
		t.Fatalf("nil send appended = %d", n)
	}
	if len(got) != 0 {
		t.Fatalf("nil send still called Discord: %d", len(got))
	}

	mustSetCase(t, sessions, "w_web", sessionstore.Entry{
		Phase: sessionstore.PhaseIntake, Severity: "critical",
		OwnerID: "u-web", OpenedAt: slaStamp(now.Add(-90 * time.Minute)),
	})
	got = nil
	if n := b.sweepSLAInboxAt(now, send); n != 1 {
		t.Fatalf("web-native appended = %d", n)
	}
	if len(got) != 0 {
		t.Fatalf("web-native Discord sends = %d", len(got))
	}
	if got := inboxKinds(t, ib, "u-web"); len(got) != 1 {
		t.Fatalf("web-native inbox = %d", len(got))
	}
}

func TestSweepSLAInboxHeldInsideTargetSilent(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-4*time.Hour - 30*time.Minute)
	mustSetCase(t, sessions, "c-held", sessionstore.Entry{
		Phase: sessionstore.PhaseAnswered, Severity: "critical",
		OwnerID: "u-owner", OpenedAt: slaStamp(opened),
		FirstResponseAt: slaStamp(opened.Add(20 * time.Minute)),
		AnsweredAt:      slaStamp(opened.Add(20 * time.Minute)),
	})
	if n := b.sweepSLAInboxAt(now, nil); n != 0 {
		t.Fatalf("held inside target appended %d", n)
	}
	if got := inboxKinds(t, ib, "u-owner"); len(got) != 0 {
		t.Fatalf("items = %d", len(got))
	}
}

func TestSweepSLAInboxStoppedAfterDeadlineStillNotifies(t *testing.T) {
	b, sessions, ib := newSLABot(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-90 * time.Minute)
	mustSetCase(t, sessions, "c-late-reply", sessionstore.Entry{
		Phase: sessionstore.PhaseInvestigate, Severity: "critical",
		OwnerID: "u-owner", OpenedAt: slaStamp(opened),
		FirstResponseAt: slaStamp(now.Add(-10 * time.Minute)),
	})
	if n := b.sweepSLAInboxAt(now, nil); n != 1 {
		t.Fatalf("stopped+breached appended %d", n)
	}
	items := inboxKinds(t, ib, "u-owner")
	if len(items) != 1 || !strings.Contains(items[0].Subject, "first response") {
		t.Fatalf("items=%v", items)
	}
}
