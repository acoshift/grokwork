package bot

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestStartErrorInvestigateThenFixIsRemoteWork(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	if err := b.cfg.SetProjectErrorsDeploys("app", true, "acme", "gke.cluster-rcf2", "api", "tok", false); err != nil {
		t.Fatal(err)
	}

	var kinds []Kind
	SetStartTaskHookForTest(b, func(opts StartTaskOpts) {
		kinds = append(kinds, opts.Kind)
	})

	actor := Actor{ID: "u1", DisplayName: "Ada"}
	opts := ErrorStartOpts{
		Provider: errsrc.ProviderDeploys,
		Intent:   ErrorIntentInvestigate,
		Project:  "app",
		Actor:    actor,
		ID:       "iss_go_nilmap",
		Title:    "nil map",
		Location: "gke.cluster-rcf2",
		Resource: "api",
		Status:   "open",
		Count:    4,
	}
	res, err := b.StartError(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.ThreadID == "" {
		t.Fatal("empty thread")
	}
	waitHistory(t, b, res.ThreadID, 1)
	ent, ok := b.sessions.Get(res.ThreadID)
	if !ok {
		t.Fatal("missing session")
	}
	if ent.Mode != ModeInvestigate {
		t.Fatalf("mode=%q", ent.Mode)
	}
	wantGoal := "deploys iss_go_nilmap: nil map"
	if ent.Goal != wantGoal {
		t.Fatalf("goal=%q", ent.Goal)
	}
	if len(ent.Errors) != 1 || ent.Errors[0].ErrorKey() != "deploys:gke.cluster-rcf2/api/iss_go_nilmap" {
		t.Fatalf("errors=%+v", ent.Errors)
	}
	if len(kinds) != 1 || kinds[0] != KindStartInvestigate {
		t.Fatalf("kinds after investigate=%v", kinds)
	}

	opts.Intent = ErrorIntentFix
	opts.Title = "should not rewrite goal"
	fix, err := b.StartError(opts)
	if err != nil {
		t.Fatal(err)
	}
	if fix.ThreadID != res.ThreadID {
		t.Fatalf("reuse thread=%q want %q", fix.ThreadID, res.ThreadID)
	}
	if len(kinds) != 2 || kinds[1] != KindStartFix {
		t.Fatalf("kinds after fix=%v", kinds)
	}
	ent, _ = b.sessions.Get(res.ThreadID)
	if ent.Goal != wantGoal {
		t.Fatalf("goal rewritten: %q", ent.Goal)
	}
	if ent.Mode != ModeInvestigate {
		t.Fatalf("mode after fix=%q (first-writer-wins)", ent.Mode)
	}
	if len(ent.Errors) != 1 || ent.Errors[0].ErrorKey() != "deploys:gke.cluster-rcf2/api/iss_go_nilmap" {
		t.Fatalf("errors after fix=%+v", ent.Errors)
	}
	waitHistory(t, b, res.ThreadID, 2)
}

func TestFindByErrorDeploysRequiresFourTuple(t *testing.T) {
	b, _ := testFixBot(t)
	e := sessionstore.Entry{Project: "app"}
	if err := e.UpsertError(sessionstore.TrackedError{
		Provider: sessionstore.ErrorProviderDeploys, ID: "iss1",
		Location: "loc-a", Resource: "api",
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.sessions.Set("t-a", e); err != nil {
		t.Fatal(err)
	}
	hits := b.FindByError("app", errsrc.ProviderDeploys, "iss1", "loc-b", "api")
	if len(hits) != 0 {
		t.Fatalf("aliased across location: %+v", hits)
	}
	hits = b.FindByError("app", errsrc.ProviderDeploys, "iss1", "loc-a", "api")
	if len(hits) != 1 || hits[0].ThreadID != "t-a" {
		t.Fatalf("%+v", hits)
	}
}

func TestFindByErrorExcludesPRReviewAndTerminal(t *testing.T) {
	b, _ := testFixBot(t)
	review := sessionstore.Entry{Project: "app", SessionKind: sessionstore.SessionKindPRReview}
	_ = review.UpsertError(sessionstore.TrackedError{
		Provider: sessionstore.ErrorProviderDeploys, ID: "iss1",
		Location: "loc", Resource: "api",
	})
	if err := b.sessions.Set("rev", review); err != nil {
		t.Fatal(err)
	}
	done := sessionstore.Entry{Project: "app", Label: sessionstore.LabelDone}
	_ = done.UpsertError(sessionstore.TrackedError{
		Provider: sessionstore.ErrorProviderDeploys, ID: "iss1",
		Location: "loc", Resource: "api",
	})
	if err := b.sessions.Set("done", done); err != nil {
		t.Fatal(err)
	}
	hits := b.FindByError("app", errsrc.ProviderDeploys, "iss1", "loc", "api")
	if len(hits) != 0 {
		t.Fatalf("%+v", hits)
	}
}

func TestStartErrorInvestigateSkipsCanShip(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.cfg.SetProjectErrorsDeploys("app", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	if err := b.cfg.SetProjectCapabilityByUser("app", "inv1", "investigator"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	_, err := b.StartError(ErrorStartOpts{
		Provider: errsrc.ProviderDeploys, Intent: ErrorIntentInvestigate,
		Project: "app", Actor: Actor{ID: "inv1", DisplayName: "Inv"},
		ID: "iss1", Location: "loc", Resource: "api", Title: "t",
	})
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	_, err = b.StartError(ErrorStartOpts{
		Provider: errsrc.ProviderDeploys, Intent: ErrorIntentFix,
		Project: "app", Actor: Actor{ID: "inv1", DisplayName: "Inv"},
		ID: "iss1", Location: "loc", Resource: "api", Title: "t",
	})
	if !errors.Is(err, ErrCannotStartFix) {
		t.Fatalf("fix: %v", err)
	}
}

func TestStartErrorDeploysMissingLocator(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.cfg.SetProjectErrorsDeploys("app", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	_, err := b.StartError(ErrorStartOpts{
		Provider: errsrc.ProviderDeploys, Intent: ErrorIntentInvestigate,
		Project: "app", Actor: Actor{ID: "u", DisplayName: "U"},
		ID: "iss1",
	})
	if !errors.Is(err, ErrInvalidError) {
		t.Fatalf("%v", err)
	}
}

func TestPreserveErrorFields(t *testing.T) {
	t.Parallel()
	prev := sessionstore.Entry{
		Errors: []sessionstore.TrackedError{{
			Provider: sessionstore.ErrorProviderDeploys, ID: "iss1",
			Location: "loc", Resource: "api",
		}},
	}
	var next sessionstore.Entry
	preserveErrorFields(&next, prev)
	if len(next.Errors) != 1 || next.Errors[0].ID != "iss1" {
		t.Fatalf("%+v", next.Errors)
	}
	next2 := sessionstore.Entry{Errors: []sessionstore.TrackedError{{ID: "keep"}}}
	preserveErrorFields(&next2, prev)
	if next2.Errors[0].ID != "keep" {
		t.Fatalf("clobber: %+v", next2.Errors)
	}
}

func TestBuildErrorPromptHasNoStackAndNamesMCP(t *testing.T) {
	t.Parallel()
	tracked := sessionstore.TrackedError{
		Provider: sessionstore.ErrorProviderDeploys,
		ID:       "iss1", Title: "nil map", URL: "https://console.deploys.app/deployment/errors?project=acme&location=l&name=n&id=iss1",
		Status: "open", Count: 3, Location: "l", Resource: "n",
	}
	fix := BuildErrorFixPrompt("Ada", tracked, false)
	inv := BuildErrorInvestigatePrompt("Ada", tracked)
	stack := "panic: assignment to entry in nil map"
	for _, p := range []string{fix, inv} {
		if strings.Contains(p, stack) {
			t.Fatal("stack leaked into start prompt")
		}
		if !strings.Contains(p, "deploys_errors_get") {
			t.Fatalf("missing mcp tool:\n%s", p)
		}
		if !strings.Contains(p, "Do not invent tokens") {
			t.Fatalf("%s", p)
		}
	}
	if !strings.Contains(fix, "Do not merge") || strings.Contains(inv, "Do not merge") {
		t.Fatalf("fix=%s\ninv=%s", fix, inv)
	}
}
