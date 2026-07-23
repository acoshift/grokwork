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
		"Prompter: Alice",
		"GitHub: @alice-gh",
		"Session: sess-abc",
		"Co-authored-by: Alice Example <42+alice-gh@users.noreply.github.com>",
		"Prompter: Alice",
		"Requested via Grok Work",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("missing %q in:\n%s", want, block)
		}
	}
	// Must not leak Discord id or thread jump link into ship text.
	for _, ban := range []string{
		"Discord 42",
		"(Discord",
		"Prompter-Discord",
		"Thread: https://discord.com",
		"https://discord.com/channels/1/2",
	} {
		if strings.Contains(block, ban) {
			t.Fatalf("must not contain %q in:\n%s", ban, block)
		}
	}
	// Footer text is reusable pure helper
	foot := AttributionPRFooterText(in)
	if !strings.Contains(foot, "@alice-gh") || !strings.Contains(foot, "Prompter: Alice") {
		t.Fatalf("footer:\n%s", foot)
	}
	if strings.Contains(foot, "Discord") || strings.Contains(foot, "Thread:") {
		t.Fatalf("footer leaked Discord fields:\n%s", foot)
	}
	trail := AttributionCommitTrailers(in)
	if !strings.HasPrefix(trail, "Co-authored-by:") {
		t.Fatalf("trailers:\n%s", trail)
	}
	if strings.Contains(trail, "Prompter-Discord") || strings.Contains(trail, "Thread:") {
		t.Fatalf("trailers leaked Discord fields:\n%s", trail)
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
	if !strings.Contains(block, "Prompter: Bob") {
		t.Fatalf("missing prompter:\n%s", block)
	}
	// Must not invent a GitHub @login
	if strings.Contains(block, "GitHub: @") {
		t.Fatalf("unmapped must not invent @login:\n%s", block)
	}
	if strings.Contains(block, "Co-authored-by:") {
		t.Fatalf("unmapped must not Co-authored-by:\n%s", block)
	}
	for _, ban := range []string{
		"Discord 99",
		"Prompter-Discord",
		"Thread: https://discord.com",
		"https://discord.com/x",
	} {
		if strings.Contains(block, ban) {
			t.Fatalf("must not contain %q in:\n%s", ban, block)
		}
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

func TestOnBehalfOfCommentBodyMapped(t *testing.T) {
	got := OnBehalfOfCommentBody("42", "Alice", "alice-gh", "please merge")
	if !strings.HasPrefix(got, "On behalf of @alice-gh (Alice):\n\n") {
		t.Fatalf("prefix:\n%s", got)
	}
	if !strings.HasSuffix(got, "please merge") {
		t.Fatalf("body lost:\n%s", got)
	}
	if strings.Contains(got, "Discord") || strings.Contains(got, "42") {
		t.Fatalf("must not include Discord id:\n%s", got)
	}
	// @ stripped from login; no display name → bare @login
	got2 := OnBehalfOfCommentBody("9", "", "@bob", "x")
	if !strings.HasPrefix(got2, "On behalf of @bob:\n\n") {
		t.Fatalf("got2:\n%s", got2)
	}
	if strings.Contains(got2, "Discord") || strings.Contains(got2, "9") {
		t.Fatalf("got2 leaked id:\n%s", got2)
	}
}

func TestOnBehalfOfCommentBodyUnmapped(t *testing.T) {
	raw := "keep me"
	if got := OnBehalfOfCommentBody("42", "Alice", "", raw); got != raw {
		t.Fatalf("unmapped: %q", got)
	}
	if got := OnBehalfOfCommentBody("42", "Alice", "  ", raw); got != raw {
		t.Fatalf("blank login: %q", got)
	}
	if got := OnBehalfOfCommentBody("42", "Alice", "@", raw); got != raw {
		t.Fatalf("at-only: %q", got)
	}
}

func TestOnBehalfOfCommentBodyEmpty(t *testing.T) {
	if got := OnBehalfOfCommentBody("42", "Alice", "alice", ""); got != "" {
		t.Fatalf("empty: %q", got)
	}
	if got := OnBehalfOfCommentBody("42", "Alice", "alice", "   \n"); got != "   \n" {
		t.Fatalf("ws: %q", got)
	}
}

func TestAttributionFooterBackwardCompat(t *testing.T) {
	// Old call site still produces display-name attribution (no Discord id / thread URL).
	p := attributionFooter("bob", "42", "https://discord.com/x")
	if !strings.Contains(p, "Prompter: bob") {
		t.Fatalf("%s", p)
	}
	if strings.Contains(p, "42") || strings.Contains(p, "https://discord.com/x") {
		t.Fatalf("must not include Discord id or thread URL:\n%s", p)
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
	if strings.Contains(p, "https://discord.com/x") {
		t.Fatalf("thread URL leaked into ship prefix:\n%s", p)
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
	if strings.Contains(block, "https://discord.com/channels/g/t") || strings.Contains(block, "Discord 42") {
		t.Fatalf("leaked Discord fields:\n%s", block)
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
