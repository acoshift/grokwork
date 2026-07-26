package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func seedLinkableCases(t *testing.T, srv *Server) {
	t.Helper()
	seed := map[string]sessionstore.Entry{
		"thread-old": {
			SessionID: "s-old", Project: "proj", Mode: "case", Phase: "closed",
			CaseKey: "PROJ-9", CustomerTitle: "Retry storm in billing", Resolution: "fixed",
		},
		"thread-new": {
			SessionID: "s-new", Project: "proj", Mode: "case", Phase: "investigate",
			CaseKey: "PROJ-14", CustomerTitle: "Retry storm in invoicing",
		},
	}
	for id, e := range seed {
		if err := srv.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
}

func caseLinkServer(t *testing.T) *Server {
	t.Helper()
	srv, _, _ := testServer(t)
	seedLinkableCases(t, srv)
	return srv
}

func TestCaseKeyURLResolves(t *testing.T) {
	srv := caseLinkServer(t)
	h := srv.Handler()
	for _, path := range []string{
		"/c/PROJ-14",
		"/c/proj-14", // whatever casing the reference was written in
		"/projects/proj/cases/PROJ-14",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Location"); got != "/sessions/thread-new?project=proj" {
			t.Fatalf("%s → %q", path, got)
		}
	}
	// "new" is a literal segment and must keep winning over {key}.
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/cases/new", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="page-case-new"`) {
		t.Fatalf("cases/new shadowed by {key}: status=%d", w.Code)
	}
	// Unknown and malformed keys are both plain misses.
	for _, path := range []string{"/c/PROJ-999", "/c/nonsense", "/projects/proj/cases/PROJ-999"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404", path, w.Code)
		}
	}
}

func TestCaseLinkRoundTrip(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedLinkableCases(t, srv)
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	h := srv.Handler()
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		return postFix(t, srv, path, sid, csrf, form)
	}
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, w.Code)
		}
		return w.Body.String()
	}

	if w := post("/sessions/thread-new/case/link", url.Values{"caseKey": {"proj-9"}}); w.Code >= 400 {
		t.Fatalf("link status=%d body=%s", w.Code, w.Body.String())
	}
	ent, _ := srv.sessions.Get("thread-new")
	if got := ent.RelatedCaseKeys(); len(got) != 1 || got[0] != "PROJ-9" {
		t.Fatalf("stored links = %v", got)
	}

	// Outbound on the case that made the link…
	newBody := get("/sessions/thread-new?project=proj")
	if !strings.Contains(newBody, `>PROJ-9</a>`) || !strings.Contains(newBody, "Retry storm in billing") {
		t.Fatal("outbound link missing from the linking case")
	}
	// …and inbound, unasked, on the case pointed at.
	oldBody := get("/sessions/thread-old?project=proj")
	if !strings.Contains(oldBody, `>PROJ-14</a>`) {
		t.Fatal("inbound link missing from the referenced case")
	}
	if !strings.Contains(oldBody, "references this") {
		t.Fatal("inbound link must be labelled as inbound")
	}

	// A reference to nothing is a typo, not a link.
	if w := post("/sessions/thread-new/case/link", url.Values{"caseKey": {"PROJ-404"}}); !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatalf("unknown key should redirect with an error, got %q", w.Header().Get("Location"))
	}
	if w := post("/sessions/thread-new/case/link", url.Values{"caseKey": {"PROJ-14"}}); !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatal("self-reference should be refused")
	}

	if w := post("/sessions/thread-new/case/unlink", url.Values{"caseKey": {"PROJ-9"}}); w.Code >= 400 {
		t.Fatalf("unlink status=%d", w.Code)
	}
	ent, _ = srv.sessions.Get("thread-new")
	if got := ent.RelatedCaseKeys(); len(got) != 0 {
		t.Fatalf("links after unlink = %v", got)
	}
	if strings.Contains(get("/sessions/thread-old?project=proj"), `>PROJ-14</a>`) {
		t.Fatal("inbound link must disappear when the outbound half is removed")
	}
}

