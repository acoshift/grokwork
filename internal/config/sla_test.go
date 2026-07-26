package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func minutesPtr(n int) *int { return &n }

func slaTestConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Config{
		DiscordToken: "tok",
		Projects:     ProjectsMap{"app": {Path: proj}},
		Channels:     map[string]string{"c1": "app"},
		GrokBin:      "grok",
		ConfigPath:   filepath.Join(dir, "config.json"),
		DataDir:      dir,
	}
}

// An unset target must read as "no SLA", never as a deadline in the past — the
// whole reason SLATarget uses pointers.
func TestProjectSLAUnsetMeansNoTarget(t *testing.T) {
	c := slaTestConfig(t)
	if first, res := c.ProjectSLA("app", "critical"); first != 0 || res != 0 {
		t.Fatalf("no table: %v %v", first, res)
	}

	pc := c.Projects["app"]
	pc.SLA = map[string]SLATarget{
		"critical": {FirstResponseMinutes: minutesPtr(60), ResolutionMinutes: minutesPtr(480)},
		// Configured explicitly to zero: still not a deadline, but it survives a
		// round-trip as an explicit value rather than vanishing.
		"high": {FirstResponseMinutes: minutesPtr(0)},
		// Only one of the two clocks set.
		"medium": {ResolutionMinutes: minutesPtr(1440)},
	}
	c.Projects["app"] = pc

	for _, tc := range []struct {
		severity   string
		first, res time.Duration
	}{
		{"critical", time.Hour, 8 * time.Hour},
		{"CRITICAL", time.Hour, 8 * time.Hour}, // severity is case-insensitive
		{"high", 0, 0},
		{"medium", 0, 24 * time.Hour},
		{"low", 0, 0},   // severity absent from the table
		{"", 0, 0},      // case with no severity
		{"bogus", 0, 0}, // never a target for a severity that cannot exist
	} {
		first, res := c.ProjectSLA("app", tc.severity)
		if first != tc.first || res != tc.res {
			t.Fatalf("%q: got %v/%v want %v/%v", tc.severity, first, res, tc.first, tc.res)
		}
	}
	if first, res := c.ProjectSLA("other", "critical"); first != 0 || res != 0 {
		t.Fatalf("unknown project: %v %v", first, res)
	}
	var nilCfg *Config
	if first, res := nilCfg.ProjectSLA("app", "critical"); first != 0 || res != 0 {
		t.Fatalf("nil config: %v %v", first, res)
	}
}

// A severity typo is a load failure, not a silently ignored row: an SLA keyed to
// "sev1" applies to nothing, and the symptom is a board that simply never badges
// anything — indistinguishable from nothing being late.
func TestSLAUnknownSeverityIsALoadError(t *testing.T) {
	var m ProjectsMap
	err := json.Unmarshal([]byte(`{"app":{"path":"/tmp","sla":{"sev1":{"firstResponseMinutes":60}}}}`), &m)
	if err == nil {
		t.Fatal("unknown severity accepted")
	}
	if got := err.Error(); !strings.Contains(got, "sev1") || !strings.Contains(got, "unknown severity") {
		t.Fatalf("error should name the bad key: %v", err)
	}

	// Keys are normalized, so hand-written "Critical" still resolves.
	if err := json.Unmarshal([]byte(`{"app":{"path":"/tmp","sla":{"Critical":{"resolutionMinutes":30}}}}`), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["app"].SLA["critical"]; !ok {
		t.Fatalf("severity key not normalized: %+v", m["app"].SLA)
	}

	// Two spellings of one severity carry two different policies; picking one
	// would be a coin flip, so refuse (same stance as colliding team keys).
	if err := json.Unmarshal([]byte(`{"app":{"path":"/tmp","sla":{"high":{"resolutionMinutes":30},"HIGH":{"resolutionMinutes":90}}}}`), &m); err == nil {
		t.Fatal("duplicate severity accepted")
	}
}

// A config write must not drop the SLA table. ProjectsMap has a hand-written
// MarshalJSON plus a hand-written clone, and a field missing from either is
// deleted from config.json by the next unrelated save — exactly what happened to
// caseKey.
func TestSLASurvivesSaveAndClone(t *testing.T) {
	c := slaTestConfig(t)
	if err := c.SetProjectSLA("app", map[string]SLATarget{
		"critical": {FirstResponseMinutes: minutesPtr(60), ResolutionMinutes: minutesPtr(480)},
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatalf("reload: %v (%s)", err, raw)
	}
	first, res := again.ProjectSLA("app", "critical")
	if first != time.Hour || res != 8*time.Hour {
		t.Fatalf("targets lost through save: %v %v\n%s", first, res, raw)
	}

	// Snapshot (what the settings form renders) keeps every severity as a row.
	snap := c.Snapshot()
	var item ProjectItem
	for _, p := range snap.Projects {
		if p.Name == "app" {
			item = p
		}
	}
	if len(item.SLA) != len(SLASeverities) || !item.SLAConfigured {
		t.Fatalf("snapshot rows=%+v configured=%v", item.SLA, item.SLAConfigured)
	}
	for _, row := range item.SLA {
		if row.Severity == "critical" {
			if row.FirstResponse != "60" || row.Resolution != "480" {
				t.Fatalf("critical row = %+v", row)
			}
			continue
		}
		if row.FirstResponse != "" || row.Resolution != "" {
			t.Fatalf("unset row must render empty, not zero: %+v", row)
		}
	}

	// Cloning the map must not hand out the caller's pointers.
	cloned := cloneSLA(c.Projects["app"].SLA)
	if cloned["critical"].FirstResponseMinutes == c.Projects["app"].SLA["critical"].FirstResponseMinutes {
		t.Fatal("cloneSLA shares its minute pointers with the original")
	}
}

func TestSetProjectSLAClearsAndValidates(t *testing.T) {
	c := slaTestConfig(t)
	if err := c.SetProjectSLA("app", map[string]SLATarget{
		"high": {FirstResponseMinutes: minutesPtr(240)},
	}); err != nil {
		t.Fatal(err)
	}
	// A row with both clocks cleared is dropped, not stored as an empty object.
	if err := c.SetProjectSLA("app", map[string]SLATarget{"high": {}}); err != nil {
		t.Fatal(err)
	}
	if got := c.Projects["app"].SLA; got != nil {
		t.Fatalf("cleared table = %+v", got)
	}
	if err := c.SetProjectSLA("app", map[string]SLATarget{"nope": {ResolutionMinutes: minutesPtr(5)}}); err == nil {
		t.Fatal("unknown severity accepted by the setter")
	}
	if err := c.SetProjectSLA("app", map[string]SLATarget{"low": {ResolutionMinutes: minutesPtr(-5)}}); err == nil {
		t.Fatal("negative minutes accepted")
	}
	if err := c.SetProjectSLA("missing", map[string]SLATarget{}); err == nil {
		t.Fatal("unknown project accepted")
	}
}

// An empty form field is "no target", not zero: clearing a box has to remove the
// deadline rather than set an impossible one.
func TestParseSLAMinutes(t *testing.T) {
	got, err := ParseSLAMinutes("  ")
	if err != nil || got != nil {
		t.Fatalf("empty = %v %v", got, err)
	}
	got, err = ParseSLAMinutes(" 90 ")
	if err != nil || got == nil || *got != 90 {
		t.Fatalf("90 = %v %v", got, err)
	}
	if _, err := ParseSLAMinutes("soon"); err == nil {
		t.Fatal("non-numeric accepted")
	}
	if _, err := ParseSLAMinutes("-1"); err == nil {
		t.Fatal("negative accepted")
	}
}
