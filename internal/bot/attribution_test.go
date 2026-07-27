package bot

import (
	"os"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/identity"
)

func TestBuildAttributionBlockLinked(t *testing.T) {
	in := AttributionInput{
		PrompterName:    "Alice",
		PrompterID:      "42",
		ThreadURL:       "https://discord.com/channels/1/2",
		SessionID:       "sess-abc",
		GitHubLogin:     "alice-gh",
		GitHubNumericID: "999",
	}
	block := BuildAttributionBlock(in)
	t.Logf("linked attribution block:\n%s", block)
	if p := os.Getenv("GROK_ATTR_EXAMPLES"); p != "" {
		_ = os.WriteFile(p, []byte("=== LINKED ===\n"+block+"\n=== TRAILERS ===\n"+AttributionCommitTrailers(in)+"\n=== FOOTER ===\n"+AttributionPRFooterText(in)+"\n"), 0o600)
	}
	for _, want := range []string{
		"Prompter: Alice",
		"GitHub: @alice-gh",
		"Session: sess-abc",
		// The email is built from GitHub's immutable numeric id, never from the
		// Discord snowflake the old config map used.
		"Co-authored-by: alice-gh <999+alice-gh@users.noreply.github.com>",
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
		"42+alice-gh",
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
	if name != "alice-gh" || email != "999+alice-gh@users.noreply.github.com" {
		t.Fatalf("author %q <%q>", name, email)
	}
}

func TestBuildAttributionBlockUnlinked(t *testing.T) {
	in := AttributionInput{
		PrompterName: "Bob",
		PrompterID:   "99",
		ThreadURL:    "https://discord.com/x",
	}
	block := BuildAttributionBlock(in)
	t.Logf("unlinked attribution block:\n%s", block)
	if p := os.Getenv("GROK_ATTR_EXAMPLES"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString("=== UNLINKED ===\n" + block + "\n=== FOOTER UNLINKED ===\n" + AttributionPRFooterText(in) + "\n")
			_ = f.Close()
		}
	}
	if !strings.Contains(block, "Prompter: Bob") {
		t.Fatalf("missing prompter:\n%s", block)
	}
	// Must not invent a GitHub @login
	if strings.Contains(block, "GitHub: @") {
		t.Fatalf("unlinked must not invent @login:\n%s", block)
	}
	if strings.Contains(block, "Co-authored-by:") {
		t.Fatalf("unlinked must not Co-authored-by:\n%s", block)
	}
	if strings.Contains(block, "users.noreply.github.com") {
		t.Fatalf("unlinked must not carry a noreply address at all:\n%s", block)
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

// A half-known identity is the dangerous case: a login with no numeric id (or
// the reverse) would build "+alice@users.noreply.github.com", which attributes
// to nobody while looking exactly like a working trailer.
func TestAttributionRefusesHalfKnownIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		login string
		id    string
	}{
		{"no numeric id", "alice-gh", ""},
		{"no login", "", "999"},
		{"blank numeric id", "alice-gh", "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := AttributionInput{PrompterName: "Alice", GitHubLogin: tc.login, GitHubNumericID: tc.id}
			name, email := AttributionAuthorFields(in)
			if name != "" || email != "" {
				t.Fatalf("author %q <%q> — a half-known identity must produce nothing", name, email)
			}
			if trail := AttributionCommitTrailers(in); strings.Contains(trail, "Co-authored-by:") {
				t.Fatalf("trailers:\n%s", trail)
			}
			if block := BuildAttributionBlock(in); strings.Contains(block, "users.noreply.github.com") {
				t.Fatalf("block leaked a malformed address:\n%s", block)
			}
		})
	}
}

func TestOnBehalfOfCommentBodyLinked(t *testing.T) {
	got := OnBehalfOfCommentBody("Alice", "alice-gh", "please merge")
	if !strings.HasPrefix(got, "On behalf of @alice-gh (Alice):\n\n") {
		t.Fatalf("prefix:\n%s", got)
	}
	if !strings.HasSuffix(got, "please merge") {
		t.Fatalf("body lost:\n%s", got)
	}
	if strings.Contains(got, "Discord") {
		t.Fatalf("must not name Discord:\n%s", got)
	}
	// @ stripped from login; no display name → bare @login
	got2 := OnBehalfOfCommentBody("", "@bob", "x")
	if !strings.HasPrefix(got2, "On behalf of @bob:\n\n") {
		t.Fatalf("got2:\n%s", got2)
	}
}

