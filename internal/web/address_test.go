package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func addressEnabledServer(t *testing.T) (*Server, *bot.Bot) {
	t.Helper()
	srv, _, b := fixEnabledServer(t)
	// Default gh runner for PR view/checks/graphql
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "graphql"):
			return []byte(`{
			  "data": {
			    "repository": {
			      "pullRequest": {
			        "reviewThreads": {
			          "nodes": [
			            {
			              "isResolved": false,
			              "path": "main.go",
			              "line": 12,
			              "comments": {
			                "nodes": [
			                  {"body":"please handle nil","url":"https://github.com/c","author":{"login":"rev"}}
			                ]
			              }
			            }
			          ]
			        }
			      }
			    }
			  }
			}`), nil
		case strings.Contains(joined, "pr view"):
			return []byte(`{
				"number":9,"url":"https://github.com/acme/app/pull/9","title":"CI PR","state":"OPEN",
				"isDraft":false,"reviewDecision":"REVIEW_REQUIRED","headRefOid":"abc","headRefName":"feat",
				"baseRefName":"main","body":"b","author":{"login":"z"},"additions":1,"deletions":0,"changedFiles":1,
				"statusCheckRollup":[{"__typename":"CheckRun","name":"ci","conclusion":"FAILURE","status":"COMPLETED"}]
			}`), nil
		case strings.Contains(joined, "pr checks"):
			return []byte(`[{"name":"ci","state":"FAILURE","bucket":"fail","link":"https://x"}]`), nil
		default:
			return []byte("{}"), nil
		}
	}
	return srv, b
}

func TestAddressCIFeatureOff(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
}

// webUnitFromLocation pulls the created unit id out of a /sessions/<id>?… redirect
// and asserts it is web-native: dispatches from the PR and commit pages must not
// open a Discord thread, so a t-prefixed id here is a regression.
func webUnitFromLocation(t *testing.T, loc string) string {
	t.Helper()
	const prefix = "/sessions/"
	if !strings.HasPrefix(loc, prefix) {
		t.Fatalf("want session redirect, got %q", loc)
	}
	id := strings.TrimPrefix(loc, prefix)
	if i := strings.IndexAny(id, "?&"); i >= 0 {
		id = id[:i]
	}
	if !strings.HasPrefix(id, "w_") {
		t.Fatalf("want web-native unit (no Discord thread), got %q", loc)
	}
	return id
}

func TestAddressCICreateRedirect(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	fake := &bot.FakeThreadAPI{NextTh: "ci-web-1"}
	bot.SetThreadAPIForTest(b, fake)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	tid := webUnitFromLocation(t, w.Header().Get("Location"))
	e, ok := srv.sessions.Get(tid)
	if !ok || len(e.PRs) != 1 || e.PRs[0].Number != 9 {
		t.Fatalf("PR bind: %+v", e)
	}
	assertAuditAction(t, srv, audit.ActionSessionStart, true)
}

