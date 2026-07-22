package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// controlPaths are the six new session-lifecycle POST endpoints.
func controlPaths(threadID string) []string {
	return []string{
		"/sessions/" + threadID + "/cancel",
		"/sessions/" + threadID + "/reset",
		"/sessions/" + threadID + "/queue/remove",
		"/sessions/" + threadID + "/label",
		"/sessions/" + threadID + "/goal",
		"/sessions/" + threadID + "/claim",
	}
}

func TestSessionControlFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t) // startSessions off
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range controlPaths("thread-x") {
		w := postFix(t, srv, p, sid, csrf, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d want 404", p, w.Code)
		}
	}
}

func TestSessionControlViewerForbidden(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, csrf, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range controlPaths("thread-x") {
		w := postFix(t, srv, p, sid, csrf, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d want 403", p, w.Code)
		}
	}
}

func TestSessionControlBadCSRF(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range controlPaths("thread-x") {
		w := postFix(t, srv, p, sid, "wrong-csrf", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d want 403", p, w.Code)
		}
	}
}

// TestSessionControlCrossProjectForbidden: a member of one project cannot
// control a thread that belongs to a project they cannot access (ensureThreadAccess).
func TestSessionControlCrossProjectForbidden(t *testing.T) {
	srv := twoProjectAuthServer(t)
	srv.cfg.WebAuth.Features.StartSessions = true
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// member-1 may access "public" but not "secret" (owner of th-secret).
	for _, p := range controlPaths("th-secret") {
		w := postFix(t, srv, p, sid, csrf, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d want 403", p, w.Code)
		}
	}
}

// seedOwned stores an idle session entry with the given owner/co-owners.
func seedOwned(t *testing.T, srv *Server, threadID, ownerID, ownerName string, coOwners ...string) {
	t.Helper()
	e := sessionstore.Entry{Project: "proj"}
	if ownerID != "" {
		e.SetOwner(ownerID, ownerName)
	}
	for _, c := range coOwners {
		e.AddCoOwner(c)
	}
	if err := srv.sessions.Set(threadID, e); err != nil {
		t.Fatal(err)
	}
}