func TestOnBehalfOfCommentBodyUnlinked(t *testing.T) {
	raw := "keep me"
	if got := OnBehalfOfCommentBody("Alice", "", raw); got != raw {
		t.Fatalf("unlinked: %q", got)
	}
	if got := OnBehalfOfCommentBody("Alice", "  ", raw); got != raw {
		t.Fatalf("blank login: %q", got)
	}
	if got := OnBehalfOfCommentBody("Alice", "@", raw); got != raw {
		t.Fatalf("at-only: %q", got)
	}
}

func TestOnBehalfOfCommentBodyEmpty(t *testing.T) {
	if got := OnBehalfOfCommentBody("Alice", "alice", ""); got != "" {
		t.Fatalf("empty: %q", got)
	}
	if got := OnBehalfOfCommentBody("Alice", "alice", "   \n"); got != "   \n" {
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

func TestAttributionInShipPrefixLinked(t *testing.T) {
	in := AttributionInput{
		PrompterName:    "bob",
		PrompterID:      "42",
		ThreadURL:       "https://discord.com/x",
		GitHubLogin:     "bobdev",
		GitHubNumericID: "4242",
	}
	p := remoteWorkPromptPrefix("grok/discord/1") + BuildAttributionBlock(in)
	if !strings.Contains(p, "gh pr create") {
		t.Fatal("missing ship contract")
	}
	if !strings.Contains(p, "@bobdev") || !strings.Contains(p, "Co-authored-by:") {
		t.Fatalf("missing linked attribution:\n%s", p)
	}
	if strings.Contains(p, "https://discord.com/x") {
		t.Fatalf("thread URL leaked into ship prefix:\n%s", p)
	}
}

// TestLinkedIdentityToAttributionEndToEnd drives the real path executeTask now
// uses: account id → identity link → BuildAttributionBlock. It replaces the old
// config-map end-to-end test, which no longer has a map to drive.
func TestLinkedIdentityToAttributionEndToEnd(t *testing.T) {
	const account = "42"
	b := &Bot{}
	b.SetIdentity(linkedIdentity(t, account, "999", "alice-gh"))

	login, numericID, ok := b.githubIdentityFor(account)
	if !ok || login != "alice-gh" || numericID != "999" {
		t.Fatalf("resolve: %q %q ok=%v", login, numericID, ok)
	}
	if _, _, ok := b.githubIdentityFor("someone-else"); ok {
		t.Fatal("an unlinked account must resolve to nothing")
	}

	in := AttributionInput{
		PrompterName:    "Alice",
		PrompterID:      account,
		ThreadURL:       "https://discord.com/channels/g/t",
		SessionID:       "s1",
		GitHubLogin:     login,
		GitHubNumericID: numericID,
	}
	block := BuildAttributionBlock(in)
	if !strings.Contains(block, "@alice-gh") ||
		!strings.Contains(block, "Co-authored-by: alice-gh <999+alice-gh@users.noreply.github.com>") {
		t.Fatalf("block:\n%s", block)
	}
	if strings.Contains(block, "https://discord.com/channels/g/t") || strings.Contains(block, "Discord 42") {
		t.Fatalf("leaked Discord fields:\n%s", block)
	}
}

// The numeric id in the trailer is GitHub's immutable subject, taken from the
// alias key — NOT from the handle, which the person can rename at will. If a
// rename moved the address, every commit before it would stop attributing, and
// the freed handle's new owner could start receiving credit.
func TestTrailerNumericIDComesFromSubjectNotHandle(t *testing.T) {
	const account = "u-7"
	links := linkedIdentity(t, account, "999", "alice-gh")

	before := trailerFor(t, links, account)
	if !strings.Contains(before, "999+alice-gh@users.noreply.github.com") {
		t.Fatalf("before rename:\n%s", before)
	}
	// Same login, renamed on GitHub: the handle moves, the subject does not.
	if err := links.RefreshHandle("github:999", "alice-renamed"); err != nil {
		t.Fatal(err)
	}
	after := trailerFor(t, links, account)
	if !strings.Contains(after, "999+alice-renamed@users.noreply.github.com") {
		t.Fatalf("after rename:\n%s", after)
	}
	if strings.Contains(after, "alice-gh") {
		t.Fatalf("stale handle survived:\n%s", after)
	}
}

func trailerFor(t *testing.T, links *identity.Store, account string) string {
	t.Helper()
	b := &Bot{}
	b.SetIdentity(links)
	login, numericID, ok := b.githubIdentityFor(account)
	if !ok {
		t.Fatalf("account %q has no linked GitHub login", account)
	}
	return AttributionCommitTrailers(AttributionInput{
		PrompterName:    "Alice",
		GitHubLogin:     login,
		GitHubNumericID: numericID,
	})
}
