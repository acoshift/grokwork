package config

import (
	"encoding/json"
	"testing"
)

func TestResolveCapabilitiesSafeTeamUnmapped(t *testing.T) {
	on := true
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {
				Path:         "/tmp/app",
				SafeTeamMode: &on,
				AllowedUserIDs: []string{"u1"},
			},
		},
	}
	caps := cfg.ResolveCapabilities("app", "u1", nil)
	if caps.CanShip() {
		t.Fatalf("unmapped under SafeTeamMode must not ship: %+v", caps)
	}
	if !caps.Investigate {
		t.Fatalf("expected investigator: %+v", caps)
	}
}

func TestResolveCapabilitiesBuilderWhenSafeOff(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {Path: "/tmp/app", AllowedUserIDs: []string{"u1"}},
		},
	}
	caps := cfg.ResolveCapabilities("app", "u1", nil)
	if !caps.CanShip() {
		t.Fatalf("legacy default builder: %+v", caps)
	}
}

func TestResolveCapabilitiesByRole(t *testing.T) {
	on := true
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {
				Path:         "/tmp/app",
				SafeTeamMode: &on,
				CapabilityByRole: map[string]string{
					"role-eng": "builder",
				},
			},
		},
	}
	caps := cfg.ResolveCapabilities("app", "u1", []string{"role-eng"})
	if !caps.CanShip() {
		t.Fatalf("mapped eng role: %+v", caps)
	}
}

func TestProjectConfigCapabilityMarshalRoundTrip(t *testing.T) {
	on := true
	m := ProjectsMap{
		"app": {
			Path:                    "/repos/app",
			SafeTeamMode:            &on,
			SafeTeamDefaultTemplate: "investigator",
			DefaultMode:             "investigate",
			CapabilityByUser:        map[string]string{"u1": "builder"},
			CapabilityByRole:        map[string]string{"r1": "investigator"},
			InvestigateTools:        "read_file,grep",
			CapabilityTemplates: map[string]Capabilities{
				"custom": {Investigate: true, StartSessions: true, GithubWrites: true},
			},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var m2 ProjectsMap
	if err := json.Unmarshal(raw, &m2); err != nil {
		t.Fatal(err)
	}
	pc := m2["app"]
	if pc.SafeTeamMode == nil || !*pc.SafeTeamMode {
		t.Fatalf("SafeTeamMode lost: %+v", pc)
	}
	if pc.DefaultMode != "investigate" || pc.CapabilityByUser["u1"] != "builder" {
		t.Fatalf("fields lost: %+v", pc)
	}
	if !pc.CapabilityTemplates["custom"].GithubWrites {
		t.Fatalf("templates lost: %+v", pc.CapabilityTemplates)
	}
	// clone
	m3 := cloneProjectsMap(m)
	if m3["app"].CapabilityByRole["r1"] != "investigator" {
		t.Fatalf("clone failed: %+v", m3["app"])
	}
}

func TestConfigFileRoundTripCapabilities(t *testing.T) {
	// ProjectsMap JSON round-trip is the critical path for web config save.
	on := true
	m := ProjectsMap{
		"p": {
			Path:             "/tmp/p",
			SafeTeamMode:     &on,
			CapabilityByUser: map[string]string{"u": "builder"},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var m2 ProjectsMap
	if err := json.Unmarshal(raw, &m2); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Projects: m2}
	caps := cfg.ResolveCapabilities("p", "u", nil)
	if !caps.CanShip() {
		t.Fatalf("loaded builder map: %+v", caps)
	}
	if !cfg.SafeTeamMode("p") {
		t.Fatal("SafeTeamMode not loaded")
	}
}
