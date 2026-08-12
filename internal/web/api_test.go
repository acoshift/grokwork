package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/apitoken"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func enableAPI(cfg *config.Config) {
	cfg.API = &config.APIConfig{Enabled: true}
}

func putTokenOnTeam(cfg *config.Config, actorID, template string, caps config.Capabilities) {
	pc := cfg.Projects["proj"]
	if pc.CapabilityTemplates == nil {
		pc.CapabilityTemplates = map[string]config.Capabilities{}
	}
	pc.CapabilityTemplates[template] = caps
	if pc.Teams == nil {
		pc.Teams = map[string]config.TeamConfig{}
	}
	pc.Teams["automation"] = config.TeamConfig{
		Label: "Automation", Members: []string{actorID}, Capabilities: template,
	}
	cfg.Projects["proj"] = pc
}

func mintAPIToken(t *testing.T, srv *Server, mask apitoken.CapsMask) (string, apitoken.Record) {
	t.Helper()
	wire, rec, err := srv.apiTokens.Mint(apitoken.MintOpts{
		Label:    "test-agent",
		Projects: []string{"proj"},
		Caps:     mask,
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire, rec
}

func apiJSON(t *testing.T, srv *Server, method, path, bearer string, body any, cookieSID string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookieSID != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieSID})
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestAPIOnDoesNotOpenBrowserWritesWhenAuthOff(t *testing.T) {
	srv, cfg, _ := testServer(t)
	enableAPI(cfg)
	if cfg.FeatureStartSessions() {
		t.Fatal("enabling API must not flip FeatureStartSessions on open LAN")
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/start", strings.NewReader("prompt=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("browser start with API on + auth off: status=%d want 404", w.Code)
	}
}

func TestAPIStartIdempotencyKey(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true})
	putTokenOnTeam(cfg, rec.ActorID, "automation-runner", config.Capabilities{StartSessions: true})

	body := map[string]string{"prompt": "investigate checkout", "mode": "investigate"}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	post := func(raw []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj/sessions", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+wire)
		req.Header.Set("Idempotency-Key", "same-key-1")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	w1 := post(raw)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first=%d %s", w1.Code, w1.Body.String())
	}
	w2 := post(raw)
	if w2.Code != http.StatusCreated {
		t.Fatalf("replay=%d %s", w2.Code, w2.Body.String())
	}
	if w1.Body.String() != w2.Body.String() {
		t.Fatalf("replay body changed:\n%s\n%s", w1.Body.String(), w2.Body.String())
	}
	other, err := json.Marshal(map[string]string{"prompt": "different", "mode": "investigate"})
	if err != nil {
		t.Fatal(err)
	}
	w3 := post(other)
	if w3.Code != http.StatusConflict {
		t.Fatalf("different body=%d %s", w3.Code, w3.Body.String())
	}
}

