package web

import (
	"net/http"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/identity"
)

// linkFixture installs an identity store on the fixture's server and returns it.
// The store lives in its own temp dir: the point of the test is the resolution,
// not where the file sits.
func linkFixture(t *testing.T, f *authFixture, links map[string]string) *identity.Store {
	t.Helper()
	st, err := identity.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for alias, canonical := range links {
		if err := st.Link(alias, canonical, ""); err != nil {
			t.Fatalf("link %s → %s: %v", alias, canonical, err)
		}
	}
	f.srv.identity = st
	return st
}

// Canonical-at-mint, web side: signing in with a linked login must produce a
// session carrying the ACCOUNT's actor id. Everything downstream compares that
// id the way it always did, so if the alias reached the session the person would
// be a stranger to their own grants, threads and spend.
//
// "google:sub-1" is on no allowlist here — the admin role can only come from the
// canonical id it resolves to, which is what makes this more than a string swap.
func TestLinkedLoginYieldsCanonicalSession(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, false))
	linkFixture(t, f, map[string]string{"google:sub-1": "admin-1"})

	w := loginVia(t, f.srv.Handler(), config.ActorKindGoogle, "g-admin", "/config")
	if w.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s", w.Code, w.Body.String())
	}
	sid := sessionCookie(w)
	if sid == "" {
		t.Fatalf("no session cookie (loc=%q)", w.Header().Get("Location"))
	}
	sess, _, ok := f.srv.webSessions.Get(sid)
	if !ok {
		t.Fatal("session not stored")
	}
	if sess.DiscordUserID != "admin-1" {
		t.Fatalf("session actor=%q want the canonical account %q", sess.DiscordUserID, "admin-1")
	}
	if sess.Role != config.WebRoleAdmin {
		t.Fatalf("role=%q want admin — the grant belongs to the account, not the login", sess.Role)
	}
	// The durable profile follows the account too: one person, one profile row.
	if _, ok := f.srv.webUsers.Get("admin-1"); !ok {
		t.Fatal("durable profile must be keyed by the canonical account")
	}
	if _, ok := f.srv.webUsers.Get("google:sub-1"); ok {
		t.Fatal("an alias must not get its own profile row")
	}
}

// The audit row has to answer BOTH questions: which account acted, and which of
// their logins they used. Recording only the account makes "who signed in with
// what" unanswerable the moment one person can arrive three ways.
func TestLinkedLoginAuditRecordsBothIDs(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, false))
	linkFixture(t, f, map[string]string{"google:sub-1": "admin-1"})

	if w := loginVia(t, f.srv.Handler(), config.ActorKindGoogle, "g-admin", ""); sessionCookie(w) == "" {
		t.Fatalf("login failed (loc=%q)", w.Header().Get("Location"))
	}
	evs, err := f.srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Fatal("no audit rows")
	}
	last := evs[len(evs)-1]
	if last.Action != audit.ActionLoginOK || !last.OK {
		t.Fatalf("action=%q ok=%v", last.Action, last.OK)
	}
	if last.Actor != "admin-1" {
		t.Fatalf("audit actor=%q want the account", last.Actor)
	}
	if got := last.Detail["loginActor"]; got != "google:sub-1" {
		t.Fatalf("detail.loginActor=%v want %q — which login was used is unanswerable without it",
			got, "google:sub-1")
	}
}

// A refused login is audited the same way, so an operator can see that a linked
// login was tried against an account with no access.
func TestLinkedLoginDenialAuditRecordsBothIDs(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, false))
	// "stranger" is on no allowlist; the Google login resolves onto it.
	linkFixture(t, f, map[string]string{"google:sub-1": "stranger"})

	w := loginVia(t, f.srv.Handler(), config.ActorKindGoogle, "g-admin", "")
	if sessionCookie(w) != "" {
		t.Fatal("an unauthorized account must not be admitted through a linked login")
	}
	evs, err := f.srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Action != audit.ActionLoginFail || last.OK {
		t.Fatalf("action=%q ok=%v", last.Action, last.OK)
	}
	if last.Actor != "stranger" {
		t.Fatalf("audit actor=%q want the account the login resolved to", last.Actor)
	}
	if got := last.Detail["loginActor"]; got != "google:sub-1" {
		t.Fatalf("detail.loginActor=%v want %q", got, "google:sub-1")
	}
}

