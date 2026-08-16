package gcperr

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	defaultTokenURI    = "https://oauth2.googleapis.com/token"
	metadataTokenURL   = "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token"
)

// TokenSource mints a Bearer token. Tests inject fakes.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

var tokenSources sync.Map // credentials path (or "\x00adc") → *FileTokenSource

// TokenSourceFor returns a process-wide cached source for the given key path
// (empty = ADC). Tests that need custom env/metadata should construct
// FileTokenSource directly.
func TokenSourceFor(credentialsFile string) *FileTokenSource {
	key := strings.TrimSpace(credentialsFile)
	if key == "" {
		key = "\x00adc"
	}
	if v, ok := tokenSources.Load(key); ok {
		return v.(*FileTokenSource)
	}
	s := &FileTokenSource{CredentialsFile: strings.TrimSpace(credentialsFile)}
	actual, _ := tokenSources.LoadOrStore(key, s)
	return actual.(*FileTokenSource)
}

// FileTokenSource reads credentialsFile, GOOGLE_APPLICATION_CREDENTIALS,
// the well-known ADC file, then GCE metadata. A configured path that fails
// to read must not fall through.
type FileTokenSource struct {
	CredentialsFile string
	HTTP            *http.Client
	// LookupEnv / Home / CloudSDKConfig are seams for tests.
	LookupEnv      func(string) string
	Home           string
	CloudSDKConfig string
	MetadataURL    string // tests only; empty → metadataTokenURL

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func (s *FileTokenSource) env(k string) string {
	if s != nil && s.LookupEnv != nil {
		return s.LookupEnv(k)
	}
	return os.Getenv(k)
}

func (s *FileTokenSource) Token(ctx context.Context) (string, error) {
	if s == nil {
		return "", fmt.Errorf("gcp: nil token source")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expiry.Add(-60*time.Second)) {
		return s.token, nil
	}
	tok, exp, err := s.resolve(ctx)
	if err != nil {
		return "", err
	}
	s.token, s.expiry = tok, exp
	return tok, nil
}

func (s *FileTokenSource) resolve(ctx context.Context) (string, time.Time, error) {
	if path := strings.TrimSpace(s.CredentialsFile); path != "" {
		return s.fromFile(ctx, path)
	}
	if path := strings.TrimSpace(s.env("GOOGLE_APPLICATION_CREDENTIALS")); path != "" {
		return s.fromFile(ctx, path)
	}
	if path := s.wellKnownADC(); path != "" {
		if _, err := os.Stat(path); err == nil {
			return s.fromFile(ctx, path)
		}
	}
	return s.fromMetadata(ctx)
}

func (s *FileTokenSource) wellKnownADC() string {
	if cfg := strings.TrimSpace(s.CloudSDKConfig); cfg != "" {
		return filepath.Join(cfg, "application_default_credentials.json")
	}
	if env := strings.TrimSpace(s.env("CLOUDSDK_CONFIG")); env != "" {
		return filepath.Join(env, "application_default_credentials.json")
	}
	home := s.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
}

func (s *FileTokenSource) fromFile(ctx context.Context, path string) (string, time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gcp: read credentials: %w", err)
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", time.Time{}, fmt.Errorf("gcp: parse credentials: %w", err)
	}
	switch probe.Type {
	case "authorized_user":
		return s.refreshAuthorizedUser(ctx, raw)
	default:
		return s.jwtBearer(ctx, raw)
	}
}

type saKeyFile struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
}

func (s *FileTokenSource) jwtBearer(ctx context.Context, raw []byte) (string, time.Time, error) {
	var key saKeyFile
	if err := json.Unmarshal(raw, &key); err != nil {
		return "", time.Time{}, fmt.Errorf("gcp: parse sa credentials: %w", err)
	}
	if strings.TrimSpace(key.ClientEmail) == "" || strings.TrimSpace(key.PrivateKey) == "" {
		return "", time.Time{}, fmt.Errorf("gcp: credentials missing client_email or private_key")
	}
	tokenURI := strings.TrimSpace(key.TokenURI)
	if tokenURI == "" {
		tokenURI = defaultTokenURI
	}
	priv, err := parsePKCS8RSA(key.PrivateKey)
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": key.PrivateKeyID})
	claims, _ := json.Marshal(map[string]any{
		"iss": key.ClientEmail, "scope": cloudPlatformScope, "aud": tokenURI,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", time.Time{}, err
	}
	assertion := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	return s.tokenPOST(ctx, tokenURI, form)
}

type authorizedUserFile struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	TokenURI     string `json:"token_uri"`
}

func (s *FileTokenSource) refreshAuthorizedUser(ctx context.Context, raw []byte) (string, time.Time, error) {
	var key authorizedUserFile
	if err := json.Unmarshal(raw, &key); err != nil {
		return "", time.Time{}, err
	}
	if key.RefreshToken == "" || key.ClientID == "" || key.ClientSecret == "" {
		return "", time.Time{}, fmt.Errorf("gcp: authorized_user credentials incomplete")
	}
	tokenURI := strings.TrimSpace(key.TokenURI)
	if tokenURI == "" {
		tokenURI = defaultTokenURI
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {key.RefreshToken},
		"client_id":     {key.ClientID},
		"client_secret": {key.ClientSecret},
	}
	return s.tokenPOST(ctx, tokenURI, form)
}

func (s *FileTokenSource) fromMetadata(ctx context.Context) (string, time.Time, error) {
	meta := strings.TrimSpace(s.MetadataURL)
	if meta == "" {
		meta = metadataTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	httpClient := s.http()
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gcp: metadata token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", time.Time{}, fmt.Errorf("gcp: metadata HTTP %d", resp.StatusCode)
	}
	return parseTokenJSON(body)
}

func (s *FileTokenSource) tokenPOST(ctx context.Context, tokenURI string, form url.Values) (string, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http().Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", time.Time{}, fmt.Errorf("gcp: token HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return parseTokenJSON(body)
}

func parseTokenJSON(body []byte) (string, time.Time, error) {
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, err
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("gcp: empty access_token")
	}
	exp := time.Now().Add(time.Hour)
	if tr.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return tr.AccessToken, exp, nil
}

func (s *FileTokenSource) http() *http.Client {
	if s != nil && s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func parsePKCS8RSA(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("gcp: no PEM block in private_key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if k, err2 := x509.ParsePKCS1PrivateKey(block.Bytes); err2 == nil {
			return k, nil
		}
		return nil, fmt.Errorf("gcp: parse private_key: %w", err)
	}
	rk, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("gcp: private_key is not RSA")
	}
	return rk, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
