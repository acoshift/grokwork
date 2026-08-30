package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/errsrc/deploys"
	"github.com/acoshift/grokwork/internal/errsrc/gcperr"
	sentrycli "github.com/acoshift/grokwork/internal/errsrc/sentry"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestErrorsLandingZeroProvidersExplainer(t *testing.T) {
	srv, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/errors", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="page-project-errors"`) {
		t.Fatal("page marker")
	}
	if !strings.Contains(body, "No error source enabled") && !strings.Contains(body, "Integrations") {
		t.Fatalf("explainer missing:\n%s", body)
	}
}

func TestErrorsContainment(t *testing.T) {
	srv, cfg, _ := testServer(t)

	code, body := errorsStatusBody(t, srv, "/projects/nope/errors")
	if code != http.StatusForbidden || !strings.Contains(body, "unknown project") {
		t.Fatalf("unknown project: %d %s", code, body)
	}

	code, body = errorsStatusBody(t, srv, "/projects/proj/errors/sentry/APP-1")
	if code != http.StatusNotFound || !strings.Contains(body, errorSourceDisabled) {
		t.Fatalf("disabled sentry: %d %s", code, body)
	}
	code, body = errorsStatusBody(t, srv, "/projects/proj/errors/nope/x")
	if code != http.StatusNotFound || !strings.Contains(body, errorSourceDisabled) {
		t.Fatalf("unknown src: %d %s", code, body)
	}

	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "", false); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/errors?src=deploys", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("enabled no key: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `id="page-project-errors"`) {
		t.Fatal("explainer chrome")
	}
	if !strings.Contains(w.Body.String(), "DEPLOYS_API_TOKEN_") && !strings.Contains(w.Body.String(), "no API token") {
		t.Fatalf("want cannot-resolve explainer:\n%s", w.Body.String())
	}

	code, body = errorsStatusBody(t, srv, "/projects/proj/errors/deploys/iss1")
	if code != http.StatusBadRequest || !strings.Contains(body, "location") {
		t.Fatalf("deploys without loc/name: %d %s", code, body)
	}
}

func TestErrorsOtherProjectForbidden(t *testing.T) {
	srv := twoProjectAuthServer(t)
	if err := srv.cfg.SetProjectErrorsDeploys("secret", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/secret/errors", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "forbidden") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestErrorsDeploysListAndDetail(t *testing.T) {
	srv, apiHits := deploysErrorsFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/errors", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("single-provider landing status=%d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "src=deploys") {
		t.Fatalf("Location=%q", loc)
	}
	body := getBody(t, srv.Handler(), "/projects/proj/errors?src=deploys")
	if !strings.Contains(body, `id="page-project-errors"`) {
		t.Fatal("list chrome")
	}
	if !strings.Contains(body, "nil map") || !strings.Contains(body, "location=gke.cluster-rcf2") || !strings.Contains(body, "name=api") {
		t.Fatalf("list row href missing locator:\n%s", body)
	}
	if !strings.Contains(body, "2h ago") {
		t.Fatalf("last seen should be relative age:\n%s", body)
	}
	if strings.Contains(body, " UTC") {
		t.Fatalf("last seen should not be an absolute UTC timestamp:\n%s", body)
	}
	assertNavActive(t, body, "Errors")

	code, miss := errorsStatusBody(t, srv, "/projects/proj/errors/deploys/missing?location=gke.cluster-rcf2&name=api")
	if code != http.StatusNotFound || miss != errorNotFound {
		// Error() may wrap; require same body as unseen.
		if code != http.StatusNotFound || !strings.Contains(miss, errorNotFound) {
			t.Fatalf("missing: %d %q", code, miss)
		}
	}
	unseen := miss

	detail := getBody(t, srv.Handler(), "/projects/proj/errors/deploys/iss_go_nilmap?location=gke.cluster-rcf2&name=api")
	if !strings.Contains(detail, `id="page-project-error"`) {
		t.Fatal("detail marker")
	}
	if !strings.Contains(detail, "panic: assignment to entry in nil map") {
		t.Fatal("sample missing on web")
	}
	if !strings.Contains(detail, `id="error-sample"`) {
		t.Fatal("sample marker")
	}
	if !strings.Contains(detail, "last 2h ago") {
		t.Fatalf("detail last seen should be relative age:\n%s", detail)
	}

	code, body404 := errorsStatusBody(t, srv, "/projects/proj/errors/deploys/no-such?location=gke.cluster-rcf2&name=api")
	if code != http.StatusNotFound || !strings.Contains(body404, errorNotFound) {
		t.Fatalf("unseen: %d %s", code, body404)
	}
	if !strings.Contains(unseen, errorNotFound) {
		t.Fatal("missing vs unseen body")
	}
	if *apiHits == 0 {
		t.Fatal("expected deploys HTTP")
	}
}

