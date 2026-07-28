package identity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir
}

// writeLinks seeds identity-links.json with raw JSON, so load-time validation
// can be tested against files the store itself would never have written.
func writeLinks(t *testing.T, dir, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLinkRoundTripsThroughDisk(t *testing.T) {
	s, dir := newStore(t)

	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if err := s.Link("google:sub-1", "42424", ""); err != nil {
		t.Fatalf("Link google: %v", err)
	}

	reopened, err := New(dir)
	if err != nil {
		t.Fatalf("New (reopen): %v", err)
	}
	if got := reopened.Canonical("github:999"); got != "42424" {
		t.Fatalf("Canonical(github:999) = %q, want %q", got, "42424")
	}
	if got := reopened.Canonical("google:sub-1"); got != "42424" {
		t.Fatalf("Canonical(google:sub-1) = %q, want %q", got, "42424")
	}
	if got := reopened.AliasesOf("42424"); len(got) != 2 || got[0] != "github:999" || got[1] != "google:sub-1" {
		t.Fatalf("AliasesOf = %v, want [github:999 google:sub-1]", got)
	}
	if w := reopened.Warnings(); len(w) != 0 {
		t.Fatalf("reopening a store we wrote warned: %v", w)
	}
}

// A canonical Discord account must come back BARE. Sessions, audit rows and
// webUsers keys have always held the raw snowflake and are compared with ==,
// so "discord:42424" here would silently detach a linked user from their own
// history — the exact failure this design exists to avoid.
func TestCanonicalReturnsWireFormNotNormalizedForm(t *testing.T) {
	s, _ := newStore(t)
	// The canonical is written the long way; the answer must still be bare.
	if err := s.Link("github:999", "discord:42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got := s.Canonical("github:999"); got != "42424" {
		t.Fatalf("Canonical = %q, want bare %q", got, "42424")
	}
	// And the other direction: a bare Discord alias lists as bare.
	s2, _ := newStore(t)
	if err := s2.Link("42424", "google:sub-1", ""); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got := s2.Canonical("42424"); got != "google:sub-1" {
		t.Fatalf("Canonical = %q, want %q", got, "google:sub-1")
	}
	if got := s2.AliasesOf("google:sub-1"); len(got) != 1 || got[0] != "42424" {
		t.Fatalf("AliasesOf = %v, want [42424]", got)
	}
}

func TestUnlinkedIDResolvesToItselfVerbatim(t *testing.T) {
	s, _ := newStore(t)
	// Empty store: the fast path must not normalize.
	for _, id := range []string{"42424", "discord:42424", "google:sub-1", "weird:thing", ""} {
		if got := s.Canonical(id); got != id {
			t.Fatalf("empty store Canonical(%q) = %q, want it unchanged", id, got)
		}
	}
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	// Non-empty store: an id that is not an alias is still returned verbatim,
	// including the canonical itself and a spelling we would normalize.
	for _, id := range []string{"42424", "discord:42424", "555", "google:sub-1", "weird:thing"} {
		if got := s.Canonical(id); got != id {
			t.Fatalf("Canonical(%q) = %q, want it unchanged", id, got)
		}
	}
}

func TestCanonicalAcceptsEitherSpellingOfAnAlias(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Link("discord:555", "google:sub-1", ""); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got := s.Canonical("555"); got != "google:sub-1" {
		t.Fatalf("Canonical(555) = %q, want %q", got, "google:sub-1")
	}
	if got := s.Canonical("discord:555"); got != "google:sub-1" {
		t.Fatalf("Canonical(discord:555) = %q, want %q", got, "google:sub-1")
	}
}

func TestUnlinkRestoresTheUnlinkedIdentity(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if err := s.Unlink("github:999"); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if got := s.Canonical("github:999"); got != "github:999" {
		t.Fatalf("Canonical after Unlink = %q, want %q", got, "github:999")
	}
	// Idempotent: the account page can be submitted twice.
	if err := s.Unlink("github:999"); err != nil {
		t.Fatalf("second Unlink: %v", err)
	}
	if err := s.Unlink(""); err == nil {
		t.Fatal("Unlink(\"\") should be an error")
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Canonical("github:999"); got != "github:999" {
		t.Fatalf("unlink did not persist: Canonical = %q", got)
	}
}

func TestGitHubForFindsGitHubAliasAndIgnoresOthers(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Link("google:sub-1", "42424", "not-a-github-login"); err != nil {
		t.Fatalf("Link google: %v", err)
	}
	// A non-GitHub alias must not cache a handle at all — GitHubFor would
	// otherwise have a display name where it expects a login.
	if h, ok := s.handles["google:sub-1"]; ok {
		t.Fatalf("google alias kept handle %q, want it dropped", h.Login)
	}
	if _, _, ok := s.GitHubFor("42424"); ok {
		t.Fatal("GitHubFor found a GitHub login with only a Google alias linked")
	}

	if err := s.Link("github:999", "42424", "@alice"); err != nil {
		t.Fatalf("Link github: %v", err)
	}
	login, id, ok := s.GitHubFor("42424")
	if !ok || login != "alice" || id != "999" {
		t.Fatalf("GitHubFor = (%q, %q, %v), want (alice, 999, true)", login, id, ok)
	}
	// Either spelling of the canonical resolves.
	if _, _, ok := s.GitHubFor("discord:42424"); !ok {
		t.Fatal("GitHubFor(discord:42424) missed the link")
	}
	if _, _, ok := s.GitHubFor("someone-else"); ok {
		t.Fatal("GitHubFor answered for an unrelated account")
	}
}

// A GitHub alias whose cached handle is missing yields no attribution — a
// "<id>+@users.noreply.github.com" trailer is malformed — but must not hide a
// sibling GitHub alias that does have one.
func TestGitHubForSkipsHandlelessAlias(t *testing.T) {
	dir := t.TempDir()
	writeLinks(t, dir, `{
	  "links": {
	    "github:111": {"canonical": "42424"},
	    "github:222": {"canonical": "42424"}
	  },
	  "handles": {"github:222": {"login": "alice", "checkedAt": "`+nowStamp()+`"}}
	}`)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	login, id, ok := s.GitHubFor("42424")
	if !ok || login != "alice" || id != "222" {
		t.Fatalf("GitHubFor = (%q, %q, %v), want (alice, 222, true)", login, id, ok)
	}

	only := t.TempDir()
	writeLinks(t, only, `{"links": {"github:111": {"canonical": "42424"}}}`)
	s2, err := New(only)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s2.GitHubFor("42424"); ok {
		t.Fatal("GitHubFor returned ok for a handleless alias")
	}
}

// nowStamp is an RFC3339 "just proved" instant for hand-written fixtures.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

func TestLinkRefreshesHandleAndKeepsLinkedAt(t *testing.T) {
	s, _ := newStore(t)
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.now = func() time.Time { return first }
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	s.now = func() time.Time { return first.Add(72 * time.Hour) }
	if err := s.Link("github:999", "discord:42424", "alice-renamed"); err != nil {
		t.Fatalf("re-Link: %v", err)
	}
	got := s.links["github:999"]
	if h := s.handles["github:999"]; h.Login != "alice-renamed" {
		t.Fatalf("handle = %q, want it refreshed to alice-renamed", h.Login)
	}
	if !got.LinkedAt.Equal(first) {
		t.Fatalf("LinkedAt = %v, want the original %v", got.LinkedAt, first)
	}
	// The re-link is a fresh proof of the handle, so its stamp moves even though
	// the link's does not: one records ownership (immutable), the other when a
	// mutable name was last verified.
	if h := s.handles["github:999"]; !h.CheckedAt.Equal(first.Add(72 * time.Hour)) {
		t.Fatalf("handle checkedAt = %v, want the re-link time", h.CheckedAt)
	}
}

func TestLinkRejectsMovingAnAliasBetweenAccounts(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	err := s.Link("github:999", "555", "alice")
	if err == nil {
		t.Fatal("re-pointing an alias at another account should be refused")
	}
	if !strings.Contains(err.Error(), "unlink") {
		t.Fatalf("error should say how to proceed, got %v", err)
	}
	if got := s.Canonical("github:999"); got != "42424" {
		t.Fatalf("refused link still moved the alias: Canonical = %q", got)
	}
}

func TestLinkRefusesChainsFromBothEnds(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	// Canonical side: 42424 already has aliases, so it cannot become one.
	if err := s.Link("42424", "google:sub-1", ""); err == nil {
		t.Fatal("linking an id that already has aliases should be refused")
	}
	// Alias side: github:999 is already an alias, so nothing may point at it.
	if err := s.Link("google:sub-1", "github:999", ""); err == nil {
		t.Fatal("linking to an id that is itself an alias should be refused")
	}
	if err := s.Link("42424", "discord:42424", ""); err == nil {
		t.Fatal("self-link should be refused")
	}
	if err := s.Link("", "42424", ""); err == nil {
		t.Fatal("empty alias should be refused")
	}
	if err := s.Link("github:1", "", ""); err == nil {
		t.Fatal("empty canonical should be refused")
	}
	if got := len(s.links); got != 1 {
		t.Fatalf("refused links mutated the store: %d entries", got)
	}
}

func TestLinkEnforcesTheAliasCap(t *testing.T) {
	s, _ := newStore(t)
	for i := range MaxAliasesPerCanonical {
		if err := s.Link(fmt.Sprintf("google:sub-%d", i), "42424", ""); err != nil {
			t.Fatalf("Link %d: %v", i, err)
		}
	}
	err := s.Link("google:one-too-many", "42424", "")
	if err == nil {
		t.Fatalf("linking past the cap of %d should be refused", MaxAliasesPerCanonical)
	}
	if got := len(s.AliasesOf("42424")); got != MaxAliasesPerCanonical {
		t.Fatalf("aliases = %d, want %d", got, MaxAliasesPerCanonical)
	}
	// Refreshing one of the existing links is not a new alias and must still work.
	if err := s.Link("google:sub-0", "42424", ""); err != nil {
		t.Fatalf("refresh at the cap: %v", err)
	}
}

func TestLoadDropsChainedLinkWithWarning(t *testing.T) {
	dir := t.TempDir()
	writeLinks(t, dir, `{
	  "github:999":  {"canonical": "google:sub-1"},
	  "google:sub-1": {"canonical": "42424"}
	}`)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The chained link is gone; that person falls back to their own identity.
	if got := s.Canonical("github:999"); got != "github:999" {
		t.Fatalf("chained link survived: Canonical = %q", got)
	}
	// The one-hop link is untouched.
	if got := s.Canonical("google:sub-1"); got != "42424" {
		t.Fatalf("Canonical(google:sub-1) = %q, want 42424", got)
	}
	assertWarned(t, s, "github:999", "google:sub-1")
}

func TestLoadDropsBothSpellingsOfADuplicateAlias(t *testing.T) {
	dir := t.TempDir()
	writeLinks(t, dir, `{
	  "555":         {"canonical": "42424"},
	  "discord:555": {"canonical": "google:sub-1"}
	}`)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Canonical("555"); got != "555" {
		t.Fatalf("conflicting alias resolved to %q, want it dropped", got)
	}
	if got := len(s.links); got != 0 {
		t.Fatalf("links = %v, want both dropped", s.links)
	}
	assertWarned(t, s, "555", "discord:555")
}

func TestLoadDropsSelfLinkAndEmptySides(t *testing.T) {
	dir := t.TempDir()
	writeLinks(t, dir, `{
	  "42424":       {"canonical": "discord:42424"},
	  "github:777":  {"canonical": "   "},
	  "":            {"canonical": "42424"},
	  "google:keep": {"canonical": "42424"}
	}`)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.links); got != 1 {
		t.Fatalf("links = %v, want only google:keep", s.links)
	}
	if got := s.Canonical("google:keep"); got != "42424" {
		t.Fatalf("Canonical(google:keep) = %q, want 42424", got)
	}
	if got := s.Canonical("42424"); got != "42424" {
		t.Fatalf("self-link survived: %q", got)
	}
	assertWarned(t, s, "42424", "github:777")
}

func TestLoadEnforcesTheAliasCap(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("{")
	for i := range MaxAliasesPerCanonical + 1 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q: {\"canonical\": \"42424\"}", fmt.Sprintf("google:sub-%d", i))
	}
	b.WriteString("}")
	writeLinks(t, dir, b.String())

	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.AliasesOf("42424")); got != MaxAliasesPerCanonical {
		t.Fatalf("aliases = %d, want the cap of %d", got, MaxAliasesPerCanonical)
	}
	// Sorted-alias order decides who survives, so the same aliases survive on
	// every boot: "google:sub-8" sorts last among sub-0..sub-8.
	if got := s.Canonical("google:sub-8"); got != "google:sub-8" {
		t.Fatalf("Canonical(google:sub-8) = %q, want it dropped", got)
	}
	assertWarned(t, s, "42424", "google:sub-8")
}

