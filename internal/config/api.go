package config

import "fmt"

// APIConfig is the machine-to-machine HTTP API flag. Tokens themselves live
// in data/api-tokens.json, not here.
type APIConfig struct {
	Enabled bool `json:"enabled"`
}

// APIEnabled reports whether /api/v1 routes should be registered.
// Nil or omitted config is off (fail-closed).
func (c *Config) APIEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.API != nil && c.API.Enabled
}

// SetAPIEnabled persists api.enabled. Used by tests and (later) admin UI.
func (c *Config) SetAPIEnabled(on bool) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.API == nil {
		c.API = &APIConfig{}
	}
	c.API.Enabled = on
	return c.saveLocked()
}

func cloneAPI(a *APIConfig) *APIConfig {
	if a == nil {
		return nil
	}
	out := *a
	return &out
}
