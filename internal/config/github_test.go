package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseGitHubRemoteURL(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		ok          bool
	}{
		{"git@" + "github.com:acme/app.git", "acme", "app", true},
		{"https://github.com/acme/app.git", "acme", "app", true},
		{"https://github.com/acme/app", "acme", "app", true},
		{"ssh://git@" + "github.com/acme/app.git", "acme", "app", true},
		{"https://gitlab.com/acme/app", "", "", false},
	}
	for _, tc := range cases {
		r, ok := ParseGitHubRemoteURL(tc.in)
		if ok != tc.ok || r.Owner != tc.owner || r.Repo != tc.repo {
			t.Fatalf("%q → %+v ok=%v want %s/%s ok=%v", tc.in, r, ok, tc.owner, tc.repo, tc.ok)
		}
	}
}

func TestProjectRepoCatalogConfigured(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {
				Path: "/x",
				GitHub: &ProjectGitHubConfig{
					Repos: []GitHubRepoRef{{Owner: "acme", Repo: "app"}, {Owner: "acme", Repo: "api"}},
				},
			},
		},
	}
	repos, err := cfg.ProjectRepoCatalogWith(context.Background(), "app", nil)
	if err != nil || len(repos) != 2 {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
	if repos[1].Slug() != "acme/api" {
		t.Fatalf("%+v", repos)
	}
}

func TestProjectRepoCatalogLegacyAndDiscover(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"leg": {Path: "/leg", GitHub: &ProjectGitHubConfig{Owner: "o", Repo: "r"}},
			"disc": {Path: "/disc"},
		},
	}
	repos, err := cfg.ProjectRepoCatalogWith(context.Background(), "leg", nil)
	if err != nil || len(repos) != 1 || repos[0].Slug() != "o/r" {
		t.Fatalf("legacy=%v err=%v", repos, err)
	}
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "remote get-url origin":
			return []byte("git@" + "github.com:disc/main.git\n"), nil
		case joined == "remote":
			return []byte("origin\nupstream\n"), nil
		case joined == "remote get-url upstream":
			return []byte("https://github.com/disc/other.git\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}
	repos, err = cfg.ProjectRepoCatalogWith(context.Background(), "disc", run)
	if err != nil || len(repos) != 2 {
		t.Fatalf("discover=%v err=%v", repos, err)
	}
	if repos[0].Slug() != "disc/main" || repos[1].Slug() != "disc/other" {
		t.Fatalf("%+v", repos)
	}
}

func TestResolveRepoPicker(t *testing.T) {
	cat := []GitHubRepoRef{{Owner: "a", Repo: "one"}, {Owner: "a", Repo: "two"}}
	r, err := ResolveRepoPicker(cat, "", "")
	if err != nil || r.Slug() != "a/one" {
		t.Fatalf("default %+v %v", r, err)
	}
	r, err = ResolveRepoPicker(cat, "a", "two")
	if err != nil || r.Slug() != "a/two" {
		t.Fatalf("pick %+v %v", r, err)
	}
	r, err = ResolveRepoPicker(cat, "", "a/two")
	if err != nil || r.Slug() != "a/two" {
		t.Fatalf("slug form %+v %v", r, err)
	}
	if _, err := ResolveRepoPicker(cat, "x", "y"); err == nil {
		t.Fatal("expected error for unknown")
	}
}

func TestPreferDiscordChannel(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {Path: "/a", DiscordChannelID: "ch-pref"},
			"api": {Path: "/b"},
			"multi": {Path: "/c"},
		},
		Channels: map[string]string{
			"ch-pref": "app",
			"ch-only": "api",
			"ch-m1":   "multi",
			"ch-m2":   "multi",
		},
	}
	id, err := cfg.PreferDiscordChannel("app")
	if err != nil || id != "ch-pref" {
		t.Fatalf("pref %q %v", id, err)
	}
	id, err = cfg.PreferDiscordChannel("api")
	if err != nil || id != "ch-only" {
		t.Fatalf("unique reverse %q %v", id, err)
	}
	if _, err := cfg.PreferDiscordChannel("multi"); err == nil {
		t.Fatal("expected multi-channel error")
	}
	cfg.Projects["app"] = ProjectConfig{Path: "/a", DiscordChannelID: "not-mapped"}
	if _, err := cfg.PreferDiscordChannel("app"); err == nil {
		t.Fatal("expected invalid preferred")
	}
	cfg.Projects["app"] = ProjectConfig{Path: "/a", DiscordChannelID: "ch-only"} // mapped to api
	if _, err := cfg.PreferDiscordChannel("app"); err == nil {
		t.Fatal("expected wrong project mapping")
	}
}

