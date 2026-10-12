package web

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveLogged hits the handler through a real net/http server so a second
// WriteHeader is reported the same way production logs it.
func serveLogged(t *testing.T, h http.Handler, path string) (code int, ctype, body, logs string) {
	t.Helper()
	var buf bytes.Buffer
	ts := httptest.NewUnstartedServer(h)
	ts.Config.ErrorLog = log.New(&buf, "", 0)
	ts.Start()
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(raw), buf.String()
}

func TestPRDetailWriteHeaderOnce(t *testing.T) {
	srv := workflowServer(t)
	h := srv.Handler()
	cases := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
		banBody    string
	}{
		{"page", "/prs/acme/app/9?project=proj", http.StatusOK, `id="page-pr-detail"`, ""},
		{"gates", "/partials/prs/acme/app/9/gates?project=proj", http.StatusOK, `class="pr-gates"`, `id="page-pr-detail"`},
		{"ask", "/partials/prs/acme/app/9/ask?project=proj", http.StatusOK, "", `id="page-pr-detail"`},
		{"ship", "/ship", http.StatusOK, `id="page-ship"`, ""},
		{"forbidden", "/prs/other/repo/1?project=proj", http.StatusForbidden, "not in project", `id="page-pr-detail"`},
		{"bad path", "/prs/acme/app/0?project=proj", http.StatusBadRequest, "invalid PR path", `id="page-pr-detail"`},
		{"gates forbidden", "/partials/prs/other/repo/1/gates?project=proj", http.StatusForbidden, "not in project", `class="pr-gates"`},
		{"ask forbidden", "/partials/prs/other/repo/1/ask?project=proj", http.StatusForbidden, "not in project", `id="pr-ask"`},
		{"ask run bad", "/partials/prs/acme/app/0/ask/run?project=proj", http.StatusBadRequest, "invalid PR path", `id="pr-ask"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, ctype, body, logs := serveLogged(t, h, tc.path)
			if code != tc.wantStatus {
				t.Fatalf("status=%d want %d body=%s", code, tc.wantStatus, body)
			}
			if tc.wantStatus >= 400 && !strings.HasPrefix(ctype, "text/plain") {
				t.Fatalf("error content-type=%q body=%s", ctype, body)
			}
			if tc.wantBody != "" && !strings.Contains(body, tc.wantBody) {
				t.Fatalf("body missing %q: %s", tc.wantBody, body)
			}
			if tc.banBody != "" && strings.Contains(body, tc.banBody) {
				t.Fatalf("body should not contain %q: %s", tc.banBody, body)
			}
			if strings.Contains(logs, "superfluous response.WriteHeader") {
				t.Fatalf("superfluous WriteHeader:\n%s\nbody=%s", logs, body)
			}
		})
	}
}
