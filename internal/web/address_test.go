package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grok-discord/internal/audit"
	"github.com/acoshift/grok-discord/internal/bot"
	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/sessionstore"
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

func TestAddressCICreateRedirect(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	bot.SetThreadAPIForTest(b, &bot.FakeThreadAPI{NextTh: "ci-web-1"})
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/ci-web-1") {
		t.Fatalf("Location=%q", loc)
	}
	e, ok := srv.sessions.Get("ci-web-1")
	if !ok || len(e.PRs) != 1 || e.PRs[0].Number != 9 {
		t.Fatalf("PR bind: %+v", e)
	}
	assertAuditAction(t, srv, audit.ActionSessionStart, true)
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

func TestAddressCIDiscordDown503(t *testing.T) {
	srv, b := addressEnabledServer(t)
	bot.SetThreadAPIForTest(b, nil)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/address-ci", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
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
	if !strings.HasPrefix(w.Header().Get("Location"), "/sessions/rev-web-1") {
		t.Fatalf("loc=%s", w.Header().Get("Location"))
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		th, err := srv.history.Get("rev-web-1")
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
	if !strings.Contains(w.Body.String(), "btn-continue") {
		t.Fatal("missing continue")
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
	for i := 0; i < 2; i++ {
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