// A file we cannot parse must not read as "nobody has linked anything": the
// next Link call would overwrite it and destroy every binding.
func TestLoadRefusesMalformedFile(t *testing.T) {
	dir := t.TempDir()
	writeLinks(t, dir, `{"github:999": `)
	if _, err := New(dir); err == nil {
		t.Fatal("New should refuse a malformed identity-links.json")
	}

	empty := t.TempDir()
	writeLinks(t, empty, "\n")
	if _, err := New(empty); err != nil {
		t.Fatalf("an empty file is not corruption: %v", err)
	}
}

func TestSaveIsAtomicAndPrivate(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	path := filepath.Join(dir, FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files after save: %v", matches)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored storeFile
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("stored file does not parse: %v", err)
	}
	// The canonical is stored NORMALIZED so two spellings cannot become two
	// accounts; only readers see the wire form.
	if got := stored.Links["github:999"].Canonical; got != "discord:42424" {
		t.Fatalf("stored canonical = %q, want discord:42424", got)
	}
	// The handle rides in its own map, keyed by the GitHub id, with the instant it
	// was proved — the stamp MaxHandleAge expires it against.
	if got := stored.Handles["github:999"]; got.Login != "alice" || got.CheckedAt.IsZero() {
		t.Fatalf("stored handle = %+v", got)
	}
}

