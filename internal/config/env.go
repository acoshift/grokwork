package config

import (
	"os"
	"strings"
)

// firstEnv returns the first non-empty trimmed environment value among keys.
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if k == "" {
			continue
		}
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// EnvPrefersWork returns the first non-empty value among GROK_WORK_<suffix>,
// GROK_DISCORD_<suffix>, then any extra keys (in order). Empty if none set.
// Packaging rename (PR 14): prefer GROK_WORK_*; keep GROK_DISCORD_* as legacy.
func EnvPrefersWork(suffix string, extra ...string) string {
	suffix = strings.TrimSpace(suffix)
	keys := make([]string, 0, 2+len(extra))
	if suffix != "" {
		keys = append(keys, "GROK_WORK_"+suffix, "GROK_DISCORD_"+suffix)
	}
	keys = append(keys, extra...)
	return firstEnv(keys...)
}
