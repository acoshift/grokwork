package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGitHubIdentityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+filepath.ToSlash(dir)+`", "allowedUserIds": ["u1"]}},
  "channels": {},
  "grokBin": "grok"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
	if err := cfg.SetGitHubIdentity("111", GitHubIdentity{Login: "@bob", Name: "Bob"}); err != nil {
		t.Fatal(err)
	}
	id, ok := cfg.LookupGitHubIdentity("111")
	if !ok || id.Login != "bob" || id.Name != "Bob" {
		t.Fatalf("lookup: ok=%v %+v", ok, id)
	}
	// saveLocked path already run by Set; re-read
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw2, &again); err != nil {
		t.Fatal(err)
	}
	if again.DiscordUserGitHub["111"].Login != "bob" {
		t.Fatalf("disk: %s", raw2)
	}
	// Also preserve other wave1 fields when map is set
	max := 3
	cfg.MaxConcurrentRuns = &max
	cfg.mu.Lock()
	err = cfg.saveLocked()
	cfg.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	raw3, _ := os.ReadFile(path)
	var third Config
	if err := json.Unmarshal(raw3, &third); err != nil {
		t.Fatal(err)
	}
	if third.DiscordUserGitHub["111"].Login != "bob" {
		t.Fatal("map wiped on second save")
	}
	if third.MaxConcurrentRuns == nil || *third.MaxConcurrentRuns != 3 {
		t.Fatal("max concurrent wiped")
	}
	if err := cfg.RemoveGitHubIdentity("111"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.LookupGitHubIdentity("111"); ok {
		t.Fatal("still mapped after remove")
	}
}

func TestNoreplyGitHubEmail(t *testing.T) {
	if got := NoreplyGitHubEmail("42", "alice"); got != "42+alice@users.noreply.github.com" {
		t.Fatal(got)
	}
	if got := NoreplyGitHubEmail("", "alice"); got != "alice@users.noreply.github.com" {
		t.Fatal(got)
	}
}
