package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Real PNG magic so http.DetectContentType returns image/png.
var testPNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 0, 0, 0, 0}

// Minimal PDF header — start/continue/case used to reject anything not image/*.
var testPDF = []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")

func postMultipart(t *testing.T, srv *Server, path, sid, csrf string, fields map[string]string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if csrf != "" {
		if err := w.WriteField("csrf", csrf); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		part, err := w.CreateFormFile("images", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSessionContinueMultipartPNG(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := srv.sessions.Set("s-att", sessionstore.Entry{Project: "proj", Origin: "web"}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postMultipart(t, srv, "/sessions/s-att/continue", sid, csrf,
		map[string]string{"prompt": "check this screenshot"},
		map[string][]byte{"shot.png": testPNG},
	)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "err=") {
		t.Fatalf("unexpected err redirect: %s", loc)
	}
	// Staging should be empty after successful hand-off (resume copies + deletes).
	bot.WaitIdleForTest(b, 5*time.Second)
	stage := filepath.Join(srv.cfg.DataDir, "attachments", "web")
	if entries, _ := os.ReadDir(stage); len(entries) != 0 {
		t.Fatalf("staging leftovers after success: %v", entries)
	}
}

func TestSessionContinuePDFAccepted(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := srv.sessions.Set("s-pdf", sessionstore.Entry{Project: "proj", Origin: "web"}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postMultipart(t, srv, "/sessions/s-pdf/continue", sid, csrf,
		map[string]string{"prompt": "here is a spec"},
		map[string][]byte{"spec.pdf": testPDF},
	)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "err=") {
		t.Fatalf("unexpected err redirect: %s", loc)
	}
	bot.WaitIdleForTest(b, 5*time.Second)
	stage := filepath.Join(srv.cfg.DataDir, "attachments", "web")
	if entries, _ := os.ReadDir(stage); len(entries) != 0 {
		t.Fatalf("staging leftovers after success: %v", entries)
	}
}

func TestSessionContinueURLEncodedStillWorks(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := srv.sessions.Set("s-url", sessionstore.Entry{Project: "proj", Origin: "web"}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/s-url/continue", sid, csrf, url.Values{
		"prompt": {"plain follow-up without files"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "err=") {
		t.Fatalf("urlencoded continue failed: %s", loc)
	}
}

func TestCaseNewMultipartPNG(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postMultipart(t, srv, "/projects/proj/cases/new", sid, csrf,
		map[string]string{
			"title":    "Error screen from customer",
			"severity": "high",
			"notes":    "see attached",
		},
		map[string][]byte{"err.png": testPNG},
	)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("Location=%q", loc)
	}
	if strings.Contains(loc, "err=") {
		t.Fatalf("unexpected err: %s", loc)
	}
	if !strings.Contains(loc, "investigat") {
		t.Fatalf("want investigating flash: %s", loc)
	}
}

func TestCaseNewPDFAccepted(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postMultipart(t, srv, "/projects/proj/cases/new", sid, csrf,
		map[string]string{"title": "Has a spec", "severity": "low"},
		map[string][]byte{"log.txt": []byte("plain text"), "spec.pdf": testPDF},
	)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("Location=%q", loc)
	}
	if strings.Contains(loc, "err=") {
		t.Fatalf("unexpected err: %s", loc)
	}
	if !strings.Contains(loc, "investigat") {
		t.Fatalf("files-only must report investigating flash: %s", loc)
	}
}

func TestCaseNewURLEncodedStillWorks(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, csrf, url.Values{
		"title":    {"URL encoded case"},
		"severity": {"medium"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "ok=Case+opened") {
		t.Fatalf("Location=%q", loc)
	}
}

func TestCaseNewAttachmentsOnlyInvestigatingFlash(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postMultipart(t, srv, "/projects/proj/cases/new", sid, csrf,
		map[string]string{"title": "Attachments only case", "severity": "medium"},
		map[string][]byte{"a.png": testPNG},
	)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "investigat") {
		t.Fatalf("attachments-only must report investigating flash: %s", loc)
	}
	// Phase should have promoted to investigate.
	// Thread id is th-web-1 from FakeThreadAPI.
	e, ok := srv.sessions.Get("th-web-1")
	if !ok {
		t.Fatal("case session missing")
	}
	if e.Phase != "investigate" {
		t.Fatalf("phase=%q want investigate", e.Phase)
	}
}

func TestStartMultipartPNG(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postMultipart(t, srv, "/projects/proj/start", sid, csrf,
		map[string]string{"prompt": "look at this screenshot"},
		map[string][]byte{"shot.png": testPNG},
	)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("Location=%q", loc)
	}
	if strings.Contains(loc, "err=") {
		t.Fatalf("unexpected err: %s", loc)
	}
	bot.WaitIdleForTest(b, 5*time.Second)
	stage := filepath.Join(srv.cfg.DataDir, "attachments", "web")
	if entries, _ := os.ReadDir(stage); len(entries) != 0 {
		t.Fatalf("staging leftovers after success: %v", entries)
	}
}

func TestStartPDFAccepted(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postMultipart(t, srv, "/projects/proj/start", sid, csrf,
		map[string]string{"prompt": "here is a spec"},
		map[string][]byte{"spec.pdf": testPDF, "notes.txt": []byte("plain notes")},
	)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("Location=%q", loc)
	}
	if strings.Contains(loc, "err=") {
		t.Fatalf("unexpected err: %s", loc)
	}
	bot.WaitIdleForTest(b, 5*time.Second)
	stage := filepath.Join(srv.cfg.DataDir, "attachments", "web")
	if entries, _ := os.ReadDir(stage); len(entries) != 0 {
		t.Fatalf("staging leftovers after success: %v", entries)
	}
}

func TestStartURLEncodedStillWorks(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"plain task without files"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("Location=%q", loc)
	}
	if strings.Contains(loc, "err=") {
		t.Fatalf("urlencoded start failed: %s", loc)
	}
}

func TestCaseNewPageRendersImageInput(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/cases/new", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="case-images"`,
		`name="images"`,
		`enctype="multipart/form-data"`,
		`id="case-attach-chips"`,
		"With notes or files",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in case new page", want)
		}
	}
	if strings.Contains(body, `accept="image/*"`) {
		t.Fatal("case file input must not restrict the picker to images")
	}
}

func TestSessionPageRendersAttachControls(t *testing.T) {
	srv, _ := addressEnabledServer(t)
	if err := srv.sessions.Set("s1", sessionstore.Entry{Project: "proj"}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`enctype="multipart/form-data"`,
		`id="continue-images"`,
		`id="btn-attach"`,
		`id="continue-attach-chips"`,
		`id="btn-continue"`,
		`id="session-continue-form"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(body, `accept="image/*"`) {
		t.Fatal("continue file input must not restrict the picker to images")
	}
	if !strings.Contains(body, `aria-label="Attach files"`) {
		t.Fatal("continue attach button must say files, not images")
	}
}
