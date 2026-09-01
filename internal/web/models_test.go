package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
)

func TestDisabledModelsConfigPageAndSave(t *testing.T) {
	srv, cfg, _ := testServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/config/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-config-models"`,
		`data-agent="claude"`,
		`data-agent="cursor"`,
		`data-family="gpt"`,
		`name="model" value="claude-opus-5"`,
		`name="model" value="gpt-5.6-sol-medium"`,
		`name="action" value="disable-agent"`,
		`name="agent" value="claude"`,
		`name="action" value="disable-family"`,
		`name="family" value="gpt"`,
		"Disable all Claude",
		"Disable all GPT",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("models page missing %q", want)
		}
	}

	post := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/config/models", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	indiv := post(url.Values{
		"action": {"disable"},
		"model":  {"claude-opus-5"},
	})
	if indiv.Code != http.StatusSeeOther && indiv.Code != http.StatusFound {
		t.Fatalf("disable one status=%d body=%s", indiv.Code, indiv.Body.String())
	}
	if cfg.ModelAllowed("claude-opus-5") {
		t.Fatal("individual disable did not persist")
	}
	if !cfg.ModelAllowed("claude-haiku-4-5") {
		t.Fatal("sibling Claude model was disabled by an individual toggle")
	}

	bulk := post(url.Values{
		"action": {"disable-family"},
		"agent":  {"cursor"},
		"family": {"gpt"},
	})
	if bulk.Code != http.StatusSeeOther && bulk.Code != http.StatusFound {
		t.Fatalf("disable family status=%d body=%s", bulk.Code, bulk.Body.String())
	}
	if cfg.ModelAllowed("gpt-5.6-sol-medium") {
		t.Fatal("Cursor GPT family disable did not persist")
	}
	if !cfg.ModelAllowed("composer-2.5") {
		t.Fatal("Cursor non-GPT was disabled with the GPT family")
	}

	// Reload the page: disabled names stay listed (admin can re-enable) and the
	// store still matches.
	req = httptest.NewRequest(http.MethodGet, "/config/models", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reload status=%d", w.Code)
	}
	reload := w.Body.String()
	if !strings.Contains(reload, `name="action" value="enable"`) {
		t.Fatal("reload must offer enable for a disabled model")
	}
	if !strings.Contains(reload, `name="model" value="claude-opus-5"`) {
		t.Fatal("disabled model missing from reload")
	}

	claude := post(url.Values{
		"action": {"disable-agent"},
		"agent":  {"claude"},
	})
	if claude.Code != http.StatusSeeOther && claude.Code != http.StatusFound {
		t.Fatalf("disable agent status=%d body=%s", claude.Code, claude.Body.String())
	}
	for _, opt := range grokrun.ModelOptions() {
		if opt.Agent == grokrun.AgentClaude && cfg.ModelAllowed(opt.Value) {
			t.Errorf("claude %q still allowed after bulk disable", opt.Value)
		}
	}
}

func TestAttachModelPickerSkipsDisabledDefaultLabel(t *testing.T) {
	srv, cfg, _ := testServer(t)
	if err := cfg.SetAgentSettings(config.AgentSettings{
		Agent: "grok", Model: "claude-opus-5",
		GrokBin: "grok", ClaudeBin: "claude", CursorBin: "cursor-agent",
	}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("claude-opus-5", true); err != nil {
		t.Fatal(err)
	}
	d := pageData{CanStartSession: true, UserID: "u0"}
	srv.attachModelPicker(&d, "proj", cfg.TaskModel())
	if !d.CanSelectModel {
		t.Fatal("builder picker should render")
	}
	if d.ModelDefaultLabel == "claude-opus-5" {
		t.Fatalf("Default option advertised the disabled model %q", d.ModelDefaultLabel)
	}
	for _, g := range d.ModelGroups {
		for _, c := range g.Choices {
			if c.Value == "claude-opus-5" {
				t.Fatal("picker still lists the disabled model")
			}
		}
	}
}

func TestDisabledModelsNonAdminPOSTDoesNotPersist(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"action": {"disable"},
		"model":  {"claude-opus-5"},
		"csrf":   {csrf},
	}
	req := httptest.NewRequest(http.MethodPost, "/config/models", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member POST status=%d body=%s", w.Code, w.Body.String())
	}
	if !cfg.ModelAllowed("claude-opus-5") {
		t.Fatal("non-admin POST persisted a disable")
	}
}
