package config

import "testing"

// TestNormalizeActorID pins the mapping table. Getting this wrong is an
// authorization bug, so the cases are explicit rather than derived.
func TestNormalizeActorID(t *testing.T) {
	cases := map[string]string{
		"1234567890123456789": "discord:1234567890123456789", // bare == legacy snowflake
		" 1234 ":              "discord:1234",
		"discord:1234":        "discord:1234",
		"Discord:1234":        "discord:1234", // kind case-folded
		"DISCORD: 1234 ":      "discord:1234",
		"web:abc":             "web:abc",
		"oidc:alice":          "oidc:alice",
		"oidc:Alice":          "oidc:Alice", // subject case preserved
		"oidc:sub-xyz":        "oidc:sub-xyz",
		"weird:thing":         "weird:thing", // unknown kind passed through
		"":                    "",
		"   ":                 "",
		"discord:":            "", // namespace with no subject is not an actor
		"oidc:  ":             "",
	}
	for in, want := range cases {
		if got := NormalizeActorID(in); got != want {
			t.Errorf("NormalizeActorID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUnknownNamespaceNotCoercedToDiscord is the security-relevant case: coercing
// an unrecognized namespace to Discord would grant whatever the id resembled.
func TestUnknownNamespaceNotCoercedToDiscord(t *testing.T) {
	if got := NormalizeActorID("saml:1234567890"); got != "saml:1234567890" {
		t.Fatalf("got %q, want the id untouched", got)
	}
	if SameActor("saml:1234567890", "1234567890") {
		t.Error("an unknown-namespace id must not match a bare Discord id")
	}
	if ActorKind("saml:123") == ActorKindDiscord {
		t.Error("unknown namespace reported as discord")
	}
}

// TestLoginProviderActorKindsAreRegistered: an unregistered kind is passed
// through un-folded, so "Google:123" and "google:123" would be two different
// actors and "google:  " would normalize to a non-empty garbage id.
func TestLoginProviderActorKindsAreRegistered(t *testing.T) {
	cases := map[string]string{
		"google:sub-1":  "google:sub-1",
		"Google:sub-1":  "google:sub-1", // kind case-folded
		"GOOGLE: sub-1": "google:sub-1",
		"google:Sub-1":  "google:Sub-1", // subject case preserved
		"github:42":     "github:42",
		"GitHub: 42 ":   "github:42",
		"google:":       "", // namespace with no subject is not an actor
		"google:  ":     "",
		"github:":       "",
	}
	for in, want := range cases {
		if got := NormalizeActorID(in); got != want {
			t.Errorf("NormalizeActorID(%q) = %q, want %q", in, got, want)
		}
	}
	if !SameActor("Google:sub-1", "google:sub-1") {
		t.Error("case-differing spellings of the same Google actor must match")
	}
	if !SameActor("github:42", "GitHub:42") {
		t.Error("case-differing spellings of the same GitHub actor must match")
	}
	for _, id := range []string{"google:sub-1", "github:42"} {
		if IsDiscordActor(id) {
			t.Errorf("%q reported as a Discord actor — it would be DMed and looked up for guild roles", id)
		}
	}
	if got := ActorKind("github:42"); got != ActorKindGitHub {
		t.Errorf("ActorKind(github:42) = %q", got)
	}
	if got := ActorSubject("google:sub-1"); got != "sub-1" {
		t.Errorf("ActorSubject(google:sub-1) = %q", got)
	}
	// A bare id still means Discord — no existing config changes meaning.
	if got := NormalizeActorID("424242424242424242"); got != "discord:424242424242424242" {
		t.Errorf("bare id = %q, want the discord namespace", got)
	}
}

// TestProviderSubjectSpacesDoNotCollide is the namespace-collision guard, and
// the reason each provider gets its own kind instead of a shared "oidc:".
// Subject spaces are independent per issuer: GitHub user id 12345 and a Google
// "sub" of 12345 are different people, and a Discord snowflake of 12345 is a
// third. Any pair matching here means one of them inherits the others' team
// memberships and admin rights.
func TestProviderSubjectSpacesDoNotCollide(t *testing.T) {
	ids := []string{"google:12345", "github:12345", "discord:12345", "12345", "oidc:12345", "web:12345"}
	for i, a := range ids {
		for j, b := range ids {
			// "discord:12345" and the bare "12345" are the one intended pair.
			same := i == j || (a == "12345" && b == "discord:12345") || (a == "discord:12345" && b == "12345")
			if got := SameActor(a, b); got != same {
				t.Errorf("SameActor(%q, %q) = %v, want %v", a, b, got, same)
			}
		}
	}

	// And the same collision through the real authorization path.
	c := &Config{
		WebAuth:  &WebAuthConfig{Enabled: true, AdminDiscordIDs: []string{"google:12345"}},
		Projects: ProjectsMap{"p": {AllowedUserIDs: []string{"github:777"}}},
	}
	if role, ok := c.ResolveWebRoleForConfig("google:12345"); !ok || role != WebRoleAdmin {
		t.Fatalf("google admin = (%q,%v), want (admin,true)", role, ok)
	}
	for _, impostor := range []string{"github:12345", "discord:12345", "12345", "oidc:12345"} {
		if role, ok := c.ResolveWebRoleForConfig(impostor); ok {
			t.Errorf("%q inherited the google:12345 admin grant as %q", impostor, role)
		}
	}
	if role, ok := c.ResolveWebRoleForConfig("github:777"); !ok || role != WebRoleMember {
		t.Fatalf("github project member = (%q,%v), want (member,true)", role, ok)
	}
	if _, ok := c.ResolveWebRoleForConfig("google:777"); ok {
		t.Error("google:777 inherited the github:777 project grant")
	}
	if !c.AccessAllowed("p", "github:777") {
		t.Error("a GitHub actor must be grantable on a project allowlist")
	}
	if c.AccessAllowed("p", "777") {
		t.Error("a bare snowflake matched a github:-namespaced allowlist entry")
	}
}

func TestSameActorAcrossSpellings(t *testing.T) {
	if !SameActor("1234", "discord:1234") {
		t.Error("bare and namespaced Discord ids must match")
	}
	if !SameActor("discord:1234", "1234") {
		t.Error("match must be symmetric")
	}
	if SameActor("oidc:1234", "discord:1234") {
		t.Error("same subject in different namespaces must not match")
	}
	if SameActor("", "") || SameActor("", "1234") {
		t.Error("empty id must never match")
	}
}

func TestIsDiscordActor(t *testing.T) {
	for id, want := range map[string]bool{
		"1234":       true, // bare is Discord
		"discord:1":  true,
		"web:1":      false,
		"oidc:alice": false,
		"oidc:x":     false,
		"":           false,
	} {
		if got := IsDiscordActor(id); got != want {
			t.Errorf("IsDiscordActor(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestAccessAllowedAcceptsNamespacedAllowlist is the point of the whole change:
// a non-Discord person can now be named in a project allowlist.
func TestAccessAllowedAcceptsNamespacedAllowlist(t *testing.T) {
	c := &Config{Projects: ProjectsMap{
		"p": {AllowedUserIDs: []string{"discord:111", "oidc:alice"}},
	}}
	if !c.AccessAllowed("p", "111") {
		t.Error("bare runtime id must match a discord:-namespaced allowlist entry")
	}
	if !c.AccessAllowed("p", "discord:111") {
		t.Error("namespaced runtime id must match")
	}
	if !c.AccessAllowed("p", "oidc:alice") {
		t.Error("a local (non-Discord) actor must be grantable — this was impossible before")
	}
	if c.AccessAllowed("p", "222") {
		t.Error("unrelated id allowed")
	}
	if c.AccessAllowed("p", "oidc:bob") {
		t.Error("unrelated local id allowed")
	}
	// Legacy config with bare snowflakes keeps working unchanged.
	legacy := &Config{Projects: ProjectsMap{"p": {AllowedUserIDs: []string{"111"}}}}
	if !legacy.AccessAllowed("p", "111") {
		t.Error("legacy bare allowlist broke")
	}
	if !legacy.AccessAllowed("p", "discord:111") {
		t.Error("legacy bare allowlist must also accept the namespaced spelling")
	}
}

func TestResolveCapabilitiesAcceptsNamespacedKeys(t *testing.T) {
	c := &Config{Projects: ProjectsMap{
		"p": {
			AllowedUserIDs:   []string{"discord:111", "oidc:alice"},
			CapabilityByUser: map[string]string{"discord:111": "builder"},
		},
	}}
	if !c.ResolveCapabilities("p", "111").CanShip() {
		t.Error("bare runtime id must hit a namespaced capabilityByUser key")
	}

	// And the reverse spelling.
	c2 := &Config{Projects: ProjectsMap{
		"p": {
			AllowedUserIDs:   []string{"111"},
			CapabilityByUser: map[string]string{"111": "builder"},
		},
	}}
	if !c2.ResolveCapabilities("p", "discord:111").CanShip() {
		t.Error("namespaced runtime id must hit a bare capabilityByUser key")
	}
}

// TestNonDiscordActorResolvesRoleFromAllowlist is the end of the identity chain:
// an actor from a non-Discord provider, named in a project allowlist, gets a web
// role — impossible while every id had to be a snowflake.
func TestNonDiscordActorResolvesRoleFromAllowlist(t *testing.T) {
	c := &Config{
		WebAuth:  &WebAuthConfig{Enabled: true},
		Projects: ProjectsMap{"p": {AllowedUserIDs: []string{"oidc:alice"}}},
	}
	role, ok := c.ResolveWebRoleForConfig("oidc:alice")
	if !ok {
		t.Fatal("non-Discord actor on a project allowlist must resolve a role")
	}
	if role != WebRoleMember {
		t.Errorf("role = %q, want member", role)
	}
	if _, ok := c.ResolveWebRoleForConfig("oidc:bob"); ok {
		t.Error("unlisted actor must be denied")
	}
}

// TestDiscordRoleResolutionSurvivesNamespacing keeps the legacy path working: a
// bare snowflake allowlist must still resolve for a bare snowflake login. Also
// covers ProjectUserSet, which is a raw map probe and so the one place where the
// two spellings could stop matching.
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
