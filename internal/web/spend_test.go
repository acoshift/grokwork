package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
)

func ratePtr(v float64) *float64 { return &v }

// opusRates prices claude-opus-5 only, which leaves every grok run unpriced —
// the mixed state a real deployment lives in and the one the report must be
// honest about.
func opusRates(t *testing.T, cfg *config.Config) {
	t.Helper()
	if err := cfg.SetModelRates(map[string]config.ModelRate{
		"claude-opus-5": {
			InputPerMTok:      ratePtr(5),
			OutputPerMTok:     ratePtr(25),
			CacheReadPerMTok:  ratePtr(0.5),
			CacheWritePerMTok: ratePtr(6.25),
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func usageTurn(project, userID, user, model string, u *history.Usage) history.Turn {
	return history.Turn{
		At: "2026-07-20T10:00:00Z", User: user, UserID: userID, Prompt: "do the thing",
		Status: "done", Project: project, Agent: "claude", Model: model, Usage: u,
	}
}

func TestSpendPageReportsCostTokensAndUnpricedModels(t *testing.T) {
	srv, cfg, _ := testServer(t)
	opusRates(t, cfg)
	// $5 + $25 on a priced model, plus 510k tokens on one with no rate.
	for _, seed := range []struct {
		thread string
		turn   history.Turn
	}{
		{"t-cost", usageTurn("proj", "u1", "alice", "claude-opus-5", &history.Usage{InputTokens: 1_000_000})},
		{"t-cost", usageTurn("proj", "u2", "bob", "claude-opus-5", &history.Usage{OutputTokens: 1_000_000})},
		{"t-nocost", usageTurn("proj", "u1", "alice", "grok-4.5", &history.Usage{InputTokens: 500_000, OutputTokens: 10_000})},
	} {
		if err := srv.history.Append(seed.thread, seed.turn); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/spend", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	assertNavActive(t, body, "Spend")
	for _, want := range []string{
		`id="page-spend"`,
		// A total with unpriced turns behind it is a floor, and says so.
		"≥ $30.00",
		"1 of 3 turns have no rate",
		// 2.51M tokens across the three turns.
		"2.5M",
		// The unpriced model is named, not silently dropped.
		`id="spend-unpriced"`,
		"grok-4.5",
		// Rollups by project, person and session.
		`href="/projects/proj/spend"`,
		"alice",
		"bob",
		`href="/sessions/t-cost?project=proj"`,
		`href="/sessions/t-nocost?project=proj"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("spend page missing %q", want)
		}
	}
	// The three seeded turns with no usage record at all (testServer's thread-99)
	// are not zero-cost turns — they are unmeasured, and counting them would put
	// three free rows on the page.
	if strings.Contains(body, "thread-99") {
		t.Fatalf("turns without usage must not appear: %s", body)
	}
}

// The whole point of the pointer rates: with none configured the page reports
// tokens and says why there are no dollars, rather than showing $0.00.
func TestSpendPageWithoutRatesShowsTokensOnly(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.history.Append("t-cost",
		usageTurn("proj", "u1", "alice", "claude-opus-5", &history.Usage{InputTokens: 1_000_000})); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/spend", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "shows tokens only") {
		t.Fatalf("missing tokens-only notice: %s", body)
	}
	if !strings.Contains(body, "1.0M") {
		t.Fatal("tokens must still be reported")
	}
	if strings.Contains(body, "$0.00") {
		t.Fatalf("unpriced spend must never render $0.00: %s", body)
	}
}

func TestSpendScopedPageStaysInsideTheWorkspace(t *testing.T) {
	srv, cfg, _ := testServer(t)
	opusRates(t, cfg)
	if err := srv.history.Append("t-cost",
		usageTurn("proj", "u1", "alice", "claude-opus-5", &history.Usage{InputTokens: 1_000_000})); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/spend", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Workspace shell, and the workspace Spend item is the active one.
	if !strings.Contains(body, `data-scope="proj"`) {
		t.Fatal("scoped spend page must render the workspace shell")
	}
	if !strings.Contains(body, `href="/projects/proj/spend" data-icon="spend" class="active">`) {
		t.Fatalf("workspace nav item not active: %s", body)
	}
	if !strings.Contains(body, "$5.00") {
		t.Fatalf("scoped cost missing: %s", body)
	}
	// A one-project page has no By project table — the column would repeat the
	// heading on every row.
	if strings.Contains(body, `id="spend-by-project"`) {
		t.Fatal("scoped page must not render the by-project table")
	}
}

// Visibility is the load-bearing property of a cross-project money view: a member
// must not learn another project's burn rate, including through the person and
// session rollups.
func TestSpendPageFiltersByProjectVisibility(t *testing.T) {
	srv := twoProjectAuthServer(t)
	if err := srv.cfg.SetModelRates(map[string]config.ModelRate{
		"claude-opus-5": {InputPerMTok: ratePtr(5), OutputPerMTok: ratePtr(25)},
	}); err != nil {
		t.Fatal(err)
	}
	// Same person runs in both projects; only the public spend may be reported.
	if err := srv.history.Append("th-public",
		usageTurn("public", "member-1", "Member", "claude-opus-5", &history.Usage{InputTokens: 1_000_000})); err != nil {
		t.Fatal(err)
	}
	if err := srv.history.Append("th-secret",
		usageTurn("secret", "member-1", "Member", "claude-opus-5", &history.Usage{InputTokens: 7_000_000})); err != nil {
		t.Fatal(err)
	}

	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	get := func(userID, path string, role config.WebRole) string {
		t.Helper()
		id := sid
		if userID != "member-1" {
			var err error
			id, _, err = srv.LoginAs(userID, userID, role)
			if err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	body := get("member-1", "/spend", config.WebRoleMember)
	if !strings.Contains(body, `href="/projects/public/spend"`) {
		t.Fatalf("member missing own project: %s", body)
	}
	if strings.Contains(body, "/projects/secret/spend") || strings.Contains(body, "th-secret") {
		t.Fatalf("member sees hidden project's spend: %s", body)
	}
	// 7M hidden tokens must not reach the total or the person row either.
	if strings.Contains(body, "7.0M") || strings.Contains(body, "8.0M") {
		t.Fatalf("hidden tokens leaked into a rollup: %s", body)
	}
	if !strings.Contains(body, "$5.00") {
		t.Fatalf("visible cost missing: %s", body)
	}

	// An admin sees both, which is what makes the filtering above a real filter
	// rather than an empty store.
	adminBody := get("admin-1", "/spend", config.WebRoleAdmin)
	if !strings.Contains(adminBody, "/projects/secret/spend") || !strings.Contains(adminBody, "$35.00") {
		t.Fatalf("admin must see every project: %s", adminBody)
	}

	// Direct navigation to the hidden project's scoped page is refused, not just
	// unlinked.
	req := httptest.NewRequest(http.MethodGet, "/projects/secret/spend", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("hidden project's spend page must not render: %s", w.Body.String())
	}
}

func TestSessionPageShowsSessionSpend(t *testing.T) {
	srv, cfg, _ := testServer(t)
	opusRates(t, cfg)
	if err := srv.history.Append("thread-99",
		usageTurn("proj", "u1", "alice", "claude-opus-5", &history.Usage{
			InputTokens: 1_000_000, ContextTokens: 8445,
		})); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`id="session-spend"`, "1.0M tokens", "$5.00", "1 billed turn"} {
		if !strings.Contains(body, want) {
			t.Fatalf("session page missing %q", want)
		}
	}
	// The strip lives outside the SSE fragment: it only changes when a run seals a
	// turn, and re-rendering it on every dashboard tick would be pure noise.
	req = httptest.NewRequest(http.MethodGet, "/partials/sessions/thread-99", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `id="session-spend"`) {
		t.Fatal("spend strip must not be inside the live region")
	}
}

func TestSessionSpendWithoutRatesShowsTokensNotZero(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.history.Append("thread-99",
		usageTurn("proj", "u1", "alice", "grok-4.5", &history.Usage{InputTokens: 1_000})); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "1.0k tokens") {
		t.Fatalf("tokens missing: %s", body)
	}
	if strings.Contains(body, "$0.00") {
		t.Fatalf("unpriced session must not show $0.00: %s", body)
	}
}

func TestModelRatesConfigPageAndSave(t *testing.T) {
	srv, cfg, _ := testServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/config/model-rates", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-config-rates"`,
		`name="model" value="claude-opus-5"`,
		`name="model" value="grok-4.5"`,
		`name="cacheWrite"`,
		// Unset renders empty, not "0" — saving the form back must not invent rates.
		`name="input" value="" placeholder="unset"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rates page missing %q", want)
		}
	}

	post := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/config/model-rates", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// Rows are matched by position, so every row posts all five fields.
	ok := post(url.Values{
		"model":      {"claude-opus-5", "grok-4.5"},
		"input":      {"$5.00", ""},
		"output":     {"25", ""},
		"cacheRead":  {"0.5", ""},
		"cacheWrite": {"6.25", ""},
	})
	if ok.Code != http.StatusSeeOther && ok.Code != http.StatusFound {
		t.Fatalf("save status=%d body=%s", ok.Code, ok.Body.String())
	}
	r, found := cfg.ModelRateFor("claude-opus-5")
	if !found || r.InputPerMTok == nil || *r.InputPerMTok != 5 || r.CacheWritePerMTok == nil || *r.CacheWritePerMTok != 6.25 {
		t.Fatalf("rate not saved: %+v %v", r, found)
	}
	// An all-blank row stays unset rather than becoming a row of zeros.
	if _, found := cfg.ModelRateFor("grok-4.5"); found {
		t.Fatal("blank row must not create a rate")
	}

	// A typo fails the whole save: a dropped figure reads as "not configured",
	// which silently turns dollars off instead of reporting the mistake.
	bad := post(url.Values{
		"model":      {"claude-opus-5"},
		"input":      {"five dollars"},
		"output":     {"25"},
		"cacheRead":  {""},
		"cacheWrite": {""},
	})
	loc := bad.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("bad rate must redirect with an error: %q", loc)
	}
	if r, found := cfg.ModelRateFor("claude-opus-5"); !found || r.OutputPerMTok == nil || *r.OutputPerMTok != 25 {
		t.Fatalf("failed save must not have partially applied: %+v %v", r, found)
	}

	// Clearing every field turns dollar reporting off, deliberately and visibly.
	if code := post(url.Values{
		"model":      {"claude-opus-5"},
		"input":      {""},
		"output":     {""},
		"cacheRead":  {""},
		"cacheWrite": {""},
	}).Code; code != http.StatusSeeOther && code != http.StatusFound {
		t.Fatalf("clear status=%d", code)
	}
	if _, found := cfg.ModelRateFor("claude-opus-5"); found {
		t.Fatal("rates should be cleared")
	}
	if got := cfg.Snapshot().ModelRatesSet; got != 0 {
		t.Fatalf("ModelRatesSet=%d want 0", got)
	}
}

func TestSpendFormatting(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{
		{0, "0"}, {350, "350"}, {1_000, "1.0k"}, {25_391, "25.4k"},
		{510_000, "510k"}, {1_000_000, "1.0M"}, {2_510_000, "2.5M"},
		{1_500_000_000, "1.5B"},
	} {
		if got := formatTokens(c.n); got != c.want {
			t.Fatalf("formatTokens(%d)=%q want %q", c.n, got, c.want)
		}
	}
	// Small figures need more digits than large ones: a single cheap turn is
	// fractions of a cent, and rounding it to $0.00 would report it as free.
	for _, c := range []struct {
		v    float64
		want string
	}{
		{0, "$0.00"}, {0.0042, "$0.0042"}, {5, "$5.00"}, {31.5, "$31.50"}, {12345.6, "$12346"},
	} {
		if got := formatUSD(c.v); got != c.want {
			t.Fatalf("formatUSD(%v)=%q want %q", c.v, got, c.want)
		}
	}
}
