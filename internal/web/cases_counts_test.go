package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func getCaseCounts(t *testing.T, srv *Server, path string, cookie *http.Cookie) (int, caseCounts, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		return w.Code, caseCounts{}, body
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control=%q want no-store", cc)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type=%q want json", ct)
	}
	var got caseCounts
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v body=%s", err, body)
	}
	for _, key := range []string{
		"intake", "investigate", "answered", "fixing", "shipping", "closed",
		"mine", "unassigned", "breached",
	} {
		if !strings.Contains(body, `"`+key+`"`) {
			t.Fatalf("missing key %q in %s", key, body)
		}
	}
	return w.Code, got, body
}

func assertCaseCounts(t *testing.T, got caseCounts, board bot.CaseBoard) {
	t.Helper()
	want := caseCounts{
		Intake: board.Intake, Investigate: board.Investigate, Answered: board.Answered,
		Fixing: board.Fixing, Shipping: board.Shipping, Closed: board.Closed,
		Mine: board.Mine, Unassigned: board.Unassigned, Breached: board.Breached,
	}
	if got != want {
		t.Fatalf("counts=%+v want %+v (board=%+v)", got, want, board)
	}
}

func TestCasesPipelineCountsHost(t *testing.T) {
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/projects/proj/cases")

	pipeStart := strings.Index(body, `id="live-cases-pipeline"`)
	formStart := strings.Index(body, `id="cases-filters"`)
	if pipeStart < 0 || formStart <= pipeStart {
		t.Fatal("pipeline host or filters missing")
	}
	pipe := body[pipeStart:formStart]
	pipeEnd := strings.Index(pipe, ">")
	if pipeEnd < 0 {
		t.Fatal("pipeline opening tag")
	}
	pipeTag := pipe[:pipeEnd]
	for _, want := range []string{
		`hx-get="/partials/cases/counts?project=proj`,
		`hx-trigger="sse:cases"`,
		`hx-target="this"`,
		`hx-select="unset"`,
		`hx-swap="none"`,
	} {
		if !strings.Contains(pipeTag, want) {
			t.Fatalf("pipeline host missing %q in %s", want, pipeTag)
		}
	}
	if strings.Contains(pipeTag, `hx-swap="innerHTML"`) {
		t.Fatal("pipeline must not innerHTML-swap stage chrome")
	}
	for _, key := range []string{"intake", "investigate", "answered", "fixing", "shipping", "closed"} {
		needle := `data-case-count="` + key + `"`
		if !strings.Contains(pipe, needle) {
			t.Fatalf("pipeline missing %s", needle)
		}
	}

	listStart := strings.Index(body, `id="live-cases-list"`)
	if listStart < 0 {
		t.Fatal("list live-region missing")
	}
	listTagEnd := strings.Index(body[listStart:], ">")
	listTag := body[listStart : listStart+listTagEnd]
	if !strings.Contains(listTag, `hx-swap="innerHTML"`) {
		t.Fatalf("list region must still innerHTML-swap rows: %s", listTag)
	}

	for _, want := range []string{
		`data-case-count="mine"`,
		`data-case-count-prefix="Mine"`,
		`>Mine (0)</option>`,
		`data-case-count="unassigned"`,
		`data-case-count-prefix="Needs an engineer"`,
		`data-case-count="breached"`,
		`data-case-count-prefix="Breached"`,
		`>Breached (0)</option>`,
		`window.__gwCaseCountsBound`,
		`htmx:afterOnLoad`,
		`htmx:afterRequest`,
		`querySelectorAll("[data-case-count]")`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("cases page missing %q", want)
		}
	}
}

