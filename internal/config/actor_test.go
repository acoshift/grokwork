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
		"local:alice":         "local:alice",
		"local:Alice":         "local:Alice", // subject case preserved
		"oidc:sub-xyz":        "oidc:sub-xyz",
		"weird:thing":         "weird:thing", // unknown kind passed through
		"":                    "",
		"   ":                 "",
		"discord:":            "", // namespace with no subject is not an actor
		"local:  ":            "",
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

func TestSameActorAcrossSpellings(t *testing.T) {
	if !SameActor("1234", "discord:1234") {
		t.Error("bare and namespaced Discord ids must match")
	}
	if !SameActor("discord:1234", "1234") {
		t.Error("match must be symmetric")
	}
	if SameActor("local:1234", "discord:1234") {
		t.Error("same subject in different namespaces must not match")
	}
	if SameActor("", "") || SameActor("", "1234") {
		t.Error("empty id must never match")
	}
}

func TestIsDiscordActor(t *testing.T) {
	for id, want := range map[string]bool{
		"1234":        true, // bare is Discord
		"discord:1":   true,
		"web:1":       false,
		"local:alice": false,
		"oidc:x":      false,
		"":            false,
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
		"p": {AllowedUserIDs: []string{"discord:111", "local:alice"}},
	}}
	if !c.AccessAllowed("p", "111", nil) {
		t.Error("bare runtime id must match a discord:-namespaced allowlist entry")
	}
	if !c.AccessAllowed("p", "discord:111", nil) {
		t.Error("namespaced runtime id must match")
	}
	if !c.AccessAllowed("p", "local:alice", nil) {
		t.Error("a local (non-Discord) actor must be grantable — this was impossible before")
	}
	if c.AccessAllowed("p", "222", nil) {
		t.Error("unrelated id allowed")
	}
	if c.AccessAllowed("p", "local:bob", nil) {
		t.Error("unrelated local id allowed")
	}
	// Legacy config with bare snowflakes keeps working unchanged.
	legacy := &Config{Projects: ProjectsMap{"p": {AllowedUserIDs: []string{"111"}}}}
	if !legacy.AccessAllowed("p", "111", nil) {
		t.Error("legacy bare allowlist broke")
	}
	if !legacy.AccessAllowed("p", "discord:111", nil) {
		t.Error("legacy bare allowlist must also accept the namespaced spelling")
	}
}

func TestResolveCapabilitiesAcceptsNamespacedKeys(t *testing.T) {
	c := &Config{Projects: ProjectsMap{
		"p": {
			AllowedUserIDs:   []string{"discord:111", "local:alice"},
			CapabilityByUser: map[string]string{"discord:111": "builder"},
		},
	}}
	if !c.ResolveCapabilities("p", "111", nil).CanShip() {
		t.Error("bare runtime id must hit a namespaced capabilityByUser key")
	}

	// And the reverse spelling.
	c2 := &Config{Projects: ProjectsMap{
		"p": {
			AllowedUserIDs:   []string{"111"},
			CapabilityByUser: map[string]string{"111": "builder"},
		},
	}}
	if !c2.ResolveCapabilities("p", "discord:111", nil).CanShip() {
		t.Error("namespaced runtime id must hit a bare capabilityByUser key")
	}
}
