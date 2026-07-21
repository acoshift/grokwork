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
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func writeEnabledServer(t *testing.T) (*Server, *config.Config, *[]string) {
	t.Helper()
	srv, cfg, _ := authOnServer(t)
	// Enable write features
	cfg.WebAuth.Features.GitHubWrites = true
	cfg.WebAuth.Features.Merge = true
	cfg.WebMergeMethod = "squash"
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	var calls []string
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		calls = append(calls, joined)
		switch {
		case strings.Contains(joined, "issue comment"):
			// verify body-file
			for i, a := range args {
				if a == "--body-file" && i+1 < len(args) {
					b, err := os.ReadFile(args[i+1])
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(b), "hello issue") {
						t.Fatalf("body=%q", b)
					}
				}
			}
			return []byte("ok"), nil
		case strings.Contains(joined, "issue close"):
			return []byte("ok"), nil
		case strings.Contains(joined, "issue view"):
			return []byte(`{
				"number":7,"url":"https://github.com/acme/app/issues/7","title":"Open bug",
				"state":"OPEN","author":{"login":"alice"},"labels":[],
				"body":"steps","comments":[]
			}`), nil
		case strings.Contains(joined, "pr comment"):
			return []byte("ok"), nil
		case strings.Contains(joined, "pr close"):
			return []byte("ok"), nil
		case strings.Contains(joined, "pr view"):
			return []byte(`{
				"number":9,"url":"https://github.com/acme/app/pull/9","title":"T","state":"OPEN",
				"isDraft":false,"reviewDecision":"APPROVED","headRefOid":"a","headRefName":"f",
				"baseRefName":"main","body":"b","mergeable":"MERGEABLE","author":{"login":"z"},
				"additions":1,"deletions":0,"changedFiles":1
			}`), nil
		case strings.Contains(joined, "pr checks"):
			return []byte(`[{"name":"ci","state":"SUCCESS","bucket":"pass"}]`), nil
		case strings.Contains(joined, "pr merge"):
			if strings.Contains(joined, "--admin") {
				t.Fatal("admin flag")
			}
			if !strings.Contains(joined, "--squash") {
				t.Fatalf("want squash: %s", joined)
			}
			if !strings.Contains(joined, "--repo acme/app") {
				t.Fatalf("want --repo: %s", joined)
			}
			return []byte("merged"), nil
		default:
			t.Fatalf("unexpected: %s", joined)
			return nil, nil
		}
	}
	return srv, cfg, &calls
}

func TestFeatureOffRejectsWrites(t *testing.T) {
	srv, _, _ := authOnServer(t) // features default false
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"body": {"x"}, "owner": {"a"}, "repo": {"b"}, "csrf": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/issues/1/comments", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 feature off", w.Code)
	}
}

func TestMemberCommentAndClose(t *testing.T) {
	srv, _, calls := writeEnabledServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// Issue comment
	form := url.Values{
		"body": {"hello issue"}, "owner": {"acme"}, "repo": {"app"}, "csrf": {csrf},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/issues/7/comments", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("comment status=%d body=%s", w.Code, w.Body.String())
	}
	if len(*calls) == 0 || !strings.Contains((*calls)[0], "issue comment") {
		t.Fatalf("calls=%v", *calls)
	}
	// Issue close with comment
	*calls = nil
	form = url.Values{
		"body": {"hello issue"}, "owner": {"acme"}, "repo": {"app"}, "csrf": {csrf},
	}
	req = httptest.NewRequest(http.MethodPost, "/projects/proj/issues/7/close", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("issue close status=%d body=%s", w.Code, w.Body.String())
	}
	if len(*calls) != 2 ||
		!strings.Contains((*calls)[0], "issue comment") ||
		!strings.Contains((*calls)[1], "issue close") {
		t.Fatalf("issue close calls=%v", *calls)
	}
	// PR close
	form = url.Values{"project": {"proj"}, "csrf": {csrf}}
	req = httptest.NewRequest(http.MethodPost, "/prs/acme/app/9/close", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("close status=%d", w.Code)
	}
	// Audit
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var sawComment, sawIssueClose, sawPRClose bool
	for _, ev := range evs {
		if ev.Action == audit.ActionIssueComment && ev.OK && ev.Actor == "member-1" {
			sawComment = true
		}
		if ev.Action == audit.ActionIssueClose && ev.OK && ev.Actor == "member-1" {
			sawIssueClose = true
		}
		if ev.Action == audit.ActionPRClose && ev.OK {
			sawPRClose = true
		}
	}
	if !sawComment || !sawIssueClose || !sawPRClose {
		t.Fatalf("audit missing: %+v", evs)
	}
}

func TestIssueCloseRequiresBody(t *testing.T) {
	srv, _, calls := writeEnabledServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"body": {"  "}, "owner": {"acme"}, "repo": {"app"}, "csrf": {csrf},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/issues/7/close", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("Location=%q want err", loc)
	}
	if len(*calls) > 0 {
		t.Fatalf("gh should not run: %v", *calls)
	}
}

func TestMemberCanMerge(t *testing.T) {
	srv, _, calls := writeEnabledServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"project": {"proj"}, "csrf": {csrf}, "method": {"squash"}}
	req := httptest.NewRequest(http.MethodPost, "/prs/acme/app/9/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	joined := strings.Join(*calls, " | ")
	if !strings.Contains(joined, "pr merge") || !strings.Contains(joined, "--squash") {
		t.Fatalf("calls=%v", *calls)
	}
}

