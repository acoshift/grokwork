package web

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/identity"
)

// The self-service half of identity linking (see internal/identity for the
// model). One person may sign in through several providers; each of those
// logins mints its own actor id, and a link says "these are one account" so
// that every id born afterwards resolves to the same one.
//
// Everything here is deliberately ungated beyond "is signed in": your own
// logins are yours, whatever your role, and gating them behind a capability
// would leave a viewer permanently split across two identities with no way to
// ask for it to be fixed.

// accountIdentityRow is one login on the account page.
type accountIdentityRow struct {
	// ActorID is the login's actor id in wire form — the exact string an
	// allowlist has to name.
	ActorID string
	// Provider is the namespace ("discord", "google", "github").
	Provider string
	// Label is that namespace's human name.
	Label string
	// Subject is the provider's id for the account, without the namespace.
	Subject string
	// Name is display metadata: the account profile for the canonical row, the
	// cached handle for a GitHub alias, empty otherwise.
	Name string
	// Canonical marks the row every other login resolves to.
	Canonical bool
	// LinkedAt is when the link was made (empty for the canonical row).
	LinkedAt string
}

// accountLinkOption is a "Link…" button for a provider this account has no
// login from yet.
type accountLinkOption struct {
	Key      string
	Label    string
	LinkPath string
}

