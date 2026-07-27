package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func rewriteFixture(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		DiscordToken: "tok",
		GrokBin:      "grok",
		ConfigPath:   filepath.Join(dir, "config.json"),
		DataDir:      filepath.Join(dir, "data"),
		WebAuth: &WebAuthConfig{
			Enabled:          true,
			SessionSecret:    "test-session-secret-32-bytes-long!",
			AdminDiscordIDs:  []string{"boss"},
			MemberDiscordIDs: []string{"github:999", "someone-else"},
			ViewerDiscordIDs: []string{"github:999"},
		},
		Projects: ProjectsMap{
			"app": {
				Path:           dir,
				AllowedUserIDs: []string{"github:999", "other"},
				Teams: map[string]TeamConfig{
					"support": {Label: "Support", Members: []string{"github:999"}, Capabilities: "investigator"},
					"idle":    {Label: "Idle"},
				},
				CapabilityByUser: map[string]string{"github:999": "investigator"},
			},
		},
		Channels: map[string]string{},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The whole point of absorbing: a grant written against a login that is now an
// alias would never match again, because a session never carries an alias.
func TestRewriteActorIDMovesEveryGrantKind(t *testing.T) {
	cfg := rewriteFixture(t)
	n, err := cfg.RewriteActorID("github:999", "42424")
	if err != nil {
		t.Fatal(err)
	}
	// member + viewer + allowlist + team member + capabilityByUser
	if n != 5 {
		t.Fatalf("rewrote %d entries, want 5", n)
	}
	if containsID(cfg.WebAuth.MemberDiscordIDs, "github:999") || !containsID(cfg.WebAuth.MemberDiscordIDs, "42424") {
		t.Fatalf("member list=%v", cfg.WebAuth.MemberDiscordIDs)
	}
	if !containsID(cfg.WebAuth.ViewerDiscordIDs, "42424") {
		t.Fatalf("viewer list=%v", cfg.WebAuth.ViewerDiscordIDs)
	}
	if !containsID(cfg.WebAuth.MemberDiscordIDs, "someone-else") {
		t.Fatal("an unrelated grant was disturbed")
	}
	pc := cfg.Projects["app"]
	if containsID(pc.AllowedUserIDs, "github:999") || !containsID(pc.AllowedUserIDs, "42424") {
		t.Fatalf("allowlist=%v", pc.AllowedUserIDs)
	}
	if !containsID(pc.Teams["support"].Members, "42424") || containsID(pc.Teams["support"].Members, "github:999") {
		t.Fatalf("team members=%v", pc.Teams["support"].Members)
	}
	if _, ok := pc.CapabilityByUser["github:999"]; ok {
		t.Fatalf("capabilityByUser kept the alias key: %v", pc.CapabilityByUser)
	}
	if pc.CapabilityByUser["42424"] != "investigator" {
		t.Fatalf("capabilityByUser=%v", pc.CapabilityByUser)
	}

	// It persisted: a rewrite that only lives in memory is undone by the next
	// restart, at which point the person silently loses access again.
	raw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	if !containsID(again.Projects["app"].AllowedUserIDs, "42424") {
		t.Fatalf("disk: %s", raw)
	}
}

// Absorbing runs again whenever a link is repeated (the recovery path when a
// rewrite failed halfway), so it has to be a no-op the second time.
func TestRewriteActorIDIsIdempotent(t *testing.T) {
	cfg := rewriteFixture(t)
	if _, err := cfg.RewriteActorID("github:999", "42424"); err != nil {
		t.Fatal(err)
	}
	before := slices.Clone(cfg.Projects["app"].AllowedUserIDs)
	n, err := cfg.RewriteActorID("github:999", "42424")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second pass rewrote %d entries", n)
	}
	if !slices.Equal(cfg.Projects["app"].AllowedUserIDs, before) {
		t.Fatalf("allowlist changed on the second pass: %v → %v", before, cfg.Projects["app"].AllowedUserIDs)
	}
}

// The account is frequently already in the same list as the login it absorbs —
// that is what happens when an admin worked around the split by granting both.
// Replacing blindly would leave the person in it twice, which then shows up as
// two roster rows for one human.
func TestRewriteActorIDDoesNotDuplicate(t *testing.T) {
	cfg := rewriteFixture(t)
	pc := cfg.Projects["app"]
	pc.AllowedUserIDs = []string{"github:999", "42424", "other"}
	pc.Teams = map[string]TeamConfig{
		"support": {Members: []string{"42424", "github:999"}},
	}
	pc.CapabilityByUser = map[string]string{"github:999": "investigator", "42424": "builder"}
	cfg.Projects["app"] = pc

	if _, err := cfg.RewriteActorID("github:999", "42424"); err != nil {
		t.Fatal(err)
	}
	got := cfg.Projects["app"]
	if n := countID(got.AllowedUserIDs, "42424"); n != 1 {
		t.Fatalf("allowlist names the account %d times: %v", n, got.AllowedUserIDs)
	}
	if n := countID(got.Teams["support"].Members, "42424"); n != 1 {
		t.Fatalf("team names the account %d times: %v", n, got.Teams["support"].Members)
	}
	// The account's own capability template wins: it is the one already in
	// force for the session doing the linking.
	if got.CapabilityByUser["42424"] != "builder" || len(got.CapabilityByUser) != 1 {
		t.Fatalf("capabilityByUser=%v", got.CapabilityByUser)
	}
}

// "discord:42424" and "42424" are the same person; a grant spelled either way
// has to be found, exactly as containsID finds it everywhere else.
func TestRewriteActorIDMatchesEitherSpelling(t *testing.T) {
	cfg := rewriteFixture(t)
	pc := cfg.Projects["app"]
	pc.AllowedUserIDs = []string{"google:sub-7"}
	cfg.Projects["app"] = pc
	n, err := cfg.RewriteActorID("GOOGLE:sub-7", "discord:42424")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || !containsID(cfg.Projects["app"].AllowedUserIDs, "42424") {
		t.Fatalf("n=%d allowlist=%v", n, cfg.Projects["app"].AllowedUserIDs)
	}
}

// Rewriting an id onto itself is a no-op, not an error: the caller cannot
// always tell that two spellings denote one actor.
func TestRewriteActorIDSelfIsNoOp(t *testing.T) {
	cfg := rewriteFixture(t)
	n, err := cfg.RewriteActorID("42424", "discord:42424")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("n=%d", n)
	}
}

func countID(ids []string, want string) int {
	n := 0
	for _, id := range ids {
		if SameActor(id, want) {
			n++
		}
	}
	return n
}
