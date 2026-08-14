package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
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
	// A session can stamp agent=claude (the model picker), and an unset ClaudeBin
	// normalizes to "claude" — the operator's real CLI, run with permissions
	// bypassed and, on the default 30-minute timeout, orphaned when the test binary
	// exits. Fake it, and cap the timeout so nothing can outlive the test.
	cfg.ClaudeBin = writeWebFakeClaude(t)
	cfg.TimeoutMs = 5000
	// Isolation off for simpler runs
	cfg.WorktreeIsolation = new(false)
	// Title summarize is async and shares the fake CLI with the task run;
	// leave it off so start-form tests are not racing a Goal rename.
	cfg.SummarizeThreadTitle = new(false)

	// Bot.threadAPI is unexported, so the thread-create path is driven through the
	// exported SetThreadAPIForTest seam rather than a real gateway.
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

// writeWebFakeClaude mirrors writeWebFakeGrok for the claude driver's stream shape
// (the reply lives in `result`, and only on subtype=success).
func writeWebFakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude")
	script := `#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-web-claude"}'
printf '%s\n' '{"type":"result","subtype":"success","session_id":"sess-web-claude","result":"web claude ok","num_turns":1,"usage":{"input_tokens":3,"output_tokens":3}}'
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeWebFakeGrok(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-grok")
	// Streaming runs use streaming-json line events; tools-off helpers like
	// SummarizeTitle use --output-format json and need a single object.
	script := `#!/bin/sh
fmt=streaming-json
prev=
for a in "$@"; do
  if [ "$prev" = "--output-format" ]; then
    fmt=$a
  fi
  prev=$a
done
if [ "$fmt" = "json" ]; then
  printf '%s\n' '{"text":"web fix ok","sessionId":"sess-web","num_turns":1,"usage":{"total_tokens":3}}'
  exit 0
