package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestPRAskCreatesHiddenUnitAndStaysOnPR(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/ask", sid, csrf, url.Values{
		"project": {"proj"}, "prompt": {"Does retry cover 429s?"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/prs/acme/app/9") || strings.Contains(loc, "/sessions/") {
		t.Fatalf("must stay on PR page, got %q", loc)
	}
	tid := b.FindPRAsk("proj", "acme/app#9", "member-1")
	if tid == "" {
		t.Fatal("missing ask unit")
	}
	e, ok := srv.sessions.Get(tid)
	if !ok || !e.IsPRAsk() {
		t.Fatalf("entry: ok=%v %+v", ok, e)
	}
	e.NormalizePRs()
	if len(e.PRs) != 0 {
		t.Fatalf("must not bind PRs: %+v", e.PRs)
	}
	if e.WorktreeBranch != "" {
		t.Fatalf("WorktreeBranch=%q", e.WorktreeBranch)
	}
	assertAuditAction(t, srv, audit.ActionPRAskStart, true)
	if auditContains(t, srv, "Does retry cover 429s?") {
		t.Fatal("audit must not store the prompt")
	}

	sessions := getPageBody(t, srv, sid, "/sessions")
	if strings.Contains(sessions, tid) || strings.Contains(sessions, "Ask acme/app#9") {
		t.Fatal("ask unit must not appear on /sessions")
	}
	search := getPageBody(t, srv, sid, "/search?q="+url.QueryEscape("Does retry"))
	if strings.Contains(search, tid) {
		t.Fatal("ask unit must not appear in search")
	}

	page := getPageBody(t, srv, sid, "/prs/acme/app/9?project=proj")
	if !strings.Contains(page, `id="pr-ask"`) {
		t.Fatal("missing pr-ask panel")
	}
	if strings.Contains(page, `id="pr-sessions"`) && strings.Contains(page, tid) {
		t.Fatal("ask unit must not appear in PR Sessions table")
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/"+tid+"?project=proj", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
		t.Fatalf("session page status=%d body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/prs/acme/app/9") {
		t.Fatalf("session page redirect=%q", loc)
	}
}

func TestPRAskReusesPerViewer(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/ask", sid, csrf, url.Values{
		"project": {"proj"}, "prompt": {"first"},
	})
	if w.Code >= 400 {
		t.Fatalf("status=%d", w.Code)
	}
	first := b.FindPRAsk("proj", "acme/app#9", "member-1")
	w = postFix(t, srv, "/prs/acme/app/9/ask", sid, csrf, url.Values{
		"project": {"proj"}, "prompt": {"second"},
	})
	if w.Code >= 400 {
		t.Fatalf("status=%d", w.Code)
	}
	if got := b.FindPRAsk("proj", "acme/app#9", "member-1"); got != first {
		t.Fatalf("reuse: first=%q got=%q", first, got)
	}

	sid2, csrf2, err := srv.LoginAs("member-2", "M2", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-2")
	w = postFix(t, srv, "/prs/acme/app/9/ask", sid2, csrf2, url.Values{
		"project": {"proj"}, "prompt": {"other person"},
	})
	if w.Code >= 400 {
		t.Fatalf("other viewer status=%d body=%s", w.Code, w.Body.String())
	}
	other := b.FindPRAsk("proj", "acme/app#9", "member-2")
	if other == "" || other == first {
		t.Fatalf("other viewer unit=%q first=%q", other, first)
	}
}

func TestPRAskPartialDoesNotViewPR(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	var views atomic.Int32
	inner := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "pr view") {
			views.Add(1)
		}
		return inner(ctx, dir, name, args...)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/ask", sid, csrf, url.Values{
		"project": {"proj"}, "prompt": {"q"},
	})
	if w.Code >= 400 {
		t.Fatalf("status=%d", w.Code)
	}
	views.Store(0)
	body := getPageBody(t, srv, sid, "/partials/prs/acme/app/9/ask?project=proj")
	if views.Load() != 0 {
		t.Fatalf("ask live partial must not gh pr view, got %d", views.Load())
	}
	if !strings.Contains(body, `id="live-pr-ask-run"`) {
		t.Fatalf("missing live run region: %s", bodySnippet(body, "live-pr", 400))
	}
	views.Store(0)
	_ = getPageBody(t, srv, sid, "/partials/prs/acme/app/9/ask/run?project=proj")
	if views.Load() != 0 {
		t.Fatalf("ask run partial must not gh pr view, got %d", views.Load())
	}
}

func TestPRAskFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/ask", sid, csrf, url.Values{
		"project": {"proj"}, "prompt": {"x"},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPRDetailAskButton(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/prs/acme/app/9?project=proj")
	for _, want := range []string{
		`id="pr-ask"`,
		`id="pr-ask-form"`,
		`id="btn-ask-pr"`,
		`action="/prs/acme/app/9/ask"`,
		`rows="1"`,
		"Ask about this diff",
		"not a session",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Count(body, `name="prompt"`) != 1 {
		t.Fatal("PR detail must render exactly one Ask prompt")
	}
	if strings.Contains(body, `id="pr-ask-followup"`) {
		t.Fatal("follow-up form must not exist; one composer in the body")
	}
	rail := body
	if i := strings.Index(body, `id="pr-address-actions"`); i >= 0 {
		rail = body[i:]
		if j := strings.Index(rail, `id="pr-comment-form"`); j >= 0 {
			rail = rail[:j]
		}
		if strings.Contains(rail, `id="pr-ask-form"`) || strings.Contains(rail, `id="btn-ask-pr"`) {
			t.Fatal("Ask must not live in the Agent rail")
		}
	}
}

func TestPRDetailAskDefaultNamesAskModel(t *testing.T) {
	srv, b := addressEnabledServer(t)
	cfg := srv.cfg
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5-high", ReviewModel: "claude-opus-5-high", AskModel: "grok-4.6-high",
	})
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/prs/acme/app/9?project=proj")
	ask := body
	if i := strings.Index(body, `id="pr-ask-form"`); i >= 0 {
		ask = body[i:]
		if j := strings.Index(ask, `</form>`); j >= 0 {
			ask = ask[:j]
		}
	}
	if !strings.Contains(ask, `>Default (grok-4.6-high)</option>`) {
		t.Fatal("Ask Default must name the ask model")
	}
	if strings.Contains(ask, `>Default (claude-opus-5-high)</option>`) {
		t.Fatal("Ask Default must not name the review model when they differ")
	}
	rail := body
	if i := strings.Index(body, `id="pr-address-actions"`); i >= 0 {
		rail = body[i:]
	}
	if !strings.Contains(rail, `>Default (claude-opus-5-high)</option>`) {
		t.Fatal("Address/Review Default must still name the review model")
	}
	if strings.Contains(rail, `>Default (grok-4.6-high)</option>`) {
		t.Fatal("Address/Review Default must not name the ask model")
	}
}

func TestPRAskContinueRefused(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/ask", sid, csrf, url.Values{
		"project": {"proj"}, "prompt": {"q"},
	})
	if w.Code >= 400 {
		t.Fatalf("status=%d", w.Code)
	}
	tid := b.FindPRAsk("proj", "acme/app#9", "member-1")
	w = postFix(t, srv, "/sessions/"+tid+"/continue", sid, csrf, url.Values{"prompt": {"ship it"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("continue status=%d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/prs/acme/app/9") {
		t.Fatalf("continue redirect=%q", loc)
	}
}

func TestPRAskEmptyPrompt(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/ask", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("want error flash, got %q", loc)
	}
}

func TestPRAskDoesNotEnterFindByPR(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	work := sessionstore.Entry{Project: "proj", Goal: "CI acme/app#9"}
	work.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	if err := srv.sessions.Set("work-pr-9", work); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/ask", sid, csrf, url.Values{
		"project": {"proj"}, "prompt": {"q"},
	})
	if w.Code >= 400 {
		t.Fatalf("status=%d", w.Code)
	}
	hits := b.FindByPR("proj", "acme", "app", 9, true)
	if len(hits) != 1 || hits[0].ThreadID != "work-pr-9" {
		t.Fatalf("Address reuse: %+v", hits)
	}
}

func auditContains(t *testing.T, srv *Server, substr string) bool {
	t.Helper()
	if srv.audit == nil {
		return false
	}
	entries, err := os.ReadDir(srv.audit.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		raw, err := os.ReadFile(filepath.Join(srv.audit.Dir(), ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), substr) {
			return true
		}
	}
	return false
}
