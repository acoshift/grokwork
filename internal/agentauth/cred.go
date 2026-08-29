// Package agentauth mints short-lived session-bound credentials for in-session agents.
// Tokens are identification/audit bindings, not a secrecy boundary against same-UID siblings.
package agentauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Caps are snapshotted at mint time.
type Caps struct {
	SessionRead    bool
	SessionDone    bool
	SessionAbandon bool
	PRsList        bool
	IssuesList     bool
	ReviewRequest  bool
	StorageRead    bool
	StorageWrite   bool
	// SessionFiles is sending a file to the bound session (not project storage).
	SessionFiles      bool
	ClickUpRead       bool
	LinearRead        bool
	GCPErrorsRead     bool
	SentryRead        bool
	DeploysErrorsRead bool
}

// DefaultShipCaps is the unrestricted ship/fix tool set (rev 3).
func DefaultShipCaps() Caps {
	return Caps{
		SessionRead:       true,
		SessionDone:       true,
		SessionAbandon:    true,
		PRsList:           true,
		IssuesList:        true,
		ReviewRequest:     true,
		StorageRead:       true,
		StorageWrite:      true,
		SessionFiles:      true,
		ClickUpRead:       true,
		LinearRead:        true,
		GCPErrorsRead:     true,
		SentryRead:        true,
		DeploysErrorsRead: true,
	}
}

// DefaultInvestigateCaps is the diagnose tool set for Claude investigate
// attach. Board/lifecycle writes and project storage writes stay off so a
// diagnosis run cannot mark the session done or mutate shared project
// storage. SessionFiles is on: sending a deliverable to this session is
// not a workflow-state mutation.
func DefaultInvestigateCaps() Caps {
	return Caps{
		SessionRead:       true,
		PRsList:           true,
		IssuesList:        true,
		StorageRead:       true,
		SessionFiles:      true,
		ClickUpRead:       true,
		LinearRead:        true,
		GCPErrorsRead:     true,
		SentryRead:        true,
		DeploysErrorsRead: true,
	}
}

// Cred is a verified credential view (no raw token).
type Cred struct {
	ID       string
	ThreadID string
	Project  string
	ActorID  string
	RunID    string
	Caps     Caps
	Expires  time.Time
}

// Store holds hashed tokens in memory (v1: re-mint on resume).
type Store struct {
	mu   sync.Mutex
	byID map[string]*record // id → record
	hash map[string]string  // token hash → id
	now  func() time.Time
}

type record struct {
	Cred
	tokenHash string
	revoked   bool
}

// NewStore returns an empty credential store.
func NewStore() *Store {
	return &Store{
		byID: make(map[string]*record),
		hash: make(map[string]string),
		now:  time.Now,
	}
}

// Mint creates a new opaque token bound to thread/project. Returns raw token once.
func (s *Store) Mint(threadID, project, actorID, runID string, caps Caps, ttl time.Duration) (rawToken string, cred Cred, err error) {
	if s == nil {
		return "", Cred{}, fmt.Errorf("nil store")
	}
	threadID = trim(threadID)
	project = trim(project)
	if threadID == "" || project == "" {
		return "", Cred{}, fmt.Errorf("threadID and project required")
	}
	if ttl <= 0 {
		ttl = 45 * time.Minute
	}
	raw, err := randomHex(32)
	if err != nil {
		return "", Cred{}, err
	}
	id, err := randomHex(8)
	if err != nil {
		return "", Cred{}, err
	}
	now := s.now()
	h := hashToken(raw)
	c := Cred{
		ID:       id,
		ThreadID: threadID,
		Project:  project,
		ActorID:  trim(actorID),
		RunID:    trim(runID),
		Caps:     caps,
		Expires:  now.Add(ttl),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := &record{Cred: c, tokenHash: h}
	s.byID[id] = rec
	s.hash[h] = id
	return raw, c, nil
}

// Verify returns the cred for a raw token or an error.
func (s *Store) Verify(rawToken string) (Cred, error) {
	if s == nil {
		return Cred{}, fmt.Errorf("nil store")
	}
	rawToken = trim(rawToken)
	if rawToken == "" {
		return Cred{}, fmt.Errorf("missing token")
	}
	h := hashToken(rawToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.hash[h]
	if !ok {
		return Cred{}, fmt.Errorf("invalid token")
	}
	rec := s.byID[id]
	if rec == nil || rec.revoked {
		return Cred{}, fmt.Errorf("revoked token")
	}
	if s.now().After(rec.Expires) {
		return Cred{}, fmt.Errorf("expired token")
	}
	return rec.Cred, nil
}

// Revoke invalidates a credential by id.
func (s *Store) Revoke(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.byID[trim(id)]
	if rec == nil {
		return
	}
	rec.revoked = true
	delete(s.hash, rec.tokenHash)
}

// RevokeThread revokes all credentials for a thread.
func (s *Store) RevokeThread(threadID string) {
	if s == nil {
		return
	}
	threadID = trim(threadID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rec := range s.byID {
		if rec.ThreadID == threadID && !rec.revoked {
			rec.revoked = true
			delete(s.hash, rec.tokenHash)
			_ = id
		}
	}
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