// The PR card is the only dispatch surface with two buttons, so each carries its own
// data-confirm-select. Both must be wired, and the caps gate must hold on both POSTs
// — hidden UI is not a permission check.
func TestPRDetailModelPickerWiringAndGate(t *testing.T) {
	// addressEnabledServer, not fixEnabledServer: without its gh runner the
	// address-review POST fails on "could not list review comments" *before* the
	// model gate, so the denial assertion below would pass without testing anything.
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
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/prs/acme/app/9?project=proj")
	for _, want := range []string{
		`<select name="model" hidden>`,
		`data-confirm-title="Address CI"`,
		`data-confirm-title="Address review"`,
		// Both buttons feed the same field.
		`id="btn-address-ci"`,
		`id="btn-address-review"`,
		`<option value="">Default (claude-opus-5)</option>`,
		`<div class="rail-group-title">Agent</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("PR detail missing %q", want)
		}
	}
	if strings.Count(body, `data-confirm-select="model"`) < 2 {
		t.Fatal("both dispatch buttons must carry data-confirm-select")
	}
	if strings.Contains(body, "opens a Discord work unit") {
		t.Fatal("stale Discord copy on the PR dispatch card")
	}

	// Same page, non-builder: no field, and the POST is refused for both actions.
	if err := cfg.SetProjectCapabilityByUser("proj", "member-2", "investigator"); err != nil {
		t.Fatal(err)
	}
	sid2, csrf2, err := srv.LoginAs("member-2", "M2", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if body2 := getPageBody(t, srv, sid2, "/prs/acme/app/9?project=proj"); strings.Contains(body2, `name="model"`) {
		t.Fatal("investigator must not see the model field on the PR card")
	}
	for _, path := range []string{"/prs/acme/app/9/address-ci", "/prs/acme/app/9/address-review"} {
		w := postFix(t, srv, path, sid2, csrf2, url.Values{
			"project": {"proj"}, "force_new": {"1"}, "model": {"claude-opus-5"},
		})
		// Assert the model denial specifically — any other error here would mean the
		// request never reached the gate.
		assertRedirectErr(t, w, "/prs/acme/app/9", "not allowed to pick a model")
	}
	// And an empty model is still allowed for a non-builder: only *choosing* is gated.
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid2, csrf2, url.Values{
		"project": {"proj"}, "force_new": {"1"},
	})
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("default model must stay available to non-builders, got %q", loc)
	}
	_ = csrf
}

func TestAddressCIReuseNoCreate(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	spy := &bot.FakeThreadAPI{NextTh: "nope"}
	bot.SetThreadAPIForTest(b, spy)
	e := sessionstore.Entry{Project: "proj"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	if err := srv.sessions.Set("exist-ci", e); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Location"), "/sessions/exist-ci") {
		t.Fatalf("loc=%s", w.Header().Get("Location"))
	}
	if spy.StartCount() != 0 {
		t.Fatal("must not create")
	}
}

// TestAddressDispatchTargetPicker pins the single Session dropdown that replaced
// the per-session button pairs: one option per bound unit plus an explicit "new"
// sentinel, and the sentinel must create rather than reuse.
func TestAddressDispatchTargetPicker(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}

	// No bound unit: no dropdown to render, and the form states its intent.
	body := getPageBody(t, srv, sid, "/prs/acme/app/9?project=proj")
	if strings.Contains(body, `id="dispatch-thread"`) {
		t.Fatal("unbound PR should not render a session dropdown")
	}
	if !strings.Contains(body, `name="force_new" value="1"`) {
		t.Fatalf("unbound PR must post force_new: %s", body)
	}

	e := sessionstore.Entry{Project: "proj", Goal: "address CI", Label: sessionstore.LabelInProgress}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	if err := srv.sessions.Set("bound-1", e); err != nil {
		t.Fatal(err)
	}
	body = getPageBody(t, srv, sid, "/prs/acme/app/9?project=proj")
	for _, want := range []string{
		`id="dispatch-thread"`,
		`<option value="bound-1">`,
		`<option value="__new__">Start a new session</option>`,
		"address CI",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dispatch picker missing %q in %s", want, body)
		}
	}
	// The action pair renders once, not once per bound unit.
	if n := strings.Count(body, `id="btn-address-ci"`); n != 1 {
		t.Fatalf("Address CI rendered %d times, want 1", n)
	}

	// Sentinel creates. Without it the same POST would reuse bound-1, so landing
	// on a fresh web-native unit (startPRCreate never opens a Discord thread) is
	// the proof that force_new was inferred from the dropdown.
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, url.Values{
		"project": {"proj"}, "thread_id": {newSessionChoice},
	})
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/sessions/w_") {
		t.Fatalf("%q sentinel did not create: loc=%q", newSessionChoice, loc)
	}

	// A real id still reuses that unit.
	w = postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, url.Values{
		"project": {"proj"}, "thread_id": {"bound-1"},
	})
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/sessions/bound-1") {
		t.Fatalf("picked unit not reused: loc=%q", loc)
	}
}

func TestAddressCIDiscordDownWebNative(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	bot.SetThreadAPIForTest(b, nil)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/w_") {
		t.Fatalf("Location=%q want web-native session", loc)
	}
}

func TestAddressCIQueueFull409(t *testing.T) {
	srv, b := addressEnabledServer(t)
	e := sessionstore.Entry{Project: "proj"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9})
	if err := srv.sessions.Set("qf-ci", e); err != nil {
		t.Fatal(err)
	}
	if err := bot.FillQueueForTest(b, "qf-ci", "proj"); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAddressCIBadCSRF(t *testing.T) {
	srv, _ := addressEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, "bad", url.Values{"project": {"proj"}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestContinueSession(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := srv.sessions.Set("cont-web", sessionstore.Entry{Project: "proj", Origin: "web"}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/cont-web/continue", sid, csrf, url.Values{
		"prompt": {"ship the remaining tests"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Location"), "/sessions/cont-web") {
		t.Fatalf("loc=%s", w.Header().Get("Location"))
	}
	// Wait for history
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		th, err := srv.history.Get("cont-web")
		if err == nil && len(th.Turns) >= 1 {
			if !strings.Contains(th.Turns[0].Prompt, "remaining tests") {
				t.Fatalf("prompt=%q", th.Turns[0].Prompt)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout history")
}

func TestContinueQueueFull409(t *testing.T) {
	srv, b := addressEnabledServer(t)
	if err := srv.sessions.Set("qf-cont", sessionstore.Entry{Project: "proj"}); err != nil {
		t.Fatal(err)
	}
	if err := bot.FillQueueForTest(b, "qf-cont", "proj"); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/qf-cont/continue", sid, csrf, url.Values{"prompt": {"x"}})
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestAddressReviewCreatePromptHasComment(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	bot.SetThreadAPIForTest(b, &bot.FakeThreadAPI{NextTh: "rev-web-1"})
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-review", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	tid := webUnitFromLocation(t, w.Header().Get("Location"))
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		th, err := srv.history.Get(tid)
		if err == nil && len(th.Turns) >= 1 {
			p := th.Turns[0].Prompt
			if !strings.Contains(p, "please handle nil") {
				t.Fatalf("expected review body in prompt: %q", p)
			}
			if !strings.Contains(p, "Do not merge") {
				t.Fatalf("expected do not merge: %q", p)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout history")
}

func TestAddressReviewListFailClosed(t *testing.T) {
	srv, b := addressEnabledServer(t)
	bot.SetThreadAPIForTest(b, &bot.FakeThreadAPI{NextTh: "x"})
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "graphql") {
			return nil, context.DeadlineExceeded
		}
		return []byte(`{}`), nil
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-review", sid, csrf, url.Values{"project": {"proj"}})
	// Redirect with err flash (400-ish browser UX) or non-2xx without starting
	if w.Code == http.StatusFound || w.Code == http.StatusSeeOther {
		loc := w.Header().Get("Location")
		if !strings.Contains(loc, "err=") {
			t.Fatalf("want err flash: %s", loc)
		}
		return
	}
	if w.Code < 400 {
		t.Fatalf("status=%d want fail closed", w.Code)
	}
}

func TestAddressReviewEmptyCommentsFailClosed(t *testing.T) {
	srv, b := addressEnabledServer(t)
	bot.SetThreadAPIForTest(b, &bot.FakeThreadAPI{NextTh: "x"})
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "graphql") {
			return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`), nil
		}
		return []byte(`{}`), nil
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-review", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther && w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	// No session created
	if _, ok := srv.sessions.Get("x"); ok {
		t.Fatal("should not create on empty review list")
	}
}

