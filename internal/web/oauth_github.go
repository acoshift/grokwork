package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	githubAuthorizeEndpoint = "https://github.com/login/oauth/authorize"
	githubTokenEndpoint     = "https://github.com/login/oauth/access_token"
	githubUserEndpoint      = "https://api.github.com/user"
	githubAPIVersion        = "2022-11-28"
	// read:user is enough for id + name + login + avatar. user:email is
	// deliberately NOT requested: display already falls back to the login, which
	// always exists, so the extra scope would buy nothing but a private address
	// we would then be storing.
	githubScopes = "read:user"
)

// GitHubUser is the identity returned by GitHub's /user.
//
// ID is the account's immutable numeric id and the only field that may reach an
// allowlist. Login is a handle: a user can rename it, and the freed name is
// immediately registrable by somebody else — an allowlist naming a login would
// hand that stranger the departed user's access.
type GitHubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// DisplayName prefers the profile name, then the login handle.
func (u GitHubUser) DisplayName() string {
	if n := strings.TrimSpace(u.Name); n != "" {
		return n
	}
	return strings.TrimSpace(u.Login)
}

// Subject is the immutable actor subject for this account.
func (u GitHubUser) Subject() string {
	if u.ID <= 0 {
		return ""
	}
	return strconv.FormatInt(u.ID, 10)
}

// GitHubOAuth exchanges codes and fetches the current user.
// Tests inject fakes; production uses HTTPGitHubOAuth.
type GitHubOAuth interface {
	ExchangeCode(ctx context.Context, code, redirectURI, clientID, clientSecret string) (accessToken string, err error)
	FetchUser(ctx context.Context, accessToken string) (GitHubUser, error)
}

// HTTPGitHubOAuth talks to github.com.
type HTTPGitHubOAuth struct {
	HTTPClient *http.Client
	// Endpoint overrides for tests (empty → GitHub's real endpoints).
	tokenURL string
	userURL  string
}

func (h *HTTPGitHubOAuth) client() *http.Client {
	if h != nil && h.HTTPClient != nil {
		return h.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (h *HTTPGitHubOAuth) token() string {
	if h != nil && h.tokenURL != "" {
		return h.tokenURL
	}
	return githubTokenEndpoint
}

func (h *HTTPGitHubOAuth) user() string {
	if h != nil && h.userURL != "" {
		return h.userURL
	}
	return githubUserEndpoint
}

func (h *HTTPGitHubOAuth) ExchangeCode(ctx context.Context, code, redirectURI, clientID, clientSecret string) (string, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.token(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Without this GitHub answers form-encoded, and the JSON decode below would
	// quietly produce an empty access_token.
	req.Header.Set("Accept", "application/json")
	res, err := h.client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		// Status only, never the body — it can echo the code back.
		return "", fmt.Errorf("github token exchange: status %d", res.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("github token json: %w", err)
	}
	// GitHub reports a bad or replayed code with HTTP 200 and an "error" field.
	// Checking only the status leaves access_token empty and surfaces the
	// failure later as a confusing profile error.
	if e := strings.TrimSpace(tok.Error); e != "" {
		return "", fmt.Errorf("github token exchange: %s", e)
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return "", fmt.Errorf("github token exchange: empty access_token")
	}
	return tok.AccessToken, nil
}

func (h *HTTPGitHubOAuth) FetchUser(ctx context.Context, accessToken string) (GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.user(), nil)
	if err != nil {
		return GitHubUser{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	res, err := h.client().Do(req)
	if err != nil {
		return GitHubUser{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return GitHubUser{}, fmt.Errorf("github user: status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var u GitHubUser
	if err := json.Unmarshal(body, &u); err != nil {
		return GitHubUser{}, fmt.Errorf("github user json: %w", err)
	}
	if u.Subject() == "" {
		// Refuse rather than fall back to the login: an id of 0 means the field
		// was absent, and a handle is not an identity.
		return GitHubUser{}, fmt.Errorf("github user: missing numeric id")
	}
	return u, nil
}

// FakeGitHubOAuth is a test double for the exchange + /user pair.
type FakeGitHubOAuth struct {
	// CodeToUser maps authorization code → user (token step is simulated).
	CodeToUser map[string]GitHubUser
	// FailExchange / FailUser force errors.
	FailExchange error
	FailUser     error
	// Call counters let a test prove a refused route never reached the network.
	Exchanges int
	Fetches   int
}

func (f *FakeGitHubOAuth) ExchangeCode(_ context.Context, code, _, _, _ string) (string, error) {
	f.Exchanges++
	if f.FailExchange != nil {
		return "", f.FailExchange
	}
	if _, ok := f.CodeToUser[code]; !ok {
		return "", fmt.Errorf("unknown code %q", code)
	}
	return "fake-github-" + code, nil
}

func (f *FakeGitHubOAuth) FetchUser(_ context.Context, accessToken string) (GitHubUser, error) {
	f.Fetches++
	if f.FailUser != nil {
		return GitHubUser{}, f.FailUser
	}
	code := strings.TrimPrefix(accessToken, "fake-github-")
	u, ok := f.CodeToUser[code]
	if !ok {
		return GitHubUser{}, fmt.Errorf("unknown token")
	}
	return u, nil
}

func githubAuthorizeURL(clientID, redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("scope", githubScopes)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	return githubAuthorizeEndpoint + "?" + q.Encode()
}