fi
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
	if !strings.Contains(loc, "ok=Session+started") {
		t.Fatalf("Location=%q want ok=Session+started", loc)
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

// fixStatusFlash output is concatenated into the redirect query as a raw
// string, and sessionRedirect hand-splits that on "&" to recover the extra
// pairs appended after it (today: "&discord=offline"). An "&" inside a flash
// sentence would therefore be silently parsed as a query pair and corrupt both
// the flash and the params after it, so the sentences must stay "&"-free.
func TestFixStatusFlashHasNoQuerySeparator(t *testing.T) {
	for _, st := range []bot.FixStartStatus{
		bot.FixStatusStarted,
		bot.FixStatusQueued,
		bot.FixStatusPicker,
	} {
		got := fixStatusFlash(st)
		if got == "" || got == string(st) {
			t.Fatalf("status %q has no flash sentence, got %q", st, got)
		}
		if strings.ContainsAny(got, "&=") {
			t.Fatalf("flash for %q must not contain & or =: %q", st, got)
		}
	}
}

// The Discord-offline notice rides the redirect as a separate "discord=offline"
// pair appended to the ok flash. Now that the flash is a sentence with spaces
// rather than a bare enum token, pin that both halves survive the round trip
// and land on the session page together.
func TestSessionPageDiscordOfflineFlashRoundTrip(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedOwned(t, srv, "flash-1", "member-1", "Member One")
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// The two pairs as sessionRedirect emits them after splitting
	// "<flash>&discord=offline" back apart.
	loc := "/sessions/flash-1?ok=" + url.QueryEscape(fixStatusFlash(bot.FixStatusStarted)) + "&discord=offline"
	req := httptest.NewRequest(http.MethodGet, loc, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Session started") {
		t.Fatal("session page missing the start flash")
	}
	if !strings.Contains(body, "Discord is offline") {
		t.Fatal("session page missing the Discord-offline notice")
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
	for i := range 2 {
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

// TestBulkFixRateLimitConsumesBatchBudget proves a bulk Fix consumes budget
// proportional to its batch size rather than a single token for the whole
// request. Without that, an actor could spam bulk Fix and start unbounded
// sessions while the per-actor limiter only ever ticks once per request.
func TestBulkFixRateLimitConsumesBatchBudget(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	spy := &bot.FakeThreadAPI{NextMsg: "m1", NextTh: "budget-th"}
	bot.SetThreadAPIForTest(b, spy)
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "issue view") {
			n := "0"
			for i, a := range args {
				if a == "view" && i+1 < len(args) {
					n = args[i+1]
					break
				}
			}
			return []byte(`{
				"number":` + n + `,"title":"Bug ` + n + `","body":"body","url":"https://github.com/acme/app/issues/` + n + `",
				"state":"OPEN","author":{"login":"z"},"labels":[],"comments":[]
			}`), nil
		}
		return []byte("{}"), nil
	}
	// Budget of 6: a 5-issue bulk batch must consume exactly 5, leaving room for
	// exactly one more single start before the limiter trips.
	srv.startLimit = newStartRateLimiter(6, time.Minute)
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	w := postFix(t, srv, "/projects/proj/issues/fix", sid, csrf, url.Values{
		"owner":   {"acme"},
		"repo":    {"app"},
		"numbers": {"1", "2", "3", "4", "5"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("bulk status=%d body=%s", w.Code, w.Body.String())
	}
	if spy.StartCount() != 5 {
		t.Fatalf("bulk create count=%d want 5 (proportional to batch size)", spy.StartCount())
	}

	// 6th start overall: exactly at budget, must still be allowed.
	w = postFix(t, srv, "/projects/proj/issues/6/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("6th start status=%d body=%s", w.Code, w.Body.String())
	}
	if spy.StartCount() != 6 {
		t.Fatalf("create count=%d want 6", spy.StartCount())
	}

	// 7th start overall: if the bulk batch had only consumed 1 (the bug this
	// guards against), this would still succeed. It must trip the limiter.
	w = postFix(t, srv, "/projects/proj/issues/7/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("7th start status=%d want 429 body=%s", w.Code, w.Body.String())
	}
	if spy.StartCount() != 6 {
		t.Fatalf("rejected start must not create a thread: count=%d want 6", spy.StartCount())
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
	// Wait for the async run before reading the entry. Store.Get returns an
	// Entry copy, but its Issues slice shares a backing array with the stored
	// entry, so reading it while the run's upsertIssue is still appending is a
	// data race — caught by -race once atomic writes added fsync latency and
	// widened the window. The t.Cleanup wait above is too late: it runs after
	// the body, not before this read.
	bot.WaitIdleForTest(b, 5*time.Second)

	e, ok := srv.sessions.Get("lin-web-1")
	if !ok || len(e.Issues) != 1 || !e.Issues[0].IsLinear() {
		t.Fatalf("%+v", e)
	}
}

func TestIssueDetailShowsFixWhenAllowed(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5",
	})
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
	// Button is agent-neutral "Fix"; model choice lives in the confirm modal.
	for _, want := range []string{
		`id="btn-fix-github"`,
		`>Fix</button>`,
		`force_new`,
		`<select name="model" hidden>`,
		`data-confirm-title="Fix"`,
		`data-confirm-select="model"`,
		`<option value="">Default (grok-4.5)</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in Fix UI: %s", want, body[:min(800, len(body))])
		}
	}
	if strings.Contains(body, "Fix with Grok") {
		t.Fatal("stale Grok-branded Fix label")
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

// A named model from the issue Fix modal is stamped on the new session (agent
// follows the model). Reuse and empty pick are covered by bot unit tests.
func TestIssueFixModelPickStampsSession(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5",
	})
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "force_new": {"1"}, "model": {"claude-opus-5"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("Location=%q", loc)
	}
	tid := strings.TrimPrefix(strings.Split(loc, "?")[0], "/sessions/")
	bot.WaitIdleForTest(b, 5*time.Second)
	e, ok := srv.sessions.Get(tid)
	if !ok {
		t.Fatalf("session %s missing", tid)
	}
	if e.Model != "claude-opus-5" || e.Agent != "claude" {
		t.Fatalf("stamp agent=%q model=%q", e.Agent, e.Model)
	}
}

func TestIssuesListShowsBulkFixWhenAllowed(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5",
	})
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "issue list") {
			return []byte(`[
				{"number":1,"url":"https://github.com/acme/app/issues/1","title":"Bug A","state":"OPEN","author":{"login":"a"},"labels":[],"body":"","closedByPullRequestsReferences":[]},
				{"number":2,"url":"https://github.com/acme/app/issues/2","title":"Bug B","state":"OPEN","author":{"login":"b"},"labels":[],"body":"","closedByPullRequestsReferences":[]}
			]`), nil
		}
		return []byte("{}"), nil
	}
	// Admin bypasses project allowlist used by issues list.
	sid, _, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	// Fix/Cancel live on the shell toolbar next to Apply (like commits Fetch).
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues?owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("shell status=%d", w.Code)
	}
	shell := w.Body.String()
	for _, want := range []string{
		`id="issues-toolbar"`,
		`id="btn-issues-fix"`,
		`id="btn-issues-fix-cancel"`,
		`form="issues-bulk-fix"`,
		// Confirm lives on the toolbar button (form= associated). The select is
		// in the table partial so the modal can clone its options at submit.
		`data-confirm-select="model"`,
		`data-confirm-title="Fix"`,
	} {
		if !strings.Contains(shell, want) {
			t.Fatalf("shell missing %q", want)
		}
	}
	// Cancel must start hidden until the user enters multi-select via Fix.
	if !strings.Contains(shell, `id="btn-issues-fix-cancel" class="btn-secondary" hidden`) {
		t.Fatal("cancel button must render with hidden before Fix is clicked")
	}

	// Bulk form + row checkboxes load with the table partial.
	req = httptest.NewRequest(http.MethodGet, "/partials/issues/table?project=proj&owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="issues-bulk-fix"`,
		`action="/projects/proj/issues/fix"`,
		`name="numbers"`,
		`value="1"`,
		`value="2"`,
		`class="issue-link"`,
		`<select name="model" hidden>`,
		`<option value="">Default (grok-4.5)</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("partial missing %q", want)
		}
	}
	if strings.Contains(body, `id="btn-issues-fix"`) {
		t.Fatal("Fix button belongs on the shell toolbar, not the table partial")
	}
	if strings.Contains(body, `data-confirm-select="model"`) {
		t.Fatal("confirm attributes belong on the toolbar button, not the table partial")
	}
}

func TestIssuesListHidesBulkFixForViewer(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	// Grant project access so the viewer can open the list page.
	p := cfg.Projects["proj"]
	p.AllowedUserIDs = []string{"viewer-1"}
	cfg.Projects["proj"] = p
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "issue list") {
			return []byte(`[{"number":1,"url":"u","title":"t","state":"OPEN","author":{"login":"a"},"labels":[],"body":"","closedByPullRequestsReferences":[]}]`), nil
		}
		return []byte("{}"), nil
	}
	sid, _, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues?owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("shell status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "btn-issues-fix") {
		t.Fatal("viewer must not see bulk Fix on shell")
	}
	req = httptest.NewRequest(http.MethodGet, "/partials/issues/table?project=proj&owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "issues-bulk-fix") {
		t.Fatal("viewer must not see bulk fix form")
	}
}

func TestBulkFixStartsSessions(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	spy := &bot.FakeThreadAPI{NextMsg: "m1", NextTh: "bulk-th"}
	bot.SetThreadAPIForTest(b, spy)
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "issue view") {
			// number from args: gh issue view N
			n := "0"
			for i, a := range args {
				if a == "view" && i+1 < len(args) {
					n = args[i+1]
					break
				}
			}
			return []byte(`{
				"number":` + n + `,"title":"Bug ` + n + `","body":"body","url":"https://github.com/acme/app/issues/` + n + `",
				"state":"OPEN","author":{"login":"z"},"labels":[],"comments":[]
			}`), nil
		}
		return []byte("{}"), nil
	}
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/fix", sid, csrf, url.Values{
		"owner":   {"acme"},
		"repo":    {"app"},
		"numbers": {"10", "20"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/sessions") {
		t.Fatalf("Location=%q want /projects/proj/sessions", loc)
	}
	if !strings.Contains(loc, "ok=") || !strings.Contains(loc, "2") {
		t.Fatalf("Location=%q want ok about 2 sessions", loc)
	}
	if spy.StartCount() != 2 {
		t.Fatalf("create called %d times, want 2", spy.StartCount())
	}
	// Both sessions bound with Fixes
	var bound int
	for _, id := range []string{"bulk-th", "bulk-th-2"} {
		e, ok := srv.sessions.Get(id)
		if !ok || len(e.Issues) != 1 {
			t.Fatalf("session %s=%+v ok=%v", id, e, ok)
		}
		if e.Issues[0].Keyword != sessionstore.IssueKeywordFixes {
			t.Fatalf("keyword=%q", e.Issues[0].Keyword)
		}
		bound++
	}
	if bound != 2 {
		t.Fatalf("bound=%d", bound)
	}
	assertAuditAction(t, srv, audit.ActionSessionStart, true)
}

// A named model from the list-page confirm modal is stamped on every new
// session in the batch (agent follows the model). Empty pick is the default
// path already covered by TestBulkFixStartsSessions.
func TestBulkFixModelPickStampsSessions(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5",
	})
	spy := &bot.FakeThreadAPI{NextMsg: "m1", NextTh: "bulk-model-th"}
	bot.SetThreadAPIForTest(b, spy)
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "issue view") {
			n := "0"
			for i, a := range args {
				if a == "view" && i+1 < len(args) {
					n = args[i+1]
					break
				}
			}
			return []byte(`{
				"number":` + n + `,"title":"Bug ` + n + `","body":"body","url":"https://github.com/acme/app/issues/` + n + `",
				"state":"OPEN","author":{"login":"z"},"labels":[],"comments":[]
			}`), nil
		}
		return []byte("{}"), nil
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/fix", sid, csrf, url.Values{
		"owner":   {"acme"},
		"repo":    {"app"},
		"numbers": {"10", "20"},
		"model":   {"claude-opus-5"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/sessions") {
		t.Fatalf("Location=%q want /projects/proj/sessions", loc)
	}
	bot.WaitIdleForTest(b, 5*time.Second)
	for _, id := range []string{"bulk-model-th", "bulk-model-th-2"} {
		e, ok := srv.sessions.Get(id)
		if !ok {
			t.Fatalf("session %s missing", id)
		}
		if e.Model != "claude-opus-5" || e.Agent != "claude" {
			t.Fatalf("%s stamp agent=%q model=%q", id, e.Agent, e.Model)
		}
	}
}

func TestIssuesListHidesModelPickerWithoutShipCaps(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	if err := cfg.AddProjectAllowedUser("proj", "member-2"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "member-2", "investigator"); err != nil {
		t.Fatal(err)
	}
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5",
	})
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "issue list") {
			return []byte(`[{"number":1,"url":"u","title":"t","state":"OPEN","author":{"login":"a"},"labels":[],"body":"","closedByPullRequestsReferences":[]}]`), nil
		}
		return []byte("{}"), nil
	}
	sid, _, err := srv.LoginAs("member-2", "M2", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues?owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("shell status=%d", w.Code)
	}
	shell := w.Body.String()
	if !strings.Contains(shell, `id="btn-issues-fix"`) {
		t.Fatal("member still sees bulk Fix")
	}
	if strings.Contains(shell, `data-confirm-select="model"`) {
		t.Fatal("investigator must not get the model confirm on the toolbar")
	}
	req = httptest.NewRequest(http.MethodGet, "/partials/issues/table?project=proj&owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `name="model"`) {
		t.Fatal("investigator must not see the model field on the bulk form")
	}
}

func TestBulkFixEmptyRedirectList(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/projects/proj/issues") || !strings.Contains(loc, "err=") {
		t.Fatalf("Location=%q", loc)
	}
}

// TestBulkFixStartsManySessions exercises parallel StartFix for a full bulk batch.
func TestBulkFixStartsManySessions(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	spy := &bot.FakeThreadAPI{NextMsg: "m1", NextTh: "many-th"}
	bot.SetThreadAPIForTest(b, spy)
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "issue view") {
			n := "0"
			for i, a := range args {
				if a == "view" && i+1 < len(args) {
					n = args[i+1]
					break
				}
			}
			return []byte(`{
				"number":` + n + `,"title":"Bug ` + n + `","body":"body","url":"https://github.com/acme/app/issues/` + n + `",
				"state":"OPEN","author":{"login":"z"},"labels":[],"comments":[]
			}`), nil
		}
		return []byte("{}"), nil
	}
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	nums := url.Values{"owner": {"acme"}, "repo": {"app"}}
	for _, n := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"} {
		nums.Add("numbers", n)
	}
	w := postFix(t, srv, "/projects/proj/issues/fix", sid, csrf, nums)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/sessions") {
		t.Fatalf("Location=%q", loc)
	}
	if spy.StartCount() != 10 {
		t.Fatalf("create called %d times, want 10", spy.StartCount())
	}
	// First id is many-th; subsequent are many-th-2 … many-th-10 (FakeThreadAPI).
	wantIDs := []string{"many-th"}
	for i := 2; i <= 10; i++ {
		wantIDs = append(wantIDs, fmt.Sprintf("many-th-%d", i))
	}
	var bound int
	for _, id := range wantIDs {
		e, ok := srv.sessions.Get(id)
		if !ok || len(e.Issues) != 1 {
			t.Fatalf("session %s ok=%v issues=%d", id, ok, len(e.Issues))
		}
		bound++
	}
	if bound != 10 {
		t.Fatalf("bound=%d", bound)
	}
}

func TestBulkFixForceNewDespiteExistingBind(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	e := sessionstore.Entry{Project: "proj", Origin: "web"}
	e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 5, Keyword: sessionstore.IssueKeywordFixes})
	if err := srv.sessions.Set("exist-bind", e); err != nil {
		t.Fatal(err)
	}
	spy := &bot.FakeThreadAPI{NextMsg: "m1", NextTh: "force-new-bulk"}
	bot.SetThreadAPIForTest(b, spy)
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "numbers": {"5"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if spy.StartCount() != 1 {
		t.Fatalf("create count=%d want 1 (force new)", spy.StartCount())
	}
	if _, ok := srv.sessions.Get("force-new-bulk"); !ok {
		t.Fatal("expected new session force-new-bulk")
	}
}

func TestParseIssueNumbers(t *testing.T) {
	got, err := parseIssueNumbers([]string{"3", "1", "3", " 2 ", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 3 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("got=%v", got)
	}
	if _, err := parseIssueNumbers([]string{"x"}); err == nil {
		t.Fatal("want error for non-int")
	}
	if _, err := parseIssueNumbers([]string{"0"}); err == nil {
		t.Fatal("want error for zero")
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

// TestStartRateMaxCoversFullBulkBatch pins the invariant that used to be
// expressed as `startRateMax = fixBulkMax`. Coupling the two meant raising the
// bulk batch cap would silently raise the per-actor rate limit; decoupling them
// needs this assertion so the pair cannot drift into a state where a full-size
// bulk Fix can never be admitted.
func TestStartRateMaxCoversFullBulkBatch(t *testing.T) {
	if startRateMax < fixBulkMax {
		t.Fatalf("startRateMax (%d) < fixBulkMax (%d): a full-size bulk Fix could never fit in one window",
			startRateMax, fixBulkMax)
	}
}

func TestFixClickUpDisabled400(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	_ = cfg.SetProjectClickUp("proj", false, "", "", "", "", false)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/clickup/DEV-1/fix", sid, csrf, nil)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFixClickUpCreate(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectClickUp("proj", true, "ws", "list1", "DEV", "cu-key", false); err != nil {
		t.Fatal(err)
	}
	bot.SetThreadAPIForTest(b, &bot.FakeThreadAPI{NextTh: "cu-web-1"})

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/clickup/DEV-88/fix", sid, csrf, nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/cu-web-1") {
		t.Fatalf("Location=%q", loc)
	}
	bot.WaitIdleForTest(b, 5*time.Second)
	e, ok := srv.sessions.Get("cu-web-1")
	if !ok {
		t.Fatal("missing session")
	}
	found := false
	for _, iss := range e.Issues {
		if iss.IsClickUp() && iss.CustomID == "DEV-88" {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues=%+v", e.Issues)
	}
}
