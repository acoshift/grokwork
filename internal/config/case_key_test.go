package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// TestCaseKeyOverrideMatchesMintedPrefix pins config's stored override against
// the prefix sessionstore would actually mint from it. config does not import
// sessionstore in production code — deriving a prefix is not config's job — so
// the two length caps could otherwise drift apart silently, and the settings
// page would promise a prefix the ids never use.
func TestCaseKeyOverrideMatchesMintedPrefix(t *testing.T) {
	for _, in := range []string{"S", "SHOP", "API2", strings.Repeat("A", 10)} {
		if !caseKeyOverridePattern.MatchString(in) {
			t.Fatalf("%q should be a legal override", in)
		}
		if got := sessionstore.CaseKeyPrefix(in); got != strings.ToUpper(in) {
			t.Errorf("override %q would mint as %q — stored and minted must agree", in, got)
		}
	}
	// One character past the cap must be refused rather than silently truncated.
	if caseKeyOverridePattern.MatchString(strings.Repeat("A", 11)) {
		t.Fatal("an 11-character override must not be storable")
	}
}

func TestProjectCaseKeyOverride(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{"webapp": {Path: filepath.Join(dir, "webapp")}},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	// Unset means "derive from the project name" — config says nothing, and
	// sessionstore.CaseKeyPrefix does the deriving.
	if got := cfg.ProjectCaseKey("webapp"); got != "" {
		t.Fatalf("default override = %q, want empty", got)
	}
	if got := cfg.ProjectCaseKey("missing"); got != "" {
		t.Fatalf("unknown project = %q, want empty", got)
	}

	if err := cfg.SetProjectCaseKey("webapp", "shop"); err != nil {
		t.Fatal(err)
	}
	// Stored uppercased, so what the settings page shows is what keys will read.
	if got := cfg.ProjectCaseKey("webapp"); got != "SHOP" {
		t.Fatalf("override = %q, want SHOP", got)
	}
	if got := cfg.Snapshot(); len(got.Projects) == 0 || got.Projects[0].CaseKey != "SHOP" {
		t.Fatalf("snapshot did not carry the override: %+v", got.Projects)
	}

	// Anything that would not survive being typed into a URL or a commit
	// message is refused at the point of saving, not silently mangled later.
	// "supportdeskportal" is the length case: stored whole it would mint as
	// SUPPORTDES-1, so the settings page and the ids would disagree forever.
	for _, bad := range []string{"my shop", "shop-1", "1shop", "shop!", "ผู้ใช้", "supportdeskportal"} {
		if err := cfg.SetProjectCaseKey("webapp", bad); err == nil {
			t.Errorf("SetProjectCaseKey(%q) should have been refused", bad)
		}
	}
	if got := cfg.ProjectCaseKey("webapp"); got != "SHOP" {
		t.Fatalf("a refused save must not change the stored value, got %q", got)
	}

	// Empty clears it.
	if err := cfg.SetProjectCaseKey("webapp", ""); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectCaseKey("webapp"); got != "" {
		t.Fatalf("cleared override = %q, want empty", got)
	}
	if err := cfg.SetProjectCaseKey("nope", "X"); err == nil {
		t.Fatal("unknown project should be refused")
	}
}