// A failed save must leave memory agreeing with disk: a link that works until
// the next restart is worse than one that never took.
func TestFailedSaveRollsBackMemory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not block root")
	}
	s, dir := newStore(t)
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := s.Link("google:sub-1", "42424", ""); err == nil {
		t.Fatal("expected an error linking with a read-only data dir")
	}
	if got := s.Canonical("google:sub-1"); got != "google:sub-1" {
		t.Fatalf("failed Link stayed in memory: Canonical = %q", got)
	}
	if err := s.Unlink("github:999"); err == nil {
		t.Fatal("expected an error unlinking with a read-only data dir")
	}
	if got := s.Canonical("github:999"); got != "42424" {
		t.Fatalf("failed Unlink stayed in memory: Canonical = %q", got)
	}
}

// Canonical runs on every Discord message and every web request that mints an
// actor, concurrently with an account page writing a link.
func TestConcurrentReadsAndWrites(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Link("github:999", "42424", "alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 50 {
				s.Canonical("github:999")
				s.AliasesOf("42424")
				s.GitHubFor("42424")
			}
		})
		wg.Go(func() {
			alias := fmt.Sprintf("google:sub-%d", i)
			_ = s.Link(alias, "42424", "")
			_ = s.Unlink(alias)
		})
	}
	wg.Wait()
	if got := s.Canonical("github:999"); got != "42424" {
		t.Fatalf("Canonical = %q, want 42424", got)
	}
}

