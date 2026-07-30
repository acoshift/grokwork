package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestResolveBackLinkRejectsAnythingButAKnownBoard(t *testing.T) {
	good := map[string][2]string{
		"/cases":                              {"/cases", "Cases"},
		"/projects/webapp/cases":              {"/projects/webapp/cases", "Cases"},
		"/projects/webapp/cases?phase=fixing": {"/projects/webapp/cases?phase=fixing", "Cases"},
		"/sessions":                           {"/sessions", "Sessions"},
		"/projects/webapp/ship":               {"/projects/webapp/ship", "Ship"},
		// Issue detail is a board for crumb purposes (feature hub provenance).
		"/projects/p/issues/42?owner=o&repo=r": {"/projects/p/issues/42?owner=o&repo=r", "Issue"},
	}
	for in, want := range good {
		href, label, ok := resolveBackLink(in)
		if !ok || href != want[0] || label != want[1] {
			t.Errorf("resolveBackLink(%q) = %q,%q,%v; want %q,%q,true", in, href, label, ok, want[0], want[1])
		}
	}
	// Everything that is not a board path this app owns, including every shape
	// a browser would re-read as an absolute URL.
	for _, in := range []string{
		"", "  ",
		"https://evil.example/cases",
		"//evil.example/cases",
		"/\\evil.example",
		"\\\\evil.example",
		"javascript:alert(1)",
		"cases",
		"/sessions/thread-1", // a detail page is not a board
		"/config",
		"/projects/webapp",
		"/projects//cases",
		"/projects/webapp/cases/extra",
		// Issue number must be all ASCII digits (segment check already rejects
		// empty/dot segments; digits-only keeps the allowlist tight).
		"/projects/p/issues/42abc",
		"/projects/p/issues/../42",
		// url.Parse decodes these into "//cases" and "/../config" — a raw-string
		// guard passes them and the browser then reads the first as
		// protocol-relative, i.e. off-origin.
		"/%2fcases",
		"/%2Fcases",
		"/.%2e/config",
		"/projects/%2e%2e/cases",
		"/cases/.",
		"/./cases",
	} {
		if href, label, ok := resolveBackLink(in); ok {
			t.Errorf("resolveBackLink(%q) accepted as %q,%q", in, href, label)
		}
	}
}

// TestSessionCrumbPrecedence pins the three-way rule in sessionBackLink: a
// stamped ?back= wins, a case falls back to its board, everything else to the
// sessions list.
func TestSessionCrumbPrecedence(t *testing.T) {
	srv, _, _ := testServer(t)
	seed := map[string]sessionstore.Entry{
		"plain-unit": {SessionID: "s1", Project: "proj"},
		"case-unit": {
			SessionID: "s2", Project: "proj", Mode: "case",
			Phase: "intake", CaseKey: "PROJ-1", CustomerTitle: "Checkout 500s",
		},
	}
	for id, e := range seed {
		if err := srv.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	h := srv.Handler()
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, w.Code)
		}
		return w.Body.String()
	}
	back := url.QueryEscape("/projects/proj/cases?phase=fixing")
	for _, tc := range []struct{ path, want string }{
		// No provenance, ordinary unit → sessions list (unchanged behaviour).
		{"/sessions/plain-unit?project=proj", `href="/projects/proj/sessions">← Sessions</a>`},
		// No provenance, case → its board. This is what survives the POST round
		// trips the case rail makes (escalate/close redirect without ?back=).
		{"/sessions/case-unit?project=proj", `href="/projects/proj/cases">← Cases</a>`},
		// Provenance wins, filters included.
		{"/sessions/case-unit?project=proj&back=" + back,
			`href="/projects/proj/cases?phase=fixing">← Cases</a>`},
		{"/sessions/plain-unit?project=proj&back=" + back,
			`href="/projects/proj/cases?phase=fixing">← Cases</a>`},
		// A hostile ?back= is ignored, not echoed.
		{"/sessions/plain-unit?project=proj&back=" + url.QueryEscape("https://evil.example/x"),
			`href="/projects/proj/sessions">← Sessions</a>`},
	} {
		if body := get(tc.path); !strings.Contains(body, tc.want) {
			t.Errorf("%s: missing %q", tc.path, tc.want)
		}
	}
	if body := get("/sessions/plain-unit?project=proj&back=" + url.QueryEscape("https://evil.example/x")); strings.Contains(body, "evil.example") {
		t.Fatal("rejected back target must not reach the page")
	}
}

// TestCasesBoardStampsItsFilters pins the reason casesBoardURL exists: the
// crumb has to restore the board the user was actually looking at, not a
// default one. Without this, dropping the filter plumbing would break nothing
// any other test checks.
func TestCasesBoardStampsItsFilters(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.sessions.Set("case-1", sessionstore.Entry{
		SessionID: "s1", Project: "proj", Mode: "case", Phase: "investigate",
		Severity: "critical", CustomerTitle: "Board filter case",
	}); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	for _, tc := range []struct{ board, wantBack string }{
		{"/projects/proj/cases", "/projects/proj/cases"},
		{"/projects/proj/cases?phase=investigate", "/projects/proj/cases?phase=investigate"},
		{"/projects/proj/cases?severity=critical&phase=investigate",
			"/projects/proj/cases?phase=investigate&severity=critical"},
		{"/projects/proj/cases?scope=all", "/projects/proj/cases?scope=all"},
		// scope=open is the resolved default, so it stays off the URL.
		{"/projects/proj/cases?scope=open", "/projects/proj/cases"},
		{"/cases?phase=investigate", "/cases?phase=investigate"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.board, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", tc.board, w.Code)
		}
		want := "back=" + url.QueryEscape(tc.wantBack)
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: row link missing %q", tc.board, want)
		}
		// And the stamp has to survive the round trip it exists for.
		if href, label, ok := resolveBackLink(tc.wantBack); !ok || label != "Cases" || href != tc.wantBack {
			t.Errorf("%s: stamped %q does not resolve back (%q, %q, %v)", tc.board, tc.wantBack, href, label, ok)
		}
	}
}