// TestAddressReviewReadsPRConversation pins that "Address review" sees top-level
// PR comments, not just unresolved inline threads. Reviewers routinely leave the
// ask as a plain comment, and an agent review posts its findings with
// `gh pr comment` — so without this the review loop never closes.
func TestAddressReviewReadsPRConversation(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		// No unresolved inline threads at all.
		case strings.Contains(joined, "graphql"):
			return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`), nil
		case strings.Contains(joined, "pr view"):
			return []byte(`{
				"number":9,"url":"https://github.com/acme/app/pull/9","title":"CI PR","state":"OPEN",
				"comments":[
					{"author":{"login":"beam"},"body":"please rework the retry logic","url":"https://gh/c1","createdAt":"2026-07-21T09:00:00Z"}
				]
			}`), nil
		default:
			return []byte(`[]`), nil
		}
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-review", sid, csrf, url.Values{"project": {"proj"}})
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("conversation-only PR must still dispatch: status=%d loc=%q", w.Code, loc)
	}
	tid := webUnitFromLocation(t, loc)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		th, err := srv.history.Get(tid)
		if err == nil && len(th.Turns) >= 1 {
			p := th.Turns[0].Prompt
			for _, want := range []string{
				"No unresolved inline review threads.",
				"PR conversation",
				"beam",
				"please rework the retry logic",
			} {
				if !strings.Contains(p, want) {
					t.Fatalf("prompt missing %q:\n%s", want, p)
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout waiting for the dispatched run's prompt")
}

func TestPRDetailShowsAddressButtons(t *testing.T) {
	srv, _ := addressEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/prs/acme/app/9?project=proj", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "btn-address-ci") || !strings.Contains(body, "btn-address-review") {
		t.Fatalf("missing address buttons")
	}
}

func TestSessionPageShowsContinue(t *testing.T) {
	srv, _ := addressEnabledServer(t)
	if err := srv.sessions.Set("s1", sessionstore.Entry{Project: "proj"}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "btn-continue") {
		t.Fatal("missing continue")
	}
	// Composer copy is agent-neutral: a session can run on claude, and this box
	// is rendered before any run stamps an agent at all.
	if !strings.Contains(body, `placeholder="What should happen next?"`) {
		t.Fatalf("composer placeholder not agent-neutral: %s", body)
	}
	for _, banned := range []string{"What should Grok do next?", "Continue with Grok"} {
		if strings.Contains(body, banned) {
			t.Fatalf("composer still hardcodes %q", banned)
		}
	}
}

func TestAddressRateLimit429(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	bot.SetThreadAPIForTest(b, &bot.FakeThreadAPI{NextTh: "rl"})
	srv.startLimit = newStartRateLimiter(2, time.Minute)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"project": {"proj"}, "force_new": {"1"}}
	for i := range 2 {
		w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, form)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("early 429 at %d", i)
		}
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, form)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", w.Code)
	}
}
