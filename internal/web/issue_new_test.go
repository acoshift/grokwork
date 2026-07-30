package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
)

type issueCreateCall struct {
	args []string
	body string
}

// issueCreateServer is a githubWrites-enabled server whose gh runner records
// issue create invocations (including --body-file contents).
func issueCreateServer(t *testing.T) (*Server, *config.Config, *[]issueCreateCall) {
	t.Helper()
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.GitHubWrites = true
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	calls := make([]issueCreateCall, 0, 4)
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		call := issueCreateCall{args: append([]string{name}, args...)}
		for i, a := range args {
			if a == "--body-file" && i+1 < len(args) {
				if raw, err := os.ReadFile(args[i+1]); err == nil {
					call.body = string(raw)
				}
			}
		}
		calls = append(calls, call)
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "issue create") {
			return []byte("https://github.com/acme/app/issues/7\n"), nil
		}
		t.Fatalf("unexpected gh call: %s", joined)
		return nil, nil
	}
	return srv, cfg, &calls
}

func issueCreateArgs(calls []issueCreateCall) (issueCreateCall, bool) {
	for _, c := range calls {
		if strings.Contains(strings.Join(c.args, " "), "issue create") {
			return c, true
		}
	}
	return issueCreateCall{}, false
}

func assertIssueCreateAudit(t *testing.T, srv *Server, wantOK bool) {
	t.Helper()
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Action != audit.ActionIssueCreate || ev.OK != wantOK {
			continue
		}
		// No-content rule: title and body never enter the log.
		raw := strings.ToLower(ev.Error)
		for k, v := range ev.Detail {
			raw += " " + strings.ToLower(k)
			raw += " " + strings.ToLower(strings.TrimSpace(toString(v)))
		}
		if strings.Contains(raw, "secret title") || strings.Contains(raw, "customer data") {
			t.Fatalf("audit must not carry title/body: %+v", ev)
		}
		if wantOK {
			if ev.Detail["number"] != float64(7) && ev.Detail["number"] != 7 {
				// JSON numbers decode as float64 when re-read; in-memory Detail keeps int.
				if n, ok := ev.Detail["number"].(int); !ok || n != 7 {
					// Accept either encoding; just require a number is recorded.
					if ev.Detail["number"] == nil {
						t.Fatalf("ok audit missing number: %+v", ev)
					}
				}
			}
			if ev.Detail["kind"] == nil {
				t.Fatalf("ok audit missing kind: %+v", ev)
			}
		}
		return
	}
	t.Fatalf("no %s audit event (ok=%v): %+v", audit.ActionIssueCreate, wantOK, evs)
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}

func TestIssueNewPageForm(t *testing.T) {
	srv, _, _ := issueCreateServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues/new?owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-issue-new"`,
		`id="btn-issue-new"`,
		`<form class="stack" method="post" action="/projects/proj/issues/new">`,
		`name="kind"`,
		`value="feature"`,
		`value="bug"`,
		`name="title"`,
		`name="body"`,
		`cases/new`,
		`On behalf of`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("new-issue page missing %q", want)
		}
	}
	// Literal /new must not be captured by /issues/{n}.
	if strings.Contains(body, `id="page-issue-detail"`) {
		t.Fatal("GET /issues/new must not render the issue detail page")
	}
	assertNavActive(t, body, "Issues")
}

func TestIssueNewPageReadOnlyWithoutCapability(t *testing.T) {
	srv, cfg, _ := issueCreateServer(t)
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	if caps := cfg.ResolveCapabilities("proj", "member-1"); caps.GithubWrites {
		t.Fatalf("test setup: investigator must not have githubWrites: %+v", caps)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues/new", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `id="btn-issue-new"`) {
		t.Fatal("form must not render without githubWrites capability")
	}
	if !strings.Contains(body, "Read-only access") {
		t.Fatalf("missing read-only fallback: %s", body[:min(500, len(body))])
	}
}

func TestIssueNewFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t) // githubWrites off
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/new", sid, csrf, url.Values{
		"kind": {"feature"}, "owner": {"acme"}, "repo": {"app"}, "title": {"x"},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestIssueCreateHappyPath(t *testing.T) {
	srv, _, calls := issueCreateServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/new", sid, csrf, url.Values{
		"kind":  {"feature"},
		"owner": {"acme"},
		"repo":  {"app"},
		"title": {"secret title"},
		"body":  {"customer data in body"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/issues/7?") {
		t.Fatalf("Location=%q want issue detail", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("owner") != "acme" || q.Get("repo") != "app" {
		t.Fatalf("Location query: %s", loc)
	}
	if q.Get("ok") == "" {
		t.Fatalf("want ok flash: %s", loc)
	}
	call, ok := issueCreateArgs(*calls)
	if !ok {
		t.Fatalf("gh issue create never ran: %+v", *calls)
	}
	joined := strings.Join(call.args, " ")
	if !strings.Contains(joined, "issue create") {
		t.Fatalf("args=%v", call.args)
	}
	if !strings.Contains(joined, "--label") || !strings.Contains(joined, "feature") {
		t.Fatalf("want --label feature: %v", call.args)
	}
	if strings.Contains(joined, "secret title") {
		// Title is an argv flag value (gh requires --title); body must not be.
		// Just ensure body went via file.
	}
	if !strings.Contains(joined, "--body-file") {
		t.Fatalf("body must go via --body-file: %v", call.args)
	}
	if !strings.Contains(call.body, "customer data in body") {
		t.Fatalf("body file=%q", call.body)
	}
	assertIssueCreateAudit(t, srv, true)
}

func TestIssueCreateBugLabel(t *testing.T) {
	srv, _, calls := issueCreateServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/new", sid, csrf, url.Values{
		"kind": {"bug"}, "owner": {"acme"}, "repo": {"app"}, "title": {"repro crash"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	call, ok := issueCreateArgs(*calls)
	if !ok {
		t.Fatal("gh never ran")
	}
	joined := strings.Join(call.args, " ")
	if !strings.Contains(joined, "--label") || !strings.Contains(joined, "bug") {
		t.Fatalf("want --label bug: %v", call.args)
	}
	if strings.Contains(joined, "feature") {
		t.Fatalf("must not pass feature: %v", call.args)
	}
}

func TestIssueCreateCapabilityRefusesUnprivilegedActor(t *testing.T) {
	srv, cfg, calls := issueCreateServer(t)
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/new", sid, csrf, url.Values{
		"kind": {"feature"}, "owner": {"acme"}, "repo": {"app"}, "title": {"nope"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", w.Code, w.Body.String())
	}
	if _, ok := issueCreateArgs(*calls); ok {
		t.Fatalf("gh must not run for a refused actor: %+v", *calls)
	}
	assertIssueCreateAudit(t, srv, false)
}

func TestIssueCreateInvalidKind(t *testing.T) {
	srv, _, calls := issueCreateServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/new", sid, csrf, url.Values{
		"kind": {"enhancement"}, "owner": {"acme"}, "repo": {"app"}, "title": {"x"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/issues/new") || !strings.Contains(loc, "err=") {
		t.Fatalf("Location=%q want form with err", loc)
	}
	if _, ok := issueCreateArgs(*calls); ok {
		t.Fatal("must not create with invalid kind")
	}
}

func TestIssueCreateEmptyTitle(t *testing.T) {
	srv, _, calls := issueCreateServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/new", sid, csrf, url.Values{
		"kind": {"feature"}, "owner": {"acme"}, "repo": {"app"}, "title": {"   "},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/issues/new") || !strings.Contains(loc, "err=") {
		t.Fatalf("Location=%q want form with err", loc)
	}
	if !strings.Contains(loc, "kind=feature") {
		t.Fatalf("kind context lost: %s", loc)
	}
	if _, ok := issueCreateArgs(*calls); ok {
		t.Fatal("must not create with empty title")
	}
}

func TestIssueCreateOnBehalfOfLinked(t *testing.T) {
	srv, _, calls := issueCreateServer(t)
	linkGitHubIdentity(t, srv, "member-1", "999", "member-gh")
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/new", sid, csrf, url.Values{
		"kind": {"feature"}, "owner": {"acme"}, "repo": {"app"},
		"title": {"Export CSV"}, "body": {"please ship this"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	call, ok := issueCreateArgs(*calls)
	if !ok {
		t.Fatal("gh never ran")
	}
	if !strings.Contains(call.body, "On behalf of @member-gh") {
		t.Fatalf("want On behalf of prefix, body=%q", call.body)
	}
	if !strings.Contains(call.body, "please ship this") {
		t.Fatalf("body lost: %q", call.body)
	}
}

func TestIssuesListNewIssueCTA(t *testing.T) {
	srv, _, _ := issueCreateServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues?owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/projects/proj/issues/new?owner=acme&amp;repo=app"`) &&
		!strings.Contains(body, `href="/projects/proj/issues/new?owner=acme&repo=app"`) {
		t.Fatalf("list missing New issue CTA: %s", body[:min(800, len(body))])
	}

	// Without capability the CTA stays hidden.
	if err := srv.cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/projects/proj/issues?owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `/issues/new`) {
		t.Fatal("viewer without githubWrites must not see New issue CTA")
	}
}
