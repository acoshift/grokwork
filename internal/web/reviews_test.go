package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/reviewstore"
)

func reviewEnabledServer(t *testing.T) (*Server, *config.Config) {
	t.Helper()
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.PRReviews = true
	cfg.WebAuth.Features.GitHubWrites = true
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	// Ensure bot has a review store (bot.New may have created one under temp dataDir).
	if srv.bot.Reviews() == nil {
		rev, err := reviewstore.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		srv.bot.SetReviews(rev)
	}
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "pr comment"):
			return []byte("https://github.com/acme/app/pull/9#issuecomment-1\n"), nil
		case strings.Contains(joined, "pr view"):
			return []byte(`{
				"number":9,"url":"https://github.com/acme/app/pull/9","title":"T","state":"OPEN",
				"isDraft":false,"reviewDecision":"REVIEW_REQUIRED","headRefOid":"abc1234","headRefName":"f",
				"baseRefName":"main","body":"b","mergeable":"MERGEABLE","author":{"login":"z"},
				"additions":1,"deletions":0,"changedFiles":1
			}`), nil
		case strings.Contains(joined, "pr checks"):
			return []byte(`[{"name":"ci","state":"SUCCESS","bucket":"pass"}]`), nil
		default:
			return []byte("{}"), nil
		}
	}
	return srv, cfg
}

func TestPRReviewFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"verdict": {"approved"}, "body": {"ok"}, "headSha": {"abc"}, "csrf": {csrf}, "project": {"proj"}}
	req := httptest.NewRequest(http.MethodPost, "/prs/acme/app/9/reviews", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestSubmitTeamReview(t *testing.T) {
	srv, _ := reviewEnabledServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"verdict": {"approved"}, "body": {"lgtm"}, "headSha": {"abc1234"},
		"csrf": {csrf}, "project": {"proj"}, "mirror": {"1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/prs/acme/app/9/reviews", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	bucket := srv.bot.Reviews().ListForPR("acme", "app", 9)
	if len(bucket.Reviews) != 1 {
		t.Fatalf("reviews=%d", len(bucket.Reviews))
	}
	if bucket.Reviews[0].ReviewerID != "member-1" {
		t.Fatalf("reviewer=%s", bucket.Reviews[0].ReviewerID)
	}
	if bucket.Reviews[0].HeadSHA != "abc1234" {
		t.Fatalf("head=%s", bucket.Reviews[0].HeadSHA)
	}
	if bucket.Reviews[0].GHCommentURL == "" {
		t.Fatal("expected GH mirror URL")
	}
}

func TestMyReviewsPage(t *testing.T) {
	srv, cfg := reviewEnabledServer(t)
	// Member needs project membership for visibility filtering on My reviews.
	if err := cfg.AddProjectAllowedUser("proj", "member-1"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.bot.Reviews().RequestReview(reviewstore.Request{
		Owner: "acme", Repo: "app", Number: 9, Project: "proj",
		RequesterID: "admin-1", ReviewerID: "member-1",
	}); err != nil {
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
	if !strings.Contains(body, `id="page-reviews"`) {
		t.Fatal("missing page marker")
	}
	if !strings.Contains(body, "acme/app#9") {
		t.Fatal("missing PR row")
	}
}

func TestPRDetailShowsTeamReviewForm(t *testing.T) {
	srv, _ := reviewEnabledServer(t)
	sid, _, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/prs/acme/app/9?project=proj", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="pr-review-form"`) {
		t.Fatal("expected review form")
	}
	if !strings.Contains(body, `name="headSha"`) {
		t.Fatal("expected hidden headSha")
	}
}

func TestReviewerOptionsBuilderClassOnly(t *testing.T) {
	srv, cfg := reviewEnabledServer(t)
	if err := cfg.SetProjectSafeTeam("proj", true, "investigator", ""); err != nil {
		t.Fatal(err)
	}
	// allow-user stays unmapped → investigator default under safe team.
	// member-1 explicitly builder; viewer-1 investigator.
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "viewer-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "approver-1", "approver"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddProjectAllowedUser("proj", "approver-1"); err != nil {
		t.Fatal(err)
	}

	opts := srv.reviewerOptions("proj")
	got := map[string]bool{}
	for _, o := range opts {
		got[o.ID] = true
	}
	if !got["member-1"] {
		t.Fatalf("builder missing from options: %+v", opts)
	}
	if !got["approver-1"] {
		t.Fatalf("approver missing from options: %+v", opts)
	}
	if got["viewer-1"] {
		t.Fatal("investigator viewer-1 must not appear in reviewer options")
	}
	if got["allow-user"] {
		t.Fatal("unmapped safe-team user (investigator default) must not appear")
	}

	if !srv.canRequestReviewer("proj", "member-1") {
		t.Fatal("builder should be requestable")
	}
	if !srv.canRequestReviewer("proj", "approver-1") {
		t.Fatal("approver should be requestable")
	}
	if srv.canRequestReviewer("proj", "viewer-1") {
		t.Fatal("investigator must not be requestable")
	}
	if srv.canRequestReviewer("proj", "allow-user") {
		t.Fatal("unmapped investigator default must not be requestable")
	}
}

func TestRequestReviewRejectsInvestigator(t *testing.T) {
	srv, cfg := reviewEnabledServer(t)
	if err := cfg.SetProjectSafeTeam("proj", true, "investigator", ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "viewer-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"reviewerId": {"viewer-1"}, "note": {"please look"}, "headSha": {"abc1234"},
		"csrf": {csrf}, "project": {"proj"},
	}
	req := httptest.NewRequest(http.MethodPost, "/prs/acme/app/9/review-requests", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") || !strings.Contains(loc, "builder-class") {
		t.Fatalf("expected builder-class error redirect, got %q", loc)
	}
	bucket := srv.bot.Reviews().ListForPR("acme", "app", 9)
	if len(bucket.Requests) != 0 {
		t.Fatalf("investigator request must not be saved: %+v", bucket.Requests)
	}
}
