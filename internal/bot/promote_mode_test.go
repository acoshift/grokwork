package bot

import (
	"errors"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestStartContinueFixPromotesInvestigate(t *testing.T) {
	b, _ := testAddressBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	if err := b.sessions.Set("inv-1", sessionstore.Entry{
		Project: "app", Origin: SourceWeb, Mode: ModeInvestigate,
	}); err != nil {
		t.Fatal(err)
	}
	var kinds []Kind
	SetStartTaskHookForTest(b, func(opts StartTaskOpts) {
		kinds = append(kinds, opts.Kind)
	})
	res, err := b.StartContinue(ContinueOpts{
		ThreadID: "inv-1", Prompt: "fix the nil deref you found",
		Actor: Actor{ID: "u", DisplayName: "U"},
		Kind:  KindStartFix,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ThreadID != "inv-1" || res.Created {
		t.Fatalf("%+v", res)
	}
	if len(kinds) != 1 || kinds[0] != KindStartFix {
		t.Fatalf("kinds=%v", kinds)
	}
	waitHistory(t, b, "inv-1", 1)
	ent, ok := b.sessions.Get("inv-1")
	if !ok {
		t.Fatal("missing session")
	}
	if ent.Mode != ModeFix {
		t.Fatalf("mode=%q want fix", ent.Mode)
	}

	item := taskItem{
		parsed:   Parsed{Kind: KindTask, Prompt: "also add tests"},
		threadID: "inv-1",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "u", DisplayName: "U"},
	}
	b.snapshotPolicyOntoItem(&item, "app")
	if item.snapMode != ModeFix {
		t.Fatalf("follow-up snapMode=%q", item.snapMode)
	}
	if !item.snapAllowPR && !item.snapAllowDirect {
		t.Fatal("follow-up after promote must ship")
	}
}

func TestKindStartFixDoesNotPromoteCase(t *testing.T) {
	b, _ := testAddressBot(t)
	if err := b.sessions.Set("case-1", sessionstore.Entry{
		Project: "app", Mode: ModeCase, Phase: sessionstore.PhaseInvestigate,
	}); err != nil {
		t.Fatal(err)
	}
	item := taskItem{
		parsed:   Parsed{Kind: KindStartFix, Prompt: "ship it"},
		threadID: "case-1",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "u", DisplayName: "U"},
	}
	b.snapshotPolicyOntoItem(&item, "app")
	ent, _ := b.sessions.Get("case-1")
	if ent.Mode != ModeCase {
		t.Fatalf("mode=%q", ent.Mode)
	}
	if ent.Phase != sessionstore.PhaseFixing {
		t.Fatalf("phase=%q", ent.Phase)
	}
}

func TestKindStartFixStampsEmptyMode(t *testing.T) {
	b, _ := testAddressBot(t)
	if err := b.sessions.Set("empty-1", sessionstore.Entry{
		Project: "app", Origin: SourceWeb,
	}); err != nil {
		t.Fatal(err)
	}
	item := taskItem{
		parsed:   Parsed{Kind: KindStartFix, Prompt: "ship it"},
		threadID: "empty-1",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "u", DisplayName: "U"},
	}
	b.snapshotPolicyOntoItem(&item, "app")
	ent, _ := b.sessions.Get("empty-1")
	if ent.Mode != ModeFix {
		t.Fatalf("mode=%q want fix (must beat a racing investigate first-writer)", ent.Mode)
	}
	if item.snapMode != ModeFix {
		t.Fatalf("snapMode=%q", item.snapMode)
	}
	if !item.snapAllowPR && !item.snapAllowDirect {
		t.Fatal("empty-mode start-fix must ship")
	}
}

func TestKindStartFixDoesNotPromoteExplain(t *testing.T) {
	b, _ := testAddressBot(t)
	if err := b.sessions.Set("exp-1", sessionstore.Entry{
		Project: "app", Mode: ModeExplain,
	}); err != nil {
		t.Fatal(err)
	}
	item := taskItem{
		parsed:   Parsed{Kind: KindStartFix, Prompt: "ship it"},
		threadID: "exp-1",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "u", DisplayName: "U"},
	}
	b.snapshotPolicyOntoItem(&item, "app")
	ent, _ := b.sessions.Get("exp-1")
	if ent.Mode != ModeExplain {
		t.Fatalf("mode=%q", ent.Mode)
	}
	if item.snapMode != ModeFix {
		t.Fatalf("this run snapMode=%q want fix", item.snapMode)
	}
}

func TestStartTaskFixDeniedKeepsInvestigate(t *testing.T) {
	b, proj := testAddressBot(t)
	pc := b.cfg.Projects["app"]
	pc.SafeTeamMode = new(true)
	pc.AllowedUserIDs = []string{"inv1"}
	pc.CapabilityByUser = map[string]string{"inv1": "investigator"}
	b.cfg.Projects["app"] = pc
	if err := b.sessions.Set("inv-deny", sessionstore.Entry{
		Project: "app", Mode: ModeInvestigate,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := b.StartTask(StartTaskOpts{
		ThreadID: "inv-deny",
		Proj:     projectRef{Name: "app", Cwd: proj},
		Prompt:   "ship it",
		Kind:     KindStartFix,
		Actor:    Actor{ID: "inv1", DisplayName: "Inv"},
		Source:   SourceWeb,
	})
	if !errors.Is(err, ErrCannotStartFix) {
		t.Fatalf("err=%v", err)
	}
	ent, _ := b.sessions.Get("inv-deny")
	if ent.Mode != ModeInvestigate {
		t.Fatalf("mode=%q", ent.Mode)
	}
}