func TestViewerCannotMerge(t *testing.T) {
	srv, _, _ := writeEnabledServer(t)
	sid, csrf, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"project": {"proj"}, "csrf": {csrf}, "method": {"squash"}}
	req := httptest.NewRequest(http.MethodPost, "/prs/acme/app/9/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", w.Code)
	}
}

func TestAdminMergeUpdatesSessions(t *testing.T) {
	srv, _, calls := writeEnabledServer(t)
	// Two sessions tracking the PR
	pr := sessionstore.TrackedPR{
		URL: "https://github.com/acme/app/pull/9", Number: 9, State: "OPEN",
		Owner: "acme", Repo: "app",
	}
	_ = srv.sessions.Set("th-a", sessionstore.Entry{Project: "proj", PRs: []sessionstore.TrackedPR{pr}})
	_ = srv.sessions.Set("th-b", sessionstore.Entry{Project: "proj", PRs: []sessionstore.TrackedPR{pr}})

	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"project": {"proj"}, "csrf": {csrf}, "method": {"squash"}}
	req := httptest.NewRequest(http.MethodPost, "/prs/acme/app/9/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("merge status=%d body=%s", w.Code, w.Body.String())
	}
	joined := strings.Join(*calls, " | ")
	if !strings.Contains(joined, "pr merge") || !strings.Contains(joined, "--squash") {
		t.Fatalf("calls=%v", *calls)
	}
	// Sessions updated or cleaned
	for _, id := range []string{"th-a", "th-b"} {
		if e, ok := srv.sessions.Get(id); ok {
			e.NormalizePRs()
			if e.PRs[0].State != "MERGED" {
				t.Fatalf("%s state=%s", id, e.PRs[0].State)
			}
		}
	}
	evs, _ := srv.audit.ReadDay(time.Now())
	found := false
	for _, ev := range evs {
		if ev.Action == audit.ActionPRMerge && ev.OK && ev.Actor == "admin-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no merge audit: %+v", evs)
	}
}

func TestMergePreflightConflict(t *testing.T) {
	srv, _, calls := writeEnabledServer(t)
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		*calls = append(*calls, joined)
		if strings.HasPrefix(joined, "pr view") {
			return []byte(`{
				"number":9,"url":"https://github.com/acme/app/pull/9","title":"T","state":"OPEN",
				"isDraft":false,"reviewDecision":"","headRefOid":"a","headRefName":"f",
				"baseRefName":"main","body":"","mergeable":"CONFLICTING","author":{"login":"z"},
				"additions":0,"deletions":0,"changedFiles":0
			}`), nil
		}
		if strings.HasPrefix(joined, "pr checks") {
			return []byte(`[]`), nil
		}
		if strings.Contains(joined, "pr merge") {
			t.Fatal("should not merge")
		}
		return nil, nil
	}
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"project": {"proj"}, "csrf": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/prs/acme/app/9/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	// redirect with err
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") || !strings.Contains(loc, "conflict") && !strings.Contains(strings.ToLower(loc), "conflict") {
		// query escaped
		if !strings.Contains(loc, "err") {
			t.Fatalf("Location=%q", loc)
		}
	}
}

func TestOffCatalogWriteRejected(t *testing.T) {
	srv, _, calls := writeEnabledServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// evil/other is not in proj catalog (acme/app only)
	form := url.Values{
		"body": {"x"}, "owner": {"evil"}, "repo": {"other"}, "csrf": {csrf},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/issues/1/comments", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	// redirect with err, or 403
	if w.Code == http.StatusOK {
		t.Fatal("should not succeed")
	}
	// Must not call gh for off-catalog
	for _, c := range *calls {
		if strings.Contains(c, "issue comment") {
			t.Fatalf("gh called for off-catalog write: %s", c)
		}
	}
	// PR path
	form = url.Values{"project": {"proj"}, "csrf": {csrf}}
	req = httptest.NewRequest(http.MethodPost, "/prs/evil/other/1/close", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	for _, c := range *calls {
		if strings.Contains(c, "pr close") {
			t.Fatalf("gh close called off-catalog: %s", c)
		}
	}
}

func TestIssueDetailShowsCommentAndClose(t *testing.T) {
	srv, _, _ := writeEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues/7?owner=acme&repo=app", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="issue-comment-form"`,
		`id="btn-issue-close"`,
		`formaction="/projects/proj/issues/7/close"`,
		`confirm('Post this comment and close the issue?')`,
		"Post comment",
		"Post comment &amp; close",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
}

func TestPRDetailShowsWriteFormsForAdmin(t *testing.T) {
	srv, _, _ := writeEnabledServer(t)
	sid, _, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
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
	for _, want := range []string{
		`id="pr-comment-form"`,
		`id="pr-close-form"`,
		`id="pr-merge-form"`,
		`confirm('Close this pull request?')`,
		`confirm('Merge this pull request?')`,
		"squash",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestPRDetailShowsWriteFormsForMember(t *testing.T) {
	srv, _, _ := writeEnabledServer(t)
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
	for _, want := range []string{
		`id="pr-comment-form"`,
		`id="pr-close-form"`,
		`id="pr-merge-form"`,
		`confirm('Close this pull request?')`,
		`confirm('Merge this pull request?')`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestPRDetailHidesWriteFormsForViewer(t *testing.T) {
	srv, _, _ := writeEnabledServer(t)
	sid, _, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
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
	for _, hide := range []string{`id="pr-close-form"`, `id="pr-merge-form"`, `id="pr-comment-form"`} {
		if strings.Contains(body, hide) {
			t.Fatalf("viewer should not see %q", hide)
		}
	}
}
