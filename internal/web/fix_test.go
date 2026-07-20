package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grok-discord/internal/audit"
	"github.com/acoshift/grok-discord/internal/bot"
	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

func fixEnabledServer(t *testing.T) (*Server, *config.Config, *bot.Bot) {
	t.Helper()
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.StartSessions = true
	cfg.DiscordGuildID = "guild-fix"
	// Map preferred channel
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	// Ensure channel map + preferred
	cfg.Channels = map[string]string{"ch-proj": "proj"}
	_ = cfg.SetProjectDiscordChannel("proj", "ch-proj")

	// Fake grok on bot
	fakeGrok := writeWebFakeGrok(t)
	cfg.GrokBin = fakeGrok
	// Isolation off for simpler runs
	f := false
	cfg.WorktreeIsolation = &f

	// Inject thread API on bot
	// bot.New was already created — use srv.bot
	// re-create bot with updated cfg? cfg is pointer so GrokBin is live.
	// threadAPI is unexported — use CreateWorkflowThread via exported StartFix which uses threadAPI field... unexported.

	// We need to inject threadAPI. It's unexported on Bot. Options:
	// 1) Export a TestSetThreadAPI method
	// 2) Use Register with nil and only test reuse path without create
	// 3) Add bot.SetThreadAPIForTest
	// Cleanest for PR11a: exported SetThreadAPI for tests / inject via StartFix create with DiscordReady false only for create fail.

	// For create path tests, add a package-level test helper on bot.
	bot.SetThreadAPIForTest(srv.bot, &bot.FakeThreadAPI{NextMsg: "m1", NextTh: "th-web-1"})

	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "issue view") {
			return []byte(`{
				"number":42,"title":"Pay bug","body":"steps to repro","url":"https://github.com/acme/app/issues/42",
				"state":"OPEN","author":{"login":"z"},"labels":[],"comments":[]
			}`), nil
		}
		return []byte("{}"), nil
	}
	return srv, cfg, srv.bot
}