func assertWarned(t *testing.T, s *Store, mustName ...string) {
	t.Helper()
	warnings := s.Warnings()
	if len(warnings) == 0 {
		t.Fatal("dropping a link must warn: got none")
	}
	all := strings.Join(warnings, "\n")
	for _, id := range mustName {
		if !strings.Contains(all, id) {
			t.Fatalf("warning must name %q, got:\n%s", id, all)
		}
	}
}

// The account page has to render what AliasesOf deliberately drops: when a
// login was attached, and under which GitHub handle. Ordering matches
// AliasesOf so the two surfaces cannot disagree about which login is which.
func TestLinksOfCarriesHandleAndTimestamp(t *testing.T) {
	s, _ := newStore(t)
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	s.now = func() time.Time { return at }
	if err := s.Link("github:999", "discord:42424", "@alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if err := s.Link("google:sub-1", "42424", ""); err != nil {
		t.Fatalf("Link google: %v", err)
	}

	got := s.LinksOf("42424")
	if len(got) != 2 {
		t.Fatalf("LinksOf = %+v, want 2 rows", got)
	}
	if got[0].Alias != "github:999" || got[0].Handle != "alice" || !got[0].LinkedAt.Equal(at) {
		t.Fatalf("github row = %+v", got[0])
	}
	if got[1].Alias != "google:sub-1" || got[1].Handle != "" {
		// A handle on a non-GitHub alias would put a display name where the
		// attribution path expects a login.
		t.Fatalf("google row = %+v", got[1])
	}
	if n := len(s.LinksOf("nobody")); n != 0 {
		t.Fatalf("LinksOf(nobody) returned %d rows", n)
	}
	var nilStore *Store
	if n := len(nilStore.LinksOf("42424")); n != 0 {
		t.Fatalf("nil store returned %d rows", n)
	}
}

