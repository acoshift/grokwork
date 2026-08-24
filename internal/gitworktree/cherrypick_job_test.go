package gitworktree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJobRoundTripAndOpenForTarget(t *testing.T) {
	dir := t.TempDir()
	id := "cp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	j := Job{
		ID:       id,
		Project:  "proj",
		RepoPath: "/tmp/repo",
		Checkout: "/tmp/co",
		Target:   "staging",
		Files:    []string{"README"},
		Status:   JobStatusConflict,
	}
	if err := SaveJob(dir, j); err != nil {
		t.Fatal(err)
	}
	got, err := LoadJob(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Project != "proj" || got.Target != "staging" || !got.Open() {
		t.Fatalf("%+v", got)
	}
	got.Files[0] = "mutated"
	again, err := LoadJob(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Files[0] != "README" {
		t.Fatal("LoadJob must clone")
	}
	if _, ok := OpenJobForTarget(dir, "proj", "/tmp/repo", "staging"); !ok {
		t.Fatal("expected open job")
	}
	if _, ok := OpenJobForTarget(dir, "proj", "/tmp/repo", "production"); ok {
		t.Fatal("wrong target")
	}
}

func TestSweepExpiredCherryPicks(t *testing.T) {
	ctx := t.Context()
	dataDir := t.TempDir()
	_, main, checkout, _, _ := parkConflict(t)
	id := "cp_" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	j := Job{
		ID:        id,
		Project:   "proj",
		RepoPath:  main,
		Checkout:  checkout,
		Target:    "staging",
		Status:    JobStatusConflict,
		CreatedAt: time.Now().Add(-48 * time.Hour),
		UpdatedAt: time.Now().Add(-48 * time.Hour),
	}
	if err := SaveJob(dataDir, j); err != nil {
		t.Fatal(err)
	}
	// SaveJob stamps UpdatedAt=now; backdate the file so the sweeper sees it as old.
	path := jobFile(dataDir, id)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored Job
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	stored.UpdatedAt = time.Now().Add(-48 * time.Hour)
	out, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}

	n := SweepExpiredCherryPicks(ctx, dataDir)
	if n != 1 {
		t.Fatalf("swept=%d", n)
	}
	got, err := LoadJob(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != JobStatusExpired {
		t.Fatalf("status=%s", got.Status)
	}
	if _, err := os.Stat(checkout); !os.IsNotExist(err) {
		t.Fatalf("checkout should be gone: %v", err)
	}

	freshID := "cp_" + "cccccccccccccccccccccccccccccccc"
	freshCheckout := filepath.Join(t.TempDir(), "fresh")
	if err := SaveJob(dataDir, Job{
		ID: freshID, Project: "proj", RepoPath: main, Checkout: freshCheckout,
		Target: "staging", Status: JobStatusConflict,
	}); err != nil {
		t.Fatal(err)
	}
	if n := SweepExpiredCherryPicks(ctx, dataDir); n != 0 {
		t.Fatalf("fresh job swept=%d", n)
	}
}

func TestValidJobID(t *testing.T) {
	t.Parallel()
	if !ValidJobID("cp_" + "0123456789abcdef0123456789abcdef") {
		t.Fatal("want valid")
	}
	if ValidJobID("cp_short") || ValidJobID("../x") || ValidJobID("") {
		t.Fatal("want invalid")
	}
}
