package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestSessionPageShowsAndServesAttachments(t *testing.T) {
	srv, _, dir := testServer(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	pngPath := filepath.Join(src, "shot.png")
	txtPath := filepath.Join(src, "stack.log")
	if err := os.WriteFile(pngPath, []byte("png-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txtPath, []byte("panic: boom"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.history.AppendFiles("thread-99", history.Turn{
		User: "alice#0", Prompt: "what's in this screenshot?",
		Response: "Looks like a checkout timeout.", Status: "done", Project: "proj",
	}, []history.File{
		{Path: pngPath, Name: "shot.png", ContentType: "image/png"},
		{Path: txtPath, Name: "stack.log", ContentType: "text/plain"},
	}); err != nil {
		t.Fatal(err)
	}

	h := srv.Handler()
	body := getBody(t, h, "/sessions/thread-99")
	for _, want := range []string{
		`class="turn-atts"`,
		`class="turn-att-img"`,
		`src="/sessions/thread-99/turns/4/files/shot.png"`,
		`class="turn-att-chip"`,
		`href="/sessions/thread-99/turns/4/files/stack.log"`,
		"stack.log",
		"what's in this screenshot?",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("session page missing %q", want)
		}
	}

	hist := getBody(t, h, "/history/thread-99")
	if !strings.Contains(hist, `src="/sessions/thread-99/turns/4/files/shot.png"`) {
		t.Fatal("history detail missing attachment")
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99/turns/4/files/shot.png", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("png status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("ctype=%q", w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("disposition=%q", w.Header().Get("Content-Disposition"))
	}
	if w.Body.String() != "png-bytes" {
		t.Fatalf("body=%q", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/sessions/thread-99/turns/4/files/stack.log", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("log status=%d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("log should download: %q", w.Header().Get("Content-Disposition"))
	}
	if w.Body.String() != "panic: boom" {
		t.Fatalf("log body=%q", w.Body.String())
	}

	for _, path := range []string{
		"/sessions/thread-99/turns/4/files/missing.bin",
		"/sessions/thread-99/turns/1/files/shot.png",
		"/sessions/thread-99/turns/99/files/shot.png",
		"/sessions/no-such/turns/1/files/shot.png",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d want 404/403", path, w.Code)
		}
	}
}

func TestSessionLiveRunAttachment(t *testing.T) {
	srv, _, dir := testServer(t)
	live := filepath.Join(dir, "live.txt")
	if err := os.WriteFile(live, []byte("live-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bot.SeedActiveRunForTest(srv.bot, "thread-99", "proj", "with file", "streaming…"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(srv.bot, "thread-99") })
	bot.PublishRunAttachmentsForTest(srv.bot, "thread-99", []string{live})

	h := srv.Handler()
	body := getBody(t, h, "/sessions/thread-99")
	if !strings.Contains(body, `href="/sessions/thread-99/run/files/live.txt"`) {
		t.Fatalf("live turn missing attachment: %s", body)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99/run/files/live.txt", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("live file status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "live-file" {
		t.Fatalf("live body=%q", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/sessions/thread-99/run/files/nope.txt", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown live file status=%d", w.Code)
	}
}

func TestSessionFileSVGIsDownload(t *testing.T) {
	srv, _, dir := testServer(t)
	src := filepath.Join(dir, "evil.svg")
	if err := os.WriteFile(src, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("s-svg", sessionstore.Entry{Project: "proj", Origin: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.history.AppendFiles("s-svg", history.Turn{
		Prompt: "logo", Status: "done", Project: "proj",
	}, []history.File{
		{Path: src, Name: "logo.svg", ContentType: "image/svg+xml"},
	}); err != nil {
		t.Fatal(err)
	}
	body := getBody(t, srv.Handler(), "/sessions/s-svg")
	if strings.Contains(body, `class="turn-att-img"`) {
		t.Fatal("svg must not render as an image")
	}
	if !strings.Contains(body, `class="turn-att-chip"`) {
		t.Fatal("svg should be a download chip")
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s-svg/turns/1/files/logo.svg", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("svg must be attachment: %q", w.Header().Get("Content-Disposition"))
	}
	if strings.Contains(w.Header().Get("Content-Type"), "svg") {
		t.Fatalf("svg ctype=%q", w.Header().Get("Content-Type"))
	}
}

func TestSessionPageShowsAndServesArtifacts(t *testing.T) {
	srv, _, dir := testServer(t)
	src := filepath.Join(dir, "report.xlsx")
	if err := os.WriteFile(src, []byte("sheet-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	att, err := srv.history.PutArtifact("thread-99", history.File{
		Path: src, Name: "report.xlsx", ContentType: "application/vnd.ms-excel", Rel: "dist/report.xlsx",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.history.Append("thread-99", history.Turn{
		User: "alice#0", Prompt: "export the sheet",
		Response: "Here is the spreadsheet.", Status: "done", Project: "proj",
		Artifacts: []history.Attachment{att},
	}); err != nil {
		t.Fatal(err)
	}

	h := srv.Handler()
	body := getBody(t, h, "/sessions/thread-99")
	for _, want := range []string{
		`class="turn-atts turn-atts-out"`,
		`href="/sessions/thread-99/artifacts/report.xlsx"`,
		"report.xlsx",
		"Files",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("session page missing %q", want)
		}
	}

	hist := getBody(t, h, "/history/thread-99")
	if !strings.Contains(hist, `href="/sessions/thread-99/artifacts/report.xlsx"`) {
		t.Fatal("history detail missing artifact")
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99/artifacts/report.xlsx", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("xlsx status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if w.Body.String() != "sheet-bytes" {
		t.Fatalf("body=%q", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/sessions/thread-99/artifacts/missing.bin", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d", w.Code)
	}
}

func TestSessionLiveRunArtifact(t *testing.T) {
	srv, _, dir := testServer(t)
	src := filepath.Join(dir, "live.xlsx")
	if err := os.WriteFile(src, []byte("live-sheet"), 0o600); err != nil {
		t.Fatal(err)
	}
	att, err := srv.history.PutArtifact("thread-99", history.File{
		Path: src, Name: "live.xlsx", ContentType: "application/vnd.ms-excel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.SeedActiveRunForTest(srv.bot, "thread-99", "proj", "export", "streaming…"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(srv.bot, "thread-99") })
	bot.PublishRunArtifactForTest(srv.bot, "thread-99", att)

	h := srv.Handler()
	body := getBody(t, h, "/sessions/thread-99")
	if !strings.Contains(body, `href="/sessions/thread-99/artifacts/live.xlsx"`) {
		t.Fatalf("live turn missing artifact: %s", body)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99/artifacts/live.xlsx", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("live artifact status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "live-sheet" {
		t.Fatalf("live body=%q", w.Body.String())
	}
}

func TestSessionLivePartialKeepsAttachments(t *testing.T) {
	srv, _, dir := testServer(t)
	src := filepath.Join(dir, "a.png")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.history.AppendFiles("thread-99", history.Turn{
		Prompt: "img", Status: "done", Project: "proj",
	}, []history.File{{Path: src, Name: "a.png", ContentType: "image/png"}}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/partials/sessions/thread-99", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="turn-att-img"`) {
		t.Fatal("record partial missing attachment")
	}
	if strings.Contains(body, "session-continue-form") {
		t.Fatal("continue form must not be in live partial")
	}
}