// TestSessionCancelOwnershipMatrix pins the soft-open control model: owner and
// co-owner may cancel, an unrelated member may cancel only an UNOWNED unit, and
// admin always may. Cancel on an idle thread still returns a redirect (no active
// run) — the point is 302 (authorized) vs 403 (blocked).
func TestSessionCancelOwnershipMatrix(t *testing.T) {
	cases := []struct {
		name      string
		threadID  string
		ownerID   string
		coOwners  []string
		loginID   string
		loginRole config.WebRole
		forbidden bool
	}{
		{"owner", "t-owner", "member-1", nil, "member-1", config.WebRoleMember, false},
		{"co-owner", "t-coowner", "member-1", []string{"allow-user"}, "allow-user", config.WebRoleMember, false},
		{"unrelated-member-owned", "t-owned", "member-1", nil, "allow-user", config.WebRoleMember, true},
		{"unrelated-member-unowned", "t-unowned", "", nil, "allow-user", config.WebRoleMember, false},
		{"admin", "t-admin", "member-1", nil, "admin-1", config.WebRoleAdmin, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := fixEnabledServer(t)
			seedOwned(t, srv, tc.threadID, tc.ownerID, "Owner", tc.coOwners...)
			sid, csrf, err := srv.LoginAs(tc.loginID, "U", tc.loginRole)
			if err != nil {
				t.Fatal(err)
			}
			w := postFix(t, srv, "/sessions/"+tc.threadID+"/cancel", sid, csrf, nil)
			if tc.forbidden {
				if w.Code != http.StatusForbidden {
					t.Fatalf("cancel status=%d want 403 body=%s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
				t.Fatalf("cancel status=%d want redirect body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestSessionResetOwnershipMatrix mirrors the cancel matrix. Reset on an idle
// authorized unit succeeds and redirects to the project sessions list.
func TestSessionResetOwnershipMatrix(t *testing.T) {
	cases := []struct {
		name      string
		threadID  string
		ownerID   string
		coOwners  []string
		loginID   string
		loginRole config.WebRole
		forbidden bool
	}{
		{"owner", "r-owner", "member-1", nil, "member-1", config.WebRoleMember, false},
		{"co-owner", "r-coowner", "member-1", []string{"allow-user"}, "allow-user", config.WebRoleMember, false},
		{"unrelated-member-owned", "r-owned", "member-1", nil, "allow-user", config.WebRoleMember, true},
		{"unrelated-member-unowned", "r-unowned", "", nil, "allow-user", config.WebRoleMember, false},
		{"admin", "r-admin", "member-1", nil, "admin-1", config.WebRoleAdmin, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := fixEnabledServer(t)
			seedOwned(t, srv, tc.threadID, tc.ownerID, "Owner", tc.coOwners...)
			sid, csrf, err := srv.LoginAs(tc.loginID, "U", tc.loginRole)
			if err != nil {
				t.Fatal(err)
			}
			w := postFix(t, srv, "/sessions/"+tc.threadID+"/reset", sid, csrf, nil)
			if tc.forbidden {
				if w.Code != http.StatusForbidden {
					t.Fatalf("reset status=%d want 403", w.Code)
				}
				if _, ok := srv.sessions.Get(tc.threadID); !ok {
					t.Fatal("forbidden reset must not delete the session")
				}
				return
			}
			if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
				t.Fatalf("reset status=%d want redirect body=%s", w.Code, w.Body.String())
			}
			if _, ok := srv.sessions.Get(tc.threadID); ok {
				t.Fatal("authorized reset must delete the session")
			}
		})
	}
}

// TestSessionResetSuccessRedirectsToList pins the redirect target after a
// successful reset: the dead unit page is left for the project sessions list.
func TestSessionResetSuccessRedirectsToList(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedOwned(t, srv, "reset-ok", "member-1", "Member One")
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/reset-ok/reset", sid, csrf, nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/sessions") {
		t.Fatalf("Location=%q want /projects/proj/sessions", loc)
	}
	if !strings.Contains(loc, "ok=") {
		t.Fatalf("Location=%q want ok flash", loc)
	}
	assertAuditAction(t, srv, audit.ActionSessionReset, true)
}

// TestSessionResetBusyStaysOnPage: a busy unit refuses reset and keeps the user
// on the session page with an error.
func TestSessionResetBusyStaysOnPage(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	seedOwned(t, srv, "reset-busy", "member-1", "Member One")
	if err := bot.SeedActiveRunForTest(b, "reset-busy", "proj", "p", "live"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(b, "reset-busy") })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/reset-busy/reset", sid, csrf, nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/reset-busy") || !strings.Contains(loc, "err=") {
		t.Fatalf("Location=%q want session page with err", loc)
	}
	if _, ok := srv.sessions.Get("reset-busy"); !ok {
		t.Fatal("busy reset must not delete the session")
	}
}

// TestSessionClaimEmptyIdentity400: an admin session without a Discord identity
// (the auth-off-style edge) cannot claim — there is no owner to assign.
func TestSessionClaimEmptyIdentity400(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedOwned(t, srv, "claim-noid", "member-1", "Member One")
	// Admin role bypasses project ACL; empty Discord id reaches the identity gate.
	sid, csrf, err := srv.LoginAs("", "No Id", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/claim-noid/claim", sid, csrf, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

// TestSessionClaimDemotesPreviousOwner: a member claims a foreign unit; the
// previous owner becomes the sole co-owner (the lockout-breaker).
func TestSessionClaimDemotesPreviousOwner(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedOwned(t, srv, "claim-th", "member-1", "Member One")
	sid, csrf, err := srv.LoginAs("allow-user", "Allow User", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/claim-th/claim", sid, csrf, nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	e, ok := srv.sessions.Get("claim-th")
	if !ok {
		t.Fatal("session gone")
	}
	if e.OwnerID != "allow-user" {
		t.Fatalf("ownerID=%q want allow-user", e.OwnerID)
	}
	if !slices.Equal(e.CoOwnerIDs, []string{"member-1"}) {
		t.Fatalf("coOwners=%v want [member-1]", e.CoOwnerIDs)
	}
	assertAuditAction(t, srv, audit.ActionSessionClaim, true)
}

// TestSessionLabelNoOwnershipGate: label is allowlist-only (like Discord
// /label) — a member who is not the owner may still relabel.
func TestSessionLabelNoOwnershipGate(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedOwned(t, srv, "label-th", "member-1", "Member One")
	sid, csrf, err := srv.LoginAs("allow-user", "Allow", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/label-th/label", sid, csrf, url.Values{"label": {"blocked"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	e, _ := srv.sessions.Get("label-th")
	if e.Label != sessionstore.LabelBlocked || !e.LabelManual {
		t.Fatalf("label=%q manual=%v", e.Label, e.LabelManual)
	}
	assertAuditAction(t, srv, audit.ActionSessionLabel, true)
}

// TestSessionGoalUpdates: a member sets the sticky goal.
func TestSessionGoalUpdates(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedOwned(t, srv, "goal-th", "member-1", "Member One")
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/goal-th/goal", sid, csrf, url.Values{"goal": {"ship the widget"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	e, _ := srv.sessions.Get("goal-th")
	if e.Goal != "ship the widget" {
		t.Fatalf("goal=%q", e.Goal)
	}
	assertAuditAction(t, srv, audit.ActionSessionGoal, true)
}

// TestSessionCancelStopsRun: the owner cancels an active run.
func TestSessionCancelStopsRun(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	seedOwned(t, srv, "cancel-th", "member-1", "Member One")
	if err := bot.SeedActiveRunForTest(b, "cancel-th", "proj", "prompt", "live"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(b, "cancel-th") })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/cancel-th/cancel", sid, csrf, nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/cancel-th") || !strings.Contains(loc, "ok=") {
		t.Fatalf("Location=%q want ok flash", loc)
	}
	assertAuditAction(t, srv, audit.ActionSessionCancel, true)
}

// TestSessionQueueListRendersWithRemove: a real queued follow-up renders with
// position/author/intent and a Remove button for the item's author, and the
// Cancel button appears for the controlling owner. The live fragment stays
// chrome-free.
func TestSessionQueueListRendersWithRemove(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	seedOwned(t, srv, "queue-th", "member-1", "Member One")
	if err := bot.SeedQueuedFollowupForTest(b, "queue-th", "proj", "task-1", "member-1", "Member One", "add more tests"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(b, "queue-th") })
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}

	// Full page.
	req := httptest.NewRequest(http.MethodGet, "/sessions/queue-th", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="btn-cancel-run"`,
		`action="/sessions/queue-th/cancel"`,
		"Member One",
		"add more tests",
		`action="/sessions/queue-th/queue/remove"`,
		`name="task_id"`,
		`value="task-1"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("session page missing %q", want)
		}
	}

	// Live fragment must still be chrome-free.
	req = httptest.NewRequest(http.MethodGet, "/partials/sessions/queue-th", nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial status=%d", w.Code)
	}
	partial := w.Body.String()
	if !strings.Contains(partial, `value="task-1"`) {
		t.Fatal("partial missing queue Remove control")
	}
	if strings.Contains(partial, "<nav") || strings.Contains(partial, "sse-status") ||
		strings.Contains(partial, "session-continue-form") {
		t.Fatal("live fragment leaked layout chrome / rail form")
	}
}

// TestSessionQueueHidesRemoveForForeignItem: an unrelated member (not owner nor
// co-owner) sees the queue but no Remove button on someone else's item, and no
// Cancel button.
func TestSessionQueueHidesRemoveForForeignItem(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	seedOwned(t, srv, "queue-foreign", "member-1", "Member One")
	if err := bot.SeedQueuedFollowupForTest(b, "queue-foreign", "proj", "task-9", "member-1", "Member One", "owner follow-up"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(b, "queue-foreign") })
	sid, _, err := srv.LoginAs("allow-user", "Allow", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/queue-foreign", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "owner follow-up") {
		t.Fatal("queue item should still be visible to project members")
	}
	if strings.Contains(body, `action="/sessions/queue-foreign/queue/remove"`) {
		t.Fatal("non-controlling member must not see Remove on a foreign queue item")
	}
	if strings.Contains(body, `id="btn-cancel-run"`) {
		t.Fatal("non-controlling member must not see the Cancel button")
	}
	// Reset is a control action: hidden for the non-controlling member.
	if strings.Contains(body, `action="/sessions/queue-foreign/reset"`) {
		t.Fatal("non-controlling member must not see the Reset control")
	}
}

