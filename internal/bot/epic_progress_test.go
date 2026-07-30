package bot

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func epicProgressBot(t *testing.T, featureOn bool) (*Bot, *sessionstore.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir: dir,
		WebAuth: &config.WebAuthConfig{
			Enabled:  true,
			Features: config.WebAuthFeatures{GitHubWrites: featureOn},
		},
	}
	return New(cfg, store, nil), store
}

func mergedPREntry(threadID string) sessionstore.Entry {
	return sessionstore.Entry{
		SessionID: "s1",
		Project:   "app",
		Cwd:       "/tmp/wt",
		MainCwd:   "/tmp/main",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/app/pull/7", Number: 7, State: "MERGED",
			Owner: "acme", Repo: "app",
		}},
		Issues: []sessionstore.TrackedIssue{{
			Number: 42, Owner: "acme", Repo: "app",
			URL: "https://github.com/acme/app/issues/42",
		}},
	}
}

type ghCapture struct {
	mu    sync.Mutex
	calls []string
	body  string
	view  string // issue body returned by issue view
}

func (c *ghCapture) runner() func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		joined := name + " " + strings.Join(args, " ")
		c.calls = append(c.calls, joined)
		if strings.Contains(joined, "issue view") {
			payload, _ := json.Marshal(map[string]any{
				"number":   42,
				"url":      "https://github.com/acme/app/issues/42",
				"title":    "Feature",
				"state":    "OPEN",
				"author":   map[string]string{"login": "a"},
				"labels":   []any{},
				"body":     c.view,
				"comments": []any{},
			})
			return payload, nil
		}
		if strings.Contains(joined, "issue edit") {
			for i, a := range args {
				if a == "--body-file" && i+1 < len(args) {
					b, err := os.ReadFile(args[i+1])
					if err != nil {
						return nil, err
					}
					c.body = string(b)
				}
			}
			return []byte("ok"), nil
		}
		return []byte("{}"), nil
	}
}

func (c *ghCapture) hasEdit() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, call := range c.calls {
		if strings.Contains(call, "issue edit") {
			return true
		}
	}
	return false
}

func (c *ghCapture) hasView() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, call := range c.calls {
		if strings.Contains(call, "issue view") {
			return true
		}
	}
	return false
}

func (c *ghCapture) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func TestSyncEpicChecklistFlipsAnnotatedLine(t *testing.T) {
	threadID := "w_item1"
	body := "## Breakdown\n" +
		"- [ ] Add OIDC callback — [session](https://gw.example/sessions/" + threadID + ")\n" +
		"- [ ] other item\n"
	cap := &ghCapture{view: body}
	b, _ := epicProgressBot(t, true)
	b.SetGHRunner(cap.runner())

	e := mergedPREntry(threadID)
	b.syncEpicChecklist(threadID, e)

	if !cap.hasView() || !cap.hasEdit() {
		t.Fatalf("want view+edit, calls=%v", cap.calls)
	}
	if !strings.Contains(cap.body, "- [x] Add OIDC callback — [session](") {
		t.Fatalf("expected checked line, body=%q", cap.body)
	}
	if !strings.Contains(cap.body, "- [ ] other item") {
		t.Fatalf("other line must stay unchecked: %q", cap.body)
	}
}

func TestSyncEpicChecklistAlreadyCheckedNoEdit(t *testing.T) {
	threadID := "w_done"
	body := "- [x] Add OIDC — [session](https://gw.example/sessions/" + threadID + ")\n"
	cap := &ghCapture{view: body}
	b, _ := epicProgressBot(t, true)
	b.SetGHRunner(cap.runner())

	b.syncEpicChecklist(threadID, mergedPREntry(threadID))

	if !cap.hasView() {
		t.Fatal("want issue view")
	}
	if cap.hasEdit() {
		t.Fatalf("already checked: no edit, calls=%v", cap.calls)
	}
}

func TestSyncEpicChecklistClosedNotMergedNoGH(t *testing.T) {
	threadID := "w_closed"
	cap := &ghCapture{view: "- [ ] x — [session](https://gw.example/sessions/" + threadID + ")\n"}
	b, _ := epicProgressBot(t, true)
	b.SetGHRunner(cap.runner())

	e := mergedPREntry(threadID)
	e.PRs[0].State = "CLOSED"
	b.syncEpicChecklist(threadID, e)

	if cap.callCount() != 0 {
		t.Fatalf("closed-unmerged must not call gh, got %v", cap.calls)
	}
}

func TestSyncEpicChecklistFeatureOffNoGH(t *testing.T) {
	threadID := "w_off"
	cap := &ghCapture{view: "- [ ] x — [session](https://gw.example/sessions/" + threadID + ")\n"}
	b, _ := epicProgressBot(t, false)
	b.SetGHRunner(cap.runner())

	b.syncEpicChecklist(threadID, mergedPREntry(threadID))

	if cap.callCount() != 0 {
		t.Fatalf("FeatureGitHubWrites off: no gh calls, got %v", cap.calls)
	}
}

func TestSyncEpicChecklistNoAnnotationViewOnly(t *testing.T) {
	threadID := "w_none"
	// Bound issue, but body has no annotation for this thread.
	body := "- [ ] Add OIDC callback\n- [ ] other — [session](https://gw.example/sessions/other)\n"
	cap := &ghCapture{view: body}
	b, _ := epicProgressBot(t, true)
	b.SetGHRunner(cap.runner())

	b.syncEpicChecklist(threadID, mergedPREntry(threadID))

	if !cap.hasView() {
		t.Fatal("want issue view")
	}
	if cap.hasEdit() {
		t.Fatalf("no matching annotation: no edit, calls=%v", cap.calls)
	}
}

func TestCleanupWhenAllPRsDoneSyncsChecklist(t *testing.T) {
	threadID := "w_hook"
	body := "- [ ] Ship it — [session](https://gw.example/sessions/" + threadID + ")\n"
	cap := &ghCapture{view: body}
	b, store := epicProgressBot(t, true)
	b.SetGHRunner(cap.runner())

	e := mergedPREntry(threadID)
	// cleanupWhenAllPRsDone tries worktree cleanup when MainCwd is a real repo;
	// leave paths empty so cleanup is a no-op beyond the sync + Patch.
	e.Cwd = ""
	e.MainCwd = ""
	e.WorktreeBranch = ""
	if err := store.Set(threadID, e); err != nil {
		t.Fatal(err)
	}

	if err := b.cleanupWhenAllPRsDone(threadID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if !cap.hasView() || !cap.hasEdit() {
		t.Fatalf("hook must attempt checklist edit, calls=%v", cap.calls)
	}
	if !strings.Contains(cap.body, "- [x] Ship it") {
		t.Fatalf("edited body=%q", cap.body)
	}
}

func TestAllPRsMergedHelper(t *testing.T) {
	if allPRsMerged(sessionstore.Entry{}) {
		t.Fatal("empty should be false")
	}
	if allPRsMerged(sessionstore.Entry{PRs: []sessionstore.TrackedPR{{State: "OPEN"}}}) {
		t.Fatal("open should be false")
	}
	if allPRsMerged(sessionstore.Entry{PRs: []sessionstore.TrackedPR{
		{State: "MERGED"}, {State: "CLOSED"},
	}}) {
		t.Fatal("mixed closed should be false")
	}
	if !allPRsMerged(sessionstore.Entry{PRs: []sessionstore.TrackedPR{
		{State: "MERGED"}, {State: "MERGED"},
	}}) {
		t.Fatal("all merged should be true")
	}
}
