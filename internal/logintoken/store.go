// Package logintoken is the system of record for browser login tokens.
//
// These are not machine API tokens (internal/apitoken). A login token admits
// a browser session for the account that minted it. Only a hash is stored;
// the plaintext exists in the Mint return value and nowhere else.
package logintoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoshift/grokwork/internal/atomicfile"
	"github.com/acoshift/grokwork/internal/config"
)

// FileName is the store file inside the data dir.
const FileName = "login-tokens.json"

const (
	wirePrefix  = "gl_"
	publicIDLen = 16 // hex chars
	secretBytes = 32
)

var (
	// ErrInvalidToken is an unknown token or a secret that does not match.
	ErrInvalidToken = errors.New("invalid token")
	// ErrRevoked is a token the owner has revoked.
	ErrRevoked = errors.New("token revoked")
	// ErrExpired is a token past its expiry.
	ErrExpired = errors.New("token expired")
	// ErrPastExpiry is a mint or parse whose expiry is not in the future.
	ErrPastExpiry = errors.New("expiry is in the past")
	// ErrBadExpiry is an expiry value that is not a date or a datetime.
	ErrBadExpiry = errors.New("invalid expiry")
	// ErrNotFound is a revoke of an id this store does not have.
	ErrNotFound = errors.New("token not found")
	// ErrNotOwner is a revoke by an account that did not mint the token.
	ErrNotOwner = errors.New("token belongs to another account")
)

// authError is a failed Authenticate. ID is set only after the secret matched,
// so a wrong guess never confirms that a public id exists.
type authError struct {
	reason error
	id     string
}

func (e *authError) Error() string {
	if e == nil || e.reason == nil {
		return ErrInvalidToken.Error()
	}
	return e.reason.Error()
}

func (e *authError) Unwrap() error { return e.reason }

// AuthTokenID returns the public id of a failed Authenticate when the secret
// matched (revoked or expired). It is empty for an unknown token or a wrong secret.
func AuthTokenID(err error) string {
	ae, ok := errors.AsType[*authError](err)
	if !ok {
		return ""
	}
	return ae.id
}

// Record is one login token. Callers outside the store receive a copy with no hash.
type Record struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"ownerId"`
	TokenHash string    `json:"tokenHash,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	RevokedAt time.Time `json:"revokedAt,omitzero"`
}

// MintOpts is input for Mint. A zero ExpiresAt means the token does not expire.
type MintOpts struct {
	OwnerID   string
	ExpiresAt time.Time
}

type storeFile struct {
	Tokens map[string]Record `json:"tokens"`
}

// Store is the hashed login-token file.
type Store struct {
	mu       sync.RWMutex
	filePath string
	tokens   map[string]Record
	now      func() time.Time
}

// New loads or creates data/login-tokens.json. Unparseable JSON is a hard error.
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		filePath: filepath.Join(dataDir, FileName),
		tokens:   map[string]Record{},
		now:      time.Now,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetNow replaces the clock. Tests move time instead of sleeping.
// A nil fn restores time.Now.
func (s *Store) SetNow(now func() time.Time) {
	if s == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// Now returns the store clock.
func (s *Store) Now() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.now()
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var f storeFile
	if err := jsonv2.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("parse %s: %w", FileName, err)
	}
	s.tokens = f.Tokens
	if s.tokens == nil {
		s.tokens = map[string]Record{}
	}
	return nil
}

func (s *Store) saveLocked() error {
	raw, err := jsonv2.Marshal(storeFile{Tokens: s.tokens}, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicfile.Write(s.filePath, raw, 0o600)
}

// ParseExpiry interprets a form expiry.
//
// Empty means no expiry. A date-only value (YYYY-MM-DD) is valid through the
// end of that calendar date in loc: the returned instant is the start of the
// next local day, and login succeeds while now is strictly before it.
// A datetime (YYYY-MM-DDTHH:MM[:SS], or RFC3339) is that instant.
// An expiry that is not strictly after now is ErrPastExpiry.
func ParseExpiry(raw string, now time.Time, loc *time.Location) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if loc == nil {
		loc = time.Local
	}
	if t, err := time.ParseInLocation("2006-01-02", raw, loc); err == nil {
		end := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		if !end.After(now) {
			return time.Time{}, ErrPastExpiry
		}
		return end, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
		t, err := time.ParseInLocation(layout, raw, loc)
		if err != nil {
			continue
		}
		if !t.After(now) {
			return time.Time{}, ErrPastExpiry
		}
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		if !t.After(now) {
			return time.Time{}, ErrPastExpiry
		}
		return t, nil
	}
	return time.Time{}, ErrBadExpiry
}

