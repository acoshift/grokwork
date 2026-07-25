package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/gitworktree"
)

// TestWebUnitKeyMatchesGitworktree pins the store's local prefix test against the
// canonical definition. If they drift, legacy migration starts misclassifying
// units and Discord threads silently lose their cards.
func TestWebUnitKeyMatchesGitworktree(t *testing.T) {
	ids := []string{
		"w_0123456789abcdef0123456789abcdef",
		"w_short",
		"1234567890123456789", // Discord snowflake
		"",
		"  ",
		"web_1234", // not the prefix
		"W_upper",  // case-sensitive
	}
	for _, id := range ids {
		if got, want := isWebUnitKey(id), gitworktree.IsWebUnitID(id); got != want {
			t.Errorf("isWebUnitKey(%q) = %v, gitworktree.IsWebUnitID = %v", id, got, want)
		}
	}
}

func TestHasDiscordDistinguishesSurfaces(t *testing.T) {
	if (Entry{}).HasDiscord() {
		t.Error("zero Entry must not claim a Discord surface")
	}
	if (Entry{Discord: &DiscordRef{}}).HasDiscord() {
		t.Error("empty DiscordRef (no ThreadID) must not claim a Discord surface")
	}
	if !(Entry{Discord: &DiscordRef{ThreadID: "123"}}).HasDiscord() {
		t.Error("DiscordRef with a thread id must claim a Discord surface")
	}
	// Origin is where the run STARTED and must never be mistaken for the surface:
	// a web-started run that opened a thread is Origin=web *and* has Discord.
	e := Entry{Origin: "web", Discord: &DiscordRef{ThreadID: "999"}}
	if !e.HasDiscord() {
		t.Error("Origin=web must not suppress a real thread — this is the misclassification that would kill card rendering")
	}
}

func TestLoadMigratesLegacyEntries(t *testing.T) {
	dir := t.TempDir()
	// Legacy file: no "discord" key at all, one thread-keyed and one web-keyed unit.
	legacy := map[string]map[string]any{
		"1234567890123456789": {"sessionId": "s1", "project": "p", "discordUrl": "https://discord.com/x"},
		"w_abc":               {"sessionId": "s2", "project": "p", "origin": "web"},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	thread, ok := st.Get("1234567890123456789")
	if !ok {
		t.Fatal("thread entry missing after load")
	}
	if !thread.HasDiscord() {
		t.Fatal("legacy thread entry did not get a DiscordRef")
	}
	if thread.Discord.ThreadID != "1234567890123456789" {
		t.Errorf("ThreadID = %q, want the store key", thread.Discord.ThreadID)
	}
	if thread.Discord.URL != "https://discord.com/x" {
		t.Errorf("URL = %q, want the legacy discordUrl folded in", thread.Discord.URL)
	}

	web, ok := st.Get("w_abc")
	if !ok {
		t.Fatal("web entry missing after load")
	}
	if web.HasDiscord() {
		t.Error("web-native unit must not get a DiscordRef")
	}
}

func TestSetAndPatchNormalizeDiscordRef(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A creation site that knows nothing about DiscordRef still gets a correct one:
	// the store is the choke point, not the ~45 callers.
	if err := st.Set("555000111222333", Entry{SessionID: "s", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get("555000111222333")
	if !got.HasDiscord() || got.Discord.ThreadID != "555000111222333" {
		t.Fatalf("Set did not normalize: %+v", got.Discord)
	}

	if err := st.Set("w_xyz", Entry{SessionID: "s", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if w, _ := st.Get("w_xyz"); w.HasDiscord() {
		t.Error("Set must not invent a Discord surface for a web unit")
	}

	// Patch sets the jump link; both the ref and the legacy field must agree.
	if _, _, err := st.Patch("555000111222333", func(e *Entry) {
		e.Discord.URL = "https://discord.com/y"
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Get("555000111222333")
	if got.DiscordURL != "https://discord.com/y" {
		t.Errorf("legacy DiscordURL = %q, want mirrored so an older binary still reads it", got.DiscordURL)
	}
}

// TestMigrationSurvivesRoundTrip guards the back-compat rule: a migrated file
// must still be readable by a binary that predates DiscordRef, i.e. the legacy
// flat field is still written.
func TestMigrationSurvivesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("777", Entry{SessionID: "s", Project: "p", DiscordURL: "https://d/1"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk["777"]["discordUrl"] != "https://d/1" {
		t.Errorf("legacy discordUrl missing from disk: %v", onDisk["777"])
	}
	if _, ok := onDisk["777"]["discord"]; !ok {
		t.Errorf("new discord ref missing from disk: %v", onDisk["777"])
	}
}