// An unlinked login is unchanged in every respect, including its audit row:
// loginActor is written only when it differs from the actor, so the
// overwhelmingly common case keeps exactly the record it had.
func TestUnlinkedLoginAuditIsUnchanged(t *testing.T) {
	f := newAuthFixture(t, nil)
	if w := loginVia(t, f.srv.Handler(), config.ActorKindDiscord, "code-admin", ""); sessionCookie(w) == "" {
		t.Fatalf("login failed (loc=%q)", w.Header().Get("Location"))
	}
	evs, err := f.srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Actor != "admin-1" {
		t.Fatalf("audit actor=%q", last.Actor)
	}
	if _, ok := last.Detail["loginActor"]; ok {
		t.Fatalf("unlinked login wrote a redundant loginActor: %v", last.Detail)
	}
}

// A GitHub login is renameable and the freed name is immediately re-registrable,
// so the handle cached against the alias is metadata that has to be re-proven on
// every sign-in — not just at link time. Nothing else in the tree exercises the
// wire from oauthCallback to identity.RefreshHandle: the store's own tests can
// only show that refreshing works if somebody calls it, and dropping the call
// from the callback leaves attribution writing "@alice" into public git history
// forever after alice renamed herself.
func TestRenamedGitHubHandleRefreshesOnNextLogin(t *testing.T) {
	f := newAuthFixture(t, withProviders(false, true))
	st := linkFixture(t, f, nil)
	if err := st.Link("github:999", "admin-1", "alice"); err != nil {
		t.Fatal(err)
	}
	if login, id, ok := st.GitHubFor("admin-1"); !ok || login != "alice" || id != "999" {
		t.Fatalf("before rename: (%q,%q,%v)", login, id, ok)
	}

	// The person renames alice → alice-renamed on GitHub. The numeric id, which
	// is the identity, is unchanged — only the handle moved.
	f.github.CodeToUser["h-admin"] = GitHubUser{ID: 999, Login: "alice-renamed", Name: "Alice GH"}

	if w := loginVia(t, f.srv.Handler(), config.ActorKindGitHub, "h-admin", ""); sessionCookie(w) == "" {
		t.Fatalf("login failed (loc=%q)", w.Header().Get("Location"))
	}

	login, id, ok := st.GitHubFor("admin-1")
	if !ok || login != "alice-renamed" || id != "999" {
		t.Fatalf("after rename: (%q,%q,%v) want the new handle against the same id", login, id, ok)
	}
	// Attribution therefore follows the rename, and follows it via the id.
	if got := identity.NoreplyEmail(login, id); got != "999+alice-renamed@users.noreply.github.com" {
		t.Fatalf("trailer email=%q", got)
	}
	// Refreshing must not have invented a second binding or moved the account.
	if aliases := st.AliasesOf("admin-1"); len(aliases) != 1 || aliases[0] != "github:999" {
		t.Fatalf("aliases=%v — refresh must only update, never link", aliases)
	}
	if got := st.Canonical("github:999"); got != "admin-1" {
		t.Fatalf("canonical=%q", got)
	}
}

// The same call must be inert for the overwhelming majority of sign-ins. If a
// login could acquire a link merely by being used, attribution would start
// naming a GitHub account nobody ever asserted was theirs — the exact inference
// the whole package refuses to make, arriving through the back door.
func TestLoginWithoutALinkCreatesNoBinding(t *testing.T) {
	f := newAuthFixture(t, func(cfg *config.Config) {
		withProviders(false, true)(cfg)
		cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "github:999")
	})
	st := linkFixture(t, f, nil)

	if w := loginVia(t, f.srv.Handler(), config.ActorKindGitHub, "h-admin", ""); sessionCookie(w) == "" {
		t.Fatalf("login failed (loc=%q)", w.Header().Get("Location"))
	}

	if got := st.Canonical("github:999"); got != "github:999" {
		t.Fatalf("canonical=%q — a sign-in must not link anything", got)
	}
	if aliases := st.AliasesOf("github:999"); len(aliases) != 0 {
		t.Fatalf("aliases=%v", aliases)
	}
	if login, id, ok := st.GitHubFor("github:999"); ok {
		t.Fatalf("GitHubFor invented (%q,%q) for an unlinked login", login, id)
	}
}
