package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkflowFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Entry{
		SessionID:     "sess-w",
		Project:       "app",
		LastUser:      "Alice",
		Origin:        "web",
		CreatedBy:     "u-42",
		CreatedByName: "Alice Web",
		DiscordURL:    "https://discord.com/channels/g/c/t",
		OwnerID:       "u-42",
		OwnerName:     "Alice Web",
	}
	if err := s.Set("thread-web-1", want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("thread-web-1")
	if !ok {
		t.Fatal("missing entry")
	}
	if got.Origin != "web" || got.CreatedBy != "u-42" || got.CreatedByName != "Alice Web" {
		t.Fatalf("workflow fields: %+v", got)
	}
	if got.DiscordURL != want.DiscordURL {
		t.Fatalf("discordURL=%q", got.DiscordURL)
	}
	// Reload from disk.
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	got2, ok := s2.Get("thread-web-1")
	if !ok {
		t.Fatal("missing after reload")
	}
	if got2.Origin != "web" || got2.CreatedBy != "u-42" || got2.DiscordURL != want.DiscordURL {
		t.Fatalf("reload dropped workflow fields: %+v", got2)
	}
}

func TestListAndCount(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Count() != 0 {
		t.Fatalf("Count=%d", s.Count())
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("List=%v", list)
	}

	if err := s.Set("t2", Entry{SessionID: "s2", Project: "p", LastUser: "alice", UpdatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("t1", Entry{SessionID: "s1", Project: "q", LastUser: "bob", UpdatedAt: "2026-06-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}

	if s.Count() != 2 {
		t.Fatalf("Count=%d want 2", s.Count())
	}
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("List len=%d", len(list))
	}
	// Newest UpdatedAt first (turn stamp, not last Set).
	if list[0].ThreadID != "t1" || list[0].SessionID != "s1" || list[0].Project != "q" {
		t.Fatalf("first listed = %+v", list[0])
	}
	if list[1].ThreadID != "t2" {
		t.Fatalf("second listed = %+v", list[1])
	}
	if list[0].UpdatedAt != "2026-06-01T00:00:00Z" || list[0].LastUser != "bob" {
		t.Fatalf("entry fields: %+v", list[0])
	}

	// Reload from disk via new store on same data dir.
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Count() != 2 {
		t.Fatalf("reloaded Count=%d", s2.Count())
	}
	// sessions.json path is under data dir.
	if _, err := filepath.Glob(filepath.Join(dir, "sessions.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRevIncrementsOnMutation(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Rev() != 0 {
		t.Fatalf("Rev=%d want 0", s.Rev())
	}
	if err := s.Set("t1", Entry{SessionID: "s1", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if s.Rev() != 1 {
		t.Fatalf("Rev after Set=%d want 1", s.Rev())
	}
	if _, _, err := s.Patch("t1", func(e *Entry) { e.Goal = "g" }); err != nil {
		t.Fatal(err)
	}
	if s.Rev() != 2 {
		t.Fatalf("Rev after Patch=%d want 2", s.Rev())
	}
	_ = s.List()
	if s.Rev() != 2 {
		t.Fatalf("List must not bump Rev: %d", s.Rev())
	}
	if err := s.Delete("t1"); err != nil {
		t.Fatal(err)
	}
	if s.Rev() != 3 {
		t.Fatalf("Rev after Delete=%d want 3", s.Rev())
	}
}

func TestUpdatedAtOnlyOnTurn(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	fixed := "2026-01-15T12:00:00Z"
	if err := s.Set("t1", Entry{SessionID: "s1", Project: "p", UpdatedAt: fixed}); err != nil {
		t.Fatal(err)
	}
	// Metadata Patch must not invent a new turn time.
	if _, ok, err := s.Patch("t1", func(e *Entry) { e.Goal = "ship it" }); err != nil || !ok {
		t.Fatalf("Patch: ok=%v err=%v", ok, err)
	}
	got, ok := s.Get("t1")
	if !ok || got.UpdatedAt != fixed || got.Goal != "ship it" {
		t.Fatalf("after Patch: %+v", got)
	}
	// Set that rebuilds without UpdatedAt preserves the prior turn stamp.
	if err := s.Set("t1", Entry{SessionID: "s1", Project: "p", Goal: "again"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get("t1")
	if got.UpdatedAt != fixed {
		t.Fatalf("Set preserved UpdatedAt=%q want %q", got.UpdatedAt, fixed)
	}
	// TouchTurn is the turn clock.
	if _, ok, err := s.TouchTurn("t1", "alice"); err != nil || !ok {
		t.Fatalf("TouchTurn: ok=%v err=%v", ok, err)
	}
	got, _ = s.Get("t1")
	if got.UpdatedAt == "" || got.UpdatedAt == fixed || got.LastUser != "alice" {
		t.Fatalf("after TouchTurn: %+v", got)
	}
	// Missing unit: no-op.
	if _, ok, err := s.TouchTurn("missing", "bob"); err != nil || ok {
		t.Fatalf("TouchTurn missing: ok=%v err=%v", ok, err)
	}
}

func TestOwnershipHelpers(t *testing.T) {
	var e Entry
	if e.HasOwner() || e.CanControl("u1") {
		t.Fatal("empty entry should be unowned")
	}
	e.SetOwner("u1", "alice")
	if !e.HasOwner() || !e.IsOwner("u1") || e.OwnerName != "alice" {
		t.Fatalf("SetOwner: %+v", e)
	}
	e.AddCoOwner("u2")
	e.AddCoOwner("u2") // dedupe
	e.AddCoOwner("u1") // no-op (owner)
	if !e.IsCoOwner("u2") || e.IsCoOwner("u1") || len(e.CoOwnerIDs) != 1 {
		t.Fatalf("co-owners: %+v", e.CoOwnerIDs)
	}
	if !e.CanControl("u1") || !e.CanControl("u2") || e.CanControl("u3") {
		t.Fatalf("CanControl: owner=%v co=%v other=%v", e.CanControl("u1"), e.CanControl("u2"), e.CanControl("u3"))
	}

	e.HandOff("u3", "carol")
	if e.OwnerID != "u3" || e.OwnerName != "carol" {
		t.Fatalf("HandOff owner: %+v", e)
	}
	if !e.IsCoOwner("u1") || !e.IsCoOwner("u2") {
		t.Fatalf("HandOff co-owners: %+v", e.CoOwnerIDs)
	}
	// Claim-style SetOwner removes claimer from co-owners.
	e.SetOwner("u2", "bob")
	if e.IsCoOwner("u2") || e.OwnerID != "u2" {
		t.Fatalf("SetOwner clears co-owner slot: %+v", e)
	}
}

func TestPatchPRFields(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("t1", Entry{SessionID: "s1", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := s.Patch("t1", func(ent *Entry) {
		ent.PRNumber = 42
		ent.PRURL = "https://github.com/o/r/pull/42"
		ent.PRState = "OPEN"
		ent.PRStatusMsgID = "m1"
	})
	if err != nil || !ok {
		t.Fatalf("Patch: ok=%v err=%v", ok, err)
	}
	if e.PRNumber != 42 || e.SessionID != "s1" || e.PRStatusMsgID != "m1" {
		t.Fatalf("patched=%+v", e)
	}
	got, ok := s.Get("t1")
	if !ok || got.PRNumber != 42 {
		t.Fatalf("Get=%+v ok=%v", got, ok)
	}
	if _, ok, err := s.Patch("missing", func(*Entry) {}); err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
}

func TestSaveAtomicNoLeftoverTmpAndPerm(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("t1", Entry{SessionID: "s1", Project: "p"}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "sessions.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", info.Mode().Perm())
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files after successful save: %v", matches)
	}
}

// TestSaveFailurePreservesPreviousContent simulates a crash mid-write by
// making the temp-file create fail (read-only parent directory) and asserts
// the previous sessions.json survives untouched rather than being
// truncated — the whole point of writing to a temp file and renaming
// instead of writing the target in place.
func TestSaveFailurePreservesPreviousContent(t *testing.T) {
	// Root ignores the read-only directory mode below, so the save would
	// succeed and the assertion would report a false FAILURE (not a false
	// pass). Skip rather than assert something untrue of the environment.
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not block root")
	}
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("t1", Entry{SessionID: "s1", Project: "p"}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "sessions.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Read-only directory: os.CreateTemp cannot create the temp file, so the
	// write must fail before sessions.json is ever touched.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := s.Set("t2", Entry{SessionID: "s2", Project: "q"}); err == nil {
		t.Fatal("expected error saving to a read-only directory")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed save altered file on disk:\nbefore=%s\nafter=%s", before, after)
	}
	var entries map[string]Entry
	if err := json.Unmarshal(after, &entries); err != nil {
		t.Fatalf("previous content no longer parses: %v", err)
	}
	if _, ok := entries["t1"]; !ok {
		t.Fatalf("expected original entry t1 preserved: %+v", entries)
	}
	if _, ok := entries["t2"]; ok {
		t.Fatalf("t2 should not have been persisted by the failed save: %+v", entries)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files after failed save: %v", matches)
	}
}
