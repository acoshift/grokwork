package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func seedCaseSession(t *testing.T, srv *Server, threadID, ownerID string) {
	t.Helper()
	if err := srv.sessions.Set(threadID, sessionstore.Entry{
		Project:       "proj",
		Mode:          "case",
		Phase:         sessionstore.PhaseIntake,
		CustomerTitle: "Pay wall loops",
		Severity:      "high",
		OwnerID:       ownerID,
		OwnerName:     "Owner",
		Origin:        "web",
		IntakeSource:  "web",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCasePanelRendersOnSession(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedCaseSession(t, srv, "t-case-panel", "member-1")
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// Ensure project membership for member-1
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	req := httptest.NewRequest(http.MethodGet, "/sessions/t-case-panel?project=proj", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="session-case-panel"`,
		`id="session-case-actions"`,
		"Pay wall loops",
		"btn-case-escalate",
		"btn-case-investigate",
		"btn-case-answer",
		"btn-case-close",
		"btn-case-customer",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestCasePanelHidesSupportActionsOnEngPhases(t *testing.T) {
	// fixing/shipping: investigate, escalate, answer go away; customer update + close remain.
	srv, _, _ := fixEnabledServer(t)
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}

	for _, phase := range []string{sessionstore.PhaseFixing, sessionstore.PhaseShipping} {
		tid := "t-case-eng-" + phase
		if err := srv.sessions.Set(tid, sessionstore.Entry{
			Project:       "proj",
			Mode:          "case",
			Phase:         phase,
			CustomerTitle: "Escalated pay wall",
			OwnerID:       "member-1",
			OwnerName:     "Member",
			Origin:        "web",
		}); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/sessions/"+tid+"?project=proj", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("phase=%s status=%d body=%s", phase, w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, want := range []string{
			`id="session-case-panel"`,
			`id="session-case-actions"`,
			"btn-case-customer",
			"btn-case-close",
			"btn-continue", // eng work via Grok box
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("phase=%s missing %q", phase, want)
			}
		}
		for _, hide := range []string{
			"btn-case-investigate",
			"btn-case-escalate",
			"btn-case-answer",
		} {
			if strings.Contains(body, hide) {
				t.Fatalf("phase=%s should hide %q", phase, hide)
			}
		}
	}
}

func TestPostCaseEscalate(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	seedCaseSession(t, srv, "t-esc", "member-1")
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/t-esc/case/escalate", sid, csrf, url.Values{
		"note": {"repro attached"},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	e, ok := srv.sessions.Get("t-esc")
	if !ok || e.Phase != sessionstore.PhaseFixing {
		t.Fatalf("phase after escalate: ok=%v %+v", ok, e)
	}
}

// Escalating is a handoff, and who ends up holding the case depends on the
// escalator's role: an engineer claims it, support releases it to the queue.
func TestPostCaseEscalateAssignsByRole(t *testing.T) {
	for _, tc := range []struct {
		name      string
		template  string
		wantEng   string
		wantFlash string
	}{
		{"builder claims", "builder", "member-1", "assigned to you"},
		{"investigator releases", "investigator", "", "No engineer assigned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cfg, _ := fixEnabledServer(t)
			_ = cfg.AddProjectAllowedUser("proj", "member-1")
			if err := cfg.SetProjectCapabilityByUser("proj", "member-1", tc.template); err != nil {
				t.Fatal(err)
			}
			// Pre-assigned so the release case proves it clears rather than no-ops.
			if err := srv.sessions.Set("t-role", sessionstore.Entry{
				Project: "proj", Mode: "case", Phase: sessionstore.PhaseIntake,
				CustomerTitle: "Pay wall loops", OwnerID: "u-support",
				EngineerID: "u-old", EngineerName: "old",
			}); err != nil {
				t.Fatal(err)
			}
			sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
			if err != nil {
				t.Fatal(err)
			}
			w := postFix(t, srv, "/sessions/t-role/case/escalate", sid, csrf, url.Values{})
			if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			e, _ := srv.sessions.Get("t-role")
			if e.EngineerID != tc.wantEng {
				t.Fatalf("engineer=%q want %q", e.EngineerID, tc.wantEng)
			}
			if e.Phase != sessionstore.PhaseFixing {
				t.Fatalf("phase=%q", e.Phase)
			}
			// Thread ownership gates cancel/reset and must not move either way.
			if e.OwnerID != "u-support" {
				t.Fatalf("thread owner changed: %q", e.OwnerID)
			}
			if loc := w.Header().Get("Location"); !strings.Contains(loc, url.QueryEscape(tc.wantFlash)) &&
				!strings.Contains(loc, strings.ReplaceAll(tc.wantFlash, " ", "+")) {
				t.Fatalf("Location=%q want flash %q", loc, tc.wantFlash)
			}
		})
	}
}

// The board's owner filter is the other half: an engineer must be able to find
// what nobody has picked up.
func TestCaseBoardOwnerFilterPage(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	_ = cfg.AddProjectAllowedUser("proj", "member-1")
	seed := map[string]sessionstore.Entry{
		"t-unassigned": {
			Project: "proj", Mode: "case", Phase: sessionstore.PhaseFixing,
			CustomerTitle: "Nobody on this",
		},
		"t-claimed": {
			Project: "proj", Mode: "case", Phase: sessionstore.PhaseFixing,
			CustomerTitle: "Someone else has it", EngineerID: "u-other", EngineerName: "other",
		},
		"t-mine": {
			Project: "proj", Mode: "case", Phase: sessionstore.PhaseFixing,
			CustomerTitle: "I have this", EngineerID: "member-1", EngineerName: "Member",
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

	all := getPageBody(t, srv, sid, "/projects/proj/cases")
	for _, want := range []string{"Nobody on this", "Someone else has it", "I have this", `id="owner"`} {
		if !strings.Contains(all, want) {
			t.Fatalf("unfiltered board missing %q", want)
		}
	}
	// The unassigned row is called out inline, so the queue is visible without
	// switching filters at all.
	if !strings.Contains(all, "needs an engineer") {
		t.Fatal("unassigned row must be flagged inline")
	}

	unassigned := getPageBody(t, srv, sid, "/projects/proj/cases?owner=unassigned")
	if !strings.Contains(unassigned, "Nobody on this") {
		t.Fatal("unassigned filter dropped the unassigned case")
	}
	if strings.Contains(unassigned, "Someone else has it") || strings.Contains(unassigned, "I have this") {
		t.Fatal("unassigned filter must hide claimed cases")
	}

	mine := getPageBody(t, srv, sid, "/projects/proj/cases?owner=mine")
	if !strings.Contains(mine, "I have this") {
		t.Fatal("mine filter dropped my case")
	}
	if strings.Contains(mine, "Someone else has it") || strings.Contains(mine, "Nobody on this") {
		t.Fatal("mine filter must hide other people's cases")
	}
	// Stage links keep the owner filter, or clicking a lane silently widens it.
	if !strings.Contains(mine, "?phase=fixing&amp;owner=mine") {
		t.Fatal("pipeline stage links must carry the owner filter")
	}
}

func TestPostCaseClose(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	seedCaseSession(t, srv, "t-close", "member-1")
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/t-close/case/close", sid, csrf, url.Values{
		"resolution": {"answered"},
		"note":       {"kb article"},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	e, _ := srv.sessions.Get("t-close")
	if !e.IsCaseClosed() {
		t.Fatalf("want closed: %+v", e)
	}
}

func TestPostCaseCustomerUpdate(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	seedCaseSession(t, srv, "t-cu", "member-1")
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/t-cu/case/customer-update", sid, csrf, url.Values{
		"text": {"Please try again after updating the app."},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	e, _ := srv.sessions.Get("t-cu")
	if e.CustomerUpdate == "" {
		t.Fatal("customer update not saved")
	}
}

func TestOverviewCaseCounts(t *testing.T) {
	// Auth-off testServer still renders overview (CanStartSession false but counts show).
	srv, _, _ := testServer(t)
	seedCaseSession(t, srv, "t-ov-1", "u0")
	if err := srv.sessions.Set("t-ov-2", sessionstore.Entry{
		Project: "proj", Mode: "case", Phase: sessionstore.PhaseInvestigate, CustomerTitle: "B",
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="pulse-cases-open"`,
		`id="pulse-cases-investigate"`,
		`id="pulse-cases-eng"`,
		"Open cases",
		"Looking into it",
		"With engineering",
		"sse:cases",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in overview", want)
		}
	}
}

// TestClosedCaseHidesContinueAndRejectsPost: a closed case hides the composer
// and open-case actions until reopen; continue is refused; reopen is offered.
func TestClosedCaseHidesContinueAndRejectsPost(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	seedCaseSession(t, srv, "t-done", "member-1")
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/t-done/case/close", sid, csrf, url.Values{
		"resolution": {"answered"},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("close status=%d body=%s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/t-done", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session page status=%d", rec.Code)
	}
	body := rec.Body.String()
	// Match element ids — the layout JS legitimately mentions the composer id.
	for _, ban := range []string{`id="session-continue-form"`, `id="btn-continue"`, `id="session-lifecycle"`} {
		if strings.Contains(body, ban) {
			t.Fatalf("closed case page must not render %q", ban)
		}
	}
	// Reopen control is offered; open-case action buttons stay hidden.
	// Closed cases are labeled abandoned — web danger zone (Abandon) is hidden.
	for _, want := range []string{`id="btn-case-reopen"`, `id="session-case-actions"`, "session-ownership"} {
		if !strings.Contains(body, want) {
			t.Fatalf("closed case page missing %q", want)
		}
	}
	if strings.Contains(body, `id="session-danger"`) || strings.Contains(body, `id="btn-abandon"`) {
		t.Fatal("closed/abandoned case must not show Abandon danger zone")
	}
	if strings.Contains(body, "Reopen is not implemented") {
		t.Fatal("closed case page still claims reopen is not implemented")
	}
	for _, hide := range []string{"btn-case-investigate", "btn-case-escalate", "btn-case-answer", "btn-case-close"} {
		if strings.Contains(body, hide) {
			t.Fatalf("closed case page should hide %q", hide)
		}
	}

	w = postFix(t, srv, "/sessions/t-done/continue", sid, csrf, url.Values{
		"prompt": {"please revive"},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("continue status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") || !strings.Contains(loc, "closed") {
		t.Fatalf("continue on closed case must redirect with error, got %q", loc)
	}
}

func TestPostCaseReopen(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	if err := srv.sessions.Set("t-reopen", sessionstore.Entry{
		Project: "proj", Mode: "case", Phase: sessionstore.PhaseClosed,
		CustomerTitle: "Still broken checkout", Severity: "high",
		OwnerID: "member-1", OwnerName: "Member", Origin: "web",
		Resolution: "fixed", ResolutionNote: "thought it shipped",
		ResolvedAt: "2026-01-01T00:00:00Z", ResolvedBy: "member-1",
		Dossier: &sessionstore.Dossier{Summary: "payment race"},
		Label:   sessionstore.LabelDone,
	}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}

	// Non-closed reject
	seedCaseSession(t, srv, "t-open-reopen", "member-1")
	w := postFix(t, srv, "/sessions/t-open-reopen/case/reopen", sid, csrf, url.Values{
		"phase": {"investigate"},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("reopen open status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("reopen on open case must error, loc=%q", loc)
	}
	e, _ := srv.sessions.Get("t-open-reopen")
	if e.Phase != sessionstore.PhaseIntake {
		t.Fatalf("open phase clobbered: %q", e.Phase)
	}

	// Closed → investigate (default empty phase)
	w = postFix(t, srv, "/sessions/t-reopen/case/reopen", sid, csrf, url.Values{})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("reopen status=%d body=%s", w.Code, w.Body.String())
	}
	e, ok := srv.sessions.Get("t-reopen")
	if !ok || e.IsCaseClosed() || e.Mode != "case" {
		t.Fatalf("after reopen: ok=%v %+v", ok, e)
	}
	if e.Phase != sessionstore.PhaseInvestigate {
		t.Fatalf("phase=%q", e.Phase)
	}
	if e.Resolution != "" || e.ResolvedBy != "" {
		t.Fatalf("resolution not cleared: %+v", e)
	}
	if e.Dossier == nil || e.Dossier.Summary != "payment race" {
		t.Fatalf("dossier: %+v", e.Dossier)
	}

	// Re-close and reopen as fixing
	if err := srv.bot.CloseCase("t-reopen", "member-1", "fixed", "again"); err != nil {
		t.Fatal(err)
	}
	w = postFix(t, srv, "/sessions/t-reopen/case/reopen", sid, csrf, url.Values{
		"phase": {"fixing"},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("reopen fixing status=%d", w.Code)
	}
	e, _ = srv.sessions.Get("t-reopen")
	if e.Phase != sessionstore.PhaseFixing || e.IsCaseClosed() {
		t.Fatalf("after reopen fixing: %+v", e)
	}

	// After reopen, page shows continue + open-case actions (not reopen-only).
	req := httptest.NewRequest(http.MethodGet, "/sessions/t-reopen", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session page status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="btn-continue"`) {
		t.Fatal("after reopen, continue composer should be back")
	}
	if strings.Contains(body, `id="btn-case-reopen"`) {
		t.Fatal("after reopen, reopen control should hide")
	}
}

// Member with only draftCustomerReply (no investigate/fileEscalation/startSessions)
// and not the case owner must not reopen.
func TestPostCaseReopenForbiddenWithoutCaps(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	_ = cfg.AddProjectAllowedUser("proj", "owner-1")
	_ = cfg.AddProjectAllowedUser("proj", "viewer-1")
	pc := cfg.Projects["proj"]
	pc.CapabilityTemplates = map[string]config.Capabilities{
		"support-view": {DraftCustomerReply: true},
	}
	pc.CapabilityByUser = map[string]string{"viewer-1": "support-view"}
	cfg.Projects["proj"] = pc

	if err := srv.sessions.Set("t-reopen-deny", sessionstore.Entry{
		Project: "proj", Mode: "case", Phase: sessionstore.PhaseClosed,
		CustomerTitle: "Owned by someone else", OwnerID: "owner-1", OwnerName: "Owner",
		Resolution: "fixed", Label: sessionstore.LabelDone,
	}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("viewer-1", "Viewer", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/t-reopen-deny/case/reopen", sid, csrf, url.Values{
		"phase": {"investigate"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s loc=%s", w.Code, w.Body.String(), w.Header().Get("Location"))
	}
	e, _ := srv.sessions.Get("t-reopen-deny")
	if !e.IsCaseClosed() {
		t.Fatalf("denied reopen must not mutate: %+v", e)
	}
}