func TestValidatePreferredChannelsOnLoad(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "p")
	_ = os.MkdirAll(proj, 0o755)
	cfgPath := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(map[string]any{
		"discordToken":   "tok",
		"allowedUserIds": []string{"u"},
		"channels":       map[string]string{"ch1": "p"},
		"projects": map[string]any{
			"p": map[string]any{
				"path":             proj,
				"discordChannelId": "bad",
			},
		},
	})
	_ = os.WriteFile(cfgPath, raw, 0o600)
	t.Setenv("GROK_WORK_CONFIG", "")
	t.Setenv("GROK_DISCORD_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected load fail on bad preferred channel")
	}
	// fix and load
	raw, _ = json.Marshal(map[string]any{
		"discordToken":   "tok",
		"allowedUserIds": []string{"u"},
		"channels":       map[string]string{"ch1": "p"},
		"projects": map[string]any{
			"p": map[string]any{
				"path":             proj,
				"discordChannelId": "ch1",
				"github": map[string]any{
					"repos": []map[string]string{{"owner": "o", "repo": "r"}},
				},
			},
		},
	})
	_ = os.WriteFile(cfgPath, raw, 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	repos, err := cfg.ProjectRepoCatalog(context.Background(), "p")
	if err != nil || len(repos) != 1 || repos[0].Slug() != "o/r" {
		t.Fatalf("%v %v", repos, err)
	}
}

// TestCatalogCacheNoRace exercises concurrent discover cache reads and
// SetProjectGitHubRepos invalidation (run with -race).
func TestCatalogCacheNoRace(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"disc": {Path: "/disc"},
		},
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		DataDir:    t.TempDir(),
	}
	// Minimal file so Save works.
	if err := os.WriteFile(cfg.ConfigPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Allow saveLocked to write known schema fields.
	cfg.DiscordToken = "tok"
	cfg.AllowedUserIDs = []string{}
	cfg.Channels = map[string]string{}

	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch joined {
		case "remote get-url origin":
			return []byte("git@" + "github.com:disc/main.git\n"), nil
		case "remote":
			return []byte("origin\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = cfg.ProjectRepoCatalogWith(context.Background(), "disc", run)
		}()
		go func() {
			defer wg.Done()
			_ = cfg.SetProjectGitHubRepos("disc", []GitHubRepoRef{{Owner: "disc", Repo: "main"}})
			_ = cfg.SetProjectGitHubRepos("disc", nil) // back to discover
		}()
	}
	wg.Wait()
}

func TestSetProjectGitHubAndChannel(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "p")
	_ = os.MkdirAll(proj, 0o755)
	cfgPath := filepath.Join(dir, "config.json")
	cfg := &Config{
		DiscordToken:   "tok",
		AllowedUserIDs: []string{"u"},
		AllowedUsers:   map[string]struct{}{"u": {}},
		Projects:       PathProjects(map[string]string{"p": proj}),
		Channels:       map[string]string{"ch1": "p"},
		ConfigPath:     cfgPath,
		DataDir:        filepath.Join(dir, "data"),
	}
	if err := cfg.SetProjectGitHubRepos("p", []GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDiscordChannel("p", "ch1"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDiscordChannel("p", "nope"); err == nil {
		t.Fatal("expected bad channel")
	}
	if err := cfg.SetDiscordGuildID("guild-9"); err != nil {
		t.Fatal(err)
	}
	snap := cfg.Snapshot()
	if snap.DiscordGuildID != "guild-9" {
		t.Fatalf("guild=%q", snap.DiscordGuildID)
	}
	if snap.Projects[0].GitHubReposText != "acme/app" {
		t.Fatalf("repos text=%q", snap.Projects[0].GitHubReposText)
	}
	if snap.Projects[0].DiscordChannelID != "ch1" {
		t.Fatalf("channel=%q", snap.Projects[0].DiscordChannelID)
	}
	// Project guild overrides global default.
	if got := cfg.ProjectDiscordGuildID("p"); got != "guild-9" {
		t.Fatalf("fallback project guild=%q", got)
	}
	if err := cfg.SetProjectDiscord("p", "ch1", "guild-project"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectDiscordGuildID("p"); got != "guild-project" {
		t.Fatalf("project guild=%q", got)
	}
	snap = cfg.Snapshot()
	if snap.Projects[0].DiscordGuildID != "guild-project" {
		t.Fatalf("snap project guild=%q", snap.Projects[0].DiscordGuildID)
	}
	// Unknown project → global fallback.
	if got := cfg.ProjectDiscordGuildID("missing"); got != "guild-9" {
		t.Fatalf("missing project guild=%q", got)
	}
}
