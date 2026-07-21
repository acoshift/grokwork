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
	// Login sessions last 2 days. When the user is active with ≤1 day left,
	// ExpiresAt is extended back to 2 days so regular use never forces re-login.
	sessionTTL       = 48 * time.Hour
	sessionRenewWhen = 24 * time.Hour
	oauthStateTTL    = 10 * time.Minute
	sessionsFileName = "web-sessions.json"
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

// sessionStore keeps sessions in memory and persists on Create/Delete or sliding renew.
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

// Get returns a non-expired session. When remaining life is at most
// sessionRenewWhen, ExpiresAt is extended to now+sessionTTL and persisted;
// renewed is true so callers can re-issue the browser cookie.
func (st *sessionStore) Get(id string) (sess *Session, renewed bool, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessions == nil {
		return nil, false, false
	}
	s, found := st.sessions[id]
	if !found {
		return nil, false, false
	}
	now := time.Now()
	if now.After(s.ExpiresAt) {
		delete(st.sessions, id)
		_ = st.saveLocked()
		return nil, false, false
	}
	remaining := s.ExpiresAt.Sub(now)
	if remaining <= sessionRenewWhen {
		s.ExpiresAt = now.Add(sessionTTL)
		st.sessions[id] = s
		_ = st.saveLocked()
		renewed = true
	}
	out := s
	return &out, renewed, true
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