func TestErrorsDeploysGetNoDefaultFill(t *testing.T) {
	hits := 0
	srv, cfg, _ := testServer(t)
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "gke.cluster-rcf2", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(api.Close)
	srv.deploysNew = func(opts deploys.Options) *deploys.Client {
		c := deploys.New(opts)
		c.Endpoint = api.URL
		c.HTTP = api.Client()
		return c
	}
	code, body := errorsStatusBody(t, srv, "/projects/proj/errors/deploys/iss1")
	if code != http.StatusBadRequest || !strings.Contains(body, "location") {
		t.Fatalf("%d %s", code, body)
	}
	if hits != 0 {
		t.Fatalf("default fill hit HTTP %d times", hits)
	}
}

func TestErrorsInvestigateThenFixGates(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.StartSessions = true
	cfg.GrokBin = writeWebFakeGrok(t)
	cfg.ClaudeBin = writeWebFakeClaude(t)
	cfg.TimeoutMs = 5000
	cfg.WorktreeIsolation = new(false)
	cfg.SummarizeThreadTitle = new(false)
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"issue": map[string]any{"id": "iss1", "deployment": "api", "location": "loc", "title": "boom"},
			},
		})
	}))
	t.Cleanup(api.Close)
	srv.deploysNew = func(opts deploys.Options) *deploys.Client {
		c := deploys.New(opts)
		c.Endpoint = api.URL
		c.HTTP = api.Client()
		return c
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	bot.SetThreadAPIForTest(srv.bot, &bot.FakeThreadAPI{NextTh: "th-err-1", NextMsg: "m1"})
	t.Cleanup(func() { bot.WaitIdleForTest(srv.bot, 5*time.Second) })

	sid, csrf, err := srv.LoginAs("member-1", "Inv", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/errors/deploys/iss1/investigate", sid, csrf, url.Values{
		"location": {"loc"}, "name": {"api"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("investigate status=%d body=%s", w.Code, w.Body.String())
	}
	w = postFix(t, srv, "/projects/proj/errors/deploys/iss1/fix", sid, csrf, url.Values{
		"location": {"loc"}, "name": {"api"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("fix as investigator status=%d body=%s", w.Code, w.Body.String())
	}

	// Disabled src POST is 404 (same as GET).
	w = postFix(t, srv, "/projects/proj/errors/sentry/x/investigate", sid, csrf, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled src post=%d", w.Code)
	}
	w = postFix(t, srv, "/projects/proj/errors/deploys/iss1/investigate", sid, csrf, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing locator post=%d body=%s", w.Code, w.Body.String())
	}
}

func TestErrorsGrokBannerCopy(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.StartSessions = true
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"issue": map[string]any{"id": "iss1", "deployment": "api", "location": "loc", "title": "boom", "sampleMessage": "stack-here"},
			},
		})
	}))
	t.Cleanup(api.Close)
	srv.deploysNew = func(opts deploys.Options) *deploys.Client {
		c := deploys.New(opts)
		c.Endpoint = api.URL
		c.HTTP = api.Client()
		return c
	}

	adminSid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/errors/deploys/iss1?location=loc&name=api", adminSid)
	if !strings.Contains(body, `id="error-grok-banner"`) || !strings.Contains(body, "Pick Claude") {
		t.Fatalf("builder banner:\n%s", body)
	}
	for _, want := range []string{
		`id="btn-fix-error"`,
		`>Fix</button>`,
		`data-confirm-title="Fix"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in Fix UI:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Fix with Grok") {
		t.Fatal("stale Grok-branded Fix label")
	}
	if i := strings.Index(body, `id="btn-fix-error"`); i >= 0 {
		j := strings.Index(body[i:], `id="error-grok-banner"`)
		k := strings.Index(body[i:], `id="btn-investigate-error"`)
		if j >= 0 && k >= 0 && j < k {
			t.Fatal("banner between Fix and Investigate — must not attach to Fix")
		}
	}

	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "Inv", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body = getAuthed(t, srv, "/projects/proj/errors/deploys/iss1?location=loc&name=api", sid)
	if !strings.Contains(body, "Ask a builder") {
		t.Fatalf("investigator copy:\n%s", body)
	}
	if strings.Contains(body, "Pick Claude") {
		t.Fatal("investigator must not be told to pick Claude")
	}

	e := sessionstore.Entry{Project: "proj", Mode: bot.ModeInvestigate, Agent: "grok"}
	_ = e.UpsertError(sessionstore.TrackedError{Provider: "deploys", ID: "iss1", Location: "loc", Resource: "api"})
	if err := srv.sessions.Set("th-banner", e); err != nil {
		t.Fatal(err)
	}
	sessBody := getAuthed(t, srv, "/sessions/th-banner?project=proj", sid)
	if !strings.Contains(sessBody, `id="error-grok-banner"`) {
		t.Fatal("session page banner")
	}
}

func deploysErrorsFixture(t *testing.T) (*Server, *int) {
	t.Helper()
	hits := 0
	srv, cfg, _ := testServer(t)
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "gke.cluster-rcf2", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, _ := io.ReadAll(r.Body)
		action := strings.TrimPrefix(r.URL.Path, "/")
		switch action {
		case "error.list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"issues": []map[string]any{{
						"id": "iss_go_nilmap", "deployment": "api", "location": "gke.cluster-rcf2",
						"title": "nil map", "status": "open", "count": 4,
						"lastSeen": time.Now().UTC().Add(-2 * time.Hour),
					}},
				},
			})
		case "error.get":
			var req deploys.GetReq
			_ = json.Unmarshal(body, &req)
			if req.ID != "iss_go_nilmap" {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not found"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"issue": map[string]any{
						"id": "iss_go_nilmap", "deployment": "api", "location": "gke.cluster-rcf2",
						"title": "nil map", "status": "open", "count": 4,
						"lastSeen":      time.Now().UTC().Add(-2 * time.Hour),
						"sampleMessage": "panic: assignment to entry in nil map",
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	srv.deploysNew = func(opts deploys.Options) *deploys.Client {
		c := deploys.New(opts)
		c.Endpoint = api.URL
		c.HTTP = api.Client()
		return c
	}
	return srv, &hits
}

func errorsStatusBody(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

type staticGCPTok string

func (s staticGCPTok) Token(context.Context) (string, error) { return string(s), nil }

type sentryRewrite struct{ base string }

func (r sentryRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := http.NewRequestWithContext(req.Context(), req.Method, r.base+req.URL.RequestURI(), req.Body)
	if err != nil {
		return nil, err
	}
	u.Header = req.Header
	return http.DefaultTransport.RoundTrip(u)
}

func TestErrorResolveDeploysBuilder(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.StartSessions = true
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	var updates []deploys.UpdateReq
	const stack = "panic: assignment to entry in nil map STACK-SHOULD-NOT-AUDIT"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "error.get":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"issue": map[string]any{
						"id": "iss1", "deployment": "api", "location": "loc",
						"title": "boom", "status": "open", "sampleMessage": stack,
					},
				},
			})
		case "error.update":
			var req deploys.UpdateReq
			_ = json.Unmarshal(body, &req)
			updates = append(updates, req)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	srv.deploysNew = func(opts deploys.Options) *deploys.Client {
		c := deploys.New(opts)
		c.Endpoint = api.URL
		c.HTTP = api.Client()
		return c
	}

	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	detail := getAuthed(t, srv, "/projects/proj/errors/deploys/iss1?location=loc&name=api", sid)
	if !strings.Contains(detail, `id="btn-resolve-error"`) {
		t.Fatalf("builder resolve button:\n%s", detail)
	}
	if strings.Contains(detail, `id="btn-reopen-error"`) {
		t.Fatal("open error should not show Reopen")
	}
	if !strings.Contains(detail, "deploys.app") {
		t.Fatal("confirm copy should name deploys.app")
	}

	w := postFix(t, srv, "/projects/proj/errors/deploys/iss1/resolve", sid, csrf, url.Values{
		"location": {"loc"}, "name": {"api"}, "status": {"resolved"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "ok=Resolved") {
		t.Fatalf("Location=%q", loc)
	}
	if len(updates) != 1 || updates[0].Project != "acme" || updates[0].ID != "iss1" || updates[0].Status != "resolved" {
		t.Fatalf("updates=%+v", updates)
	}
	if updates[0].Location != "loc" || updates[0].Name != "api" {
		t.Fatalf("locator=%+v", updates[0])
	}

	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, ev := range evs {
		if ev.Action != audit.ActionErrorResolve {
			continue
		}
		saw = true
		if !ev.OK || ev.Actor != "member-1" {
			t.Fatalf("%+v", ev)
		}
		if fmtDetail := fmtAudit(ev.Detail); strings.Contains(fmtDetail, "STACK-SHOULD-NOT-AUDIT") {
			t.Fatalf("stack in audit: %s", fmtDetail)
		}
	}
	if !saw {
		t.Fatalf("missing audit: %+v", evs)
	}
}

func fmtAudit(d map[string]any) string {
	b, _ := json.Marshal(d)
	return string(b)
}

func TestErrorResolveInvestigatorForbidden(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.StartSessions = true
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	hits := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"issue": map[string]any{"id": "iss1", "deployment": "api", "location": "loc", "title": "boom", "status": "open"},
			},
		})
	}))
	t.Cleanup(api.Close)
	srv.deploysNew = func(opts deploys.Options) *deploys.Client {
		c := deploys.New(opts)
		c.Endpoint = api.URL
		c.HTTP = api.Client()
		return c
	}
	sid, csrf, err := srv.LoginAs("member-1", "Inv", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getAuthed(t, srv, "/projects/proj/errors/deploys/iss1?location=loc&name=api", sid)
	if strings.Contains(body, `id="btn-resolve-error"`) {
		t.Fatal("investigator must not see Resolve")
	}
	getHits := hits
	w := postFix(t, srv, "/projects/proj/errors/deploys/iss1/resolve", sid, csrf, url.Values{
		"location": {"loc"}, "name": {"api"}, "status": {"resolved"},
	})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), errorCannotResolve) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if hits != getHits {
		t.Fatalf("POST must not hit provider (hits %d → %d)", getHits, hits)
	}
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, ev := range evs {
		if ev.Action == audit.ActionErrorResolve && !ev.OK {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("denial audit missing: %+v", evs)
	}
}

func TestErrorResolveStartSessionsOff404(t *testing.T) {
	srv, cfg, _ := authOnServer(t) // startSessions false
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/errors/deploys/iss1/resolve", sid, csrf, url.Values{
		"location": {"loc"}, "name": {"api"}, "status": {"resolved"},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestErrorResolveOpenLAN404(t *testing.T) {
	srv, cfg, _ := testServer(t)
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/errors/deploys/iss1/resolve", strings.NewReader("status=resolved&location=loc&name=api"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestErrorResolveOtherProjectForbidden(t *testing.T) {
	srv := twoProjectAuthServer(t)
	srv.cfg.WebAuth.Features.StartSessions = true
	if err := srv.cfg.SetProjectErrorsDeploys("secret", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/secret/errors/deploys/iss1/resolve", sid, csrf, url.Values{
		"location": {"loc"}, "name": {"api"}, "status": {"resolved"},
	})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "forbidden") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestErrorResolveDisabledSrc404(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.StartSessions = true
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/errors/sentry/x/resolve", sid, csrf, url.Values{"status": {"resolved"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestErrorResolveDeploysMissingLocator400(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.StartSessions = true
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	hits := 0
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(api.Close)
	srv.deploysNew = func(opts deploys.Options) *deploys.Client {
		c := deploys.New(opts)
		c.Endpoint = api.URL
		c.HTTP = api.Client()
		return c
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/errors/deploys/iss1/resolve", sid, csrf, url.Values{"status": {"resolved"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "location") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if hits != 0 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestErrorResolveSentrySlugMismatchNoPUT(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.StartSessions = true
	if err := cfg.SetProjectErrorsSentry("proj", true, "acme", "web", "tok", "", false); err != nil {
		t.Fatal(err)
	}
	puts := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.URL.Path, "/events/latest") {
			io.WriteString(w, `{"dateCreated":"2026-08-01T00:00:00Z","entries":[]}`)
			return
		}
		io.WriteString(w, `{"id":"9","shortId":"APP-1A","title":"boom","count":"1","project":{"slug":"other"}}`)
	}))
	t.Cleanup(api.Close)
	srv.sentryNew = func(token, org, project, baseURL string) *sentrycli.Client {
		c := sentrycli.New(token, org, project, "https://sentry.test")
		c.HTTP = &http.Client{Transport: sentryRewrite{base: api.URL}}
		return c
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/errors/sentry/9/resolve", sid, csrf, url.Values{"status": {"resolved"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if puts != 0 {
		t.Fatalf("PUT hits=%d", puts)
	}
}

func TestErrorResolveGCPReopenButton(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.StartSessions = true
	if err := cfg.SetProjectErrorsGCP("proj", true, "acme", "", "", ""); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/groupStats") {
			io.WriteString(w, `{"errorGroupStats":[{"group":{"groupId":"g1","resolutionStatus":"RESOLVED"},"count":"1","representative":{"message":"boom"}}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			io.WriteString(w, `{"errorEvents":[]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)
	srv.gcpNew = func(string) *gcperr.Client {
		return &gcperr.Client{
			ProjectID: "acme",
			Tokens:    staticGCPTok("t"),
			HTTP:      api.Client(),
			Endpoint:  api.URL,
		}
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getAuthed(t, srv, "/projects/proj/errors/gcp/g1", sid)
	if !strings.Contains(body, `id="btn-reopen-error"`) {
		t.Fatalf("want Reopen:\n%s", body)
	}
	if strings.Contains(body, `id="btn-resolve-error"`) {
		t.Fatal("resolved should not show Resolve")
	}
}
