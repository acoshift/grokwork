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

// Absorbing must not turn a DORMANT capabilityByUser entry into a live one.
//
// capabilityByUser is not an access grant, so an entry for a non-member is inert
// (the roster UI labels it exactly that) and is easy to create: setting one
// accepts any id, and removing a team member does not remove it. Because
// ResolveCapabilities ORs capabilityByUser with every team template, putting
// membership and an inert entry on the same id activates the entry — and linking
// is self-service, so an admin-granted adminProject/merge template would be
// reachable by linking two logins you already own.
//
// Both directions matter, and only one of them touches the capabilityByUser
// branch at all.
func TestRewriteActorIDDoesNotActivateADormantCapabilityEntry(t *testing.T) {
	// Direction 1: the ALIAS carries the dormant entry, the account is the member.
	t.Run("alias entry is not carried onto a member account", func(t *testing.T) {
		cfg := rewriteFixture(t)
		pc := cfg.Projects["app"]
		pc.AllowedUserIDs = nil
		pc.Teams = map[string]TeamConfig{
			"support": {Members: []string{"google:alice"}, Capabilities: "investigator"},
		}
		// 4242424242 is on no team and no allowlist: the entry cannot be used.
		pc.CapabilityByUser = map[string]string{"4242424242": "admin"}
		cfg.Projects["app"] = pc
		if cfg.AccessAllowed("app", "4242424242") {
			t.Fatal("fixture: the alias must have no access, or the entry is not dormant")
		}
		if caps := cfg.ResolveCapabilities("app", "google:alice"); caps.AdminProject || caps.Merge {
			t.Fatalf("fixture: the account already had admin: %+v", caps)
		}

		if _, err := cfg.RewriteActorID("4242424242", "google:alice"); err != nil {
			t.Fatal(err)
		}
		got := cfg.Projects["app"]
		if _, ok := got.CapabilityByUser["google:alice"]; ok {
			t.Fatalf("a dormant admin template was moved onto a member: %v", got.CapabilityByUser)
		}
		caps := cfg.ResolveCapabilities("app", "google:alice")
		if caps.AdminProject || caps.Merge || caps.Approve || caps.GithubWrites {
			t.Fatalf("linking granted capabilities neither login had: %+v", caps)
		}
		if !caps.Investigate {
			t.Fatalf("the account's own team template was lost: %+v", caps)
		}
	})

	// Direction 2: the ACCOUNT carries the dormant entry and the alias is the
	// member, so absorbing the membership alone activates it. No capabilityByUser
	// rewrite happens here at all.
	t.Run("the account's own dormant entry is dropped when membership arrives", func(t *testing.T) {
		cfg := rewriteFixture(t)
		pc := cfg.Projects["app"]
		pc.AllowedUserIDs = nil
		pc.Teams = map[string]TeamConfig{
			"support": {Members: []string{"4242424242"}, Capabilities: "investigator"},
		}
		pc.CapabilityByUser = map[string]string{"google:alice": "admin"}
		cfg.Projects["app"] = pc
		if cfg.AccessAllowed("app", "google:alice") {
			t.Fatal("fixture: the account must have no access, or its entry is not dormant")
		}

		if _, err := cfg.RewriteActorID("4242424242", "google:alice"); err != nil {
			t.Fatal(err)
		}
		got := cfg.Projects["app"]
		if _, ok := got.CapabilityByUser["google:alice"]; ok {
			t.Fatalf("a dormant admin template went live with the absorbed membership: %v",
				got.CapabilityByUser)
		}
		caps := cfg.ResolveCapabilities("app", "google:alice")
		if caps.AdminProject || caps.Merge || caps.Approve || caps.GithubWrites {
			t.Fatalf("linking granted capabilities neither login had: %+v", caps)
		}
		if !caps.Investigate {
			t.Fatalf("the absorbed team template was lost: %+v", caps)
		}
	})

	// A LIVE entry is still absorbed: the person could already use it, so keeping
	// it is what stops the link from silently demoting them.
	t.Run("a live entry is still carried", func(t *testing.T) {
		cfg := rewriteFixture(t)
		pc := cfg.Projects["app"]
		pc.AllowedUserIDs = []string{"github:999"}
		pc.Teams = nil
		pc.CapabilityByUser = map[string]string{"github:999": "admin"}
		cfg.Projects["app"] = pc

		if _, err := cfg.RewriteActorID("github:999", "42424"); err != nil {
			t.Fatal(err)
		}
		if got := cfg.Projects["app"].CapabilityByUser["42424"]; got != "admin" {
			t.Fatalf("capabilityByUser=%v — a template the alias could use must survive",
				cfg.Projects["app"].CapabilityByUser)
		}
	})

	// Neither id has access: nothing can go live either way, so the operator's
	// pre-provisioned entry is preserved rather than silently dropped.
	t.Run("two access-less ids keep the mapping", func(t *testing.T) {
		cfg := rewriteFixture(t)
		pc := cfg.Projects["app"]
		pc.AllowedUserIDs = []string{"somebody-else"}
		pc.Teams = nil
		pc.CapabilityByUser = map[string]string{"github:999": "builder"}
		cfg.Projects["app"] = pc

		if _, err := cfg.RewriteActorID("github:999", "42424"); err != nil {
			t.Fatal(err)
		}
		got := cfg.Projects["app"].CapabilityByUser
		if got["42424"] != "builder" {
			t.Fatalf("capabilityByUser=%v want the mapping moved to the account", got)
		}
		if cfg.AccessAllowed("app", "42424") {
			t.Fatal("and it is still dormant: the rewrite must not have granted access")
		}
	})
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

// TestRewriteActorIDRollsBackOnSaveFailure pins the recovery finishOAuthLink
// documents: "a rewrite that fails halfway leaves the person correctly linked
// with some grants still on the old id — repairable by simply linking again,
// which redoes the (idempotent) rewrite."
//
// That promise only holds if a failed save leaves NO trace in memory. Without
// the rollback the rewrite lands in memory anyway, so a retry finds `from`
// already gone, computes n == 0 and returns before it would call saveLocked —
// the file is never repaired while the process lives, and after a restart the
// config reloads still naming the alias, which (with every comparison now
// running against the canonical id) matches nobody.
func TestRewriteActorIDRollsBackOnSaveFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not block root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"discordToken":"t","projects":{},"channels":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		ConfigPath: path,
		DataDir:    dir,
		Projects: ProjectsMap{"app": ProjectConfig{
			Path:           dir,
			AllowedUserIDs: []string{"111"},
		}},
		Channels: map[string]string{},
	}

	// Make the save fail: writeFileAtomic cannot create its temp file.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	n, err := cfg.RewriteActorID("111", "google:alice")
	if err == nil {
		t.Fatal("expected the save to fail")
	}
	if n != 0 {
		t.Fatalf("reported %d rewrites despite failing to persist", n)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// In-memory must still name the alias, or the retry below is a no-op.
	if !containsID(cfg.Projects["app"].AllowedUserIDs, "111") {
		t.Fatalf("failed save left the rewrite in memory: %v", cfg.Projects["app"].AllowedUserIDs)
	}

	// The documented repair now works.
	n, err = cfg.RewriteActorID("111", "google:alice")
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if n == 0 {
		t.Fatal("retry rewrote nothing — the documented recovery is a no-op")
	}
	if !containsID(cfg.Projects["app"].AllowedUserIDs, "google:alice") {
		t.Fatalf("retry did not rewrite: %v", cfg.Projects["app"].AllowedUserIDs)
	}
}