// TestCaseLinksAreSameProjectOnly pins the containment: the session page
// renders a link's title and phase with no per-link authorization, so a
// reference reaching into another project would be a read primitive for any
// project whose case keys you can guess.
func TestCaseLinksAreSameProjectOnly(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedLinkableCases(t, srv)
	// A case in a project this member is not on, with a title worth leaking.
	if err := srv.cfg.AddProject("secret", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("thread-secret", sessionstore.Entry{
		SessionID: "s-secret", Project: "secret", Mode: "case", Phase: "fixing",
		CaseKey: "SECRET-1", CustomerTitle: "Acquisition pricing incident",
	}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")

	// Outbound: refused, and the error must not quote the other case's title.
	w := postFix(t, srv, "/sessions/thread-new/case/link", sid, csrf,
		url.Values{"caseKey": {"SECRET-1"}})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("cross-project link should be refused, got %q", loc)
	}
	if strings.Contains(loc, "Acquisition") {
		t.Fatal("refusal leaked the other project's case title")
	}
	if ent, _ := srv.sessions.Get("thread-new"); len(ent.RelatedCaseKeys()) != 0 {
		t.Fatal("cross-project link must not be stored")
	}

	// Inbound: the other project points at ours. Nothing about it may render.
	if err := srv.sessions.Set("thread-secret", sessionstore.Entry{
		SessionID: "s-secret", Project: "secret", Mode: "case", Phase: "fixing",
		CaseKey: "SECRET-1", CustomerTitle: "Acquisition pricing incident",
		RelatedCases: []string{"PROJ-14"},
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-new?project=proj", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, leak := range []string{"SECRET-1", "Acquisition pricing incident", "thread-secret"} {
		if strings.Contains(body, leak) {
			t.Fatalf("cross-project inbound link leaked %q", leak)
		}
	}
}

// TestCaseLinkCap pins the cap and, more importantly, what it counts: the
// canonicalised list, so duplicates and a self-reference cannot pad it.
func TestCaseLinkCap(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedLinkableCases(t, srv)
	// bot.MaxRelatedCases targets to point at, plus one over.
	for i := 1; i <= bot.MaxRelatedCases+1; i++ {
		if err := srv.sessions.Set(fmt.Sprintf("t-%d", i), sessionstore.Entry{
			Project: "proj", Mode: "case", Phase: "intake",
			CaseKey: fmt.Sprintf("PROJ-%d", 100+i), CustomerTitle: "filler",
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= bot.MaxRelatedCases; i++ {
		if err := srv.bot.LinkCase("thread-new", fmt.Sprintf("PROJ-%d", 100+i)); err != nil {
			t.Fatalf("link %d: %v", i, err)
		}
	}
	if err := srv.bot.LinkCase("thread-new", fmt.Sprintf("PROJ-%d", 100+bot.MaxRelatedCases+1)); err == nil {
		t.Fatalf("linking past the cap of %d should be refused", bot.MaxRelatedCases)
	}
	// Re-linking one that is already there is a no-op, not a cap violation.
	if err := srv.bot.LinkCase("thread-new", "PROJ-101"); err != nil {
		t.Fatalf("re-linking an existing reference should succeed: %v", err)
	}
	if got := len(mustEntry(t, srv, "thread-new").RelatedCaseKeys()); got != bot.MaxRelatedCases {
		t.Fatalf("links = %d, want %d", got, bot.MaxRelatedCases)
	}
}

func mustEntry(t *testing.T, srv *Server, threadID string) sessionstore.Entry {
	t.Helper()
	e, ok := srv.sessions.Get(threadID)
	if !ok {
		t.Fatalf("no entry %s", threadID)
	}
	return e
}

func TestCaseKeyOnBoard(t *testing.T) {
	srv := caseLinkServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/cases", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `<span class="mono case-key">PROJ-14</span>`) {
		t.Fatal("board row missing its case key")
	}
}
