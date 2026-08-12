package gdrive

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
	"strings"
	"sync"
	"time"
)

// DefaultDriveScope is the OAuth scope for Shared Drive access.
const DefaultDriveScope = "https://www.googleapis.com/auth/drive"

const defaultTokenURI = "https://oauth2.googleapis.com/token"

// TokenSource mints Bearer tokens for Drive API calls.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// JWTBearer reads a Google service-account JSON key file and exchanges a
// signed JWT for an access token (OAuth 2.0 service account flow).
type JWTBearer struct {
	CredentialsFile string
	// Scope default: https://www.googleapis.com/auth/drive
	Scope string
	HTTP  *http.Client

	mu          sync.Mutex
	accessToken string
	expiry      time.Time
}

type saKeyFile struct {
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
}

// Token returns a cached access token or mints a new one.
func (j *JWTBearer) Token(ctx context.Context) (string, error) {
	if j == nil {
		return "", fmt.Errorf("drive: nil JWTBearer")
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.accessToken != "" && time.Now().Before(j.expiry.Add(-60*time.Second)) {
		return j.accessToken, nil
	}

	path := strings.TrimSpace(j.CredentialsFile)
	if path == "" {
		return "", fmt.Errorf("drive: credentials file is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("drive: read credentials: %w", err)
	}
	var key saKeyFile
	if err := json.Unmarshal(raw, &key); err != nil {
		return "", fmt.Errorf("drive: parse credentials: %w", err)
	}
	if strings.TrimSpace(key.ClientEmail) == "" || strings.TrimSpace(key.PrivateKey) == "" {
		return "", fmt.Errorf("drive: credentials missing client_email or private_key")
	}
	tokenURI := strings.TrimSpace(key.TokenURI)
	if tokenURI == "" {
		tokenURI = defaultTokenURI
	}
	scope := strings.TrimSpace(j.Scope)
	if scope == "" {
		scope = DefaultDriveScope
	}

	priv, err := parsePKCS8RSA(key.PrivateKey)
	if err != nil {
		return "", err
	}

	now := time.Now()
	header := map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	}
	if kid := strings.TrimSpace(key.PrivateKeyID); kid != "" {
		header["kid"] = kid
	}
	claims := map[string]any{
		"iss":   key.ClientEmail,
		"scope": scope,
		"aud":   tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		// No sub claim — domain-wide delegation is out of scope.
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("drive: sign jwt: %w", err)
	}
	assertion := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	httpClient := j.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("drive: token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("drive: token exchange read: %w", err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("drive: token exchange decode: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tok.AccessToken == "" {
		reason := tok.Error
		if tok.ErrorDesc != "" {
			if reason != "" {
				reason += ": " + tok.ErrorDesc
			} else {
				reason = tok.ErrorDesc
			}
		}
		if reason == "" {
			reason = fmt.Sprintf("status %d", resp.StatusCode)
		}
		// Never include the assertion JWT or private key in the error.
		return "", fmt.Errorf("drive: token exchange failed: %s", reason)
	}
	expiresIn := tok.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	j.accessToken = tok.AccessToken
	j.expiry = now.Add(time.Duration(expiresIn) * time.Second)
	return j.accessToken, nil
}

func parsePKCS8RSA(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("drive: private_key is not a valid PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("drive: parse PKCS#8 private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("drive: private key is not RSA")
	}
	return rsaKey, nil
}