// TestDiscordSubjectForFindsALinkedDiscordLogin pins the reverse lookup that
// keeps linked people reachable in Discord.
//
// Canonical-at-mint means an account's id is whichever login created it, so a
// Google-first person who links Discord has a non-snowflake canonical id while
// being perfectly reachable in Discord. Anything addressing a human *in Discord*
// must resolve through here instead of testing the shape of the id it holds —
// otherwise linking a second login silently removes someone from DMs, mentions
// and the reviewer list.
func TestDiscordSubjectForFindsALinkedDiscordLogin(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	const snowflake = "424242424242424242"
	if err := s.Link(snowflake, "google:alice", ""); err != nil {
		t.Fatal(err)
	}

	// Google-canonical account, reachable through its Discord alias.
	got, ok := s.DiscordSubjectFor("google:alice")
	if !ok || got != snowflake {
		t.Fatalf("DiscordSubjectFor(google:alice) = (%q,%v), want (%q,true)", got, ok, snowflake)
	}

	// A Discord-canonical account answers with its own id, linked or not.
	got, ok = s.DiscordSubjectFor(snowflake)
	if !ok || got != snowflake {
		t.Fatalf("DiscordSubjectFor(discord) = (%q,%v), want (%q,true)", got, ok, snowflake)
	}

	// No Discord login anywhere is a real state — callers must degrade, not
	// invent a target.
	if got, ok := s.DiscordSubjectFor("google:bob"); ok {
		t.Fatalf("DiscordSubjectFor(unlinked google) = (%q,true), want not ok", got)
	}
	var nilStore *Store
	if _, ok := nilStore.DiscordSubjectFor("google:alice"); ok {
		t.Fatal("nil store reported a Discord subject")
	}
}
