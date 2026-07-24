package config

import (
	"fmt"
	"strings"
)

// NotifyOnDone modes for author @mention after a Grok run finishes.
const (
	NotifyOnDoneNever    = "never"
	NotifyOnDoneErrors   = "errors"
	NotifyOnDoneAlways   = "always"
	NotifyOnDoneLongOnly = "long_only"
)

// DefaultNotifyOnDone is used when notifyOnDone is unset.
const DefaultNotifyOnDone = NotifyOnDoneErrors

// DefaultNotifyOnDoneLongMs is 5 minutes (long_only threshold).
const DefaultNotifyOnDoneLongMs = 300_000

// NormalizeNotifyOnDone returns a valid mode (default errors).
func NormalizeNotifyOnDone(v string) string {
	return notifyOnDoneEffectiveLocked(v)
}

func notifyOnDoneEffectiveLocked(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case NotifyOnDoneNever, NotifyOnDoneErrors, NotifyOnDoneAlways, NotifyOnDoneLongOnly:
		return strings.ToLower(strings.TrimSpace(v))
	case "":
		return DefaultNotifyOnDone
	default:
		return DefaultNotifyOnDone
	}
}

func notifyOnDoneLongMsEffectiveLocked(ms int) int {
	if ms <= 0 {
		return DefaultNotifyOnDoneLongMs
	}
	return ms
}

// NotifyOnDoneValue returns the effective author-notify mode.
func (c *Config) NotifyOnDoneValue() string {
	if c == nil {
		return DefaultNotifyOnDone
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return notifyOnDoneEffectiveLocked(c.NotifyOnDone)
}

// NotifyOnDoneLongMsValue returns the long_only threshold in milliseconds.
func (c *Config) NotifyOnDoneLongMsValue() int {
	if c == nil {
		return DefaultNotifyOnDoneLongMs
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return notifyOnDoneLongMsEffectiveLocked(c.NotifyOnDoneLongMs)
}

// SetNotifyOnDone sets mode (+ optional long threshold) and persists.
// longMs is ignored unless mode is long_only; 0 keeps current / default.
func (c *Config) SetNotifyOnDone(mode string, longMs int) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case NotifyOnDoneNever, NotifyOnDoneErrors, NotifyOnDoneAlways, NotifyOnDoneLongOnly:
	default:
		return fmt.Errorf("notifyOnDone must be never, errors, always, or long_only")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.NotifyOnDone = mode
	if mode == NotifyOnDoneLongOnly && longMs > 0 {
		c.NotifyOnDoneLongMs = longMs
	}
	return c.saveLocked()
}