// TestSessionRailControlsForMember: an owning member sees the label/goal/claim
// rail controls and the danger-zone reset.
func TestSessionRailControlsForMember(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedOwned(t, srv, "rail-th", "member-1", "Member One")
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/rail-th", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`action="/sessions/rail-th/label"`,
		`id="btn-label"`,
		`action="/sessions/rail-th/goal"`,
		`id="btn-goal"`,
		`action="/sessions/rail-th/claim"`,
		`id="btn-claim"`,
		`action="/sessions/rail-th/reset"`,
		`id="btn-reset"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rail missing %q", want)
		}
	}
}

// TestSessionRailHiddenForViewer: a viewer gets the read-only fallback and none
// of the control forms.
func TestSessionRailHiddenForViewer(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	seedOwned(t, srv, "rail-viewer", "member-1", "Member One")
	sid, _, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/rail-viewer", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Read-only access") {
		t.Fatal("viewer missing read-only fallback")
	}
	for _, banned := range []string{
		`action="/sessions/rail-viewer/label"`,
		`action="/sessions/rail-viewer/goal"`,
		`action="/sessions/rail-viewer/claim"`,
		`action="/sessions/rail-viewer/reset"`,
	} {
		if strings.Contains(body, banned) {
			t.Fatalf("viewer must not see %q", banned)
		}
	}
}