func TestAPIOffMutatingStartNotCreated(t *testing.T) {
	srv, _, _ := testServer(t)
	w := apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", "", map[string]string{"prompt": "x"}, "")
	if w.Code == http.StatusCreated {
		t.Fatalf("API off must not create: %s", w.Body.String())
	}
	w = apiJSON(t, srv, http.MethodGet, "/api/v1/health", "", nil, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"api":false`) {
		t.Fatalf("health=%d %s", w.Code, w.Body.String())
	}
}

func TestAPICookieOnlyUnauthorized(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	enableAPI(cfg)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := apiJSON(t, srv, http.MethodGet, "/api/v1/whoami", "", nil, sid)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAPIStartGetRoundTripTwice(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true})
	putTokenOnTeam(cfg, rec.ActorID, "automation-runner", config.Capabilities{StartSessions: true})

	var firstID string
	for i := range 2 {
		w := apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", wire, map[string]string{
			"prompt": "investigate checkout", "mode": "investigate",
		}, "")
		if w.Code != http.StatusCreated {
			t.Fatalf("start %d status=%d body=%s", i, w.Code, w.Body.String())
		}
		var out struct {
			SessionID  string `json:"sessionId"`
			URL        string `json:"url"`
			DiscordURL string `json:"discordUrl"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !gitworktree.IsWebUnitID(out.SessionID) {
			t.Fatalf("not web-native: %q", out.SessionID)
		}
		if out.DiscordURL != "" {
			t.Fatalf("discordUrl=%q", out.DiscordURL)
		}
		if !strings.Contains(out.URL, "/sessions/"+out.SessionID) {
			t.Fatalf("url=%q", out.URL)
		}
		g := apiJSON(t, srv, http.MethodGet, "/api/v1/sessions/"+out.SessionID, wire, nil, "")
		if g.Code != http.StatusOK {
			t.Fatalf("get %d status=%d body=%s", i, g.Code, g.Body.String())
		}
		if !strings.Contains(g.Body.String(), `"sessionId":"`+out.SessionID+`"`) {
			t.Fatalf("get body=%s", g.Body.String())
		}
		firstID = out.SessionID
	}

	// Other token cannot see it.
	wire2, rec2 := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true})
	putTokenOnTeam(cfg, rec2.ActorID, "automation-runner", config.Capabilities{StartSessions: true})
	g := apiJSON(t, srv, http.MethodGet, "/api/v1/sessions/"+firstID, wire2, nil, "")
	if g.Code != http.StatusNotFound {
		t.Fatalf("other token GET status=%d", g.Code)
	}
}

func TestAPIUnmappedTokenCannotStart(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true})
	pc := cfg.Projects["proj"]
	pc.AllowedUserIDs = append(pc.AllowedUserIDs, rec.ActorID)
	cfg.Projects["proj"] = pc

	w := apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", wire, map[string]string{
		"prompt": "x", "mode": "investigate",
	}, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAPIBuilderTeamStartSessionsMaskCannotFixOrIssue(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true})
	putTokenOnTeam(cfg, rec.ActorID, "builder", config.BuiltinCapabilityTemplates["builder"])

	w := apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", wire, map[string]string{
		"prompt": "ship it", "mode": "fix",
	}, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("fix status=%d %s", w.Code, w.Body.String())
	}
	w = apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", wire, map[string]string{
		"prompt": "x", "model": "grok-4.5",
	}, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("model status=%d %s", w.Code, w.Body.String())
	}
	w = apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/issues", wire, map[string]string{
		"title": "t", "kind": "bug",
	}, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("issue status=%d %s", w.Code, w.Body.String())
	}
	w = apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", wire, map[string]string{
		"prompt": "look", "mode": "investigate",
	}, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("investigate status=%d %s", w.Code, w.Body.String())
	}
}

func TestAPIIssueKindRequired(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true, GithubWrites: true})
	putTokenOnTeam(cfg, rec.ActorID, "builder", config.BuiltinCapabilityTemplates["builder"])

	for _, kind := range []string{"", "chore"} {
		w := apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/issues", wire, map[string]string{
			"title": "t", "kind": kind,
		}, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("kind=%q status=%d %s", kind, w.Code, w.Body.String())
		}
	}
}

