package web

import (
	"context"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
)

// oauthIdentity is what a login provider tells us about the person, after a
// token exchange this process performed itself over TLS.
//
// Subject is the provider's immutable id for the account and is the ONLY field
// authorization may read. Name and AvatarURL are display metadata: an email
// address or a handle can be changed by its owner and then re-registered by
// somebody else, so resolving a role from one is account takeover with extra
// steps.
type oauthIdentity struct {
	Subject   string
	Name      string
	AvatarURL string
}

// oauthProvider is one login button end to end.
//
// Everything a mistake would actually cost — issuing and comparing state, the
// open-redirect guard, the "is this provider configured" gate, role resolution,
// the audit row, session creation — lives once in oauthStart/oauthCallback, so
// a provider can only get the four things below wrong. Three near-copies of the
// callback is exactly the shape where copy #3 loses the state check and nothing
// in the diff shows it.
type oauthProvider interface {
	// Key is the provider id used in routes, cookies and — except for Discord,
	// see actorIDFor — the actor namespace.
	Key() string
	// Label is the human name for buttons and error flashes.
	Label() string
	AuthorizeURL(clientID, redirectURI, state string) string
	Exchange(ctx context.Context, code, redirectURI, clientID, clientSecret string) (accessToken string, err error)
	Identity(ctx context.Context, accessToken string) (oauthIdentity, error)
}

// loginProviderOrder is the order buttons render in. It comes from config so
// the credential lookup and the login page cannot disagree about which
// providers exist.
func loginProviderOrder() []string { return config.OAuthProviderKinds() }

func authStartPath(key string) string    { return "/auth/" + key }
func authCallbackPath(key string) string { return "/auth/" + key + "/callback" }

// actorIDFor maps a provider subject onto the actor id space.
//
// Discord's actor id stays BARE ("424242…", not "discord:424242…"): sessions on
// disk, audit rows, webUsers keys, project allowlists, teams and per-user
// capability maps have all keyed on the raw snowflake since before namespaces
// existed, and namespacing it here would be a silent authorization migration.
// Every other provider is namespaced, and the namespaces are per-provider by
// design — GitHub user id 12345 and a Google "sub" of 12345 are different
// people, so folding them into one "oidc:" kind would let either inherit the
// other's team memberships and admin rights.
func actorIDFor(key, subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return ""
	}
	if key == config.ActorKindDiscord {
		return subject
	}
	return key + ":" + subject
}

// provider builds the adapter for a login provider key, injecting the test
// double when one is set. ok is false for an unknown key.
func (s *Server) provider(key string) (oauthProvider, bool) {
	switch key {
	case config.ActorKindDiscord:
		oauth := s.oauth
		if oauth == nil {
			oauth = &HTTPDiscordOAuth{}
		}
		return discordProvider{oauth: oauth}, true
	case config.ActorKindGoogle:
		oauth := s.oauthGoogle
		if oauth == nil {
			oauth = &HTTPGoogleOAuth{}
		}
		return googleProvider{oauth: oauth}, true
	case config.ActorKindGitHub:
		oauth := s.oauthGitHub
		if oauth == nil {
			oauth = &HTTPGitHubOAuth{}
		}
		return githubProvider{oauth: oauth}, true
	}
	return nil, false
}

// providerConfigured is the ONE definition of "this provider is usable".
//
// A hidden button is not a gate, so oauthStart and oauthCallback call this same
// function before they touch the network. The redirect URI is part of it
// because without webPublicBaseURL there is no callback to send anyone to. It
// reads live config and env on every call, so a credential that disappears at
// runtime removes the button and closes the route in the same instant.
func (s *Server) providerConfigured(key string) bool {
	if _, ok := s.provider(key); !ok {
		return false
	}
	return s.cfg.OAuthProviderConfigured(key) && s.oauthRedirectURIFor(key) != ""
}

// oauthRedirectURIFor is the absolute callback URL registered with a provider.
func (s *Server) oauthRedirectURIFor(key string) string {
	base := s.cfg.WebPublicBaseURLValue()
	if base == "" {
		return ""
	}
	return base + authCallbackPath(key)
}

// loginProviderView is one rendered login button.
type loginProviderView struct {
	Key       string
	Label     string
	StartPath string
}