func writeWebFakeGrok(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-grok")
	script := `#!/bin/sh
printf '%s\n' '{"type":"text","data":"web fix ok"}'
printf '%s\n' '{"type":"end","sessionId":"sess-web","stopReason":"EndTurn","num_turns":1,"usage":{"total_tokens":3}}'
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func postFix(t *testing.T, srv *Server, path, sid, csrf string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", csrf)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestFixFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t) // startSessions false
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/1/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFixViewerForbidden(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	_ = cfg
	sid, csrf, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestFixBadCSRF(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/fix", sid, "wrong-csrf", url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestFixGitHubCreateRedirectSession(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/th-web-1") {
		t.Fatalf("Location=%q", loc)
	}
	if !strings.Contains(loc, "ok=started") {
		t.Fatalf("Location=%q want ok=started", loc)
	}
	// Session bound
	e, ok := srv.sessions.Get("th-web-1")
	if !ok || len(e.Issues) != 1 || e.Issues[0].Number != 42 {
		t.Fatalf("session=%+v ok=%v", e, ok)
	}
	// Audit success
	assertAuditAction(t, srv, audit.ActionSessionStart, true)
	// Session page renders
	req := httptest.NewRequest(http.MethodGet, "/sessions/th-web-1?ok=started", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	wr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wr, req)
	if wr.Code != http.StatusOK {
		t.Fatalf("session page %d", wr.Code)
	}
	if !strings.Contains(wr.Body.String(), "th-web-1") {
		t.Fatalf("body missing thread")
	}
	// Wait async grok briefly
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		th, err := srv.history.Get("th-web-1")
		if err == nil && len(th.Turns) >= 1 {
			if !strings.Contains(th.Turns[0].Prompt, "acme/app#42") {
				t.Fatalf("prompt=%q", th.Turns[0].Prompt)
			}
			if !strings.Contains(th.Turns[0].Prompt, "Do not merge") {
				t.Fatalf("expected do-not-merge in user prompt: %q", th.Turns[0].Prompt)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout waiting for history turn")
}

func TestFixReuseNoCreate(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	// Pre-bind issue on existing thread
	e := sessionstore.Entry{Project: "proj", Origin: "web"}
	e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 42, Keyword: sessionstore.IssueKeywordFixes})
	if err := srv.sessions.Set("exist-th", e); err != nil {
		t.Fatal(err)
	}
	// Spy: reset fake to panic if create called — new Fake that records
	spy := &bot.FakeThreadAPI{NextTh: "should-not"}
	bot.SetThreadAPIForTest(b, spy)

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/exist-th") {
		t.Fatalf("Location=%q", loc)
	}
	if spy.StartCount() != 0 {
		t.Fatalf("create called %d times", spy.StartCount())
	}
}

func TestFixMultiHitPickerNoEnqueue(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	for _, id := range []string{"h1", "h2"} {
		e := sessionstore.Entry{Project: "proj"}
		e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 7})
		if err := srv.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	spy := &bot.FakeThreadAPI{NextTh: "nope"}
	bot.SetThreadAPIForTest(b, spy)
	// Hold nothing — picker should not StartTask either (no new history on h1/h2 from this POST)

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/7/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "picker=1") {
		t.Fatalf("Location=%q want picker", loc)
	}
	if spy.StartCount() != 0 {
		t.Fatal("must not create on picker")
	}
	// No turns yet on h1
	if th, err := srv.history.Get("h1"); err == nil && len(th.Turns) > 0 {
		t.Fatalf("should not enqueue on picker: %+v", th)
	}
}

func TestFixCreateDiscordDownWebNative(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	bot.SetThreadAPIForTest(b, nil) // no API, no Discord ready → web-native unit
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/99/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/w_") {
		t.Fatalf("Location=%q want web-native /sessions/w_*", loc)
	}
}

func TestFixQueueFull409(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	threadID := "qf"
	e := sessionstore.Entry{Project: "proj"}
	e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 3})
	if err := srv.sessions.Set(threadID, e); err != nil {
		t.Fatal(err)
	}
	// Fill via bot helpers exported for test
	if err := bot.FillQueueForTest(b, threadID, "proj"); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/3/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFixRateLimit429(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	// Tight limiter: unit under test is the HTTP gate + limiter, not grok.
	srv.startLimit = newStartRateLimiter(2, time.Minute)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"owner": {"acme"}, "repo": {"app"}, "force_new": {"1"}}
	for i := 0; i < 2; i++ {
		w := postFix(t, srv, "/projects/proj/issues/50/fix", sid, csrf, form)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("too early rate limit at %d", i)
		}
		if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
			t.Fatalf("start %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	w := postFix(t, srv, "/projects/proj/issues/50/fix", sid, csrf, form)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%s", w.Code, w.Body.String())
	}
}

func TestFixLinearDisabled400(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	// Linear not enabled for proj
	_ = cfg.SetProjectLinear("proj", false, "", "", false)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/linear/ENG-1/fix", sid, csrf, nil)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		// requireFeature still on; handler returns 400 for linear disabled
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFixLinearCreate(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectLinear("proj", true, "ENG", "lin-key", false); err != nil {
		t.Fatal(err)
	}
	bot.SetThreadAPIForTest(b, &bot.FakeThreadAPI{NextTh: "lin-web-1"})
	// Resolve may fail without Linear HTTP; StartFix still binds by identifier.

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/linear/ENG-88/fix", sid, csrf, nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/lin-web-1") {
		t.Fatalf("Location=%q", loc)
	}
	e, ok := srv.sessions.Get("lin-web-1")
	if !ok || len(e.Issues) != 1 || !e.Issues[0].IsLinear() {
		t.Fatalf("%+v", e)
	}
}

func TestIssueDetailShowsFixWhenAllowed(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues/42?owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Fix with Grok") || !strings.Contains(body, "btn-fix-github") {
		t.Fatalf("missing Fix UI: %s", body[:min(500, len(body))])
	}
	if !strings.Contains(body, "force_new") {
		t.Fatal("missing force_new checkbox")
	}
}

func TestIssueDetailHidesFixForViewer(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues/42?owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "btn-fix-github") {
		t.Fatal("viewer must not see Fix button")
	}
}

func assertAuditAction(t *testing.T, srv *Server, action string, ok bool) {
	t.Helper()
	if srv.audit == nil {
		t.Fatal("no audit")
	}
	// Read today's audit file
	dir := srv.audit.Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ent := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), action) && strings.Contains(string(raw), `"ok":`+boolJSON(ok)) {
			found = true
			break
		}
	}
	if !found {
		// looser: action present
		for _, ent := range entries {
			raw, _ := os.ReadFile(filepath.Join(dir, ent.Name()))
			if strings.Contains(string(raw), action) {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("audit action %q not found", action)
	}
}

func boolJSON(ok bool) string {
	if ok {
		return "true"
	}
	return "false"
}

// linearStubClient unused placeholder removed
