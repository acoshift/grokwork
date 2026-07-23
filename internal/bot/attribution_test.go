package bot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

func TestBuildAttributionBlockMapped(t *testing.T) {
	in := AttributionInput{
		PrompterName: "Alice",
		PrompterID:   "42",
		ThreadURL:    "https://discord.com/channels/1/2",
		SessionID:    "sess-abc",
		GitHubLogin:  "alice-gh",
		GitHubName:   "Alice Example",
	}
	block := BuildAttributionBlock(in)
	t.Logf("mapped attribution block:\n%s", block)
	if p := os.Getenv("GROK_ATTR_EXAMPLES"); p != "" {
		_ = os.WriteFile(p, []byte("=== MAPPED ===\n"+block+"\n=== TRAILERS ===\n"+AttributionCommitTrailers(in)+"\n=== FOOTER ===\n"+AttributionPRFooterText(in)+"\n"), 0o600)
	}
	for _, want := range []string{
		"Prompter: Alice (Discord 42)",
		"GitHub: @alice-gh",
		"Thread: https://discord.com/channels/1/2",
		"Session: sess-abc",
		"Co-authored-by: Alice Example <42+alice-gh@users.noreply.github.com>",
		"Prompter-Discord: 42; Thread: https://discord.com/channels/1/2",
		"Requested via Grok Work",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("missing %q in:\n%s", want, block)
		}
	}
	// Footer text is reusable pure helper
	foot := AttributionPRFooterText(in)
	if !strings.Contains(foot, "@alice-gh") || !strings.Contains(foot, "Discord 42") {
		t.Fatalf("footer:\n%s", foot)
	}
	trail := AttributionCommitTrailers(in)
	if !strings.HasPrefix(trail, "Co-authored-by:") {
		t.Fatalf("trailers:\n%s", trail)
	}
	name, email := AttributionAuthorFields(in)
	if name != "Alice Example" || email != "42+alice-gh@users.noreply.github.com" {
		t.Fatalf("author %q <%q>", name, email)
	}
}

func TestBuildAttributionBlockUnmapped(t *testing.T) {
	in := AttributionInput{
		PrompterName: "Bob",
		PrompterID:   "99",
		ThreadURL:    "https://discord.com/x",
	}
	block := BuildAttributionBlock(in)
	t.Logf("unmapped attribution block:\n%s", block)
	if p := os.Getenv("GROK_ATTR_EXAMPLES"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString("=== UNMAPPED ===\n" + block + "\n=== FOOTER UNMAPPED ===\n" + AttributionPRFooterText(in) + "\n")
			_ = f.Close()
		}
	}
	if !strings.Contains(block, "Prompter: Bob (Discord 99)") {
		t.Fatalf("missing prompter:\n%s", block)
	}
	if !strings.Contains(block, "Thread: https://discord.com/x") {
		t.Fatalf("missing thread:\n%s", block)
	}
	// Must not invent a GitHub @login
	if strings.Contains(block, "GitHub: @") {
		t.Fatalf("unmapped must not invent @login:\n%s", block)
	}
	if strings.Contains(block, "Co-authored-by:") {
		t.Fatalf("unmapped must not Co-authored-by:\n%s", block)
	}
	if !strings.Contains(block, "Prompter-Discord: 99") {
		t.Fatalf("missing discord trailer:\n%s", block)
	}
	foot := AttributionPRFooterText(in)
	if strings.Contains(foot, "GitHub:") {
		t.Fatalf("footer github: %s", foot)
	}
	name, email := AttributionAuthorFields(in)
	if name != "" || email != "" {
		t.Fatalf("author should be empty: %q %q", name, email)
	}
}

func TestAttributionFooterBackwardCompat(t *testing.T) {
	// Old call site shape still produces Discord attribution.
	p := attributionFooter("bob", "42", "https://discord.com/x")
	if !strings.Contains(p, "Prompter: bob") || !strings.Contains(p, "42") {
		t.Fatalf("%s", p)
	}
}

func TestAttributionInShipPrefixMapped(t *testing.T) {
	in := AttributionInput{
		PrompterName: "bob",
		PrompterID:   "42",
		ThreadURL:    "https://discord.com/x",
		GitHubLogin:  "bobdev",
	}
	p := remoteWorkPromptPrefix("grok/discord/1") + BuildAttributionBlock(in)
	if !strings.Contains(p, "gh pr create") {
		t.Fatal("missing ship contract")
	}
	if !strings.Contains(p, "@bobdev") || !strings.Contains(p, "Co-authored-by:") {
		t.Fatalf("missing map attribution:\n%s", p)
	}
}

// TestLookupAndBuildEndToEnd drives config map → lookup → BuildAttributionBlock
// (the real path executeTask uses), without re-implementing string rules.
func TestLookupAndBuildEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+dir+`", "allowedUserIds": ["u1"]}},
  "channels": {},
  "grokBin": "grok"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
	cfg.DataDir = dir
	if err := cfg.SetGitHubIdentity("42", config.GitHubIdentity{Login: "alice-gh", Name: "Alice"}); err != nil {
		t.Fatal(err)
	}
	// Round-trip
	gh, ok := cfg.LookupGitHubIdentity("42")
	if !ok || gh.Login != "alice-gh" {
		t.Fatalf("lookup: ok=%v %+v", ok, gh)
	}
	if _, ok := cfg.LookupGitHubIdentity("nope"); ok {
		t.Fatal("unexpected map hit")
	}
	in := AttributionInput{
		PrompterName: "Alice",
		PrompterID:   "42",
		ThreadURL:    "https://discord.com/channels/g/t",
		SessionID:    "s1",
		GitHubLogin:  gh.Login,
		GitHubName:   gh.Name,
		GitHubEmail:  gh.Email,
	}
	block := BuildAttributionBlock(in)
	if !strings.Contains(block, "@alice-gh") || !strings.Contains(block, "Co-authored-by: Alice <") {
		t.Fatalf("block:\n%s", block)
	}
	// Reload from disk
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var again config.Config
	if err := json.Unmarshal(raw2, &again); err != nil {
		t.Fatal(err)
	}
	if again.DiscordUserGitHub == nil || again.DiscordUserGitHub["42"].Login != "alice-gh" {
		t.Fatalf("save lost map: %+v", again.DiscordUserGitHub)
	}
}
