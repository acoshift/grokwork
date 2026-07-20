package web

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// TestPreviewServer boots the admin UI with seeded demo data for visual review
// of template/CSS changes. It never runs in CI: skipped unless
// GROKWORK_WEB_PREVIEW=1, then serves on 127.0.0.1:18787 until killed.
//
//	GROKWORK_WEB_PREVIEW=1 go test ./internal/web -run TestPreviewServer -timeout 0
func TestPreviewServer(t *testing.T) {
	if os.Getenv("GROKWORK_WEB_PREVIEW") != "1" {
		t.Skip("set GROKWORK_WEB_PREVIEW=1 to boot the preview server")
	}

	dir := t.TempDir()
	mkProj := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := &config.Config{
		DiscordToken:    "tok",
		DiscordClientID: "424242424242424242",
		AllowedUserIDs:  []string{"111111111111111111", "222222222222222222"},
		AllowedRoleIDs:  []string{"333333333333333333"},
		Projects: config.PathProjects(map[string]string{
			"webapp": mkProj("webapp"),
			"api":    mkProj("api"),
		}),
		Channels: map[string]string{
			"900000000000000001": "webapp",
			"900000000000000002": "api",
		},
		AllowedUsers: map[string]struct{}{"111111111111111111": {}, "222222222222222222": {}},
		AllowedRoles: map[string]struct{}{"333333333333333333": {}},
		GrokBin:      "grok",
		MaxTurns:     40,
		TimeoutMs:    1800000,
		HTTPListen:   "127.0.0.1:18787",
		ConfigPath:   filepath.Join(dir, "config.json"),
		DataDir:      filepath.Join(dir, "data"),
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	seedSessions := map[string]sessionstore.Entry{
		"1390000000000000001": {
			SessionID:      "sess-a1",
			Project:        "webapp",
			LastUser:       "mint#0",
			OwnerName:      "mint",
			OwnerID:        "111111111111111111",
			Origin:         "discord",
			Goal:           "Fix flaky checkout test and stabilise the payment retry queue",
			WorktreeBranch: "grok/discord/1390000000000000001",
			Label:          "needs_review",
			PRs: []sessionstore.TrackedPR{{
				URL: "https://github.com/acme/webapp/pull/128", Number: 128, State: "OPEN",
				Title: "fix: debounce payment retry queue", Checks: "✓ 4 · ✗ 1",
				Review: "CHANGES_REQUESTED", Owner: "acme", Repo: "webapp",
			}},
		},
		"1390000000000000002": {
			SessionID: "sess-b2", Project: "api", LastUser: "poon#0",
			OwnerName: "poon", Origin: "web",
			Goal:  "Add rate-limit headers to the public API",
			Label: "done",
			PRs: []sessionstore.TrackedPR{{
				URL: "https://github.com/acme/api/pull/86", Number: 86, State: "MERGED",
				Title: "feat: X-RateLimit headers", Checks: "✓ 6", Review: "APPROVED",
				Owner: "acme", Repo: "api",
			}},
		},
		"1390000000000000003": {
			SessionID: "sess-c3", Project: "webapp", LastUser: "mint#0",
			OwnerName: "mint", Origin: "discord",
			Goal:  "Migrate session cookies to SameSite=Lax",
			Label: "in_progress",
			PRs: []sessionstore.TrackedPR{{
				URL: "https://github.com/acme/webapp/pull/131", Number: 131, State: "OPEN",
				Title: "chore: SameSite=Lax rollout", Checks: "· 2 pending", IsDraft: true,
				Owner: "acme", Repo: "webapp",
			}},
		},
		"1390000000000000004": {
			SessionID: "sess-d4", Project: "api", LastUser: "beam#0",
			OwnerName: "beam", Origin: "discord",
			Goal: "Investigate slow N+1 queries on /orders",
		},
	}
	for id, e := range seedSessions {
		if err := store.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}

	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	turns := []struct {
		thread string
		turn   history.Turn
	}{
		{"1390000000000000001", history.Turn{User: "mint#0", Prompt: "The checkout E2E test is flaky on CI — it fails roughly 1 in 5 runs with a timeout waiting for the payment webhook. Find the race and fix it.", Response: "Found it: the retry queue debounces per-order but the test asserts on the first attempt. I widened the webhook wait and debounced the queue flush.\n\nOpened PR: https://github.com/acme/webapp/pull/128", Status: "done", Elapsed: "4m12s", Project: "webapp", SessionID: "sess-a1"}},
		{"1390000000000000001", history.Turn{User: "mint#0", Prompt: "CI is still red on the lint job — fix it and push.", Response: "Working…", Status: "error", ExitCode: 1, Error: "Reached max turns before a final reply", Elapsed: "12m40s", Project: "webapp", SessionID: "sess-a1"}},
		{"1390000000000000001", history.Turn{User: "poon#0", Prompt: "/fix-ci", Response: "gofmt drift in payment_retry.go — formatted, pushed, checks green except the flaky E2E which is queued.", Status: "done", Elapsed: "2m03s", Project: "webapp", SessionID: "sess-a1"}},
		{"1390000000000000002", history.Turn{User: "poon#0", Prompt: "Add standard X-RateLimit-Limit / Remaining / Reset headers to all public endpoints, with tests.", Response: "Done — middleware emits the three headers from the token bucket state; added table-driven tests.\n\nPR: https://github.com/acme/api/pull/86 (merged)", Status: "done", Elapsed: "9m51s", Project: "api", SessionID: "sess-b2"}},
		{"1390000000000000003", history.Turn{User: "mint#0", Prompt: "Start the SameSite=Lax migration for session cookies behind a flag.", Response: "", Status: "cancelled", Error: "Cancelled by owner via /cancel", Elapsed: "0m48s", Project: "webapp", SessionID: "sess-c3"}},
		{"1390000000000000003", history.Turn{User: "mint#0", Prompt: "Resume: SameSite=Lax behind GROK_COOKIE_LAX flag, draft PR is fine.", Response: "Draft PR up: https://github.com/acme/webapp/pull/131 — flag default off, e2e matrix added for both modes.", Status: "done", Elapsed: "6m22s", Project: "webapp", SessionID: "sess-c3"}},
	}
	for _, tt := range turns {
		if err := hist.Append(tt.thread, tt.turn); err != nil {
			t.Fatal(err)
		}
	}

	srv := New(cfg, store, hist, bot.New(cfg, store, hist))
	ln, err := net.Listen("tcp", "127.0.0.1:18787")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "preview: http://%s (kill the test to stop)\n", ln.Addr())
	if err := srv.App().Serve(ln); err != nil {
		t.Fatal(err)
	}
}
