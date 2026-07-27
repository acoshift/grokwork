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
				Path:           "/tmp/app",
				SafeTeamMode:   &on,
				AllowedUserIDs: []string{"u1"},
			},
		},
	}
	caps := cfg.ResolveCapabilities("app", "u1")
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
	caps := cfg.ResolveCapabilities("app", "u1")
	if !caps.CanShip() {
		t.Fatalf("legacy default builder: %+v", caps)
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
			Teams: map[string]TeamConfig{
				"support": {Label: "Support", Members: []string{"discord:9"}, Capabilities: "investigator"},
			},
			InvestigateTools: "read_file,grep",
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
	if tm := pc.Teams["support"]; tm.Capabilities != "investigator" || len(tm.Members) != 1 {
		t.Fatalf("team lost in round trip: %+v", pc.Teams)
	}
	// clone
	m3 := cloneProjectsMap(m)
	if m3["app"].Teams["support"].Label != "Support" {
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
	caps := cfg.ResolveCapabilities("p", "u")
	if !caps.CanShip() {
		t.Fatalf("loaded builder map: %+v", caps)
	}
	if !cfg.SafeTeamMode("p") {
		t.Fatal("SafeTeamMode not loaded")
	}
}

func TestCanInvestigateShell(t *testing.T) {
	if !BuiltinCapabilityTemplates["investigator"].CanInvestigateShell() {
		t.Fatal("investigator must get diagnostic shell (psql, …)")
	}
	if !BuiltinCapabilityTemplates["operator"].CanInvestigateShell() {
		t.Fatal("operator has Investigate and must get diagnostic shell")
	}
	if !BuiltinCapabilityTemplates["builder"].CanInvestigateShell() {
		t.Fatal("builder must get investigate shell")
	}
	if !BuiltinCapabilityTemplates["admin"].CanInvestigateShell() {
		t.Fatal("admin must get investigate shell")
	}
	if !(Capabilities{Investigate: true}).CanInvestigateShell() {
		t.Fatal("Investigate alone must unlock investigate shell")
	}
	if !(Capabilities{SafeOps: true}).CanInvestigateShell() {
		t.Fatal("SafeOps alone must unlock investigate shell")
	}
	if (Capabilities{}).CanInvestigateShell() {
		t.Fatal("zero caps must not unlock shell")
	}
}
