package web

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/deploy"
	"github.com/acoshift/grokwork/internal/filestore"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/reviewstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// TestPreviewServer boots the admin UI with seeded demo data for visual review
// of template/CSS changes. It never runs in CI: skipped unless
// GROKWORK_WEB_PREVIEW=1, then serves on 127.0.0.1:18787 until killed.
// GROKWORK_WEB_PREVIEW_DELAY_MS adds artificial latency to page/partial
// requests (not /static, not /events) so loading states are observable.
//
//	GROKWORK_WEB_PREVIEW=1 GROKWORK_WEB_PREVIEW_DELAY_MS=800 go test ./internal/web -run TestPreviewServer -timeout 0
func TestPreviewServer(t *testing.T) {
	if os.Getenv("GROKWORK_WEB_PREVIEW") != "1" {
		t.Skip("set GROKWORK_WEB_PREVIEW=1 to boot the preview server")
	}

	dir := t.TempDir()
	mkProj := func(name string) string {
		p := filepath.Join(dir, name)
		// .git marker so ResolveLocalRepo treats the project as a checkout and
		// the commits browser renders; every git call hits the fake runner.
		if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := &config.Config{
		DiscordToken:    "tok",
		DiscordClientID: "424242424242424242",
		Projects: config.ProjectsMap{
			// webapp exercises the full Access tab: safe team on, a builder team
			// and an access-only team, an explicit direct-member template, an
			// unmapped direct member on the default fallback, and an inert
			// capability map (no membership at all).
			"webapp": {
				Path:              mkProj("webapp"),
				AllowedUserIDs:    []string{"111111111111111111", "222222222222222222"},
				SafeTeamMode:      new(true),
				CherryPickTargets: []string{"staging", "production"},
				// SLA targets so the case board shows breached / on-hold chips
				// and the Workflow tab renders a filled-in form.
				SLA: map[string]config.SLATarget{
					"critical": {FirstResponseMinutes: previewMinutes(60), ResolutionMinutes: previewMinutes(480)},
					"high":     {FirstResponseMinutes: previewMinutes(240), ResolutionMinutes: previewMinutes(2880)},
					"medium":   {FirstResponseMinutes: previewMinutes(1440)},
				},
				CapabilityByUser: map[string]string{
					"111111111111111111": "admin",
					"444444444444444444": "approver",
				},
				Teams: map[string]config.TeamConfig{
					"eng": {
						Label:        "Engineering",
						Members:      []string{"discord:555555555555555555", "666666666666666666"},
						Capabilities: "builder",
					},
					"support": {
						Label:   "Support",
						Members: []string{"oidc:alice"},
					},
				},
			},
			"api": {
				Path:              mkProj("api"),
				AllowedUserIDs:    []string{"111111111111111111", "222222222222222222"},
				CherryPickTargets: []string{"staging"},
				ForcePushTargets:  new(true),
				Teams: map[string]config.TeamConfig{
					"eng": {Label: "Engineering", Members: []string{"discord:555555555555555555"}, Capabilities: "builder"},
				},
			},
		},
		Channels: map[string]string{
			"900000000000000001": "webapp",
			"900000000000000002": "api",
		},
		GrokBin: "grok",
		// Only the claude models are priced, deliberately: the spend report has to
		// be reviewable in the mixed state a real deployment lives in, where some
		// runs show dollars and the rest show tokens with a "no rate" badge.
		ModelRates: map[string]config.ModelRate{
			"claude-opus-5":    previewRate(5, 25, 0.5, 6.25),
			"claude-sonnet-5":  previewRate(3, 15, 0.3, 3.75),
			"claude-haiku-4-5": previewRate(1, 5, 0.1, 1.25),
		},
		MaxTurns:   40,
		TimeoutMs:  1800000,
		HTTPListen: "127.0.0.1:18787",
		ConfigPath: filepath.Join(dir, "config.json"),
		DataDir:    filepath.Join(dir, "data"),
	}
	// GROKWORK_WEB_PREVIEW_AUTH=1: turn on web auth + every write feature so
	// action surfaces (PR detail rail, merge/review forms) render; a ready-made
	// admin session cookie is printed at startup.
	previewAuth := os.Getenv("GROKWORK_WEB_PREVIEW_AUTH") == "1"
	if previewAuth {
		cfg.DiscordClientSecret = "preview-secret"
		cfg.WebPublicBaseURL = "http://127.0.0.1:18787"
		cfg.WebAuth = &config.WebAuthConfig{
			Enabled:         true,
			SessionSecret:   "preview-secret",
			AdminDiscordIDs: []string{"111111111111111111"},
			Features: config.WebAuthFeatures{
				GitHubWrites:  true,
				Merge:         true,
				StartSessions: true,
				PRReviews:     true,
				Storage:       true,
			},
		}
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
		// A second unit on #128 — the ship board must still show one row for it,
		// and the PR rail must offer both as dispatch targets.
		"1390000000000000009": {
			SessionID: "sess-a2", Project: "webapp", LastUser: "beam#0",
			OwnerName: "beam", Origin: "web", Label: "needs_review",
			Goal: "Review acme/webapp#128: fix: debounce payment retry queue",
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
		// Support cases (Mode=case) across the phase pipeline → /projects/webapp/cases.
		"1390000000000000021": {
			SessionID: "case-a", Project: "webapp", Mode: "case", Phase: "intake",
			CaseKey:  "WEBAPP-21",
			Severity: "critical", CustomerTitle: "Checkout 500s for EU Visa cards",
			CustomerRef: "ZD-4821", ReporterName: "beam", Origin: "discord",
			OwnerID: "222222222222222222", OwnerName: "poon",
			// Filed three hours ago and still unanswered: past the one-hour
			// critical first-response target, so this row wears the breach chip.
			OpenedAt: previewStamp(-3 * time.Hour),
		},
		"1390000000000000022": {
			SessionID: "case-b", Project: "webapp", Mode: "case", Phase: "investigate",
			CaseKey:  "WEBAPP-22",
			Severity: "high", CustomerTitle: "Webhook retries fire twice for one order",
			CustomerRef: "ZD-4780", ReporterName: "mint", Origin: "discord",
			OwnerID: "111111111111111111", OwnerName: "mint",
			// Cross-reference to the older duplicate-charge case: same root
			// cause, different module. Renders outbound here, inbound there.
			RelatedCases:    []string{"WEBAPP-25"},
			OpenedAt:        previewStamp(-6 * time.Hour),
			FirstResponseAt: previewStamp(-5 * time.Hour),
			CustomerUpdate:  "We reproduced the duplicate retries on staging and are tracing the debounce window now — next update within the hour.",
			Dossier: &sessionstore.Dossier{
				Summary: "Retry queue re-enqueues per webhook event instead of per order; burst of events → duplicate settle jobs.",
			},
		},
		"1390000000000000023": {
			SessionID: "case-c", Project: "webapp", Mode: "case", Phase: "answered",
			CaseKey:  "WEBAPP-23",
			Severity: "medium", CustomerTitle: "How do refunds settle across currencies?",
			CustomerRef: "ZD-4790", ReporterName: "beam", Origin: "web",
			OwnerName: "poon",
			// Answered two days ago: the resolution clock is frozen while the
			// customer has it, so this shows "on hold" rather than a breach.
			OpenedAt:        previewStamp(-50 * time.Hour),
			FirstResponseAt: previewStamp(-48 * time.Hour),
			AnsweredAt:      previewStamp(-48 * time.Hour),
			CustomerUpdate:  "Refunds settle in the original charge currency at the captured FX rate; a worked example is in the reply draft.",
			Label:           "blocked",
		},
		"1390000000000000024": {
			SessionID: "case-d", Project: "webapp", Mode: "case", Phase: "fixing",
			CaseKey:  "WEBAPP-24",
			Severity: "high", CustomerTitle: "Session cookies dropped on Safari 17",
			CustomerRef: "ZD-4711", ReporterName: "mint", Origin: "discord",
			OwnerName: "mint",
			Dossier: &sessionstore.Dossier{
				Summary: "SameSite=Lax rollout regressed the payment-redirect return leg on Safari 17; needs Secure+None on the return cookie only.",
			},
		},
		"1390000000000000025": {
			SessionID: "case-e", Project: "webapp", Mode: "case", Phase: "shipping",
			CaseKey:  "WEBAPP-25",
			Severity: "critical", CustomerTitle: "Duplicate charges on retried payments",
			CustomerRef: "ZD-4695", ReporterName: "beam", Origin: "discord",
			OwnerName: "mint",
			// Assigned to the preview's logged-in admin, so the board shows an "eng"
			// chip and a non-zero "Mine" count next to case-d's "needs an engineer".
			EngineerID: "111111111111111111", EngineerName: "mint",
			CustomerUpdate: "Engineering has a fix in review; charges are deduplicated by idempotency key once it ships.",
			PRs: []sessionstore.TrackedPR{{
				URL: "https://github.com/acme/webapp/pull/133", Number: 133, State: "OPEN",
				Title: "fix: idempotent settle jobs", Checks: "✓ 3 · ✗ 1",
				Owner: "acme", Repo: "webapp",
			}},
		},
		"1390000000000000026": {
			SessionID: "case-f", Project: "webapp", Mode: "case", Phase: "closed",
			CaseKey:  "WEBAPP-26",
			Severity: "low", CustomerTitle: "Typo on the invoice footer",
			CustomerRef: "ZD-4602", ReporterName: "beam", Origin: "web",
			Resolution: "fixed", ResolutionNote: "Shipped in the July invoice template refresh.",
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
	// Token usage on the seeded turns so the spend report has something to fold.
	// The shape matters for review: claude turns carry a big cache-read share (the
	// prefix is re-charged every call), grok-4.5 is left unpriced below so the
	// "tokens but no dollars" rendering is visible on the same page as real costs.
	claudeUsage := func(in, cacheRead, cacheWrite, out, resident int) *history.Usage {
		return &history.Usage{
			InputTokens: in, CacheReadTokens: cacheRead, CacheCreationTokens: cacheWrite,
			OutputTokens: out, ContextTokens: resident, ContextWindowTokens: 1_000_000,
		}
	}
	turns := []struct {
		thread string
		turn   history.Turn
	}{
		{"1390000000000000001", history.Turn{User: "mint#0", Prompt: "The checkout E2E test is flaky on CI — it fails roughly 1 in 5 runs with a timeout waiting for the payment webhook. Find the race and fix it.", Response: "Found it: the retry queue debounces per-order but the test asserts on the first attempt. I widened the webhook wait and debounced the queue flush.\n\nOpened PR: https://github.com/acme/webapp/pull/128", Status: "done", Elapsed: "4m12s", Project: "webapp", SessionID: "sess-a1", Agent: "claude", Model: "claude-opus-5", Usage: claudeUsage(1_240, 186_400, 41_900, 9_820, 62_300)}},
		{"1390000000000000001", history.Turn{User: "mint#0", Prompt: "CI is still red on the lint job — fix it and push.", Response: "Working…", Status: "error", ExitCode: 1, Error: "Reached max turns before a final reply", Elapsed: "12m40s", Project: "webapp", SessionID: "sess-a1", Agent: "claude", Model: "claude-opus-5", Usage: claudeUsage(3_100, 812_000, 96_400, 28_300, 141_800)}},
		{"1390000000000000001", history.Turn{User: "poon#0", Prompt: "/fix-ci", Response: "gofmt drift in payment_retry.go — formatted, pushed, checks green except the flaky E2E which is queued.", Status: "done", Elapsed: "2m03s", Project: "webapp", SessionID: "sess-a1", Agent: "claude", Model: "claude-haiku-4-5", Usage: claudeUsage(880, 42_100, 11_200, 3_400, 28_900)}},
		{"1390000000000000002", history.Turn{User: "poon#0", Prompt: "Add standard X-RateLimit-Limit / Remaining / Reset headers to all public endpoints, with tests.", Response: "Done — middleware emits the three headers from the token bucket state; added table-driven tests.\n\nPR: https://github.com/acme/api/pull/86 (merged)", Status: "done", Elapsed: "9m51s", Project: "api", SessionID: "sess-b2", Agent: "grok", Model: "grok-4.5", Usage: &history.Usage{TotalTokens: 318_400}}},
		{"1390000000000000003", history.Turn{User: "mint#0", Prompt: "Start the SameSite=Lax migration for session cookies behind a flag.", Response: "", Status: "cancelled", Error: "Cancelled by owner via /cancel", Elapsed: "0m48s", Project: "webapp", SessionID: "sess-c3", Agent: "claude", Model: "claude-opus-5", Usage: claudeUsage(410, 9_800, 6_100, 1_240, 17_500)}},
		{"1390000000000000003", history.Turn{User: "mint#0", Prompt: "Resume: SameSite=Lax behind GROK_COOKIE_LAX flag, draft PR is fine.", Response: "Draft PR up: https://github.com/acme/webapp/pull/131 — flag default off, e2e matrix added for both modes.", Status: "done", Elapsed: "6m22s", Project: "webapp", SessionID: "sess-c3", Agent: "claude", Model: "claude-opus-5", Usage: claudeUsage(1_020, 274_500, 33_800, 12_600, 84_200)}},
	}
	for _, tt := range turns {
		if err := hist.Append(tt.thread, tt.turn); err != nil {
			t.Fatal(err)
		}
	}
	// A case with a long investigation, so the phone layout can be reviewed in
	// the state it is actually hard in: the rail's actions (escalate, close)
	// sit past a screen-height of turns and are only reachable through the
	// action sheet.
	shotPath := filepath.Join(cfg.DataDir, "preview-shot.png")
	writePreviewShot(t, shotPath)
	logPath := filepath.Join(cfg.DataDir, "preview-webhook.log")
	if err := os.WriteFile(logPath, []byte("2026-08-28T09:12:04Z charge.updated order=9313 evt=evt_1\n2026-08-28T09:12:04Z charge.updated order=9313 evt=evt_2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 14; i++ {
		turn := history.Turn{
			User: "mint#0",
			Prompt: fmt.Sprintf("Round %d: narrow the duplicate-settle window — check the debounce key, "+
				"the webhook dedupe table and the retry backoff for order #9313.", i),
			Response: fmt.Sprintf("Round %d findings: the debounce key is per *event id*, not per order, so a "+
				"burst of `charge.updated` webhooks for one order each open their own settle job.\n\n"+
				"- `webhook/dedupe.go:71` keys on `evt.ID`\n"+
				"- `settle/queue.go:184` enqueues unconditionally after dedupe\n"+
				"- reproduced locally with 6 events in a 200ms window\n\n"+
				"Next: confirm the same key is used on the invoicing path before proposing a fix.", i),
			Status: "done", Elapsed: "3m0" + strconv.Itoa(i%10) + "s",
			Project: "webapp", SessionID: "case-b",
			Agent: "claude", Model: "claude-sonnet-5",
			Usage: claudeUsage(700+i*40, 30_000+i*9_000, 8_000+i*400, 2_100+i*90, 24_000+i*7_000),
		}
		var files []history.File
		if i == 1 {
			files = []history.File{
				{Path: shotPath, Name: "checkout-timeout.png", ContentType: "image/png"},
				{Path: logPath, Name: "webhook-burst.log", ContentType: "text/plain"},
			}
		}
		if err := hist.AppendFiles("1390000000000000022", turn, files); err != nil {
			t.Fatal(err)
		}
	}
	xlsx := filepath.Join(cfg.DataDir, "preview-report.xlsx")
	if err := os.WriteFile(xlsx, []byte("xlsx-demo"), 0o600); err != nil {
		t.Fatal(err)
	}
	att, err := hist.PutArtifact("1390000000000000022", history.File{
		Path: xlsx, Name: "settle-dupes.xlsx", ContentType: "application/vnd.ms-excel", Rel: "notes/settle-dupes.xlsx",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hist.Append("1390000000000000022", history.Turn{
		User: "mint#0", Prompt: "Export the duplicate-settle cases as a spreadsheet.",
		Response: "Wrote notes/settle-dupes.xlsx — download it from this session.",
		Status:   "done", Elapsed: "1m12s", Project: "webapp", SessionID: "case-b",
		Agent: "claude", Model: "claude-sonnet-5",
		Usage:     claudeUsage(640, 22_000, 6_400, 1_800, 31_000),
		Artifacts: []history.Attachment{att},
	}); err != nil {
		t.Fatal(err)
	}

	// Repo catalog so the commits browser and PR diff resolve without gh.
	if err := cfg.SetProjectGitHubRepos("webapp", []config.GitHubRepoRef{{Owner: "acme", Repo: "webapp"}}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectGitHubRepos("api", []config.GitHubRepoRef{{Owner: "acme", Repo: "api"}}); err != nil {
		t.Fatal(err)
	}

	srv := New(cfg, store, hist, bot.New(cfg, store, hist))
	// A live case run so the board shows the running chip on the investigate lane.
	if err := bot.SeedActiveRunForTest(srv.bot, "1390000000000000022", "webapp",
		"Trace the duplicate webhook retries",
		"Reproduced: two settle jobs enqueue for order #9313 when webhooks burst…"); err != nil {
		t.Fatal(err)
	}
	liveShot := filepath.Join(cfg.DataDir, "preview-live.png")
	writePreviewShot(t, liveShot)
	bot.PublishRunAttachmentsForTest(srv.bot, "1390000000000000022", []string{liveShot, logPath})
	if previewAuth {
		sid, _, err := srv.LoginAs("111111111111111111", "mint", config.WebRoleAdmin)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(os.Stderr, "preview: auth on — in devtools run: document.cookie = %q\n",
			sessionCookieName+"="+sid+"; path=/")
	}
	// Team review history for acme/webapp#128 (PR detail card + ship rollup).
	if rev := srv.bot.Reviews(); rev != nil {
		const head = "4f2c9ae0b17d43c2e8a95f61b2d4c8e9a1f03b57"
		_, _ = rev.SubmitReview(reviewstore.Review{
			Owner: "acme", Repo: "webapp", Number: 128, Project: "webapp",
			HeadSHA:    "b7d21c3aa90f14e2d6c88b5f0a3e97d1c2f4a6b8",
			Verdict:    reviewstore.VerdictApproved,
			Body:       "Queue drain logic looks right — ship once e2e is green.",
			ReviewerID: "222222222222222222", ReviewerName: "poon",
			At: time.Date(2026, 7, 20, 16, 5, 0, 0, time.UTC),
		})
		_, _ = rev.SubmitReview(reviewstore.Review{
			Owner: "acme", Repo: "webapp", Number: 128, Project: "webapp",
			HeadSHA:    head,
			Verdict:    reviewstore.VerdictChangesRequested,
			Body:       "Debounce window swallows the first retry when the webhook lands mid-settle — needs a test for that path.",
			ReviewerID: "333333333333333333", ReviewerName: "beam",
			At: time.Date(2026, 7, 21, 10, 40, 0, 0, time.UTC),
		})
		_, _ = rev.RequestReview(reviewstore.Request{
			Owner: "acme", Repo: "webapp", Number: 128, Project: "webapp",
			HeadSHA:     head,
			RequesterID: "222222222222222222", RequesterName: "poon",
			ReviewerID: "111111111111111111", ReviewerName: "mint",
			Note: "Re-check the idempotency key change in the settle path.",
		})
	}
	seedPreviewDeploys(t, srv)
	seedPreviewFiles(t, srv, cfg)
	// Synthetic git/gh so the diff review UI can be exercised with a large
	// changeset: /projects/webapp/commits → commit detail (lazy per-file
	// hunks), /prs/acme/webapp/128/diff for the PR surface.
	srv.ghRunner = previewGitRunner()
	// Agents work this repo in parallel worktrees, so a hardcoded port means the
	// second preview to boot dies on "address already in use" — or, worse, the
	// operator kills someone else's server to free it. GROKWORK_WEB_PREVIEW_PORT
	// gives each worktree its own.
	addr := "127.0.0.1:18787"
	if p := strings.TrimSpace(os.Getenv("GROKWORK_WEB_PREVIEW_PORT")); p != "" {
		addr = "127.0.0.1:" + p
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "preview: http://%s (kill the test to stop)\n", ln.Addr())
	if ms, _ := strconv.Atoi(os.Getenv("GROKWORK_WEB_PREVIEW_DELAY_MS")); ms > 0 {
		delay := time.Duration(ms) * time.Millisecond
		slow := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/static/") && r.URL.Path != "/events" {
				fmt.Fprintf(os.Stderr, "preview: %s %s\n", r.Method, r.URL.Path)
				time.Sleep(delay)
			}
			srv.Handler().ServeHTTP(w, r)
		})}
		if err := slow.Serve(ln); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := srv.App().Serve(ln); err != nil {
		t.Fatal(err)
	}
}

// ── Synthetic large changeset for the diff review preview ──────────────────

type previewFile struct {
	path, status, old string
	adds, dels        int
	binary            bool
}

// previewChangeset builds a deterministic ~120-file commit: mostly small
// modifications, a new subsystem, fixtures, a few deletes, one rename, one
// binary, and big generated files — every diff review affordance on one page.
func previewChangeset() []previewFile {
	var fs []previewFile
	seed := uint32(20260721)
	rnd := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>16) % n
	}
	add := func(dir string, names []string, status string) {
		for _, n := range names {
			total := 4 + rnd(56)
			switch r := rnd(10); {
			case r > 8:
				total = 250 + rnd(450)
			case r > 6:
				total = 60 + rnd(190)
			}
			f := previewFile{path: dir + "/" + n, status: status}
			switch status {
			case "A":
				f.adds = total
			case "D":
				f.dels = total
			default:
				f.adds = total * (35 + rnd(50)) / 100
				f.dels = total - f.adds
			}
			fs = append(fs, f)
		}
	}
	add("internal/billing", []string{"ledger.go", "ledger_test.go", "jobs.go", "jobs_test.go", "invoice.go", "invoice_test.go", "refund.go", "refund_test.go", "webhook.go", "webhook_test.go", "currency.go", "tax.go", "tax_test.go", "store.go", "store_test.go", "retry.go", "retry_test.go", "metrics.go", "doc.go"}, "A")
	add("internal/billing/ledgerstore", []string{"store.go", "store_test.go", "sqlite.go", "sqlite_test.go", "schema.go", "snapshot.go", "snapshot_test.go", "compact.go", "compact_test.go"}, "A")
	add("internal/billing/webhookq", []string{"queue.go", "queue_test.go", "dispatch.go", "dispatch_test.go", "backoff.go"}, "A")
	add("internal/web", []string{"invoices.go", "invoices_test.go", "webhooks.go", "webhooks_test.go", "web.go", "web_test.go", "live.go", "auth.go", "writes.go"}, "M")
	add("internal/web/templates", []string{"billing.tmpl", "invoice_detail.tmpl", "ledger.tmpl", "refunds.tmpl", "layout.tmpl", "config.tmpl"}, "M")
	add("internal/bot", []string{"bot.go", "bot_test.go", "completion.go", "prompt.go", "stream.go", "ci_triage.go", "brief.go"}, "M")
	add("internal/ghpr", []string{"ghpr.go", "diff.go", "diff_test.go", "issues.go", "timeline.go", "write.go"}, "M")
	add("internal/config", []string{"config.go", "config_test.go", "billing.go", "billing_test.go"}, "M")
	add("cmd/grokwork", []string{"main.go"}, "M")
	add("migrations", []string{"0041_ledger.sql", "0042_ledger_idx.sql", "0043_invoice_state.sql", "0044_webhook_queue.sql"}, "A")
	add("docs", []string{"billing.md", "ledger.md", "runbook.md", "webhooks.md"}, "M")
	for i := 1; i <= 24; i++ {
		add("testdata/billing", []string{fmt.Sprintf("ledger_case_%02d.json", i)}, "A")
	}
	add("internal/legacy", []string{"payments_v1.go", "payments_v1_test.go"}, "D")
	fs = append(fs,
		previewFile{path: "internal/web/billing.go", old: "internal/web/payments.go", status: "R", adds: 208, dels: 96},
		previewFile{path: "internal/billing/ledger.go", status: "A", adds: 934},
		previewFile{path: "docs/assets/billing-flow.png", status: "M", binary: true},
		previewFile{path: "package-lock.json", status: "M", adds: 1893, dels: 538},
		previewFile{path: "internal/billing/billingpb/billing.pb.go", status: "A", adds: 1204},
	)
	// Dedup hand-tuned overrides (ledger.go seeded twice), then git-sort.
	seen := map[string]int{}
	out := fs[:0]
	for _, f := range fs {
		if i, ok := seen[f.path]; ok {
			out[i] = f
			continue
		}
		seen[f.path] = len(out)
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b previewFile) int { return cmp.Compare(a.path, b.path) })
	return out
}

var previewLinePool = []string{
	`func (s *Store) ApplyEntry(ctx context.Context, e Entry) error {`,
	`	if err := s.validate(e); err != nil {`,
	`		return fmt.Errorf("apply ledger entry: %w", err)`,
	`	}`,
	`	s.mu.Lock()`,
	`	defer s.mu.Unlock()`,
	`	job := queue.NewJob(e.InvoiceID, queue.KindSettle)`,
	`	balance := s.balances[e.AccountID]`,
	`	s.balances[e.AccountID] = balance.Add(e.Amount)`,
	`	slog.Info("ledger entry applied", "account", e.AccountID, "seq", e.Seq)`,
	`	return nil`,
	`}`,
	`	case <-ctx.Done():`,
	`		return ctx.Err()`,
	`	got, err := store.Balance(ctx, "acct_9")`,
	`	if got.Cents != 1250 {`,
	`		t.Fatalf("balance = %d, want 1250", got.Cents)`,
}

// previewFilePatch synthesizes a plausible unified diff for one file.
func previewFilePatch(f previewFile) string {
	if f.binary {
		return fmt.Sprintf("diff --git a/%s b/%s\nBinary files a/%s and b/%s differ\n", f.path, f.path, f.path, f.path)
	}
	oldP := f.path
	if f.old != "" {
		oldP = f.old
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n", oldP, f.path)
	switch f.status {
	case "A":
		b.WriteString("new file mode 100644\n--- /dev/null\n")
	case "D":
		fmt.Fprintf(&b, "deleted file mode 100644\n--- a/%s\n", oldP)
	default:
		if f.old != "" {
			fmt.Fprintf(&b, "similarity index 71%%\nrename from %s\nrename to %s\n", f.old, f.path)
		}
		fmt.Fprintf(&b, "--- a/%s\n", oldP)
	}
	if f.status == "D" {
		b.WriteString("+++ /dev/null\n")
	} else {
		fmt.Fprintf(&b, "+++ b/%s\n", f.path)
	}
	seed := uint32(len(f.path)) * 2654435761
	rnd := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>16) % n
	}
	line := func() string { return previewLinePool[rnd(len(previewLinePool))] }
	adds, dels := f.adds, f.dels
	hunks := max(1, min(9, (adds+dels)/40))
	oldLn, newLn := 1+rnd(60), 0
	newLn = oldLn
	for h := range hunks {
		hA, hD := adds/(hunks-h), dels/(hunks-h)
		adds -= hA
		dels -= hD
		ctxN := 3
		if f.status == "A" || f.status == "D" {
			ctxN = 0
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@ func ApplyEntry\n", oldLn, hD+ctxN*2, newLn, hA+ctxN*2)
		for range ctxN {
			b.WriteString(" " + line() + "\n")
			oldLn++
			newLn++
		}
		for a, d := hA, hD; a > 0 || d > 0; {
			burst := 1 + rnd(6)
			for range min(burst, d) {
				b.WriteString("-" + line() + "\n")
				oldLn++
			}
			d -= min(burst, d)
			for range min(burst, a) {
				b.WriteString("+" + line() + "\n")
				newLn++
			}
			a -= min(burst, a)
			if rnd(10) < 4 && (a > 0 || d > 0) {
				b.WriteString(" " + line() + "\n")
				oldLn++
				newLn++
			}
		}
		for range ctxN {
			b.WriteString(" " + line() + "\n")
			oldLn++
			newLn++
		}
		oldLn += 8 + rnd(60)
		newLn = oldLn + hA - hD
	}
	return b.String()
}

// previewCommitRows returns a deterministic 137-commit history (newest first,
// git log \x1f-delimited format) so the commits browser shows 3 pages at the
// 50-row page size: full first pages plus a short tail.
func previewCommitRows(headSHA string) []string {
	rows := []string{
		headSHA + "\x1fRework billing pipeline into async ledger jobs\x1fGrok\x1fgrok@grokwork.local\x1f2026-07-21T09:14:00Z",
		"b7d21c3aa90f14e2d6c88b5f0a3e97d1c2f4a6b8\x1fweb: invoice detail drawer\x1fmint\x1fmint@acme.dev\x1f2026-07-20T17:41:00Z",
		"9e04f7d2c5b8a1e6f3d0c9b4a7e2f5d8c1b6a3e0\x1ffix: webhook retry off-by-one\x1fpoon\x1fpoon@acme.dev\x1f2026-07-20T11:08:00Z",
	}
	subjects := []string{
		"web: tighten invoice table spacing",
		"fix: ledger settle job idempotency key",
		"refactor: extract webhook signature check",
		"chore: bump payment SDK",
		"feat: per-order retry debounce window",
		"test: checkout webhook burst e2e",
		"docs: settlement flow runbook",
		"perf: batch ledger row inserts",
	}
	authors := [][2]string{
		{"Grok", "grok@grokwork.local"},
		{"mint", "mint@acme.dev"},
		{"poon", "poon@acme.dev"},
	}
	base := time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)
	for i := 0; len(rows) < 137; i++ {
		h := uint64(i)*0x9E3779B97F4A7C15 + 0xC0FFEE
		sha := fmt.Sprintf("%016x%016x%08x", h, h*0xD1B54A32D192ED03, uint32(i)+0xabcde01)
		au := authors[i%len(authors)]
		rows = append(rows, fmt.Sprintf("%s\x1f%s\x1f%s\x1f%s\x1f%s",
			sha,
			subjects[i%len(subjects)],
			au[0], au[1],
			base.Add(-time.Duration(i)*7*time.Hour).Format(time.RFC3339)))
	}
	return rows
}

// previewGitRunner fakes git/gh for the commits browser and diff review UI.
func previewGitRunner() func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	files := previewChangeset()
	const sha = "4f2c9ae0b17d43c2e8a95f61b2d4c8e9a1f03b57"
	const meta = sha + "\x1fRework billing pipeline into async ledger jobs\x1fGrok\x1fgrok@grokwork.local\x1f2026-07-21T09:14:00Z\x1f" +
		"Ledger entries now settle through idempotent queue jobs instead of inline webhook handlers.\n"
	return func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "git" && strings.HasPrefix(joined, "log"):
			// Honor -n / --skip so the commits pager renders multiple pages.
			limit, skip := 50, 0
			for i, a := range args {
				if a == "-n" && i+1 < len(args) {
					limit, _ = strconv.Atoi(args[i+1])
				}
				if a == "--skip" && i+1 < len(args) {
					skip, _ = strconv.Atoi(args[i+1])
				}
			}
			rows := previewCommitRows(sha)
			if skip >= len(rows) {
				return []byte{}, nil
			}
			rows = rows[skip:]
			if limit > 0 && limit < len(rows) {
				rows = rows[:limit]
			}
			return []byte(strings.Join(rows, "\n") + "\n"), nil
		case name == "git" && strings.HasPrefix(joined, "rev-parse"):
			return []byte(sha + "\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "show" && strings.Contains(joined, "--numstat"):
			var b strings.Builder
			for _, f := range files {
				a, d := strconv.Itoa(f.adds), strconv.Itoa(f.dels)
				if f.binary {
					a, d = "-", "-"
				}
				if f.old != "" {
					fmt.Fprintf(&b, "%s\t%s\t\x00%s\x00%s\x00", a, d, f.old, f.path)
				} else {
					fmt.Fprintf(&b, "%s\t%s\t%s\x00", a, d, f.path)
				}
			}
			return []byte(b.String()), nil
		case name == "git" && len(args) > 0 && args[0] == "show" && strings.Contains(joined, "--name-status"):
			var b strings.Builder
			for _, f := range files {
				if f.old != "" {
					fmt.Fprintf(&b, "R071\x00%s\x00%s\x00", f.old, f.path)
				} else {
					fmt.Fprintf(&b, "%s\x00%s\x00", f.status, f.path)
				}
			}
			return []byte(b.String()), nil
		case name == "git" && len(args) > 0 && args[0] == "show" && strings.Contains(joined, "-s"):
			return []byte(meta), nil
		case name == "git" && len(args) > 0 && args[0] == "show":
			// Per-file patch: first pathspec after "--".
			for i, a := range args {
				if a == "--" && i+1 < len(args) {
					for _, f := range files {
						if f.path == args[i+1] || f.old == args[i+1] {
							return []byte(previewFilePatch(f)), nil
						}
					}
				}
			}
			return nil, nil
		case name == "gh" && strings.HasPrefix(joined, "issue list"):
			return []byte(`[
				{"number":12,"url":"https://github.com/acme/webapp/issues/12","title":"Checkout webhook duplicates settle jobs","state":"OPEN","author":{"login":"beam"},
				 "createdAt":"2026-07-18T09:00:00Z","updatedAt":"2026-07-21T11:05:00Z",
				 "closedByPullRequestsReferences":[{"number":128,"url":"https://github.com/acme/webapp/pull/128","repository":{"name":"webapp","owner":{"login":"acme"}}}]},
				{"number":19,"url":"https://github.com/acme/webapp/issues/19","title":"Rate limit headers missing on public API","state":"OPEN","author":{"login":"poon"},
				 "createdAt":"2026-07-19T14:20:00Z","updatedAt":"2026-07-20T08:11:00Z",
				 "closedByPullRequestsReferences":[]},
				{"number":24,"url":"https://github.com/acme/webapp/issues/24","title":"Safari 17 drops session cookie on payment return","state":"OPEN","author":{"login":"mint"},
				 "createdAt":"2026-07-20T16:40:00Z","updatedAt":"2026-07-21T09:02:00Z",
				 "closedByPullRequestsReferences":[{"number":131,"url":"https://github.com/acme/webapp/pull/131","repository":{"name":"webapp","owner":{"login":"acme"}}},
				  {"number":133,"url":"https://github.com/acme/webapp/pull/133","repository":{"name":"webapp","owner":{"login":"acme"}}}]}
			]`), nil
		case name == "gh" && strings.HasPrefix(joined, "pr view"):
			return []byte(`{
				"number":128,"url":"https://github.com/acme/webapp/pull/128",
				"title":"fix: debounce payment retry queue","state":"OPEN","isDraft":false,
				"reviewDecision":"CHANGES_REQUESTED",
				"headRefOid":"` + sha + `",
				"headRefName":"grok/discord/1390000000000000001","baseRefName":"main",
				"body":"## What\n\nPayment webhook retries were re-enqueuing per event, so a burst of webhooks for one order queued duplicate settle jobs. This debounces the queue flush per order and makes the settle job idempotent.\n\n## Why\n\nThe checkout E2E was flaky (~1 in 5 runs): the test asserted on the first settle attempt while a duplicate job raced it.\n\n## Testing\n\n- unit: retry queue debounce window\n- e2e: checkout happy path, webhook burst",
				"mergeable":"MERGEABLE","author":{"login":"grok-work"},
				"additions":214,"deletions":96,"changedFiles":9,
				"comments":[
					{"author":{"login":"beam"},"body":"The debounce window is 500ms — is that enough when the webhook lands twice in the same tick?","url":"https://github.com/acme/webapp/pull/128#issuecomment-1","createdAt":"2026-07-21T10:42:00Z"},
					{"author":{"login":"grok-work"},"body":"Widened to 2s and added a burst test. See ` + "`retry_queue_test.go`" + `.","url":"https://github.com/acme/webapp/pull/128#issuecomment-2","createdAt":"2026-07-21T11:05:00Z"},
					{"author":{"login":"poon"},"body":"Queue drain logic looks right to me now.","url":"https://github.com/acme/webapp/pull/128#issuecomment-3","createdAt":"2026-07-21T12:20:00Z"}
				]
			}`), nil
		case name == "gh" && strings.HasPrefix(joined, "pr checks"):
			return []byte(`[
				{"name":"build","state":"SUCCESS","bucket":"pass"},
				{"name":"lint","state":"SUCCESS","bucket":"pass"},
				{"name":"unit","state":"SUCCESS","bucket":"pass"},
				{"name":"vet","state":"SUCCESS","bucket":"pass"},
				{"name":"e2e-checkout","state":"FAILURE","bucket":"fail"}
			]`), nil
		case name == "gh" && strings.HasPrefix(joined, "pr diff"):
			var b strings.Builder
			for _, f := range files {
				b.WriteString(previewFilePatch(f))
			}
			return []byte(b.String()), nil
		}
		return nil, nil
	}
}

