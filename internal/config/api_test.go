package config

import "testing"

func TestAPIEnabledDefaultOff(t *testing.T) {
	if (*Config)(nil).APIEnabled() {
		t.Fatal("nil config must not enable the API")
	}
	if (&Config{}).APIEnabled() {
		t.Fatal("omitted api block must be off")
	}
	if (&Config{API: &APIConfig{Enabled: false}}).APIEnabled() {
		t.Fatal("explicit false must be off")
	}
	if !(&Config{API: &APIConfig{Enabled: true}}).APIEnabled() {
		t.Fatal("enabled true must be on")
	}
}

func TestNormalizeActorIDTokenKind(t *testing.T) {
	cases := map[string]string{
		"token:k7m2p9qx": "token:k7m2p9qx",
		"Token:k7m2p9qx": "token:k7m2p9qx",
		"TOKEN: abc ":    "token:abc",
		"token:":         "",
	}
	for in, want := range cases {
		if got := NormalizeActorID(in); got != want {
			t.Errorf("NormalizeActorID(%q) = %q, want %q", in, got, want)
		}
	}
	if !SameActor("Token:abc", "token:abc") {
		t.Fatal("token kind must case-fold")
	}
	if SameActor("token:abc", "abc") {
		t.Fatal("token id must not match a bare Discord id")
	}
	if ActorKind("token:abc") != ActorKindToken {
		t.Fatal("ActorKind(token:abc) must be token")
	}
	if IsDiscordActor("token:abc") {
		t.Fatal("token actor must not be treated as Discord")
	}
}

func TestResolveCapabilitiesUnmappedTokenIsZero(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {Path: "/tmp/app", AllowedUserIDs: []string{"token:abc"}},
		},
	}
	caps := cfg.ResolveCapabilities("app", "token:abc")
	if caps != (Capabilities{}) {
		t.Fatalf("unmapped token must be zero, got %+v", caps)
	}
	if caps.CanShip() || caps.StartSessions || caps.CanInvestigateShell() {
		t.Fatalf("unmapped token leaked human builder default: %+v", caps)
	}
	// Human on the same allowlist still gets builder when safeTeam is off.
	human := &Config{
		Projects: ProjectsMap{
			"app": {Path: "/tmp/app", AllowedUserIDs: []string{"u1"}},
		},
	}
	if !human.ResolveCapabilities("app", "u1").CanShip() {
		t.Fatal("human unmapped default must stay builder")
	}
}

func TestResolveCapabilitiesTokenOnNamedTemplate(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {
				Path: "/tmp/app",
				CapabilityTemplates: map[string]Capabilities{
					"automation-runner": {StartSessions: true},
				},
				Teams: map[string]TeamConfig{
					"automation": {Members: []string{"token:abc"}, Capabilities: "automation-runner"},
				},
			},
		},
	}
	caps := cfg.ResolveCapabilities("app", "token:abc")
	if !caps.StartSessions || caps.GithubWrites || caps.CanShip() {
		t.Fatalf("named token template = %+v", caps)
	}
}

func TestResolveCapabilitiesTokenSafeTeamStillZeroWhenUnmapped(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {
				Path:           "/tmp/app",
				SafeTeamMode:   new(true),
				AllowedUserIDs: []string{"token:abc"},
			},
		},
	}
	caps := cfg.ResolveCapabilities("app", "token:abc")
	if caps != (Capabilities{}) {
		t.Fatalf("unmapped token under safeTeam must still be zero, got %+v", caps)
	}
}
