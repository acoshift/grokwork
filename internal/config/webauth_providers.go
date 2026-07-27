package config

import (
	"fmt"
	"os"
	"strings"
)

// OAuthProviderConfig is one non-Discord login provider's credentials.
//
// Pointer-valued in WebAuthProviders so an untouched provider writes nothing
// back to config.json: a Discord-only config must round-trip byte-identically
// through the web config UI's save.
type OAuthProviderConfig struct {
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// WebAuthProviders holds the login providers configured beside Discord, whose
// credentials stay at their historical top-level keys (discordClientId /
// discordClientSecret) so no existing config has to be rewritten.
//
// A struct rather than map[string]OAuthProviderConfig: only these keys exist in
// code, and a struct makes a misspelled provider key impossible to mistake for a
// working one in review (json.Unmarshal drops unknown keys either way — Load is
// not strict).
type WebAuthProviders struct {
	Google *OAuthProviderConfig `json:"google,omitempty"`
	GitHub *OAuthProviderConfig `json:"github,omitempty"`
}

// get returns the credentials block for an actor kind, or nil.
func (p *WebAuthProviders) get(kind string) *OAuthProviderConfig {
	if p == nil {
		return nil
	}
	switch kind {
	case ActorKindGoogle:
		return p.Google
	case ActorKindGitHub:
		return p.GitHub
	}
	return nil
}

// oauthProviderKinds is the canonical order login providers are offered in.
// Discord is first because it is the historical provider.
var oauthProviderKinds = []string{ActorKindDiscord, ActorKindGoogle, ActorKindGitHub}

// OAuthProviderKinds returns the login provider actor kinds in display order.
// The web layer renders buttons from this so the config and the login page
// cannot disagree about which providers exist.
func OAuthProviderKinds() []string {
	return append([]string(nil), oauthProviderKinds...)
}

// providerFieldNames names the config fields (and env fallbacks) a provider is
// missing, for the ValidateWebAuth error message.
type providerFieldNames struct {
	id     string
	secret string
}

var providerFields = map[string]providerFieldNames{
	ActorKindDiscord: {
		id:     "discordClientId (or decodable bot token)",
		secret: "discordClientSecret (or DISCORD_CLIENT_SECRET)",
	},
	ActorKindGoogle: {
		id:     "webAuth.providers.google.clientId",
		secret: "webAuth.providers.google.clientSecret (or GOOGLE_CLIENT_SECRET)",
	},
	ActorKindGitHub: {
		id:     "webAuth.providers.github.clientId",
		secret: "webAuth.providers.github.clientSecret (or GITHUB_CLIENT_SECRET)",
	},
}

// providerCredsFromConfig reads one provider's credentials out of webAuth.providers.
func (c *Config) providerCredsFromConfig(kind string) (clientID, clientSecret string) {
	if c == nil {
		return "", ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var p *OAuthProviderConfig
	if c.WebAuth != nil {
		p = c.WebAuth.Providers.get(kind)
	}
	if p == nil {
		return "", ""
	}
	return strings.TrimSpace(p.ClientID), strings.TrimSpace(p.ClientSecret)
}

// GoogleClientIDValue returns the Google OAuth client id (config only).
//
// Client ids are public — they travel in the authorize URL — so unlike the
// secret they get no env fallback, mirroring discordClientId.
func (c *Config) GoogleClientIDValue() string {
	id, _ := c.providerCredsFromConfig(ActorKindGoogle)
	return id
}

// GoogleClientSecretValue returns the Google OAuth client secret (config or env).
// Plain provider name first, then the product-prefixed name — the same asymmetry
// DiscordClientSecretValue has.
func (c *Config) GoogleClientSecretValue() string {
	_, secret := c.providerCredsFromConfig(ActorKindGoogle)
	if secret != "" {
		return secret
	}
	return firstEnv(
		"GOOGLE_CLIENT_SECRET",
		"GROK_WORK_GOOGLE_CLIENT_SECRET",
	)
}

// GitHubClientIDValue returns the GitHub OAuth client id (config only).
func (c *Config) GitHubClientIDValue() string {
	id, _ := c.providerCredsFromConfig(ActorKindGitHub)
	return id
}

// GitHubClientSecretValue returns the GitHub OAuth client secret (config or env).
func (c *Config) GitHubClientSecretValue() string {
	_, secret := c.providerCredsFromConfig(ActorKindGitHub)
	if secret != "" {
		return secret
	}
	return firstEnv(
		"GITHUB_CLIENT_SECRET",
		"GROK_WORK_GITHUB_CLIENT_SECRET",
	)
}

// OAuthProviderCreds is the one credential lookup the web layer calls, keyed by
// actor kind so a provider table row carries no per-provider plumbing.
//
// Never call it while holding c.mu: it delegates to the accessors above, each of
// which takes its own RLock.
func (c *Config) OAuthProviderCreds(kind string) (clientID, clientSecret string) {
	if c == nil {
		return "", ""
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case ActorKindDiscord:
		return strings.TrimSpace(c.EffectiveClientID()), c.DiscordClientSecretValue()
	case ActorKindGoogle:
		return c.GoogleClientIDValue(), c.GoogleClientSecretValue()
	case ActorKindGitHub:
		return c.GitHubClientIDValue(), c.GitHubClientSecretValue()
	}
	return "", ""
}

// OAuthProviderConfigured reports whether a provider has BOTH halves of its
// credentials. Half a credential pair is not a provider: the token exchange
// would fail with an empty secret, so the button must not render and — more
// importantly — the callback route must refuse before it reaches the network.
//
// This reads live config and env on every call, so a credential that disappears
// at runtime closes the route in the same instant it removes the button.
func (c *Config) OAuthProviderConfigured(kind string) bool {
	id, secret := c.OAuthProviderCreds(kind)
	return id != "" && secret != ""
}

// AnyOAuthProviderConfigured reports whether at least one login provider is
// usable. Web auth with zero providers locks everyone out, so Load refuses it.
func (c *Config) AnyOAuthProviderConfigured() bool {
	for _, kind := range oauthProviderKinds {
		if c.OAuthProviderConfigured(kind) {
			return true
		}
	}
	return false
}

// missingProviderFields lists the credential fields ValidateWebAuth should
// complain about.
//
// One complete provider is enough, so a deployment that logs people in with
// Google is not forced to also hold Discord OAuth credentials. When nothing is
// complete we name what was half-attempted; when nothing was attempted at all we
// fall back to the Discord fields, which is what a fresh config is expected to
// fill in.
func (c *Config) missingProviderFields() []string {
	var partial []string
	attempted := false
	for _, kind := range oauthProviderKinds {
		id, secret := c.OAuthProviderCreds(kind)
		if id != "" && secret != "" {
			return nil
		}
		if id == "" && secret == "" {
			continue
		}
		attempted = true
		fields := providerFields[kind]
		if id == "" {
			partial = append(partial, fields.id)
		}
		if secret == "" {
			partial = append(partial, fields.secret)
		}
	}
	if !attempted {
		return []string{
			providerFields[ActorKindDiscord].id,
			providerFields[ActorKindDiscord].secret,
		}
	}
	return partial
}

// warnHalfConfiguredProviders prints a warning for every provider holding
// exactly one half of a credential pair, once some other provider makes the
// config valid. Without it the button simply never appears and there is nothing
// anywhere saying why.
//
// A warning and not a failure: a stray field left behind must not take down a
// deployment that is otherwise logging people in.
func (c *Config) warnHalfConfiguredProviders() {
	if !c.AnyOAuthProviderConfigured() {
		return // ValidateWebAuth is about to fail with a full list
	}
	for _, kind := range oauthProviderKinds {
		id, secret := c.OAuthProviderCreds(kind)
		if (id == "") == (secret == "") {
			continue
		}
		half := "clientSecret"
		if id == "" {
			half = "clientId"
		}
		fmt.Fprintf(os.Stderr,
			"[warn] webAuth: login provider %q is missing its %s — its button will not render "+
				"and /auth/%s will refuse. Set both halves or remove the credential.\n",
			kind, half, kind)
	}
}

func cloneWebAuthProviders(p *WebAuthProviders) *WebAuthProviders {
	if p == nil {
		return nil
	}
	return &WebAuthProviders{
		Google: cloneOAuthProviderConfig(p.Google),
		GitHub: cloneOAuthProviderConfig(p.GitHub),
	}
}

func cloneOAuthProviderConfig(p *OAuthProviderConfig) *OAuthProviderConfig {
	if p == nil {
		return nil
	}
	out := *p
	return &out
}
