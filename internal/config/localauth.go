package config

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// LocalAccount is an operator-provisioned web login that does not involve Discord.
//
// This is what makes the product usable by someone with no Discord account: every
// other identity path here starts from a snowflake. Accounts are provisioned in
// config.json by an admin — there is no self-signup, matching the standing
// non-goal of a public web app.
type LocalAccount struct {
	// ID is the bare subject. The actor id is "local:<ID>", which is what lands in
	// allowlists, ownership fields, and audit records.
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	// PasswordHash is the encoded PBKDF2 string from HashPassword. Never a plaintext
	// password: config.json is world-readable to anyone who can read the host.
	PasswordHash string `json:"passwordHash"`
	// Role overrides list-based resolution for this account. Empty falls through to
	// the normal admin/member/viewer lists and project membership.
	Role WebRole `json:"role,omitempty"`
	// Disabled keeps the record (and its audit trail) while refusing logins.
	Disabled bool `json:"disabled,omitempty"`
}

// ActorID returns the namespaced actor id for this account.
func (a LocalAccount) ActorID() string {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return ""
	}
	return ActorKindLocal + ":" + id
}

var (
	// ErrLocalAuthFailed is returned for every rejection reason — unknown account,
	// wrong password, disabled — so a caller cannot use the error to discover which
	// accounts exist.
	ErrLocalAuthFailed = errors.New("invalid credentials")
	// ErrWeakPassword rejects a password too short to be worth hashing.
	ErrWeakPassword = errors.New("password must be at least 12 characters")
)

const (
	pbkdf2Iterations = 600_000 // OWASP guidance for PBKDF2-HMAC-SHA256
	pbkdf2SaltBytes  = 16
	pbkdf2KeyBytes   = 32
	minPasswordRunes = 12
)

// HashPassword derives a storable hash: "pbkdf2$sha256$<iter>$<salt-b64>$<key-b64>".
// The parameters are stored with the hash so raising the iteration count later
// does not invalidate existing accounts.
func HashPassword(password string) (string, error) {
	if len([]rune(password)) < minPasswordRunes {
		return "", ErrWeakPassword
	}
	salt := make([]byte, pbkdf2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2$sha256$%d$%s$%s",
		pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches an encoded hash.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(strings.TrimSpace(encoded), "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	// Constant time: a timing difference here leaks the hash prefix.
	return subtle.ConstantTimeCompare(got, want) == 1
}

// AuthenticateLocal verifies a local login and returns the account.
//
// Comparison of the account id is case-insensitive (people type their own name
// inconsistently) but the password never is.
func (c *Config) AuthenticateLocal(id, password string) (LocalAccount, error) {
	if c == nil {
		return LocalAccount{}, ErrLocalAuthFailed
	}
	id = strings.TrimSpace(id)
	// Accept either "alice" or "local:alice" from a form.
	if kind, subject, found := strings.Cut(id, ":"); found && strings.EqualFold(kind, ActorKindLocal) {
		id = strings.TrimSpace(subject)
	}
	if id == "" || password == "" {
		return LocalAccount{}, ErrLocalAuthFailed
	}

	c.mu.RLock()
	var accounts []LocalAccount
	if c.WebAuth != nil {
		accounts = append(accounts, c.WebAuth.LocalAccounts...)
	}
	c.mu.RUnlock()

	for _, a := range accounts {
		if !strings.EqualFold(strings.TrimSpace(a.ID), id) {
			continue
		}
		if a.Disabled || strings.TrimSpace(a.PasswordHash) == "" {
			return LocalAccount{}, ErrLocalAuthFailed
		}
		if !VerifyPassword(a.PasswordHash, password) {
			return LocalAccount{}, ErrLocalAuthFailed
		}
		return a, nil
	}
	return LocalAccount{}, ErrLocalAuthFailed
}

// LocalAccountsEnabled reports whether any local login is provisioned. Used to
// decide whether the login page offers a password form at all.
func (c *Config) LocalAccountsEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.WebAuth == nil {
		return false
	}
	for _, a := range c.WebAuth.LocalAccounts {
		if !a.Disabled && strings.TrimSpace(a.ID) != "" && strings.TrimSpace(a.PasswordHash) != "" {
			return true
		}
	}
	return false
}
