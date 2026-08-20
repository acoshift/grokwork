package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListDiscoversUserAndProjectSkills(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()

	writeSkill(t, filepath.Join(home, ".grok", "skills", "alpha"), "alpha", "Alpha skill for tests.")
	writeSkill(t, filepath.Join(home, ".claude", "skills", "beta"), "beta", "Beta from claude.")
	// Same real file via two names would collapse — here two distinct skills.
	writeSkill(t, filepath.Join(proj, ".grok", "skills", "gamma"), "gamma", "Project gamma.")
	// Symlink skill under grok that points at claude's beta → one entry.
	if err := os.Symlink(
		filepath.Join(home, ".claude", "skills", "beta"),
		filepath.Join(home, ".grok", "skills", "beta-link"),
	); err != nil {
		t.Fatal(err)
	}

	// Extra path from config.toml
	extra := filepath.Join(home, "extra-skills", "delta")
	writeSkill(t, extra, "delta", "From config paths.")
	writeFile(t, filepath.Join(home, ".grok", "config.toml"), `
[skills]
paths = ["`+filepath.Join(home, "extra-skills")+`"]
`)

	got := List(ListOpts{
		Home:     home,
		Projects: map[string]string{"app": proj},
	})
	byName := map[string]Info{}
	for _, sk := range got {
		byName[sk.Name] = sk
	}
	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("missing skill %q in %#v", name, names(got))
		}
	}
	if byName["alpha"].Source != "user (grok)" {
		t.Fatalf("alpha source=%q", byName["alpha"].Source)
	}
	if byName["gamma"].Source != "project:app" {
		t.Fatalf("gamma source=%q", byName["gamma"].Source)
	}
	if byName["delta"].Source != "config path" {
		t.Fatalf("delta source=%q", byName["delta"].Source)
	}
	// Symlink collapsed with claude beta — only one of beta / beta-link.
	nBeta := 0
	for _, sk := range got {
		if sk.Name == "beta" || sk.Name == "beta-link" {
			nBeta++
		}
	}
	if nBeta != 1 {
		t.Fatalf("expected one realpath for beta skill, got %d", nBeta)
	}
}

func TestListDiscoversClaudeInstalledPluginSkills(t *testing.T) {
	home := t.TempDir()

	pluginRoot := filepath.Join(home, ".claude", "plugins", "cache", "mkt", "design", "1.0.0")
	writeSkill(t, filepath.Join(pluginRoot, "skills", "frontend-design"), "frontend-design", "Distinctive UI.")

	// Same marketplace clone has a skill that is NOT in installed_plugins.json.
	market := filepath.Join(home, ".claude", "plugins", "marketplaces", "mkt", "plugins", "example-plugin")
	writeSkill(t, filepath.Join(market, "skills", "example-skill"), "example-skill", "Catalog only.")

	// A leftover cache plugin with no index entry must stay hidden.
	stale := filepath.Join(home, ".claude", "plugins", "cache", "mkt", "stale", "1.0.0")
	writeSkill(t, filepath.Join(stale, "skills", "stale-skill"), "stale-skill", "Uninstalled leftover.")

	writeFile(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), `{
  "version": 2,
  "plugins": {
    "frontend-design@mkt": [
      {
        "scope": "user",
        "installPath": "`+pluginRoot+`"
      }
    ],
    "empty-plugin@mkt": [
      {
        "scope": "user",
        "installPath": "`+filepath.Join(home, ".claude", "plugins", "cache", "mkt", "empty", "1.0.0")+`"
      }
    ]
  }
}
`)

	got := List(ListOpts{Home: home})
	byName := map[string]Info{}
	for _, sk := range got {
		byName[sk.Name] = sk
	}
	if sk, ok := byName["frontend-design"]; !ok {
		t.Fatalf("missing plugin skill, got %v", names(got))
	} else if sk.Source != "plugin:frontend-design" {
		t.Fatalf("source=%q", sk.Source)
	}
	for _, name := range []string{"example-skill", "stale-skill"} {
		if _, ok := byName[name]; ok {
			t.Fatalf("uninstalled plugin skill %q should not appear in %v", name, names(got))
		}
	}
}

func TestListClaudePluginSkillsCollapseWithUserCopy(t *testing.T) {
	home := t.TempDir()
	pluginRoot := filepath.Join(home, ".claude", "plugins", "cache", "mkt", "design", "1.0.0")
	skillDir := filepath.Join(pluginRoot, "skills", "frontend-design")
	writeSkill(t, skillDir, "frontend-design", "Distinctive UI.")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(skillDir, filepath.Join(home, ".claude", "skills", "frontend-design")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), `{
  "plugins": {
    "frontend-design@mkt": [{"installPath": "`+pluginRoot+`"}]
  }
}
`)

	got := List(ListOpts{Home: home})
	n := 0
	for _, sk := range got {
		if sk.Name == "frontend-design" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected one frontend-design after realpath collapse, got %d in %v", n, names(got))
	}
}

func TestClaudeInstalledPluginSkillsSkipsBadJSON(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), `{not json`)
	if got := claudeInstalledPluginSkills(home); got != nil {
		t.Fatalf("got %#v", got)
	}
}

func TestParseFrontmatter(t *testing.T) {
	name, desc := parseFrontmatter(`---
name: scrutinize
description: Outsider review. Trigger on /scrutinize.
when-to-use: /scrutinize
---

# Scrutinize
body
`)
	if name != "scrutinize" {
		t.Fatalf("name=%q", name)
	}
	if !strings.Contains(desc, "Outsider review") {
		t.Fatalf("desc=%q", desc)
	}
}

func TestParseTOMLStringArray(t *testing.T) {
	got := parseTOMLStringArray(`["~/a", "~/b"]`)
	if len(got) != 2 || got[0] != "~/a" || got[1] != "~/b" {
		t.Fatalf("%v", got)
	}
}

func writeSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# " + name + "\n"
	writeFile(t, filepath.Join(dir, "SKILL.md"), body)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(in []Info) []string {
	out := make([]string, len(in))
	for i, sk := range in {
		out[i] = sk.Name + "@" + sk.Source
	}
	return out
}
