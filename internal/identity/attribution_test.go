package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNoreplyEmail(t *testing.T) {
	if got := NoreplyEmail("alice", "999"); got != "999+alice@users.noreply.github.com" {
		t.Fatalf("got %q", got)
	}
	// The @ a human types is not part of the address.
	if got := NoreplyEmail(" @alice ", " 999 "); got != "999+alice@users.noreply.github.com" {
		t.Fatalf("trim: %q", got)
	}
	// Half an identity builds an address that attributes to nobody while looking
	// like it worked, so it must build nothing at all.
	for _, tc := range []struct{ login, id string }{
		{"", "999"},
		{"alice", ""},
		{"", ""},
		{"@", "999"},
		{"alice", "   "},
	} {
		if got := NoreplyEmail(tc.login, tc.id); got != "" {
			t.Fatalf("NoreplyEmail(%q, %q) = %q, want empty", tc.login, tc.id, got)
		}
	}
}

// A GitHub login is mutable and the numeric id is not, so the cached handle has
// to be re-proven on every GitHub sign-in. Without this, a renamed account keeps
// being credited in commit trailers under a name that may now belong to a
// stranger (which is also why the cache expires — see MaxHandleAge).
func TestRefreshHandleUpdatesARenamedLogin(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatal(err)
	}
	if login, id, ok := s.GitHubFor("42424"); !ok || login != "alice" || id != "999" {
		t.Fatalf("before: %q %q ok=%v", login, id, ok)
	}

	if err := s.RefreshHandle("github:999", "@alice-renamed"); err != nil {
		t.Fatalf("RefreshHandle: %v", err)
	}
	login, id, ok := s.GitHubFor("42424")
	if !ok || login != "alice-renamed" {
		t.Fatalf("after rename: %q ok=%v", login, ok)
	}
	// The identity itself did not move: the subject is what GitHub matches the
	// noreply address on, and it is the alias key, not the handle.
	if id != "999" {
		t.Fatalf("numeric id changed to %q — it must come from the immutable subject", id)
	}
	// And it is durable, or the next restart silently reverts to the old name.
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if login, _, _ := reopened.GitHubFor("42424"); login != "alice-renamed" {
		t.Fatalf("reopened handle = %q", login)
	}
	// The link itself is untouched: refreshing a name is not re-linking.
	links := reopened.LinksOf("42424")
	if len(links) != 1 || links[0].Alias != "github:999" {
		t.Fatalf("links = %+v", links)
	}
	if links[0].LinkedAt.IsZero() {
		t.Fatal("RefreshHandle dropped the original link time")
	}
}