// previewRate builds one model's price row for the preview config (dollars per
// million tokens).
func previewRate(in, out, cacheRead, cacheWrite float64) config.ModelRate {
	return config.ModelRate{
		InputPerMTok: &in, OutputPerMTok: &out,
		CacheReadPerMTok: &cacheRead, CacheWritePerMTok: &cacheWrite,
	}
}

// previewMinutes / previewStamp keep the seeded SLA data readable: minutes are
// pointers (unset must stay distinguishable from zero) and the case clocks are
// relative to now, so the preview shows a breach and a hold no matter when it
// runs.
func previewMinutes(n int) *int { return &n }

func previewStamp(d time.Duration) string {
	return time.Now().UTC().Add(d).Format(time.RFC3339)
}

func writePreviewShot(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 480, 270))
	for y := range 270 {
		for x := range 480 {
			if y < 36 {
				img.SetRGBA(x, y, color.RGBA{R: 28, G: 28, B: 32, A: 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{R: 246, G: 245, B: 242, A: 255})
			}
		}
	}
	for y := 88; y < 128; y++ {
		for x := 24; x < 456; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 196, G: 64, B: 64, A: 255})
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// seedPreviewDeploys fills the run store so /deploys (the cross-project board)
// and each project's own Deploys page have something to render. Every state the
// board can show is present: a settled lane, a lane with a failure sitting on
// top of a good deploy, a deploy in flight, and a lane that has never shipped.
func seedPreviewDeploys(t *testing.T, srv *Server) {
	t.Helper()
	store := srv.Deploys().Store()
	add := func(r deploy.Run) string {
		t.Helper()
		r.ID = deploy.NewRunID()
		r.ShortSHA = r.SHA[:7]
		if err := store.Create(r); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	// webapp/web: two quiet environments, prod one release behind stag.
	add(deploy.Run{
		Project: "webapp", Repo: "acme/webapp", Service: "web", Env: "stag",
		Ref: "main", SHA: "9f31c07ab4e25d18", Subject: "Debounce the settle retries",
		Status: deploy.StatusSucceeded, ActorName: "mint",
		QueuedAt: previewStamp(-3 * time.Hour), StartedAt: previewStamp(-3 * time.Hour),
		EndedAt: previewStamp(-170 * time.Minute),
	})
	add(deploy.Run{
		Project: "webapp", Repo: "acme/webapp", Service: "web", Env: "prod",
		Ref: "main", SHA: "4f2c9ae0b17d43c2", Subject: "Idempotency key on the settle path",
		Status: deploy.StatusSucceeded, ActorName: "poon",
		QueuedAt: previewStamp(-30 * time.Hour), StartedAt: previewStamp(-30 * time.Hour),
		EndedAt: previewStamp(-29 * time.Hour),
	})
	// webapp/api prod: live at an older commit, newest attempt failed.
	live := add(deploy.Run{
		Project: "webapp", Repo: "acme/webapp", Service: "api", Env: "prod",
		Ref: "main", SHA: "b7d21c3aa90f14e2", Subject: "Drain the webhook queue on boot",
		Status: deploy.StatusSucceeded, ActorName: "poon",
		QueuedAt: previewStamp(-52 * time.Hour), StartedAt: previewStamp(-52 * time.Hour),
		EndedAt: previewStamp(-51 * time.Hour),
	})
	_ = live
	add(deploy.Run{
		Project: "webapp", Repo: "acme/webapp", Service: "api", Env: "prod",
		Ref: "main", SHA: "c0ffee11d00d2233", Subject: "Bump the settle worker pool",
		Status: deploy.StatusFailed, ActorName: "beam",
		Error:    `step "rollout" failed (exit 1)`,
		QueuedAt: previewStamp(-40 * time.Minute), StartedAt: previewStamp(-40 * time.Minute),
		EndedAt: previewStamp(-38 * time.Minute),
	})
	// api project: one deploy in flight, one environment nobody has shipped to.
	add(deploy.Run{
		Project: "api", Repo: "acme/api", Service: "api", Env: "dev",
		Ref: "main", SHA: "3d5a81b6c9074fe0", Subject: "Split the ingest handler",
		Status: deploy.StatusRunning, ActorName: "beam",
		QueuedAt: previewStamp(-4 * time.Minute), StartedAt: previewStamp(-4 * time.Minute),
	})
	add(deploy.Run{
		Project: "api", Repo: "acme/api", Service: "worker", Env: "prod",
		Ref: "main", SHA: "aa17e930b2c4415f", Subject: "First cut of the worker chart",
		Status: deploy.StatusFailed, ActorName: "mint",
		Error:    `step "helm-upgrade" failed (exit 1)`,
		QueuedAt: previewStamp(-6 * 24 * time.Hour), StartedAt: previewStamp(-6 * 24 * time.Hour),
		EndedAt: previewStamp(-6*24*time.Hour + 3*time.Minute),
	})
}

// seedPreviewFiles links webapp to a bucket and serves a nested listing so
// /projects/webapp/files?path=Docs%20for%20Customer/CR%20AMB exercises the
// path trail (the screenshot that used to stack each crumb on its own line).
func seedPreviewFiles(t *testing.T, srv *Server, cfg *config.Config) {
	t.Helper()
	if err := cfg.SetProjectStorageGCS("webapp", "preview-files", "webapp", ""); err != nil {
		t.Fatal(err)
	}
	srv.filesBackendFn = func(filestore.Target) (filestore.Backend, error) {
		return previewFiles{}, nil
	}
}

type previewFiles struct{}

func (previewFiles) List(_ context.Context, _ filestore.Target, subPath string) ([]filestore.Entry, error) {
	stamp := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	switch strings.Trim(subPath, "/") {
	case "":
		return []filestore.Entry{{Name: "Docs for Customer", IsDir: true}}, nil
	case "Docs for Customer":
		return []filestore.Entry{{Name: "CR AMB", IsDir: true}}, nil
	case "Docs for Customer/CR AMB":
		return []filestore.Entry{
			{Name: "handoff.pdf", Size: int64(len(previewPDF)), Updated: stamp, ContentType: "application/pdf"},
			{Name: "photo.png", Size: int64(len(previewPNG)), Updated: stamp, ContentType: "image/png"},
			{Name: "notes [v2].txt", Size: int64(len(previewNotes)), Updated: stamp, ContentType: "text/plain"},
			{Name: "Sheet", Size: 0, Updated: stamp, ContentType: "application/vnd.google-apps.spreadsheet"},
		}, nil
	default:
		return nil, nil
	}
}

func previewFileByObject(object string) (filestore.Entry, []byte, bool) {
	stamp := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	switch strings.Trim(object, "/") {
	case "Docs for Customer/CR AMB/handoff.pdf":
		return filestore.Entry{Name: "handoff.pdf", Size: int64(len(previewPDF)), Updated: stamp, ContentType: "application/pdf"}, previewPDF, true
	case "Docs for Customer/CR AMB/photo.png":
		return filestore.Entry{Name: "photo.png", Size: int64(len(previewPNG)), Updated: stamp, ContentType: "image/png"}, previewPNG, true
	case "Docs for Customer/CR AMB/notes [v2].txt":
		return filestore.Entry{Name: "notes [v2].txt", Size: int64(len(previewNotes)), Updated: stamp, ContentType: "text/plain"}, previewNotes, true
	case "Docs for Customer/CR AMB/Sheet":
		return filestore.Entry{Name: "Sheet", Size: 0, Updated: stamp, ContentType: "application/vnd.google-apps.spreadsheet"}, previewPDF, true
	default:
		return filestore.Entry{}, nil, false
	}
}

func (previewFiles) Describe(_ context.Context, _ filestore.Target, object string) (filestore.Entry, bool, error) {
	e, _, ok := previewFileByObject(object)
	return e, ok, nil
}
func (previewFiles) Upload(context.Context, string, filestore.Target, string, bool) error {
	return nil
}
func (previewFiles) Download(_ context.Context, _ filestore.Target, object, destPath string) error {
	_, data, ok := previewFileByObject(object)
	if !ok {
		return fmt.Errorf("object not found")
	}
	if data == nil {
		return fmt.Errorf("object not found")
	}
	return os.WriteFile(destPath, data, 0o600)
}
func (previewFiles) Delete(context.Context, filestore.Target, string) error {
	return nil
}

// Minimal valid PDF / 1×1 PNG so the preview server can actually render them.
var previewPDF = []byte("%PDF-1.1\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/MediaBox[0 0 300 200]/Parent 2 0 R>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n")

var previewPNG = mustPreviewB64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

var previewNotes = []byte("handoff notes\n")

func mustPreviewB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
