package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/reviewstore"
)

type ghReviewCall struct {
	args []string
	body string
}

// githubReviewServer is a githubWrites-enabled server whose gh runner records
// every invocation (and the contents of any --body-file, which is deleted as
// soon as the call returns).
func githubReviewServer(t *testing.T) (*Server, *config.Config, *[]ghReviewCall) {
	t.Helper()
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.GitHubWrites = true
	cfg.WebAuth.Features.PRReviews = true
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	if srv.bot.Reviews() == nil {
		rev, err := reviewstore.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		srv.bot.SetReviews(rev)
	}
	calls := make([]ghReviewCall, 0, 4)
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		call := ghReviewCall{args: append([]string{name}, args...)}
		for i, a := range args {
			if a == "--body-file" && i+1 < len(args) {
				if raw, err := os.ReadFile(args[i+1]); err == nil {
					call.body = string(raw)
				}
			}
		}
		calls = append(calls, call)
		joined := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "pr review"):
			return []byte("https://github.com/acme/app/pull/9#pullrequestreview-1\n"), nil
		case strings.Contains(joined, "pr view"):
			return []byte(`{
				"number":9,"url":"https://github.com/acme/app/pull/9","title":"T","state":"OPEN",
				"isDraft":false,"reviewDecision":"REVIEW_REQUIRED","headRefOid":"abc1234","headRefName":"f",
				"baseRefName":"main","body":"b","mergeable":"MERGEABLE","author":{"login":"z"},
				"additions":1,"deletions":0,"changedFiles":1
			}`), nil
		case strings.Contains(joined, "pr checks"):
			return []byte(`[{"name":"ci","state":"SUCCESS","bucket":"pass"}]`), nil
		}
		return []byte("{}"), nil
	}
	return srv, cfg, &calls
}

func ghReviewCallArgs(calls []ghReviewCall) (ghReviewCall, bool) {
	for _, c := range calls {
		if strings.Contains(strings.Join(c.args, " "), "pr review") {
			return c, true
		}
	}
	return ghReviewCall{}, false
}

// assertGHReviewAudit is exact where assertAuditAction is loose: a GitHub review
// and its refusal must both be findable, and a refusal must carry its reason.
func assertGHReviewAudit(t *testing.T, srv *Server, wantOK bool, wantVerdict string) {
	t.Helper()
	// The two reviews must stay separable in the log — an auditor asking "who
	// approved this on GitHub" must not get team verdicts back.
	if audit.ActionPRReviewGitHub == audit.ActionPRReviewSubmit {
		t.Fatal("GitHub review must not reuse the team-review action name")
	}
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Action != audit.ActionPRReviewGitHub || ev.OK != wantOK {
			continue
		}
		if wantVerdict != "" && ev.Detail["verdict"] != wantVerdict {
			continue
		}
		if !wantOK && strings.TrimSpace(ev.Error) == "" {
			t.Fatalf("a refused review must record why: %+v", ev)
		}
		return
	}
	t.Fatalf("no %s audit event (ok=%v verdict=%q): %+v", audit.ActionPRReviewGitHub, wantOK, wantVerdict, evs)
}

func TestGitHubReviewFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t) // githubWrites off
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/github-reviews", sid, csrf,
		url.Values{"project": {"proj"}, "verdict": {"approve"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

// The real GitHub review reaches gh with the flag its verdict names — the whole
// point of the action is that GitHub records it, so a mis-mapped flag files the
// wrong verdict under the bot's identity.
func TestSubmitGitHubReviewVerdictReachesGH(t *testing.T) {
	cases := []struct {
		form string
		flag string
		nope string
	}{
		{"approve", "--approve", "--request-changes"},
		{"request-changes", "--request-changes", "--approve"},
		{"comment", "--comment", "--approve"},
	}
	for _, tc := range cases {
		t.Run(tc.form, func(t *testing.T) {
			srv, _, calls := githubReviewServer(t)
			sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
			if err != nil {
				t.Fatal(err)
			}
			w := postFix(t, srv, "/prs/acme/app/9/github-reviews", sid, csrf, url.Values{
				"project": {"proj"}, "verdict": {tc.form}, "body": {"careful here"},
			})
			if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			call, ok := ghReviewCallArgs(*calls)
			if !ok {
				t.Fatalf("gh pr review never ran: %+v", *calls)
			}
			joined := strings.Join(call.args, " ")
			if !strings.Contains(joined, "gh pr review 9") {
				t.Fatalf("args=%v", call.args)
			}
			if !strings.Contains(joined, tc.flag) {
				t.Fatalf("want %s: %v", tc.flag, call.args)
			}
			if strings.Contains(joined, tc.nope) {
				t.Fatalf("must not pass %s: %v", tc.nope, call.args)
			}
			// Body via temp file, never argv.
			if !strings.Contains(joined, "--body-file") || strings.Contains(joined, "careful here") {
				t.Fatalf("body must go via --body-file: %v", call.args)
			}
			if !strings.Contains(call.body, "careful here") {
				t.Fatalf("body file=%q", call.body)
			}
			// The audit records the normalized verdict, i.e. the flag it sent.
			assertGHReviewAudit(t, srv, true, strings.TrimPrefix(tc.flag, "--"))
		})
	}
}

// The capability that governs GitHub writes gates it — hiding the rail action is
// not the gate. An investigator on the project is a member with web access and
// still may not spend the GitHub credential on a review.
func TestGitHubReviewCapabilityRefusesUnprivilegedActor(t *testing.T) {
	srv, cfg, calls := githubReviewServer(t)
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	if caps := cfg.ResolveCapabilities("proj", "member-1"); caps.GithubWrites {
		t.Fatalf("test setup: investigator must not have githubWrites: %+v", caps)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/github-reviews", sid, csrf, url.Values{
		"project": {"proj"}, "verdict": {"approve"}, "body": {"lgtm"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", w.Code, w.Body.String())
	}
	if _, ok := ghReviewCallArgs(*calls); ok {
		t.Fatalf("gh pr review must not run for a refused actor: %+v", *calls)
	}
	// A refusal is exactly what an operator goes looking for.
	assertGHReviewAudit(t, srv, false, "")
}

// GitHub's refusal (self-approval, not a collaborator) is the whole answer and
// must reach the operator rather than being swallowed into a success flash.
func TestGitHubReviewGHFailureSurfaces(t *testing.T) {
	srv, _, _ := githubReviewServer(t)
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "pr review") {
			return nil, fmt.Errorf("gh pr review 9 --approve: can not approve your own pull request")
		}
		return []byte("{}"), nil
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/github-reviews", sid, csrf, url.Values{
		"project": {"proj"}, "verdict": {"approve"}, "body": {"lgtm"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if q.Get("ok") != "" {
		t.Fatalf("failed review must not flash success: %s", loc)
	}
	if !strings.Contains(q.Get("err"), "can not approve your own pull request") {
		t.Fatalf("gh reason lost: %s", loc)
	}
	assertGHReviewAudit(t, srv, false, "")
}

func TestGitHubReviewRejectsUnknownVerdict(t *testing.T) {
	srv, _, calls := githubReviewServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/github-reviews", sid, csrf, url.Values{
		"project": {"proj"}, "verdict": {"lgtm-ish"}, "body": {"x"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	if _, ok := ghReviewCallArgs(*calls); ok {
		t.Fatal("must not guess a verdict")
	}
}

// The two reviews stay visibly separate: one records an internal verdict, the
// other speaks to GitHub. Collapsing the labels is what would make a local
// verdict look like it satisfies branch protection.
func TestPRDetailSeparatesTeamAndGitHubReview(t *testing.T) {
	srv, _, _ := githubReviewServer(t)
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
		`id="pr-review-form"`,                     // local team verdict
		`action="/prs/acme/app/9/reviews"`,        //   posts to the team store
		`id="pr-github-review-form"`,              // real GitHub review
		`action="/prs/acme/app/9/github-reviews"`, //   posts to gh
		"Submit team review",
		"Submit GitHub review",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	// The disclaimer must no longer flatly deny that a branch-protection review
	// is available — it is, right next to it.
	if strings.Contains(body, "Does not satisfy GitHub branch protection.") {
		t.Fatal("stale disclaimer: a real GitHub review is now offered on this page")
	}
	if !strings.Contains(body, "Branch protection is satisfied by") {
		t.Fatalf("blurb should point at the GitHub review action: %s", body[:min(400, len(body))])
	}
}

// Without the capability the action is not rendered — and the blurb goes back to
// saying plainly that a team review satisfies nothing, because for this viewer
// there is no GitHub review action to point at.
func TestPRDetailHidesGitHubReviewWithoutCapability(t *testing.T) {
	srv, cfg, _ := githubReviewServer(t)
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
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
	if strings.Contains(body, `action="/prs/acme/app/9/github-reviews"`) {
		t.Fatal("GitHub review offered without the githubWrites capability")
	}
	if !strings.Contains(body, "does not satisfy GitHub branch protection") {
		t.Fatal("blurb must stay honest when no GitHub review action is shown")
	}
}

// The mirrored PR comment is still only a mirror; now that a real review is
// reachable, it has to name what does count instead of leaving a dead end.
func TestReviewMirrorCommentNamesTheRealGate(t *testing.T) {
	out := formatReviewMirrorComment(reviewstore.Review{
		Verdict: reviewstore.VerdictApproved, ReviewerID: "1", ReviewerName: "A", Body: "lgtm",
	})
	if !strings.Contains(out, "not a GitHub review") {
		t.Fatalf("mirror must still disclaim: %s", out)
	}
	if !strings.Contains(out, "branch protection counts only a GitHub review") {
		t.Fatalf("mirror must name the real gate: %s", out)
	}
}

func TestMyReviewsDisclaimerNamesTheRealGate(t *testing.T) {
	srv, cfg, _ := githubReviewServer(t)
	if err := cfg.AddProjectAllowedUser("proj", "member-1"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/reviews", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Team review is local to Grok Work and does not satisfy GitHub branch protection.") {
		t.Fatal("stale disclaimer: a real GitHub review is now available")
	}
	if !strings.Contains(body, "Branch protection counts only a GitHub review") {
		t.Fatalf("reviews page must name the real gate: %s", body[:min(600, len(body))])
	}
}
