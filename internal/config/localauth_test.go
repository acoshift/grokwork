package config

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("plaintext leaked into the hash")
	}
	if !strings.HasPrefix(hash, "pbkdf2$sha256$") {
		t.Errorf("hash = %q, want the parameterized encoding", hash)
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Error("correct password rejected")
	}
	if VerifyPassword(hash, "wrong horse battery") {
		t.Error("wrong password accepted")
	}
	// Salted: the same password hashes differently every time.
	hash2, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if hash == hash2 {
		t.Error("hashes identical — salt is not random")
	}
	if !VerifyPassword(hash2, "correct horse battery") {
		t.Error("second hash does not verify")
	}
}

func TestWeakPasswordRejected(t *testing.T) {
	if _, err := HashPassword("short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("err = %v, want ErrWeakPassword", err)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"", "  ", "plaintext", "pbkdf2$sha256$x$y$z",
		"pbkdf2$sha512$1000$AAAA$BBBB", // wrong prf
		"pbkdf2$sha256$0$AAAA$BBBB",    // zero iterations
		"bcrypt$2a$10$abc",
		"pbkdf2$sha256$1000$!!!$BBBB", // bad base64
	} {
		if VerifyPassword(bad, "anything") {
			t.Errorf("VerifyPassword(%q) accepted a malformed hash", bad)
		}
	}
}

// TestVerifyRejectsEmptyKeyHash guards the degenerate case where a truncated hash
// could otherwise compare equal to an empty derived key.
func TestVerifyRejectsEmptyKeyHash(t *testing.T) {
	if VerifyPassword("pbkdf2$sha256$1000$AAAAAAAAAAAAAAAAAAAAAA$", "") {
		t.Error("empty stored key accepted")
	}
}

func newLocalCfg(t *testing.T, accounts ...LocalAccount) *Config {
	t.Helper()
	return &Config{WebAuth: &WebAuthConfig{Enabled: true, LocalAccounts: accounts}}
}

func TestAuthenticateLocal(t *testing.T) {
	hash, err := HashPassword("a-very-good-password")
	if err != nil {
		t.Fatal(err)
	}
	c := newLocalCfg(t, LocalAccount{ID: "alice", DisplayName: "Alice", PasswordHash: hash, Role: WebRoleMember})

	got, err := c.AuthenticateLocal("alice", "a-very-good-password")
	if err != nil {
		t.Fatalf("valid login rejected: %v", err)
	}
	if got.ActorID() != "local:alice" {
		t.Errorf("actor id = %q, want local:alice", got.ActorID())
	}

	// The namespaced spelling is accepted too, since that is what the rest of the
	// system shows the user.
	if _, err := c.AuthenticateLocal("local:alice", "a-very-good-password"); err != nil {
		t.Errorf("namespaced username rejected: %v", err)
	}
	// Username is case-insensitive; the password never is.
	if _, err := c.AuthenticateLocal("ALICE", "a-very-good-password"); err != nil {
		t.Errorf("case-different username rejected: %v", err)
	}
	if _, err := c.AuthenticateLocal("alice", "A-Very-Good-Password"); !errors.Is(err, ErrLocalAuthFailed) {
		t.Error("password comparison must be case-sensitive")
	}
}

// TestAuthenticateLocalFailuresAreIndistinguishable: every rejection returns the
// same error, so the response cannot be used to enumerate accounts.
func TestAuthenticateLocalFailuresAreIndistinguishable(t *testing.T) {
	hash, _ := HashPassword("a-very-good-password")
	c := newLocalCfg(t,
		LocalAccount{ID: "alice", PasswordHash: hash},
		LocalAccount{ID: "carol", PasswordHash: hash, Disabled: true},
		LocalAccount{ID: "dave"}, // provisioned with no hash yet
	)
	for name, args := range map[string][2]string{
		"unknown account": {"nobody", "a-very-good-password"},
		"wrong password":  {"alice", "nope-nope-nope"},
		"disabled":        {"carol", "a-very-good-password"},
		"no hash set":     {"dave", "a-very-good-password"},
		"empty password":  {"alice", ""},
		"empty username":  {"", "a-very-good-password"},
	} {
		_, err := c.AuthenticateLocal(args[0], args[1])
		if !errors.Is(err, ErrLocalAuthFailed) {
			t.Errorf("%s: err = %v, want the single generic failure", name, err)
		}
	}
}

func TestLocalAccountsEnabled(t *testing.T) {
	hash, _ := HashPassword("a-very-good-password")
	if newLocalCfg(t).LocalAccountsEnabled() {
		t.Error("no accounts should not enable the form")
	}
	if newLocalCfg(t, LocalAccount{ID: "a", PasswordHash: hash, Disabled: true}).LocalAccountsEnabled() {
		t.Error("only-disabled accounts should not enable the form")
	}
	if newLocalCfg(t, LocalAccount{ID: "a"}).LocalAccountsEnabled() {
		t.Error("an account with no hash should not enable the form")
	}
	if !newLocalCfg(t, LocalAccount{ID: "a", PasswordHash: hash}).LocalAccountsEnabled() {
		t.Error("a usable account should enable the form")
	}
	var nilCfg *Config
	if nilCfg.LocalAccountsEnabled() {
		t.Error("nil config")
	}
}

// TestLocalActorResolvesRoleFromAllowlist is the end of the identity chain: a
// local actor named in a project allowlist gets a web role, which was impossible
// while every id had to be a snowflake.
func TestLocalActorResolvesRoleFromAllowlist(t *testing.T) {
	c := &Config{
		WebAuth:  &WebAuthConfig{Enabled: true},
		Projects: ProjectsMap{"p": {AllowedUserIDs: []string{"local:alice"}}},
	}
	role, ok := c.ResolveWebRoleForConfig("local:alice")
	if !ok {
		t.Fatal("local actor on a project allowlist must resolve a role")
	}
	if role != WebRoleMember {
		t.Errorf("role = %q, want member", role)
	}
	if _, ok := c.ResolveWebRoleForConfig("local:bob"); ok {
		t.Error("unlisted local actor must be denied")
	}
}

// TestDiscordRoleResolutionSurvivesNamespacing keeps the legacy path working: a
// bare snowflake allowlist must still resolve for a bare snowflake login.
func TestDiscordRoleResolutionSurvivesNamespacing(t *testing.T) {
	c := &Config{
		WebAuth:  &WebAuthConfig{Enabled: true, AdminDiscordIDs: []string{"111222333444555666"}},
		Projects: ProjectsMap{"p": {AllowedUserIDs: []string{"777888999000111222"}}},
	}
	if role, ok := c.ResolveWebRoleForConfig("111222333444555666"); !ok || role != WebRoleAdmin {
		t.Errorf("admin snowflake = (%q,%v), want (admin,true)", role, ok)
	}
	if role, ok := c.ResolveWebRoleForConfig("777888999000111222"); !ok || role != WebRoleMember {
		t.Errorf("project member snowflake = (%q,%v), want (member,true)", role, ok)
	}
	// And the namespaced spelling of the same person.
	if role, ok := c.ResolveWebRoleForConfig("discord:777888999000111222"); !ok || role != WebRoleMember {
		t.Errorf("namespaced project member = (%q,%v), want (member,true)", role, ok)
	}
}