func TestCasesCountsJSONMatchesBoard(t *testing.T) {
	srv, cfg, _ := testServer(t)
	if err := cfg.SetProjectSLA("proj", map[string]config.SLATarget{
		"critical": {FirstResponseMinutes: new(60), ResolutionMinutes: new(240)},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := map[string]sessionstore.Entry{
		"c-intake": {
			SessionID: "s-in", Project: "proj", Mode: "case",
			Phase: sessionstore.PhaseIntake, OwnerID: "u0",
		},
		"c-investigate": {
			SessionID: "s-inv", Project: "proj", Mode: "case",
			Phase: sessionstore.PhaseInvestigate,
		},
		"c-answered": {
			SessionID: "s-ans", Project: "proj", Mode: "case",
			Phase: sessionstore.PhaseAnswered,
		},
		"c-fix-open": {
			SessionID: "s-fix", Project: "proj", Mode: "case",
			Phase: sessionstore.PhaseFixing,
		},
		"c-ship": {
			SessionID: "s-ship", Project: "proj", Mode: "case",
			Phase: sessionstore.PhaseShipping, EngineerID: "u0", EngineerName: "U",
		},
		"c-closed": {
			SessionID: "s-cl", Project: "proj", Mode: "case",
			Phase: sessionstore.PhaseClosed,
		},
		"c-late": {
			SessionID: "s-late", Project: "proj", Mode: "case",
			Phase: sessionstore.PhaseFixing, Severity: "critical",
			OpenedAt: now.Add(-5 * time.Hour).Format(time.RFC3339),
		},
	}
	for id, e := range seed {
		if err := srv.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}

	code, got, body := getCaseCounts(t, srv, "/partials/cases/counts?project=proj", nil)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	board := srv.bot.ListCaseBoardQuery(bot.CaseBoardQuery{Project: "proj"})
	assertCaseCounts(t, got, board)
	if got.Intake != 1 || got.Investigate != 1 || got.Answered != 1 || got.Fixing != 2 || got.Shipping != 1 || got.Closed != 1 {
		t.Fatalf("phase totals=%+v", got)
	}
	if got.Unassigned != 2 || got.Breached != 1 {
		t.Fatalf("unassigned/breached=%+v", got)
	}
	// Auth off: no viewer, so Mine is 0 even with OwnerID set.
	if got.Mine != 0 {
		t.Fatalf("mine=%d want 0 without a viewer", got.Mine)
	}

	code, got, body = getCaseCounts(t, srv, "/partials/cases/counts?project=proj&phase=intake", nil)
	if code != http.StatusOK {
		t.Fatalf("filtered status=%d body=%s", code, body)
	}
	board = srv.bot.ListCaseBoardQuery(bot.CaseBoardQuery{Project: "proj", Phase: "intake"})
	assertCaseCounts(t, got, board)
	if got.Intake != 1 || got.Fixing != 2 {
		t.Fatalf("phase totals must stay project-wide under a phase filter: %+v", got)
	}
	if got.Unassigned != 0 || got.Breached != 0 {
		t.Fatalf("mine/unassigned/breached are counted after other filters: %+v", got)
	}

	code, got, body = getCaseCounts(t, srv, "/partials/cases/counts?project=proj&sla=breached", nil)
	if code != http.StatusOK {
		t.Fatalf("sla status=%d body=%s", code, body)
	}
	board = srv.bot.ListCaseBoardQuery(bot.CaseBoardQuery{Project: "proj", SLA: bot.CaseSLABreached})
	assertCaseCounts(t, got, board)
	if got.Breached != 1 || got.Unassigned != 1 {
		t.Fatalf("sla=breached counts=%+v", got)
	}

	code, _, body = getCaseCounts(t, srv, "/partials/cases/counts?project=nope", nil)
	if code != http.StatusForbidden {
		t.Fatalf("unknown project status=%d body=%s", code, body)
	}
}

func TestCasesCountsHidesUnauthorizedProject(t *testing.T) {
	srv := twoProjectAuthServer(t)
	if err := srv.cfg.SetProjectSLA("public", map[string]config.SLATarget{
		"critical": {FirstResponseMinutes: new(60)},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := map[string]sessionstore.Entry{
		"c-pub-intake": {
			SessionID: "s-pub-in", Project: "public", Mode: "case",
			Phase: sessionstore.PhaseIntake, OwnerID: "member-1",
		},
		"c-pub-fix": {
			SessionID: "s-pub-fix", Project: "public", Mode: "case",
			Phase: sessionstore.PhaseFixing,
		},
		"c-pub-mine": {
			SessionID: "s-pub-mine", Project: "public", Mode: "case",
			Phase: sessionstore.PhaseShipping, EngineerID: "member-1", EngineerName: "Member",
		},
		"c-pub-late": {
			SessionID: "s-pub-late", Project: "public", Mode: "case",
			Phase: sessionstore.PhaseFixing, Severity: "critical",
			OpenedAt: now.Add(-5 * time.Hour).Format(time.RFC3339),
		},
		"c-sec-intake": {
			SessionID: "s-sec-in", Project: "secret", Mode: "case",
			Phase: sessionstore.PhaseIntake, CaseKey: "SECRET-1",
		},
	}
	for id, e := range seed {
		if err := srv.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}

	code, got, body := getCaseCounts(t, srv, "/partials/cases/counts", cookie)
	if code != http.StatusOK {
		t.Fatalf("global status=%d body=%s", code, body)
	}
	board := srv.bot.ListCaseBoardQuery(bot.CaseBoardQuery{
		ViewerID: "member-1",
		Among:    []string{"public"},
	})
	assertCaseCounts(t, got, board)
	if got.Intake != 1 {
		t.Fatalf("global intake=%d want 1 (secret hidden): %+v", got.Intake, got)
	}
	if got.Mine != 2 || got.Unassigned != 2 || got.Breached != 1 {
		t.Fatalf("global mine/unassigned/breached=%+v", got)
	}

	code, got, body = getCaseCounts(t, srv, "/partials/cases/counts?project=public", cookie)
	if code != http.StatusOK {
		t.Fatalf("public status=%d body=%s", code, body)
	}
	if got.Intake != 1 || got.Mine != 2 {
		t.Fatalf("public counts=%+v", got)
	}

	code, _, body = getCaseCounts(t, srv, "/partials/cases/counts?project=secret", cookie)
	if code != http.StatusForbidden {
		t.Fatalf("secret project status=%d body=%s", code, body)
	}
}
