package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// TestRailSheetChromeIsShellOnly pins where the phone action sheet's pieces
// live. The trigger and scrim are shell chrome (one copy, at <body> level,
// never re-rendered); the sheet header belongs to the rail so it scrolls with
// it. Getting this wrong is invisible on desktop — every rule that shows them
// is inside a phone media query — so the placement has to be asserted, not
// eyeballed.
func TestRailSheetChromeIsShellOnly(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.sessions.Set("thread-rail", sessionstore.Entry{
		SessionID: "s1", Project: "proj", Goal: "Something to act on",
	}); err != nil {
		t.Fatal(err)
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

	page := get("/sessions/thread-rail?project=proj")
	for _, want := range []string{
		`id="rail-fab"`,
		`id="rail-scrim"`,
		`<aside class="pr-rail" id="page-rail">`,
		`class="rail-sheet-head"`,
		`data-rail-close`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("session page missing %q", want)
		}
	}
	// The trigger must not be a <nav>: six partial tests reject that substring,
	// and it is chrome, not navigation.
	if i := strings.Index(page, `id="rail-fab"`); i > 0 {
		if seg := page[max(0, i-200):i]; strings.Contains(seg, "<nav") {
			t.Error("rail-fab must not be wrapped in a <nav>")
		}
	}

	// The live fragment renders the conversation only. If the sheet header ever
	// migrated into it, an SSE tick would swap the sheet's own chrome out from
	// under an open sheet.
	frag := get("/partials/sessions/thread-rail?project=proj")
	for _, banned := range []string{
		`id="rail-fab"`, `id="rail-scrim"`, `class="rail-sheet-head"`, `id="page-rail"`,
		"<nav", "sse-status",
	} {
		if strings.Contains(frag, banned) {
			t.Errorf("session live fragment must not contain %q", banned)
		}
	}
}

// TestRailSheetHeadOnEveryRail pins that all five rail-bearing detail pages
// render the sheet header. A page that skips it gets a sheet with no title and
// no close button on phones — and nothing on desktop would show it.
func TestRailSheetHeadOnEveryRail(t *testing.T) {
	tmpls := []string{
		"session.tmpl", "pr_detail.tmpl", "issue_detail.tmpl",
		"linear_detail.tmpl", "commit_detail.tmpl",
	}
	for _, name := range tmpls {
		raw, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		if !strings.Contains(body, `<aside class="pr-rail" id="page-rail">`) {
			t.Errorf("%s: rail must carry id=page-rail", name)
		}
		if !strings.Contains(body, `{{template "rail_sheet_head" $}}`) {
			t.Errorf("%s: rail must render the sheet header", name)
		}
	}
}