func TestAPIIssueCreateInjectedRunner(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true, GithubWrites: true})
	putTokenOnTeam(cfg, rec.ActorID, "builder", config.BuiltinCapabilityTemplates["builder"])
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "issue create") {
			return []byte("https://github.com/acme/app/issues/9\n"), nil
		}
		return []byte("{}"), nil
	}
	w := apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/issues", wire, map[string]string{
		"title": "Flaky checkout", "kind": "bug", "body": "repro",
	}, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"number":9`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestAPIContinueCancelOwnership(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true})
	putTokenOnTeam(cfg, rec.ActorID, "automation-runner", config.Capabilities{StartSessions: true})

	if err := srv.sessions.Set("human-th", sessionstore.Entry{
		Project: "proj", Goal: "human work",
	}); err != nil {
		t.Fatal(err)
	}
	e, _ := srv.sessions.Get("human-th")
	e.SetOwner("member-1", "M")
	if err := srv.sessions.Set("human-th", e); err != nil {
		t.Fatal(err)
	}

	w := apiJSON(t, srv, http.MethodGet, "/api/v1/sessions/human-th", wire, nil, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET other=%d", w.Code)
	}
	w = apiJSON(t, srv, http.MethodPost, "/api/v1/sessions/human-th/continue", wire, map[string]string{"prompt": "x"}, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("continue other=%d %s", w.Code, w.Body.String())
	}

	started := apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", wire, map[string]string{
		"prompt": "look", "mode": "investigate",
	}, "")
	if started.Code != http.StatusCreated {
		t.Fatalf("own start=%d %s", started.Code, started.Body.String())
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	c := apiJSON(t, srv, http.MethodPost, "/api/v1/sessions/"+out.SessionID+"/continue", wire, map[string]string{"prompt": "more"}, "")
	if c.Code != http.StatusOK {
		t.Fatalf("own continue=%d %s", c.Code, c.Body.String())
	}
	can := apiJSON(t, srv, http.MethodPost, "/api/v1/sessions/"+out.SessionID+"/cancel", wire, nil, "")
	if can.Code != http.StatusOK {
		t.Fatalf("own cancel=%d %s", can.Code, can.Body.String())
	}
}

func TestAPITwoClaimsKeepTokenControl(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true})
	putTokenOnTeam(cfg, rec.ActorID, "automation-runner", config.Capabilities{StartSessions: true})

	w := apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", wire, map[string]string{
		"prompt": "look", "mode": "investigate",
	}, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("start=%d %s", w.Code, w.Body.String())
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if err := b.ClaimThread(out.SessionID, bot.Actor{ID: "human-a", DisplayName: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := b.ClaimThread(out.SessionID, bot.Actor{ID: "human-b", DisplayName: "B"}); err != nil {
		t.Fatal(err)
	}
	g := apiJSON(t, srv, http.MethodGet, "/api/v1/sessions/"+out.SessionID, wire, nil, "")
	if g.Code != http.StatusOK {
		t.Fatalf("token GET after two claims=%d %s", g.Code, g.Body.String())
	}
}

func TestAPIStartRateKeyedByTokenNotCookie(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true})
	putTokenOnTeam(cfg, rec.ActorID, "automation-runner", config.Capabilities{StartSessions: true})

	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// Exhaust the human cookie actor bucket. API must still start.
	if !srv.startLimiter().AllowN("member-1", startRateMax) {
		t.Fatal("could not fill member bucket")
	}
	w := apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", wire, map[string]string{
		"prompt": "look", "mode": "investigate",
	}, sid)
	if w.Code != http.StatusCreated {
		t.Fatalf("API start after cookie bucket full=%d %s", w.Code, w.Body.String())
	}
	// Exhaust the token bucket (one hit already from the start above).
	for srv.startLimiter().AllowN(rec.ActorID, 1) {
	}
	w = apiJSON(t, srv, http.MethodPost, "/api/v1/projects/proj/sessions", wire, map[string]string{
		"prompt": "again", "mode": "investigate",
	}, "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d %s", w.Code, w.Body.String())
	}
}

func TestAPIWhoamiListsProjects(t *testing.T) {
	srv, cfg, _ := testServer(t)
	enableAPI(cfg)
	wire, rec := mintAPIToken(t, srv, apitoken.CapsMask{StartSessions: true})
	putTokenOnTeam(cfg, rec.ActorID, "automation-runner", config.Capabilities{StartSessions: true})
	w := apiJSON(t, srv, http.MethodGet, "/api/v1/whoami", wire, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), rec.ActorID) || !strings.Contains(w.Body.String(), `"proj"`) {
		t.Fatalf("whoami=%s", w.Body.String())
	}
}
