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

// oauthStateCookieFor names the short-lived state cookie for one provider.
//
// Discord keeps the historical name so a login already in flight across a
// restart still completes. Every other provider gets its own cookie, and that
// separation IS the cross-provider guard: a state minted at /auth/google simply
// is not present when /auth/github/callback looks for one, so a code obtained
// from one provider can never be redeemed against another's callback.
func oauthStateCookieFor(key string) string {
	if key == config.ActorKindDiscord {
		return oauthStateCookie
	}
	return oauthStateCookie + "_" + key
}

// oauthLinkStateCookieFor names the state cookie for a LINK flow.
//
// A link and a login share one callback, so the two flows need states that
// cannot be swapped: completing a link with a state minted for a login would
// skip the session binding that makes linking safe at all, and completing a
// login with a link's state would mint a session from an identity the person
// only meant to attach. Separate cookies make that a non-question — and let a
// link started in one tab coexist with a login started in another.
func oauthLinkStateCookieFor(key string) string {
	return oauthStateCookieFor(key) + "_link"
}

// Session is a server-side web login record.
type Session struct {
	ID string `json:"id"`
	// DiscordUserID is the actor id, not necessarily a Discord snowflake: since
	// multi-provider login it may be "google:<sub>" or "github:<id>". The JSON
	// key is persisted, so it keeps its original name.
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

// RevokeActor deletes every session carrying actorID and reports how many.
//
// Called after a login is absorbed into an account: the alias's live sessions
// were minted before the link and still carry its actor id, which from that
// moment names nobody — its grants have moved to the account and it will never
// be minted again. Left alone they would keep acting as a hollow identity until
// they expired, seeing none of the person's own work.
//
// Matching is SameActor, not ==, for the same reason every other id comparison
// is: a session written before namespacing may spell the id either way.
func (st *sessionStore) RevokeActor(actorID string) (int, error) {
	if st == nil {
		return 0, nil
	}
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return 0, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for id, s := range st.sessions {
		if !config.SameActor(s.DiscordUserID, actorID) {
			continue
		}
		delete(st.sessions, id)
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, st.saveLocked()
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