// requireAccount gates the self-service account routes: a live session, plus
// CSRF on mutations.
//
// Deliberately not requireMember — a viewer's logins are as much theirs as an
// admin's, and the design says any logged-in user may link and unlink their
// own. With web auth off there is no account to speak of: the page says so, and
// the mutations refuse rather than acting on an anonymous identity.
func (s *Server) requireAccount(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.WebAuthEnabled() {
			http.Error(w, "forbidden: web auth is disabled, so there is no account to manage", http.StatusForbidden)
			return
		}
		sess := sessionFromContext(r.Context())
		if sess == nil {
			sess = s.sessionFromRequest(r)
		}
		if sess == nil {
			http.Error(w, "forbidden: log in first", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if !s.checkCSRF(r, sess) {
				http.Error(w, "forbidden: invalid csrf token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	}))
}

// linkingAvailable reports whether this deployment can link logins at all.
func (s *Server) linkingAvailable() bool {
	return s != nil && s.identity != nil && s.cfg.WebAuthEnabled()
}

// accountRedirect sends the browser back to /account carrying one message.
func (s *Server) accountRedirect(ctx *hime.Context, ok, errMsg string) error {
	q := url.Values{}
	if ok != "" {
		q.Set("ok", ok)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	dst := "/account"
	if len(q) > 0 {
		dst += "?" + q.Encode()
	}
	return ctx.Redirect(dst)
}

// auditIdentity records one link/unlink attempt, successful or refused.
//
// The actor is the account (auditActor reads the session, which already carries
// the canonical id); detail names the other login and the rewrite counts. No
// display name, no handle, no path — an audit row here has to answer "which
// login was attached to which account, and what moved", nothing else.
func (s *Server) auditIdentity(ctx *hime.Context, action string, err error, detail map[string]any) {
	s.auditAction(ctx, action, err, detail)
}

func (s *Server) accountPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Account"
	d.IsAccount = true
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	d.Error = strings.TrimSpace(ctx.FormValue("err"))
	d.AccountLinkingOn = s.linkingAvailable()

	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	if sess == nil {
		// Auth off (requireAuth waves the request through) — there is no session
		// and therefore no account. The page explains that instead of 403ing a
		// read-only view.
		return s.viewPage(ctx, "account", d)
	}
	canonical := sess.DiscordUserID
	d.AccountIdentities = s.accountIdentities(canonical, sess.DisplayName)
	d.AccountLinkOptions = s.accountLinkOptions(d.AccountIdentities)
	return s.viewPage(ctx, "account", d)
}

// accountIdentities lists the canonical login first, then each alias.
func (s *Server) accountIdentities(canonical, displayName string) []accountIdentityRow {
	rows := []accountIdentityRow{accountIdentityRowFor(canonical, displayName, true, identity.AliasLink{})}
	for _, link := range s.identity.LinksOf(canonical) {
		rows = append(rows, accountIdentityRowFor(link.Alias, link.Handle, false, link))
	}
	return rows
}

func accountIdentityRowFor(actorID, name string, canonical bool, link identity.AliasLink) accountIdentityRow {
	row := accountIdentityRow{
		ActorID:   actorID,
		Provider:  config.ActorKind(actorID),
		Subject:   config.ActorSubject(actorID),
		Name:      strings.TrimSpace(name),
		Canonical: canonical,
	}
	row.Label = providerLabel(row.Provider)
	if !link.LinkedAt.IsZero() {
		row.LinkedAt = link.LinkedAt.UTC().Format("2006-01-02")
	}
	return row
}

// providerLabel names an actor namespace for display. Unknown namespaces render
// as themselves rather than as a guess.
func providerLabel(kind string) string {
	switch kind {
	case config.ActorKindDiscord:
		return "Discord"
	case config.ActorKindGoogle:
		return "Google"
	case config.ActorKindGitHub:
		return "GitHub"
	case config.ActorKindWeb:
		return "Web"
	case config.ActorKindOIDC:
		return "OIDC"
	}
	return kind
}

// accountLinkOptions offers exactly the providers that are configured and that
// this account has no login from yet. The buttons are convenience only — the
// routes refuse independently, since a hidden button is not a gate.
func (s *Server) accountLinkOptions(have []accountIdentityRow) []accountLinkOption {
	if !s.linkingAvailable() {
		return nil
	}
	taken := make(map[string]struct{}, len(have))
	for _, row := range have {
		taken[row.Provider] = struct{}{}
	}
	var out []accountLinkOption
	for _, key := range loginProviderOrder() {
		if _, ok := taken[key]; ok {
			continue
		}
		p, known := s.provider(key)
		if !known || !s.providerConfigured(key) {
			continue
		}
		out = append(out, accountLinkOption{Key: key, Label: p.Label(), LinkPath: authLinkPath(key)})
	}
	return out
}

// verifyLinkSession is the single most important check in the linking flow.
//
// Without it, account linking is a takeover primitive: the attacker starts a
// link flow for THEIR provider account, gets a signed-in victim to load the
// resulting callback URL, and their login is now attached to the victim's
// account — they log in as the victim from then on, with the victim's grants.
// The state cookie alone does not stop it, because in that attack the attacker
// supplies the state.
//
// So the link is authorized by the session that STARTED it: the callback must
// arrive in the same browser session whose id was baked into the state cookie.
// No live session, or a different one, is refused and audited.
func (s *Server) verifyLinkSession(ctx *hime.Context, flow oauthFlow) (*Session, error) {
	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	if sess == nil {
		return nil, fmt.Errorf("link refused: no signed-in session to link to")
	}
	if flow.sessionID == "" ||
		subtle.ConstantTimeCompare([]byte(sess.ID), []byte(flow.sessionID)) != 1 {
		return nil, fmt.Errorf("link refused: this link was started by a different session")
	}
	return sess, nil
}

// finishOAuthLink attaches a freshly proven identity to the signed-in account.
//
// Order matters. The link is written FIRST, because it is the assertion of
// ownership and the identity store enforces every structural invariant
// atomically (no chains, no second account, no re-pointing). Absorbing after it
// means a rewrite that fails halfway leaves the person correctly linked with
// some grants still on the old id — under-granting, and repairable by simply
// linking again, which redoes the (idempotent) rewrite. The reverse order would
// move grants for a link that never happened.
func (s *Server) finishOAuthLink(ctx *hime.Context, key string, sess *Session, id oauthIdentity) error {
	detail := map[string]any{"provider": key}
	fail := func(err error) error {
		s.auditIdentity(ctx, audit.ActionIdentityLink, err, detail)
		return s.accountRedirect(ctx, "", err.Error())
	}
	if sess == nil {
		return fail(fmt.Errorf("link refused: no signed-in session to link to"))
	}
	if s.identity == nil {
		return fail(fmt.Errorf("identity linking is not available"))
	}
	alias := actorIDFor(key, id.Subject)
	if alias == "" {
		return fail(fmt.Errorf("link refused: %s returned no usable account id", key))
	}
	canonical := strings.TrimSpace(sess.DiscordUserID)
	if canonical == "" {
		return fail(fmt.Errorf("link refused: this session has no account id"))
	}
	detail["alias"] = alias
	if config.SameActor(alias, canonical) {
		return fail(fmt.Errorf("that login is already this account"))
	}
	// Refuse before writing anything, so the message names the real situation.
	// identity.Link enforces both of these too; here they get an explanation.
	if owner := s.identity.Canonical(alias); !config.SameActor(owner, alias) &&
		!config.SameActor(owner, canonical) {
		return fail(fmt.Errorf("that login is already linked to another account (%s) — unlink it there first", owner))
	}
	if aliases := s.identity.AliasesOf(alias); len(aliases) > 0 {
		return fail(fmt.Errorf("that login is an account of its own with %d linked login(s) — merging two accounts is not supported", len(aliases)))
	}

	if err := s.identity.Link(alias, canonical, id.Handle); err != nil {
		return fail(err)
	}
	// Auto-absorb: the alias may already own grants, threads and cases from
	// before the link. They are the same person's, and the alias is never minted
	// again, so anything still naming it would be orphaned.
	grants, units, revoked, err := s.absorbActor(alias, canonical)
	detail["grants"] = grants
	detail["units"] = units
	detail["sessionsRevoked"] = revoked
	if err != nil {
		// The link stands; say what did not move. Linking again repeats the
		// rewrite, which is idempotent.
		return fail(fmt.Errorf("linked, but moving existing access failed: %v", err))
	}
	s.auditIdentity(ctx, audit.ActionIdentityLink, nil, detail)
	msg := providerLabel(config.ActorKind(alias)) + " login linked"
	if grants+units > 0 {
		msg += fmt.Sprintf(" — moved %d grant(s) and %d work unit(s) onto this account", grants, units)
	}
	return s.accountRedirect(ctx, msg, "")
}

// absorbActor rewrites everything still naming alias to name the account, and
// revokes the alias's live web sessions.
//
// Idempotent by construction: every step matches on the alias, which after the
// first pass no longer appears anywhere. Sessions are revoked last, and only
// after the rewrites, because a session carrying the alias is precisely the
// thing that could still be acting under the old id while the rewrite runs.
func (s *Server) absorbActor(alias, canonical string) (grants, units, revoked int, err error) {
	same := func(id string) bool { return config.SameActor(id, alias) }
	if s.cfg != nil {
		grants, err = s.cfg.RewriteActorID(alias, canonical)
		if err != nil {
			return grants, 0, 0, err
		}
	}
	if s.sessions != nil {
		units, err = s.sessions.RewriteActor(same, canonical)
		if err != nil {
			return grants, units, 0, err
		}
	}
	if s.webSessions != nil {
		revoked, err = s.webSessions.RevokeActor(alias)
		if err != nil {
			return grants, units, revoked, err
		}
	}
	return grants, units, revoked, nil
}

// postAccountUnlink detaches one login from the signed-in account.
//
// Refused when it would leave the account with no way to sign in — which is the
// case exactly when the login being removed is the only one that can reach it,
// i.e. the canonical id is not itself a usable login. Grants are NOT moved
// back: they were rewritten onto the account and belong to it now, which the
// page says out loud before anyone clicks.
func (s *Server) postAccountUnlink(ctx *hime.Context) error {
	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	alias := strings.TrimSpace(ctx.PostFormValue("alias"))
	detail := map[string]any{"alias": alias}
	fail := func(err error) error {
		s.auditIdentity(ctx, audit.ActionIdentityUnlink, err, detail)
		return s.accountRedirect(ctx, "", err.Error())
	}
	if sess == nil {
		return fail(fmt.Errorf("unlink refused: no signed-in session"))
	}
	if s.identity == nil {
		return fail(fmt.Errorf("identity linking is not available"))
	}
	if alias == "" {
		return fail(fmt.Errorf("unlink refused: no login named"))
	}
	canonical := sess.DiscordUserID
	// Only your own logins: an alias of somebody else's account is not yours to
	// detach, and answering "not linked to this account" for both a foreign
	// alias and a nonexistent one keeps this from being a probe.
	if !config.SameActor(s.identity.Canonical(alias), canonical) || config.SameActor(alias, canonical) {
		return fail(fmt.Errorf("that login is not linked to this account"))
	}
	if s.lastLoginFor(canonical, alias) {
		return fail(fmt.Errorf("unlink refused: %s is the only login that can reach this account", providerLabel(config.ActorKind(alias))))
	}
	if err := s.identity.Unlink(alias); err != nil {
		return fail(err)
	}
	s.auditIdentity(ctx, audit.ActionIdentityUnlink, nil, detail)
	return s.accountRedirect(ctx, providerLabel(config.ActorKind(alias))+" login unlinked", "")
}

// lastLoginFor reports whether removing alias would lock the account out.
//
// The canonical id is itself a login whenever it belongs to a provider
// namespace this build can authenticate — a Discord snowflake, a "google:sub".
// When it does, no alias is load-bearing. When it does not (or the provider is
// no longer configured), the aliases are the only doors, and the last one may
// not be removed.
func (s *Server) lastLoginFor(canonical, alias string) bool {
	if s.usableLogin(canonical) {
		return false
	}
	for _, other := range s.identity.AliasesOf(canonical) {
		if config.SameActor(other, alias) {
			continue
		}
		if s.usableLogin(other) {
			return false
		}
	}
	return true
}

// usableLogin reports whether an actor id could be minted again by a login.
func (s *Server) usableLogin(actorID string) bool {
	kind := config.ActorKind(actorID)
	if kind == "" {
		return false
	}
	if _, known := s.provider(kind); !known {
		return false
	}
	return s.providerConfigured(kind)
}
