package bot

import (
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestApplyPRTerminalStateMultiSession(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Projects:   config.PathProjects(map[string]string{"p": dir}),
		Channels:   map[string]string{"c": "p"},
		DataDir:    filepath.Join(dir, "data"),
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(cfg, store, hist)
	pr := sessionstore.TrackedPR{
		URL: "https://github.com/acme/app/pull/5", Number: 5, State: "OPEN",
		Owner: "acme", Repo: "app", Title: "t",
	}
	if err := store.Set("t1", sessionstore.Entry{Project: "p", PRs: []sessionstore.TrackedPR{pr}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("t2", sessionstore.Entry{Project: "p", PRs: []sessionstore.TrackedPR{pr}}); err != nil {
		t.Fatal(err)
	}
	// Unrelated PR
	if err := store.Set("t3", sessionstore.Entry{Project: "p", PRs: []sessionstore.TrackedPR{{
		URL: "https://github.com/acme/app/pull/99", Number: 99, State: "OPEN", Owner: "acme", Repo: "app",
	}}}); err != nil {
		t.Fatal(err)
	}

	got := b.ApplyPRTerminalState("acme", "app", 5, "MERGED")
	if len(got) != 2 {
		t.Fatalf("affected=%v", got)
	}
	// Terminal cleanup may remove sessions that only tracked the merged PR.
	// Unrelated PR thread must remain OPEN.
	e3, ok := store.Get("t3")
	if !ok {
		t.Fatal("t3 removed")
	}
	e3.NormalizePRs()
	if e3.PRs[0].State != "OPEN" {
		t.Fatalf("unrelated changed: %+v", e3.PRs)
	}
	// If t1/t2 still present (cleanup deferred), state must be MERGED.
	for _, id := range []string{"t1", "t2"} {
		if e, ok := store.Get(id); ok {
			e.NormalizePRs()
			if len(e.PRs) != 1 || e.PRs[0].State != "MERGED" {
				t.Fatalf("%s: %+v", id, e.PRs)
			}
		}
	}
}