// Mint creates a token, returns the plaintext once, and stores only the hash.
// A non-zero ExpiresAt that is not after the store clock is refused and stored
// nowhere.
func (s *Store) Mint(opts MintOpts) (wire string, rec Record, err error) {
	if s == nil {
		return "", Record{}, fmt.Errorf("login token store is not configured")
	}
	owner := strings.TrimSpace(opts.OwnerID)
	if owner == "" {
		return "", Record{}, fmt.Errorf("owner is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var expires time.Time
	if !opts.ExpiresAt.IsZero() {
		if !opts.ExpiresAt.After(now) {
			return "", Record{}, ErrPastExpiry
		}
		expires = opts.ExpiresAt.UTC()
	}
	id, err := newPublicID()
	if err != nil {
		return "", Record{}, err
	}
	if _, exists := s.tokens[id]; exists {
		return "", Record{}, fmt.Errorf("token id collision")
	}
	secret, err := newSecret()
	if err != nil {
		return "", Record{}, err
	}
	wire = wirePrefix + id + "_" + secret
	rec = Record{
		ID:        id,
		OwnerID:   owner,
		TokenHash: sha256Hex(wire),
		CreatedAt: now.UTC(),
		ExpiresAt: expires,
	}
	s.tokens[id] = rec
	if err := s.saveLocked(); err != nil {
		delete(s.tokens, id)
		return "", Record{}, err
	}
	return wire, rec.public(), nil
}

// Authenticate verifies a plaintext token. The returned record has no hash.
// Unknown, wrong, revoked, and expired tokens all fail. The error's public id
// is available via AuthTokenID only when the secret matched.
func (s *Store) Authenticate(wire string) (Record, error) {
	if s == nil {
		return Record{}, &authError{reason: ErrInvalidToken}
	}
	id, _, parsed := parseWire(wire)
	got := sha256Hex(strings.TrimSpace(wire))
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, found := s.tokens[id]
	want := dummyHash
	if found && rec.TokenHash != "" {
		want = rec.TokenHash
	}
	match := subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	if !parsed || !found || !match {
		return Record{}, &authError{reason: ErrInvalidToken}
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if !rec.RevokedAt.IsZero() {
		return Record{}, &authError{reason: ErrRevoked, id: rec.ID}
	}
	if !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) {
		return Record{}, &authError{reason: ErrExpired, id: rec.ID}
	}
	return rec.public(), nil
}

// ListByOwner returns the account's tokens, newest first, with no hashes.
// Another account's tokens are not included.
func (s *Store) ListByOwner(ownerID string) []Record {
	if s == nil {
		return nil
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0)
	for _, rec := range s.tokens {
		if !config.SameActor(rec.OwnerID, ownerID) {
			continue
		}
		out = append(out, rec.public())
	}
	slices.SortFunc(out, func(a, b Record) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Revoke stamps RevokedAt on one of owner's tokens. A second revoke is a no-op.
// An id owned by someone else is left unchanged.
func (s *Store) Revoke(ownerID, id string) error {
	if s == nil {
		return fmt.Errorf("login token store is not configured")
	}
	ownerID = strings.TrimSpace(ownerID)
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tokens[id]
	if !ok {
		return ErrNotFound
	}
	if !config.SameActor(rec.OwnerID, ownerID) {
		return ErrNotOwner
	}
	if !rec.RevokedAt.IsZero() {
		return nil
	}
	prev := rec
	rec.RevokedAt = s.now().UTC()
	s.tokens[id] = rec
	if err := s.saveLocked(); err != nil {
		s.tokens[id] = prev
		return err
	}
	return nil
}

func (r Record) public() Record {
	r.TokenHash = ""
	return r
}

func parseWire(wire string) (id, secret string, ok bool) {
	wire = strings.TrimSpace(wire)
	rest, found := strings.CutPrefix(wire, wirePrefix)
	if !found {
		return "", "", false
	}
	id, secret, found = strings.Cut(rest, "_")
	if !found || len(id) != publicIDLen || secret == "" {
		return "", "", false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", "", false
		}
	}
	return id, secret, true
}

func newPublicID() (string, error) {
	var b [publicIDLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func newSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// dummyHash keeps the compare length stable when the id is unknown.
var dummyHash = sha256Hex("gl_missing_dummyplaceholder")