// loginProviders is the rendering half of the gate: the same predicate the
// routes enforce, in a fixed order.
func (s *Server) loginProviders() []loginProviderView {
	var out []loginProviderView
	for _, key := range loginProviderOrder() {
		p, ok := s.provider(key)
		if !ok || !s.providerConfigured(key) {
			continue
		}
		out = append(out, loginProviderView{Key: p.Key(), Label: p.Label(), StartPath: authStartPath(key)})
	}
	return out
}

// safeAvatarURL keeps only http(s) profile images. Providers return their own
// CDN URLs, but this value is rendered into an <img src> and stored durably, so
// an unexpected scheme is dropped rather than passed through.
func safeAvatarURL(raw string) string {
	raw = strings.TrimSpace(raw)
	low := strings.ToLower(raw)
	if strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://") {
		return raw
	}
	return ""
}

// --- Discord ---------------------------------------------------------------

// discordProvider adapts the pre-existing DiscordOAuth onto oauthProvider.
// It must stay a pure shim: every string it produces is byte-identical to what
// the single-provider handlers produced.
type discordProvider struct{ oauth DiscordOAuth }

func (p discordProvider) Key() string   { return config.ActorKindDiscord }
func (p discordProvider) Label() string { return "Discord" }

func (p discordProvider) AuthorizeURL(clientID, redirectURI, state string) string {
	return discordAuthorizeURL(clientID, redirectURI, state)
}

func (p discordProvider) Exchange(ctx context.Context, code, redirectURI, clientID, clientSecret string) (string, error) {
	return p.oauth.ExchangeCode(ctx, code, redirectURI, clientID, clientSecret)
}

func (p discordProvider) Identity(ctx context.Context, accessToken string) (oauthIdentity, error) {
	u, err := p.oauth.FetchUser(ctx, accessToken)
	if err != nil {
		return oauthIdentity{}, err
	}
	return oauthIdentity{Subject: u.ID, Name: u.DisplayName(), AvatarURL: u.AvatarURL()}, nil
}

// --- Google ----------------------------------------------------------------

type googleProvider struct{ oauth GoogleOAuth }

func (p googleProvider) Key() string   { return config.ActorKindGoogle }
func (p googleProvider) Label() string { return "Google" }

func (p googleProvider) AuthorizeURL(clientID, redirectURI, state string) string {
	return googleAuthorizeURL(clientID, redirectURI, state)
}

func (p googleProvider) Exchange(ctx context.Context, code, redirectURI, clientID, clientSecret string) (string, error) {
	return p.oauth.ExchangeCode(ctx, code, redirectURI, clientID, clientSecret)
}

func (p googleProvider) Identity(ctx context.Context, accessToken string) (oauthIdentity, error) {
	u, err := p.oauth.FetchUser(ctx, accessToken)
	if err != nil {
		return oauthIdentity{}, err
	}
	// Subject is "sub" — never the email, which the account owner can change
	// and a domain owner can re-issue to someone else.
	return oauthIdentity{
		Subject:   strings.TrimSpace(u.Sub),
		Name:      u.DisplayName(),
		AvatarURL: safeAvatarURL(u.Picture),
	}, nil
}

// --- GitHub ----------------------------------------------------------------

type githubProvider struct{ oauth GitHubOAuth }

func (p githubProvider) Key() string   { return config.ActorKindGitHub }
func (p githubProvider) Label() string { return "GitHub" }

func (p githubProvider) AuthorizeURL(clientID, redirectURI, state string) string {
	return githubAuthorizeURL(clientID, redirectURI, state)
}

func (p githubProvider) Exchange(ctx context.Context, code, redirectURI, clientID, clientSecret string) (string, error) {
	return p.oauth.ExchangeCode(ctx, code, redirectURI, clientID, clientSecret)
}

func (p githubProvider) Identity(ctx context.Context, accessToken string) (oauthIdentity, error) {
	u, err := p.oauth.FetchUser(ctx, accessToken)
	if err != nil {
		return oauthIdentity{}, err
	}
	// Subject is the numeric id — never the login, which is renameable and
	// whose freed name anyone may re-register.
	return oauthIdentity{
		Subject:   u.Subject(),
		Name:      u.DisplayName(),
		AvatarURL: safeAvatarURL(u.AvatarURL),
	}, nil
}
