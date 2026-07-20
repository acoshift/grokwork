package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/acoshift/grokwork/internal/config"
)

const (
	sessionCookieName = "grok_web_sid"
	oauthStateCookie  = "grok_web_oauth_state"
	sessionTTL        = 12 * time.Hour
	oauthStateTTL     = 10 * time.Minute
	sessionsFileName  = "web-sessions.json"
)

// Session is a server-side web login record.
type Session struct {
	ID            string         `json:"id"`
	DiscordUserID string         `json:"discordUserId"`
	DisplayName   string         `json:"displayName"`
	AvatarURL     string         `json:"avatarUrl,omitempty"`
	Role          config.WebRole `json:"role"`
	CSRF          string         `json:"csrf"`
	ExpiresAt     time.Time      `json:"expiresAt"`
}

type sessionFile struct {
	Sessions map[string]Session `json:"sessions"`
}

// sessionStore keeps sessions in memory and persists on Create/Delete or lazy TTL renew.
type sessionStore struct {
	path     string
	mu       sync.Mutex
	sessions map[string]Session
}

func newSessionStore(dataDir string) (*sessionStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, sessionsFileName)
	st := &sessionStore{path: path, sessions: map[string]Session{}}
	loaded, err := st.loadFromDisk()
	if err != nil {
		return nil, err
	}
	st.sessions = loaded
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := st.saveLocked(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (st *sessionStore) loadFromDisk() (map[string]Session, error) {
	raw, err := os.ReadFile(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Session{}, nil
		}
		return nil, err
	}
	var f sessionFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if f.Sessions == nil {
		f.Sessions = map[string]Session{}
	}
	now := time.Now()
	for id, s := range f.Sessions {
		if now.After(s.ExpiresAt) {
			delete(f.Sessions, id)
		}
	}
	return f.Sessions, nil
}

func (st *sessionStore) saveLocked() error {
	raw, err := json.MarshalIndent(sessionFile{Sessions: st.sessions}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

func (st *sessionStore) Create(discordUserID, displayName, avatarURL string, role config.WebRole) (*Session, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessions == nil {
		st.sessions = map[string]Session{}
	}
	id, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	s := Session{
		ID:            id,
		DiscordUserID: discordUserID,
		DisplayName:   displayName,
		AvatarURL:     avatarURL,
		Role:          role,
		CSRF:          csrf,
		ExpiresAt:     time.Now().Add(sessionTTL),
	}
	st.sessions[id] = s
	if err := st.saveLocked(); err != nil {
		return nil, err
	}
	out := s
	return &out, nil
}

func (st *sessionStore) Get(id string) (*Session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessions == nil {
		return nil, false
	}
	s, ok := st.sessions[id]
	if !ok {
		return nil, false
	}
	now := time.Now()
	if now.After(s.ExpiresAt) {
		delete(st.sessions, id)
		_ = st.saveLocked()
		return nil, false
	}
	// Sliding TTL in memory; persist only when remaining life was low to avoid
	// rewriting the session file on every authenticated request / SSE partial.
	remaining := s.ExpiresAt.Sub(now)
	s.ExpiresAt = now.Add(sessionTTL)
	st.sessions[id] = s
	if remaining < sessionTTL/2 {
		_ = st.saveLocked()
	}
	out := s
	return &out, true
}

func (st *sessionStore) Delete(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessions == nil {
		st.sessions = map[string]Session{}
	}
	delete(st.sessions, id)
	return st.saveLocked()
}

// displayNames returns Discord user id → display name for non-expired sessions.
func (st *sessionStore) displayNames() map[string]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := map[string]string{}
	if st.sessions == nil {
		return out
	}
	now := time.Now()
	for _, s := range st.sessions {
		if now.After(s.ExpiresAt) {
			continue
		}
		id := strings.TrimSpace(s.DiscordUserID)
		name := strings.TrimSpace(s.DisplayName)
		if id == "" || name == "" {
			continue
		}
		out[id] = name
	}
	return out
}

func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}
