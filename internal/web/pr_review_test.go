package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestPRAgentReviewCreatesWebNativeSession(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	tid := webUnitFromLocation(t, w.Header().Get("Location"))
	e, ok := srv.sessions.Get(tid)
	if !ok || e.Project != "proj" {
		t.Fatalf("session missing: ok=%v %+v", ok, e)
	}
	// The goal is the only label the sessions list has for a web-native unit.
	if !strings.Contains(e.Goal, "acme/app#9") {
		t.Fatalf("goal=%q", e.Goal)
	}
	assertAuditAction(t, srv, audit.ActionPRReviewStart, true)
}

// A review is a fresh read of the current head, so it never joins the session that
// owns the PR — otherwise it could not be dispatched at all while that session was
// mid-run, and it would inherit an older diff's reasoning.
func TestPRAgentReviewNeverReusesBoundSession(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	bound := sessionstore.Entry{Project: "proj"}
	bound.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	if err := srv.sessions.Set("owns-pr-9", bound); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	tid := webUnitFromLocation(t, w.Header().Get("Location"))
	if tid == "owns-pr-9" {
		t.Fatal("review must not reuse the session bound to the PR")
	}
	// And it must not bind the PR itself, or the next Address CI dispatch would be
	// forced through the reuse picker.
	e, _ := srv.sessions.Get(tid)
	e.NormalizePRs()
	if len(e.PRs) != 0 {
		t.Fatalf("review unit must not bind the PR, got %+v", e.PRs)
	}
}

func TestPRAgentReviewFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t) // startSessions false
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// The review card carries its own model select + confirm modal (the Address pair
// share one form, so a third one is easy to forget), and the caps gate must hold on
// the POST — hiding the field is not a permission check.
func TestPRDetailReviewButtonAndModelGate(t *testing.T) {
	srv, b := addressEnabledServer(t)
	cfg := srv.cfg
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	_ = cfg.AddProjectAllowedUser("proj", "member-1")
	_ = cfg.AddProjectAllowedUser("proj", "member-2")
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5",
	})
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/prs/acme/app/9?project=proj")
	for _, want := range []string{
		`action="/prs/acme/app/9/agent-review"`,
		`id="btn-review-pr"`,
		`data-confirm-title="Review PR"`,
		`>Review in new session</button>`,
		// The copy must state the two things that distinguish this from the commit
		// review card and the Address pair.
		"single PR comment",
		"no GitHub issues",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("PR detail missing %q", want)
		}
	}
	// Address CI, Address review, and Review each own a confirm-select.
	if got := strings.Count(body, `data-confirm-select="model"`); got < 3 {
		t.Fatalf("want a confirm-select on all three dispatch buttons, got %d", got)
	}
	if got := strings.Count(body, `<select name="model" hidden>`); got < 2 {
		t.Fatalf("the review form needs its own hidden select to clone from, got %d", got)
	}

	// Non-builder: no model field, and a forged model is refused on the POST.
	if err := cfg.SetProjectCapabilityByUser("proj", "member-2", "investigator"); err != nil {
		t.Fatal(err)
	}
	sid2, csrf2, err := srv.LoginAs("member-2", "M2", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if body2 := getPageBody(t, srv, sid2, "/prs/acme/app/9?project=proj"); strings.Contains(body2, `name="model"`) {
		t.Fatal("investigator must not see the model field on the review card")
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid2, csrf2, url.Values{
		"project": {"proj"}, "model": {"claude-opus-5"},
	})
	assertRedirectErr(t, w, "/prs/acme/app/9", "not allowed to pick a model")

	// An empty model stays available to a non-builder: only *choosing* is gated.
	w = postFix(t, srv, "/prs/acme/app/9/agent-review", sid2, csrf2, url.Values{"project": {"proj"}})
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("default model must stay available to non-builders, got %q", loc)
	}
}
