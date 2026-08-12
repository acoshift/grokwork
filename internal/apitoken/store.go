// Package apitoken is the system of record for machine API tokens.
package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
const FileName = "api-tokens.json"

const (
	wirePrefix     = "gw_"
	publicIDBytes  = 5 // 10 hex chars
	secretBytes    = 32
	lastUsedMinGap = time.Minute
	idemTTL        = 24 * time.Hour
	idemCap        = 256
)

// ErrUnauthorized is a missing, bad, expired, or revoked token.
var ErrUnauthorized = errors.New("unauthorized")

// ErrIdempotencyConflict is the same Idempotency-Key with a different body.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different body")

// Record is one token. Auth callers receive a public copy with no hash.
type Record struct {
	ID          string                `json:"id"`
	Label       string                `json:"label"`
	TokenHash   string                `json:"tokenHash"`
	ActorID     string                `json:"actorId"`
	Projects    []string              `json:"projects"`
	Caps        CapsMask              `json:"capabilities"`
	CreatedAt   time.Time             `json:"createdAt"`
	CreatedBy   string                `json:"createdBy"`
	ExpiresAt   time.Time             `json:"expiresAt,omitzero"`
	LastUsedAt  time.Time             `json:"lastUsedAt,omitzero"`
	RevokedAt   time.Time             `json:"revokedAt,omitzero"`
	Idempotency map[string]IdemRecord `json:"idempotency,omitempty"`
}

// IdemRecord is one cached creating-POST response.
type IdemRecord struct {
	BodyHash  string    `json:"bodyHash"`
	Status    int       `json:"status"`
	Response  []byte    `json:"response"`
	CreatedAt time.Time `json:"createdAt"`
}

// MintOpts is input for Mint.
type MintOpts struct {
	Label     string
	Projects  []string
	Caps      CapsMask
	CreatedBy string
	ExpiresAt time.Time
}

type storeFile struct {
	Tokens map[string]Record `json:"tokens"`
}

// Store is the hashed token file.
type Store struct {
	mu       sync.RWMutex
	filePath string
	tokens   map[string]Record
	now      func() time.Time
}

// New loads or creates data/api-tokens.json. Unparseable JSON is a hard error.
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
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
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("parse %s: %w", FileName, err)
	}
	s.tokens = f.Tokens
	if s.tokens == nil {
		s.tokens = map[string]Record{}
	}
	return nil
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(storeFile{Tokens: s.tokens}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicfile.Write(s.filePath, raw, 0o600)
}

// Mint creates a token, returns the wire secret once, and stores only the hash.
func (s *Store) Mint(opts MintOpts) (wire string, rec Record, err error) {
	if s == nil {
		return "", Record{}, fmt.Errorf("token store is not configured")
	}
	id, err := newPublicID()
	if err != nil {
		return "", Record{}, err
	}
	secret, err := newSecret()
	if err != nil {
		return "", Record{}, err
	}
	wire = wirePrefix + id + "_" + secret
	now := s.now().UTC()
	rec = Record{
		ID:        id,
		Label:     strings.TrimSpace(opts.Label),
		TokenHash: sha256Hex(wire),
		ActorID:   config.NormalizeActorID("token:" + id),
		Projects:  compactProjects(opts.Projects),
		Caps:      opts.Caps,
		CreatedAt: now,
		CreatedBy: strings.TrimSpace(opts.CreatedBy),
		ExpiresAt: opts.ExpiresAt.UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tokens[id]; exists {
		return "", Record{}, fmt.Errorf("token id collision")
	}
	s.tokens[id] = rec
	if err := s.saveLocked(); err != nil {
		delete(s.tokens, id)
		return "", Record{}, err
	}
	return wire, rec.publicCopy(), nil
}

// Authenticate verifies a full wire token. The returned record has no hash.
func (s *Store) Authenticate(wire string) (Record, error) {
	if s == nil {
		return Record{}, ErrUnauthorized
	}
	id, _, ok := parseWire(wire)
	got := sha256Hex(wire)
	s.mu.RLock()
	rec, found := s.tokens[id]
	s.mu.RUnlock()
	want := dummyHash
	if found && rec.TokenHash != "" {
		want = rec.TokenHash
	}
	match := subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	if !ok || !found || !match {
		return Record{}, ErrUnauthorized
	}
	now := s.now()
	if !rec.RevokedAt.IsZero() {
		return Record{}, ErrUnauthorized
	}
	if !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) {
		return Record{}, ErrUnauthorized
	}
	s.maybeTouch(id, now)
	return rec.publicCopy(), nil
}

// Revoke stamps RevokedAt. The row stays as a tombstone (K21).
func (s *Store) Revoke(id string) error {
	if s == nil {
		return fmt.Errorf("token store is not configured")
	}
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tokens[id]
	if !ok {
		return fmt.Errorf("token %q not found", id)
	}
	if rec.RevokedAt.IsZero() {
		rec.RevokedAt = s.now().UTC()
		s.tokens[id] = rec
		if err := s.saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// Get returns a public copy by public id.
func (s *Store) Get(id string) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.tokens[strings.TrimSpace(id)]
	if !ok {
		return Record{}, false
	}
	return rec.publicCopy(), true
}

// List returns public copies, newest first.
func (s *Store) List() []Record {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.tokens))
	for _, rec := range s.tokens {
		out = append(out, rec.publicCopy())
	}
	slices.SortFunc(out, func(a, b Record) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return out
}

// HasPublicID reports whether any row (including revoked) exists for id.
func (s *Store) HasPublicID(id string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.tokens[strings.TrimSpace(id)]
	return ok
}

func (s *Store) maybeTouch(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tokens[id]
	if !ok {
		return
	}
	if !rec.LastUsedAt.IsZero() && now.Sub(rec.LastUsedAt) < lastUsedMinGap {
		return
	}
	rec.LastUsedAt = now.UTC()
	s.tokens[id] = rec
	_ = s.saveLocked()
}

func (r Record) publicCopy() Record {
	out := r
	out.TokenHash = ""
	out.Projects = slices.Clone(r.Projects)
	out.Idempotency = nil
	return out
}

func compactProjects(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func parseWire(wire string) (id, secret string, ok bool) {
	wire = strings.TrimSpace(wire)
	rest, found := strings.CutPrefix(wire, wirePrefix)
	if !found {
		return "", "", false
	}
	id, secret, found = strings.Cut(rest, "_")
	if !found || id == "" || secret == "" {
		return "", "", false
	}
	for _, r := range id {
		if r < 'a' || r > 'z' {
			if r < '0' || r > '9' {
				return "", "", false
			}
		}
	}
	if len(id) < 8 || len(id) > 12 {
		return "", "", false
	}
	return id, secret, true
}

func newPublicID() (string, error) {
	var b [publicIDBytes]byte
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

var dummyHash = sha256Hex("gw_missing_dummyplaceholder")

// MintForTest inserts a token with a caller-chosen public id (tests / other packages).
func (s *Store) MintForTest(id, wire string, rec Record) error {
	if s == nil {
		return fmt.Errorf("token store is not configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id required")
	}
	if rec.ActorID == "" {
		rec.ActorID = config.NormalizeActorID("token:" + id)
	}
	if rec.TokenHash == "" {
		rec.TokenHash = sha256Hex(wire)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = s.now().UTC()
	}
	rec.ID = id
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[id] = rec
	return s.saveLocked()
}
