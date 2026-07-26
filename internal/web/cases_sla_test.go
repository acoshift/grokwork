package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// The case board's SLA chip and breach filter. Breach is computed per render, so
// the assertions here are about a page drawn from timestamps alone — nothing in
// the store says "late".
func TestCasesBoardSLABadgeAndFilter(t *testing.T) {
	srv, cfg, _ := testServer(t)
	minutes := func(n int) *int { return &n }
	if err := cfg.SetProjectSLA("proj", map[string]config.SLATarget{
		"critical": {FirstResponseMinutes: minutes(60), ResolutionMinutes: minutes(240)},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := map[string]sessionstore.Entry{
		"case-late": {
			SessionID: "sla1", Project: "proj", Mode: "case", Phase: "fixing",
			Severity: "critical", CustomerTitle: "Late critical case",
			OpenedAt: now.Add(-5 * time.Hour).Format(time.RFC3339),
		},
		"case-fresh": {
			SessionID: "sla2", Project: "proj", Mode: "case", Phase: "fixing",
			Severity: "critical", CustomerTitle: "Fresh critical case",
			OpenedAt: now.Add(-2 * time.Minute).Format(time.RFC3339),
		},
		// Old and unanswered, but medium has no target configured.
		"case-untargeted": {
			SessionID: "sla3", Project: "proj", Mode: "case", Phase: "fixing",
			Severity: "medium", CustomerTitle: "Untargeted case",
			OpenedAt: now.Add(-5 * time.Hour).Format(time.RFC3339),
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

	body := get("/projects/proj/cases")
	for _, want := range []string{
		// Filter control with the breach count in its label.
		`<label for="sla">SLA</label>`,
		`>Breached (1)</option>`,
		// Chip on the late row, and the tooltip that explains it.
		"SLA · first response",
		`class="badge status-error"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("cases board missing %q", want)
		}
	}

	// Filtering keeps only the late case, and the filter rides the SSE partial
	// URLs so a refresh does not silently widen the board.
	filtered := get("/projects/proj/cases?sla=breached")
	if !strings.Contains(filtered, "Late critical case") {
		t.Fatal("breach filter dropped the late case")
	}
	for _, gone := range []string{"Fresh critical case", "Untargeted case"} {
		if strings.Contains(filtered, gone) {
			t.Fatalf("breach filter kept %q", gone)
		}
	}
	if !strings.Contains(filtered, "&amp;sla=breached") {
		t.Fatal("live-region URLs must carry the sla filter")
	}
	// Row links stamp the filtered board as the ← crumb target.
	if !strings.Contains(filtered, "back=%2Fprojects%2Fproj%2Fcases%3Fsla%3Dbreached") {
		t.Fatalf("back link should carry the filter: %s", filtered)
	}

	// The partial applies it too (SSE refresh path).
	req := httptest.NewRequest(http.MethodGet, "/partials/cases/list?project=proj&sla=breached", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	partial := w.Body.String()
	if !strings.Contains(partial, "Late critical case") || strings.Contains(partial, "Fresh critical case") {
		t.Fatalf("filtered partial wrong rows: %s", partial)
	}

	// The session page shows the same standing, so acting on a case does not
	// need the board open.
	session := get("/sessions/case-late?project=proj")
	if !strings.Contains(session, `id="session-case-sla"`) || !strings.Contains(session, "SLA · first response") {
		t.Fatal("session page missing the SLA chip")
	}

	// With no targets configured the board says nothing about SLAs at all.
	if err := cfg.SetProjectSLA("proj", nil); err != nil {
		t.Fatal(err)
	}
	plain := get("/projects/proj/cases")
	if strings.Contains(plain, "SLA · ") {
		t.Fatal("a project with no targets must not badge anything")
	}
	if !strings.Contains(plain, `>Breached (0)</option>`) {
		t.Fatal("filter should still render, with a zero count")
	}
}

// The settings form is how a project gets SLAs at all; an empty box has to mean
// "no deadline" rather than zero, which is the failure mode that would make every
// case late.
func TestProjectConfigSLAForm(t *testing.T) {
	srv, cfg, _ := testServer(t)
	h := srv.Handler()

	body := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/config/projects/proj/workflow", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("workflow tab status=%d", w.Code)
		}
		return w.Body.String()
	}
	first := body()
	for _, want := range []string{`id="project-sla"`, `name="first_critical"`, `name="resolution_low"`} {
		if !strings.Contains(first, want) {
			t.Fatalf("workflow tab missing %q", want)
		}
	}

	post := func(form string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/config/projects/sla", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	post("name=proj&first_critical=60&resolution_critical=480&first_high=240&resolution_high=")
	fr, res := cfg.ProjectSLA("proj", "critical")
	if fr != time.Hour || res != 8*time.Hour {
		t.Fatalf("critical saved as %v/%v", fr, res)
	}
	fr, res = cfg.ProjectSLA("proj", "high")
	if fr != 4*time.Hour || res != 0 {
		t.Fatalf("high saved as %v/%v — an empty box must clear, not zero", fr, res)
	}
	if !strings.Contains(body(), `value="60"`) {
		t.Fatal("saved minutes should render back into the form")
	}

	// Clearing every box turns the SLA off rather than leaving stale targets.
	post("name=proj&first_critical=&resolution_critical=&first_high=")
	if fr, res = cfg.ProjectSLA("proj", "critical"); fr != 0 || res != 0 {
		t.Fatalf("cleared critical = %v/%v", fr, res)
	}

	// Junk is refused with the old values intact.
	post("name=proj&first_critical=60")
	w := post("name=proj&first_critical=soon")
	if w.Code >= 500 {
		t.Fatalf("bad input should redirect with an error, got %d", w.Code)
	}
	if fr, _ = cfg.ProjectSLA("proj", "critical"); fr != time.Hour {
		t.Fatalf("a rejected save must not overwrite: %v", fr)
	}
}
