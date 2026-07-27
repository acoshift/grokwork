package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Google OAuth 2.0 / OpenID Connect endpoints. Not constants on the struct so
// tests can point HTTPGoogleOAuth at an httptest.Server (see tokenURL/userURL).
const (
	googleAuthorizeEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenEndpoint     = "https://oauth2.googleapis.com/token"
	googleUserInfoEndpoint  = "https://openidconnect.googleapis.com/v1/userinfo"
	// openid+email+profile: "sub" needs openid, and the other two are display
	// metadata only (name / picture / email).
	googleScopes = "openid email profile"
)

// GoogleUser is the identity returned by Google's OIDC userinfo endpoint.
//
// Sub is the immutable subject identifier and the only field that may reach an
// allowlist. Email is display metadata: an address can be changed by its owner
// and, on a corporate domain, re-issued to a different person entirely.
type GoogleUser struct {
	Sub     string `json:"sub"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	Picture string `json:"picture"`
}

// DisplayName prefers the profile name, then the email address.
func (u GoogleUser) DisplayName() string {
	if n := strings.TrimSpace(u.Name); n != "" {
		return n
	}
	return strings.TrimSpace(u.Email)
}

// GoogleOAuth exchanges codes and fetches the current user.
// Tests inject fakes; production uses HTTPGoogleOAuth.
type GoogleOAuth interface {
	ExchangeCode(ctx context.Context, code, redirectURI, clientID, clientSecret string) (accessToken string, err error)
	FetchUser(ctx context.Context, accessToken string) (GoogleUser, error)
}

// HTTPGoogleOAuth talks to accounts.google.com.
//
// It reads the subject from the userinfo endpoint rather than parsing the
// id_token JWT. The token response is fetched server-side straight from Google
// over TLS, so OIDC permits skipping signature validation — but hand-rolling
// base64url JWT decoding to save one HTTP call is a needless place to get
// "trust the payload" wrong, and userinfo yields the same "sub".
type HTTPGoogleOAuth struct {
	HTTPClient *http.Client
	// Endpoint overrides for tests (empty → Google's real endpoints).
	tokenURL string
	userURL  string
}

func (h *HTTPGoogleOAuth) client() *http.Client {
	if h != nil && h.HTTPClient != nil {
		return h.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (h *HTTPGoogleOAuth) token() string {
	if h != nil && h.tokenURL != "" {
		return h.tokenURL
	}
	return googleTokenEndpoint
}

func (h *HTTPGoogleOAuth) userinfo() string {
	if h != nil && h.userURL != "" {
		return h.userURL
	}
	return googleUserInfoEndpoint
}

func (h *HTTPGoogleOAuth) ExchangeCode(ctx context.Context, code, redirectURI, clientID, clientSecret string) (string, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.token(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := h.client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		// Status only, never the body: a token-endpoint error can quote the
		// authorization code (and the client_id) straight back at us, and this
		// error text ends up in a log line.
		return "", fmt.Errorf("google token exchange: status %d", res.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("google token json: %w", err)
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return "", fmt.Errorf("google token exchange: empty access_token")
	}
	return tok.AccessToken, nil
}

func (h *HTTPGoogleOAuth) FetchUser(ctx context.Context, accessToken string) (GoogleUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.userinfo(), nil)
	if err != nil {
		return GoogleUser{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	res, err := h.client().Do(req)
	if err != nil {
		return GoogleUser{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return GoogleUser{}, fmt.Errorf("google userinfo: status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var u GoogleUser
	if err := json.Unmarshal(body, &u); err != nil {
		return GoogleUser{}, fmt.Errorf("google userinfo json: %w", err)
	}
	if strings.TrimSpace(u.Sub) == "" {
		// No subject means no identity. Falling back to the email here would
		// turn a mutable, re-issuable address into an authorization key.
		return GoogleUser{}, fmt.Errorf("google userinfo: empty sub")
	}
	return u, nil
}

// FakeGoogleOAuth is a test double for the exchange + userinfo pair.
type FakeGoogleOAuth struct {
	// CodeToUser maps authorization code → user (token step is simulated).
	CodeToUser map[string]GoogleUser
	// FailExchange / FailUser force errors.
	FailExchange error
	FailUser     error
	// Call counters let a test prove a refused route never reached the network.
	Exchanges int
	Fetches   int
}

func (f *FakeGoogleOAuth) ExchangeCode(_ context.Context, code, _, _, _ string) (string, error) {
	f.Exchanges++
	if f.FailExchange != nil {
		return "", f.FailExchange
	}
	if _, ok := f.CodeToUser[code]; !ok {
		return "", fmt.Errorf("unknown code %q", code)
	}
	return "fake-google-" + code, nil
}

func (f *FakeGoogleOAuth) FetchUser(_ context.Context, accessToken string) (GoogleUser, error) {
	f.Fetches++
	if f.FailUser != nil {
		return GoogleUser{}, f.FailUser
	}
	code := strings.TrimPrefix(accessToken, "fake-google-")
	u, ok := f.CodeToUser[code]
	if !ok {
		return GoogleUser{}, fmt.Errorf("unknown token")
	}
	return u, nil
}

func googleAuthorizeURL(clientID, redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("scope", googleScopes)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	return googleAuthorizeEndpoint + "?" + q.Encode()
}
