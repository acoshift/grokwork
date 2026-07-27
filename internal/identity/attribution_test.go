package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
// to be re-proven on every sign-in. Without this, a renamed account keeps being
// credited in commit trailers under a name that may now belong to a stranger.
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
	s, dir := newStore(t)
	// Not linked: a sign-in is not an assertion of ownership, and inventing a
	// binding from one is exactly the inference this package refuses to make.
	if err := s.RefreshHandle("github:999", "alice"); err != nil {
		t.Fatalf("RefreshHandle: %v", err)
	}
	if got := s.Canonical("github:999"); got != "github:999" {
		t.Fatalf("a refresh created a link: Canonical = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("a refresh of an unlinked login wrote the store file (err=%v)", err)
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
