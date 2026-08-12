package gdrive

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeSAKey(t *testing.T, dir string, tokenURI string) (path string, key *rsa.PrivateKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	body := map[string]string{
		"client_email":   "sa@example.iam.gserviceaccount.com",
		"private_key":    string(pemBytes),
		"private_key_id": "kid-1",
		"token_uri":      tokenURI,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "sa.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, priv
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body any) *http.Response {
	raw, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	}
}

func TestJWTBearerTokenExchangeAndCache(t *testing.T) {
	var mu sync.Mutex
	var posts int
	var lastForm url.Values

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		posts++
		lastForm = form
		mu.Unlock()
		if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Fatalf("grant_type = %q", form.Get("grant_type"))
		}
		if form.Get("assertion") == "" {
			t.Fatal("missing assertion")
		}
		// Assertion is three base64 segments.
		parts := strings.Split(form.Get("assertion"), ".")
		if len(parts) != 3 {
			t.Fatalf("jwt parts = %d", len(parts))
		}
		return jsonResponse(200, map[string]any{
			"access_token": "tok-abc",
			"expires_in":   3600,
		}), nil
	})

	// Use a custom token URI so the fake RoundTripper receives the exchange.
	// The JWTBearer POSTs to token_uri from the key file.
	dir := t.TempDir()
	// We intercept via HTTP client — token URI must be reachable via that client.
	// Point token_uri at a dummy host; RoundTripper sees every request.
	path, _ := writeSAKey(t, dir, "https://oauth.test/token")

	j := &JWTBearer{
		CredentialsFile: path,
		HTTP:            &http.Client{Transport: rt, Timeout: 5 * time.Second},
	}
	ctx := t.Context()
	tok1, err := j.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != "tok-abc" {
		t.Fatalf("token = %q", tok1)
	}
	tok2, err := j.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != tok1 {
		t.Fatal("cache miss")
	}
	mu.Lock()
	n := posts
	mu.Unlock()
	if n != 1 {
		t.Fatalf("token posts = %d, want 1 (cached)", n)
	}
	if lastForm.Get("assertion") == "" {
		t.Fatal("assertion empty")
	}
}

func TestJWTBearerErrorDoesNotLeakAssertion(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(400, map[string]any{
			"error":             "invalid_grant",
			"error_description": "bad jwt",
		}), nil
	})
	dir := t.TempDir()
	path, _ := writeSAKey(t, dir, "https://oauth.test/token")
	j := &JWTBearer{
		CredentialsFile: path,
		HTTP:            &http.Client{Transport: rt},
	}
	_, err := j.Token(t.Context())
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	if strings.Contains(msg, "BEGIN PRIVATE KEY") || strings.Contains(msg, "assertion=") {
		t.Fatalf("error leaked secrets: %v", err)
	}
	if !strings.Contains(msg, "invalid_grant") {
		t.Fatalf("error = %v", err)
	}
}

func TestParsePKCS8RSA(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	got, err := parsePKCS8RSA(string(pemBytes))
	if err != nil {
		t.Fatal(err)
	}
	if got.N.Cmp(priv.N) != 0 {
		t.Fatal("key mismatch")
	}
}