// RefreshHandle runs on every login, so everything it must NOT do matters more
// than what it does.
func TestRefreshHandleNeverCreatesALink(t *testing.T) {
	s, _ := newStore(t)
	// Not linked: a sign-in is not an assertion of ownership, and inventing a
	// binding from one is exactly the inference this package refuses to make.
	//
	// Caching the handle IS allowed, and is the whole reason a GitHub-first
	// account (whose canonical id is its GitHub login, with no alias anywhere)
	// can be attributed at all — "999 is called alice" is a fact GitHub just
	// told us, while "999 is this account" would be the inference. So the
	// assertions here are about the LINK graph, not about the file staying absent.
	if err := s.RefreshHandle("github:999", "alice"); err != nil {
		t.Fatalf("RefreshHandle: %v", err)
	}
	if got := s.Canonical("github:999"); got != "github:999" {
		t.Fatalf("a refresh created a link: Canonical = %q", got)
	}
	if got := s.AliasesOf("github:999"); len(got) != 0 {
		t.Fatalf("a refresh gave the login aliases: %v", got)
	}
	if got := len(s.links); got != 0 {
		t.Fatalf("a refresh wrote %d link(s)", got)
	}

	// Non-GitHub aliases have no handle to cache; storing one would put a display
	// name where GitHubFor expects a login.
	if err := s.Link("google:sub-1", "42424", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshHandle("google:sub-1", "not-a-login"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.GitHubFor("42424"); ok {
		t.Fatal("a Google alias must never answer GitHubFor")
	}

	// An empty handle is a provider hiccup, not an instruction to forget the one
	// we have: dropping it would silently switch that person's attribution off.
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshHandle("github:999", "  "); err != nil {
		t.Fatal(err)
	}
	if login, _, ok := s.GitHubFor("42424"); !ok || login != "alice" {
		t.Fatalf("empty refresh clobbered the handle: %q ok=%v", login, ok)
	}
}

// The common path is "every login, nothing changed", and it must not write.
func TestRefreshHandleUnchangedDoesNotRewrite(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	// Same handle, and the same handle with the @ a provider might include.
	for _, h := range []string{"alice", "@alice"} {
		if err := s.RefreshHandle("github:999", h); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged handle rewrote the store on a plain sign-in")
	}
}

func TestRefreshHandleIsNilAndEmptySafe(t *testing.T) {
	var s *Store
	if err := s.RefreshHandle("github:1", "x"); err != nil {
		t.Fatalf("nil store: %v", err)
	}
	real, _ := newStore(t)
	if err := real.RefreshHandle("", "x"); err != nil {
		t.Fatalf("empty alias: %v", err)
	}
}

// An account whose canonical id IS a GitHub login (a GitHub-first signup) must be
// attributable. Walking only the ALIASES left that person with no trailer, no
// "@login" and no way to fix it: /account offers no button for a provider already
// on the account, and linking the canonical to itself is refused as a self-link.
// In a GitHub-only web deployment that meant nobody in the org ever got a trailer.
func TestGitHubForUsesTheAccountsOwnLogin(t *testing.T) {
	s, dir := newStore(t)
	// The reviewer's repro, in both orders: sign-in-then-link and link-then-sign-in.
	if err := s.Link("42424", "github:777", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshHandle("github:777", "bob-gh"); err != nil {
		t.Fatal(err)
	}
	login, id, ok := s.GitHubFor("github:777")
	if !ok || login != "bob-gh" || id != "777" {
		t.Fatalf("GitHubFor = (%q, %q, %v), want (bob-gh, 777, true)", login, id, ok)
	}
	if got := NoreplyEmail(login, id); got != "777+bob-gh@users.noreply.github.com" {
		t.Fatalf("trailer email = %q", got)
	}
	// The Discord alias resolves to the account first (canonical-at-mint), and the
	// account then answers for its own login.
	if got := s.Canonical("42424"); got != "github:777" {
		t.Fatalf("Canonical(42424) = %q", got)
	}
	// Durable: the handle survives a restart like the link does.
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if login, _, ok := reopened.GitHubFor("github:777"); !ok || login != "bob-gh" {
		t.Fatalf("after reopen: (%q, %v)", login, ok)
	}
	// A GitHub login with no cached handle still yields nothing: the numeric id
	// alone builds a malformed address.
	if _, _, ok := s.GitHubFor("github:888"); ok {
		t.Fatal("GitHubFor answered for a GitHub id with no proved handle")
	}
	// An ALIAS's handle still wins nothing away from the account's own login, but
	// both are reachable — the account's first, since it is the account.
	if err := s.RefreshHandle("github:777", "bob-gh"); err != nil {
		t.Fatal(err)
	}
	if _, checkedAt, fresh := s.HandleOf("github:777"); !fresh || checkedAt.IsZero() {
		t.Fatalf("HandleOf = (fresh=%v, checkedAt=%v)", fresh, checkedAt)
	}
}

// A cached handle is only as good as its last proof. It is re-proved when the
// person signs in WITH GitHub — which a Discord- or Google-first user may never do
// again, and a Discord-only user may never do at all. Believing it forever means
// "@alice" in a PR body, an "On behalf of @alice" comment and an --add-reviewer
// can all name whoever holds that freed name now.
func TestGitHubForExpiresAStaleHandle(t *testing.T) {
	s, _ := newStore(t)
	linked := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.now = func() time.Time { return linked }
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.GitHubFor("42424"); !ok {
		t.Fatal("a handle proved at link time must be usable")
	}
	// Just inside the window.
	s.now = func() time.Time { return linked.Add(MaxHandleAge - time.Minute) }
	if _, _, ok := s.GitHubFor("42424"); !ok {
		t.Fatal("a handle inside MaxHandleAge must still be usable")
	}
	// Past it: no attribution at all rather than a name that may now be somebody
	// else's. The numeric id has not expired — it cannot — but half an identity
	// builds nothing, which is the design's own safe default.
	s.now = func() time.Time { return linked.Add(MaxHandleAge + time.Minute) }
	if login, id, ok := s.GitHubFor("42424"); ok {
		t.Fatalf("an expired handle was used: (%q, %q)", login, id)
	}
	// The page can still see it, and say it is stale: a blank cell is not
	// actionable, "sign in with GitHub" is.
	links := s.LinksOf("42424")
	if len(links) != 1 || links[0].Handle != "alice" || links[0].HandleFresh {
		t.Fatalf("LinksOf = %+v want the handle rendered as stale", links)
	}
	// And signing in with GitHub restores it, which is the whole recovery path.
	if err := s.RefreshHandle("github:999", "alice"); err != nil {
		t.Fatal(err)
	}
	if login, _, ok := s.GitHubFor("42424"); !ok || login != "alice" {
		t.Fatalf("re-proving did not restore attribution: (%q, %v)", login, ok)
	}
}

// The legacy file format (the handle inline on each link) must keep working, or
// upgrading silently drops every binding in it.
func TestLoadMigratesLegacyInlineHandles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{
	  "github:999": {"canonical": "42424", "handle": "alice", "linkedAt": "`+
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)+`"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Canonical("github:999"); got != "42424" {
		t.Fatalf("legacy link lost: Canonical = %q", got)
	}
	login, id, ok := s.GitHubFor("42424")
	if !ok || login != "alice" || id != "999" {
		t.Fatalf("legacy handle lost: (%q, %q, %v)", login, id, ok)
	}
	if w := s.Warnings(); len(w) != 0 {
		t.Fatalf("migrating warned: %v", w)
	}

	// A legacy link with no timestamp has no proof instant, so its handle reads as
	// unproven rather than as just-checked.
	old := t.TempDir()
	if err := os.WriteFile(filepath.Join(old, FileName),
		[]byte(`{"github:999": {"canonical": "42424", "handle": "alice"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := New(old)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Canonical("github:999"); got != "42424" {
		t.Fatalf("stamp-less legacy link lost: %q", got)
	}
	if _, _, ok := s2.GitHubFor("42424"); ok {
		t.Fatal("a handle with no recorded proof was believed")
	}
}

// GitHubFor is what attribution reads, so the two ways it can legitimately find
// nothing must stay distinguishable from finding half a thing.
func TestGitHubForNeedsBothHalves(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Link("github:999", "42424", ""); err != nil {
		t.Fatal(err)
	}
	if login, id, ok := s.GitHubFor("42424"); ok {
		t.Fatalf("handle-less alias answered: %q %q", login, id)
	}
	if got := NoreplyEmail("", "999"); got != "" {
		t.Fatalf("and the address it would have built is malformed: %q", got)
	}
	// A sibling that does carry a handle is still found.
	if err := s.Link("github:1000", "42424", "alice"); err != nil {
		t.Fatal(err)
	}
	login, id, ok := s.GitHubFor("42424")
	if !ok || login != "alice" || id != "1000" {
		t.Fatalf("sibling: %q %q ok=%v", login, id, ok)
	}
	if got := NoreplyEmail(login, id); !strings.HasSuffix(got, "@users.noreply.github.com") {
		t.Fatalf("address: %q", got)
	}
}
