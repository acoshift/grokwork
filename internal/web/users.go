package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const usersFileName = "web-users.json"

// UserProfile is a durable Discord identity cache (name + avatar URL).
// Written on web login; not removed on logout or session TTL.
type UserProfile struct {
	DiscordUserID string    `json:"discordUserId"`
	DisplayName   string    `json:"displayName"`
	AvatarURL     string    `json:"avatarUrl,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type usersFile struct {
	Users map[string]UserProfile `json:"users"`
}

// userStore keeps Discord profiles in memory and persists on Upsert.
type userStore struct {
	path  string
	mu    sync.Mutex
	users map[string]UserProfile
}

func newUserStore(dataDir string) (*userStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, usersFileName)
	st := &userStore{path: path, users: map[string]UserProfile{}}
	loaded, err := st.loadFromDisk()
	if err != nil {
		return nil, err
	}
	st.users = loaded
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := st.saveLocked(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (st *userStore) loadFromDisk() (map[string]UserProfile, error) {
	raw, err := os.ReadFile(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]UserProfile{}, nil
		}
		return nil, err
	}
	var f usersFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if f.Users == nil {
		f.Users = map[string]UserProfile{}
	}
	return f.Users, nil
}

func (st *userStore) saveLocked() error {
	raw, err := json.MarshalIndent(usersFile{Users: st.users}, "", "  ")
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

// Upsert creates or refreshes a profile by Discord user id. Never deletes.
func (st *userStore) Upsert(discordUserID, displayName, avatarURL string) error {
	id := strings.TrimSpace(discordUserID)
	if id == "" {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.users == nil {
		st.users = map[string]UserProfile{}
	}
	name := strings.TrimSpace(displayName)
	avatar := strings.TrimSpace(avatarURL)
	prev, ok := st.users[id]
	if ok {
		if name == "" {
			name = prev.DisplayName
		}
		if avatar == "" {
			avatar = prev.AvatarURL
		}
		// Skip disk write when nothing meaningful changed.
		if name == prev.DisplayName && avatar == prev.AvatarURL {
			return nil
		}
	}
	st.users[id] = UserProfile{
		DiscordUserID: id,
		DisplayName:   name,
		AvatarURL:     avatar,
		UpdatedAt:     time.Now().UTC(),
	}
	return st.saveLocked()
}

// Get returns a profile by Discord user id.
func (st *userStore) Get(discordUserID string) (UserProfile, bool) {
	id := strings.TrimSpace(discordUserID)
	if id == "" {
		return UserProfile{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := st.users[id]
	return p, ok
}

// displayNames returns Discord user id → display name for all known profiles.
func (st *userStore) displayNames() map[string]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]string, len(st.users))
	for id, p := range st.users {
		name := strings.TrimSpace(p.DisplayName)
		if id == "" || name == "" {
			continue
		}
		out[id] = name
	}
	return out
}
