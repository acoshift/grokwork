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
