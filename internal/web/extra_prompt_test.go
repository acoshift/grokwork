package web

import (
	"context"
	"encoding/json"
	"io"
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
	"github.com/acoshift/grokwork/internal/clickup"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/errsrc/deploys"
	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const extraNote = "watch the #nil? path & retries"

func TestExtraPromptDispatch(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	spy := &bot.FakeThreadAPI{NextMsg: "m1", NextTh: "fix-extra"}
	bot.SetThreadAPIForTest(b, spy)
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		case name == "git" && len(args) > 0 && args[0] == "show" && strings.Contains(joined, "-s"):
			return []byte(sha + "\x1fFixture commit\x1fAlice\x1fa@ex.com\x1f2026-07-20T12:00:00Z\x1fbody note\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "rev-parse":
			return []byte(sha + "\n"), nil
		case strings.Contains(joined, "issue view"):
			n := "42"
			for i, a := range args {
				if a == "view" && i+1 < len(args) {
					n = args[i+1]
					break
				}
			}
			return []byte(`{
				"number":` + n + `,"title":"Pay bug","body":"steps to repro","url":"https://github.com/acme/app/issues/` + n + `",
				"state":"OPEN","author":{"login":"z"},"labels":[],"comments":[]
			}`), nil
		case strings.Contains(joined, "issue list"):
			return []byte(`[
				{"number":10,"url":"u","title":"Ten","state":"OPEN","author":{"login":"a"},"labels":[],"body":"","closedByPullRequestsReferences":[]},
				{"number":20,"url":"u","title":"Twenty","state":"OPEN","author":{"login":"a"},"labels":[],"body":"","closedByPullRequestsReferences":[]}
			]`), nil
		default:
			return []byte("{}"), nil
		}
	}

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}

	// Review start: posted extra reaches the turn, and the goal stays the commit subject.
	w := postFix(t, srv, "/projects/proj/commits/"+sha+"/review", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "extra": {extraNote},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("review status=%d body=%s", w.Code, w.Body.String())
	}
	reviewID := webUnitFromLocation(t, w.Header().Get("Location"))
	reviewPrompt := waitTurnPrompt(t, srv, reviewID)
	assertTurnHasExtra(t, reviewPrompt, []string{"Do not merge anything", "gh issue create"})
	if e, ok := srv.sessions.Get(reviewID); !ok || strings.Contains(e.Goal, extraNote) || !strings.Contains(e.Goal, "Fixture") {
		t.Fatalf("review goal=%q ok=%v", e.Goal, ok)
	}
	if auditBlob(t, srv, audit.ActionCommitReviewStart); strings.Contains(auditBlob(t, srv, ""), extraNote) {
		t.Fatal("commit-review audit contains the extra text")
	}

	// Omitted field does not add a section. Whitespace-only matches that prompt.
	w = postFix(t, srv, "/projects/proj/commits/"+sha+"/review", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	plainID := webUnitFromLocation(t, w.Header().Get("Location"))
	plain := waitTurnPrompt(t, srv, plainID)
	if strings.Contains(plain, "### Additional instructions") || strings.Contains(plain, extraNote) {
		t.Fatalf("omitted extra changed the review prompt:\n%s", plain)
	}
	w = postFix(t, srv, "/projects/proj/commits/"+sha+"/review", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "extra": {" \n\t "},
	})
	blankID := webUnitFromLocation(t, w.Header().Get("Location"))
	blank := waitTurnPrompt(t, srv, blankID)
	if blank != plain {
		t.Fatalf("whitespace extra changed the review prompt")
	}

	// Fix start. Discord title and starter stay free of the note.
	w = postFix(t, srv, "/projects/proj/issues/42/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "extra": {extraNote},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("fix status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/fix-extra") {
		t.Fatalf("fix location=%q", loc)
	}
	fixPrompt := waitTurnPrompt(t, srv, "fix-extra")
	assertTurnHasExtra(t, fixPrompt, []string{"Do not merge", "acme/app#42"})
	if e, ok := srv.sessions.Get("fix-extra"); !ok || strings.Contains(e.Goal, extraNote) {
		t.Fatalf("fix goal=%q ok=%v", e.Goal, ok)
	}
	discordText := strings.Join(spy.Starts, "\n") + "\n" + strings.Join(spy.Sends, "\n")
	if strings.Contains(discordText, extraNote) {
		t.Fatalf("discord title or starter contains the extra note:\n%s", discordText)
	}
	if !strings.Contains(auditBlob(t, srv, audit.ActionSessionStart), audit.ActionSessionStart) {
		t.Fatal("fix start was not audited")
	}
	if strings.Contains(auditBlob(t, srv, ""), extraNote) {
		t.Fatal("audit log contains the extra text")
	}

	// Reuse of an existing fix session carries the same note onto the next turn.
	// Issue 7 has a single bound session, so discovery takes the reuse path.
	reused := sessionstore.Entry{Project: "proj", Goal: "existing goal"}
	reused.UpsertIssue(sessionstore.TrackedIssue{
		Owner: "acme", Repo: "app", Number: 7, Keyword: sessionstore.IssueKeywordFixes,
	})
	if err := srv.sessions.Set("reuse-fix", reused); err != nil {
		t.Fatal(err)
	}
	w = postFix(t, srv, "/projects/proj/issues/7/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "extra": {extraNote},
	})
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/sessions/reuse-fix") {
		t.Fatalf("reuse location=%q body=%s", loc, w.Body.String())
	}
	reusePrompt := waitTurnPrompt(t, srv, "reuse-fix")
	assertTurnHasExtra(t, reusePrompt, []string{"Do not merge"})
	if e, ok := srv.sessions.Get("reuse-fix"); !ok || e.Goal != "existing goal" {
		t.Fatalf("reuse goal=%q", e.Goal)
	}

	// Bulk Fix puts the same note on every session that request starts.
	spy2 := &bot.FakeThreadAPI{NextMsg: "m1", NextTh: "bulk-extra"}
	bot.SetThreadAPIForTest(b, spy2)
	w = postFix(t, srv, "/projects/proj/issues/fix", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "numbers": {"10", "20"}, "extra": {extraNote},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("bulk status=%d body=%s", w.Code, w.Body.String())
	}
	if spy2.StartCount() != 2 {
		t.Fatalf("bulk creates=%d", spy2.StartCount())
	}
	var first string
	for _, id := range []string{"bulk-extra", "bulk-extra-2"} {
		p := waitTurnPrompt(t, srv, id)
		assertTurnHasExtra(t, p, []string{"Do not merge"})
		if first == "" {
			first = p
		}
		if !strings.Contains(p, extraNote) {
			t.Fatalf("bulk %s missing extra", id)
		}
	}
	if strings.Contains(strings.Join(spy2.Starts, "\n")+strings.Join(spy2.Sends, "\n"), extraNote) {
		t.Fatal("bulk discord title or starter contains the extra note")
	}
	if strings.Contains(auditBlob(t, srv, ""), extraNote) {
		t.Fatal("bulk audit contains the extra text")
	}

	// Implement ignores a forged extra field.
	setIssueViewLabels(t, srv, `[{"name":"plan"}]`)
	w = postFix(t, srv, "/projects/proj/issues/42/implement", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "force_new": {"1"}, "extra": {extraNote},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("implement status=%d body=%s", w.Code, w.Body.String())
	}
	implID := strings.TrimPrefix(w.Header().Get("Location"), "/sessions/")
	if i := strings.IndexAny(implID, "?&"); i >= 0 {
		implID = implID[:i]
	}
	impl := waitTurnPrompt(t, srv, implID)
	if strings.Contains(impl, extraNote) || strings.Contains(impl, "### Additional instructions") {
		t.Fatalf("implement prompt took the extra note:\n%s", impl)
	}
	if !strings.Contains(impl, "/goal") {
		t.Fatalf("implement prompt=%s", impl)
	}

	// A start that is already forbidden stays forbidden when the field is set.
	if err := cfg.AddProjectAllowedUser("proj", "member-2"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "member-2", "investigator"); err != nil {
		t.Fatal(err)
	}
	sid2, csrf2, err := srv.LoginAs("member-2", "Inv", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w = postFix(t, srv, "/projects/proj/issues/42/fix", sid2, csrf2, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "force_new": {"1"}, "extra": {extraNote},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("investigator fix status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(auditBlob(t, srv, ""), extraNote) {
		t.Fatal("refused fix audit contains the extra text")
	}

	vsid, vcsrf, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	w = postFix(t, srv, "/projects/proj/issues/42/fix", vsid, vcsrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "extra": {extraNote},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer fix status=%d", w.Code)
	}
}

func TestExtraPromptAddressAndPRReview(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid, csrf, url.Values{
		"project": {"proj"}, "extra": {extraNote},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("pr review status=%d body=%s", w.Code, w.Body.String())
	}
	id := webUnitFromLocation(t, w.Header().Get("Location"))
	p := waitTurnPrompt(t, srv, id)
	assertTurnHasExtra(t, p, []string{"never merge", "gh pr comment"})
	if e, ok := srv.sessions.Get(id); !ok || strings.Contains(e.Goal, extraNote) {
		t.Fatalf("pr review goal=%q", e.Goal)
	}
	if strings.Contains(auditBlob(t, srv, ""), extraNote) {
		t.Fatal("pr review audit contains the extra text")
	}

	w = postFix(t, srv, "/prs/acme/app/9/address-review", sid, csrf, url.Values{
		"project": {"proj"}, "extra": {extraNote},
	})
	id = webUnitFromLocation(t, w.Header().Get("Location"))
	p = waitTurnPrompt(t, srv, id)
	assertTurnHasExtra(t, p, []string{"Do not merge", "please handle nil"})

	w = postFix(t, srv, "/prs/acme/app/10/address-ci", sid, csrf, url.Values{
		"project": {"proj"}, "extra": {extraNote},
	})
	id = webUnitFromLocation(t, w.Header().Get("Location"))
	p = waitTurnPrompt(t, srv, id)
	assertTurnHasExtra(t, p, []string{"Do not merge"})

	// Model gate still refuses when the extra field is set.
	if err := srv.cfg.AddProjectAllowedUser("proj", "member-2"); err != nil {
		t.Fatal(err)
	}
	if err := srv.cfg.SetProjectCapabilityByUser("proj", "member-2", "investigator"); err != nil {
		t.Fatal(err)
	}
	sid2, csrf2, err := srv.LoginAs("member-2", "Inv", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w = postFix(t, srv, "/prs/acme/app/9/agent-review", sid2, csrf2, url.Values{
		"project": {"proj"}, "model": {"claude-opus-5-high"}, "extra": {extraNote},
	})
	assertRedirectErr(t, w, "/prs/acme/app/9", "not allowed to pick a model")
	if strings.Contains(auditBlob(t, srv, ""), extraNote) {
		t.Fatal("refused review audit contains the extra text")
	}
}

func TestExtraInstructionsControlRendered(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	if err := cfg.AddProjectAllowedUser("proj", "member-2"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCapabilityByUser("proj", "member-2", "investigator"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectLinear("proj", true, "ENG", "lin-key", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectClickUp("proj", true, "123", "155", "DEV", "cu-key", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		case name == "git" && len(args) > 0 && args[0] == "show" && strings.Contains(joined, "-s"):
			return []byte(sha + "\x1fFixture commit\x1fAlice\x1fa@ex.com\x1f2026-07-20T12:00:00Z\x1fbody note\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "rev-parse":
			return []byte(sha + "\n"), nil
		case strings.Contains(joined, "issue view"):
			labels := `[{"name":"bug"}]`
			if strings.Contains(joined, "issue view 99") {
				labels = `[{"name":"plan"}]`
			}
			return []byte(`{
				"number":42,"title":"Pay bug","body":"steps","url":"https://github.com/acme/app/issues/42",
				"state":"OPEN","author":{"login":"z"},"labels":` + labels + `,"comments":[]
			}`), nil
		case strings.Contains(joined, "issue list"):
			return []byte(`[{"number":10,"url":"u","title":"Ten","state":"OPEN","author":{"login":"a"},"labels":[],"body":"","closedByPullRequestsReferences":[]}]`), nil
		case strings.Contains(joined, "graphql"):
			return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`), nil
		case strings.Contains(joined, "pr view"):
			return []byte(`{"number":9,"url":"https://github.com/acme/app/pull/9","title":"CI PR","state":"OPEN","headRefOid":"abc","headRefName":"feat","baseRefName":"main","body":"b","author":{"login":"z"},"comments":[]}`), nil
		case strings.Contains(joined, "pr checks"):
			return []byte(`[]`), nil
		default:
			return []byte("{}"), nil
		}
	}
	srv.linearNew = func(apiKey string) *linear.Client {
		c := linear.New(apiKey)
		c.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			payload := map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes": []map[string]any{{
							"id": "1", "identifier": "ENG-1", "title": "Lin fixture",
							"url": "https://linear.app/x/issue/ENG-1", "description": "detail desc",
							"state": map[string]string{"name": "Todo"},
							"team":  map[string]string{"key": "ENG"},
						}},
					},
				},
			}
			raw, _ := json.Marshal(payload)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(string(raw))),
				Header:     make(http.Header),
			}, nil
		})}
		return c
	}
	srv.clickupNew = func(apiKey string) *clickup.Client {
		c := clickup.New(apiKey)
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v2/task/DEV-1", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "abc", "custom_id": "DEV-1", "name": "One",
				"status":               map[string]string{"status": "open"},
				"url":                  "https://app.clickup.com/t/abc",
				"markdown_description": "hello",
			})
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		c.Base = ts.URL
		c.HTTP = ts.Client()
		return c
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

	builder, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	investigator, _, err := srv.LoginAs("member-2", "Inv", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := adminLogin(t, srv)

	pages := []struct {
		path    string
		ids     []string
		exactly int
		texts   int // data-confirm-text="extra" on the dispatch control
	}{
		{"/projects/proj/commits/" + sha + "?owner=acme&repo=app", []string{"commit-extra"}, 1, 1},
		{"/prs/acme/app/9?project=proj", []string{"pr-address-extra", "pr-review-extra"}, 2, 3},
		{"/projects/proj/issues/42?owner=acme&repo=app", []string{"issue-fix-extra"}, 1, 1},
		{"/projects/proj/linear/ENG-1", []string{"linear-extra"}, 1, 1},
		{"/projects/proj/clickup/DEV-1", []string{"clickup-extra"}, 1, 1},
	}
	for _, pg := range pages {
		for _, who := range []struct {
			name, sid string
			model     bool
		}{
			{"builder", builder, true},
			{"investigator", investigator, false},
		} {
			body := getPageBody(t, srv, who.sid, pg.path)
			if got := strings.Count(body, `name="extra"`); got != pg.exactly {
				t.Fatalf("%s %s: name=extra count=%d want %d", who.name, pg.path, got, pg.exactly)
			}
			if strings.Contains(body, `class="extra-instructions"`) {
				t.Fatalf("%s %s still renders the extra prompt inline", who.name, pg.path)
			}
			if !strings.Contains(body, `id="gw-confirm-text"`) {
				t.Fatalf("%s %s confirm modal missing the extra prompt", who.name, pg.path)
			}
			if got := strings.Count(body, `data-confirm-text="extra"`); got != pg.texts {
				t.Fatalf("%s %s: data-confirm-text count=%d want %d", who.name, pg.path, got, pg.texts)
			}
			for _, id := range pg.ids {
				if !strings.Contains(body, `id="`+id+`" name="extra" hidden`) {
					t.Fatalf("%s %s missing hidden extra field %s", who.name, pg.path, id)
				}
			}
			hasModel := strings.Contains(body, `name="model"`)
			if hasModel != who.model {
				t.Fatalf("%s %s model field present=%v", who.name, pg.path, hasModel)
			}
		}
	}

	// Plan / Implement does not offer the control. The plan form is still there.
	for _, who := range []string{builder, investigator} {
		body := getPageBody(t, srv, who, "/projects/proj/issues/99?owner=acme&repo=app")
		if strings.Contains(body, `name="extra"`) || strings.Contains(body, "issue-fix-extra") || strings.Contains(body, `data-confirm-text="extra"`) {
			t.Fatal("implement form must not offer extra instructions")
		}
		if !strings.Contains(body, `id="btn-implement-github"`) {
			t.Fatal("plan issue missing Implement")
		}
	}

	// Bulk control lives on the shell, outside the swapped table.
	for _, who := range []struct {
		name, sid string
		model     bool
	}{
		{"builder", builder, true},
		{"investigator", investigator, false},
	} {
		body := getPageBody(t, srv, who.sid, "/projects/proj/issues?owner=acme&repo=app")
		if !strings.Contains(body, `id="issues-bulk-extra" name="extra" form="issues-bulk-fix" hidden`) {
			t.Fatalf("%s issues shell missing hidden bulk extra field", who.name)
		}
		if !strings.Contains(body, `data-confirm-text="extra"`) {
			t.Fatalf("%s bulk Fix must open the extra prompt in the confirm modal", who.name)
		}
		if strings.Contains(body, `class="extra-instructions"`) {
			t.Fatalf("%s bulk extra still renders inline", who.name)
		}
		if strings.Contains(body, `id="live-issues-table"`) {
			shell, table, ok := strings.Cut(body, `id="live-issues-table"`)
			if !ok || !strings.Contains(shell, `id="issues-bulk-extra"`) {
				t.Fatalf("%s bulk control is inside the live region", who.name)
			}
			_ = table
		}
		req := httptest.NewRequest(http.MethodGet, "/partials/issues/table?project=proj&owner=acme&repo=app", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: who.sid})
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("partial status=%d", rec.Code)
		}
		part := rec.Body.String()
		if strings.Contains(part, `name="extra"`) || strings.Contains(part, "issues-bulk-extra") {
			t.Fatal("partial must not carry the extra textarea")
		}
		hasModel := strings.Contains(part, `name="model"`)
		if hasModel != who.model {
			t.Fatalf("%s partial model=%v", who.name, hasModel)
		}
	}

	// Error Fix shows the control for someone who can ship. Investigate never does.
	// An investigator does not see the Fix form at all (same CanShip gate as the picker).
	errBody := getPageBody(t, srv, admin, "/projects/proj/errors/deploys/iss1?location=loc&name=api")
	if strings.Count(errBody, `name="extra"`) != 1 || !strings.Contains(errBody, `id="error-fix-extra" name="extra" hidden`) {
		t.Fatal("error fix form missing its hidden extra field")
	}
	if strings.Count(errBody, `data-confirm-text="extra"`) != 1 {
		t.Fatal("error Fix must be the only control that opens the extra prompt")
	}
	if !strings.Contains(errBody, `id="btn-fix-error"`) || !strings.Contains(errBody, `id="btn-investigate-error"`) {
		t.Fatal("error page missing Fix or Investigate")
	}
	if !strings.Contains(errBody, `name="model"`) {
		t.Fatal("builder error page missing the model picker")
	}
	invBody := getPageBody(t, srv, investigator, "/projects/proj/errors/deploys/iss1?location=loc&name=api")
	if strings.Contains(invBody, `name="extra"`) || strings.Contains(invBody, `id="btn-fix-error"`) || strings.Contains(invBody, `name="model"`) {
		t.Fatal("investigator must not see the error Fix form, its extra control, or the model picker")
	}
	if !strings.Contains(invBody, `id="btn-investigate-error"`) {
		t.Fatal("investigator missing Investigate")
	}
}

func waitTurnPrompt(t *testing.T, srv *Server, tid string) string {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		th, err := srv.history.Get(tid)
		if err == nil && len(th.Turns) >= 1 && strings.TrimSpace(th.Turns[len(th.Turns)-1].Prompt) != "" {
			return th.Turns[len(th.Turns)-1].Prompt
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for history %s", tid)
	return ""
}

func assertTurnHasExtra(t *testing.T, prompt string, canned []string) {
	t.Helper()
	if !strings.Contains(prompt, extraNote) {
		t.Fatalf("turn missing extra:\n%s", prompt)
	}
	if strings.Contains(prompt, "&amp;") || strings.Contains(prompt, "&#35;") {
		t.Fatalf("turn HTML-escaped the extra note:\n%s", prompt)
	}
	section := strings.Index(prompt, "### Additional instructions")
	if section < 0 {
		t.Fatalf("turn missing section:\n%s", prompt)
	}
	for _, c := range canned {
		at := strings.Index(prompt, c)
		if at < 0 || at > section {
			t.Fatalf("canned %q not before extra section:\n%s", c, prompt)
		}
	}
}

func auditBlob(t *testing.T, srv *Server, mustContain string) string {
	t.Helper()
	if srv.audit == nil {
		t.Fatal("no audit")
	}
	entries, err := os.ReadDir(srv.audit.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, ent := range entries {
		raw, err := os.ReadFile(filepath.Join(srv.audit.Dir(), ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	out := b.String()
	if mustContain != "" && !strings.Contains(out, mustContain) {
		t.Fatalf("audit missing %q", mustContain)
	}
	return out
}
