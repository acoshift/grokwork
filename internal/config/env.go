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

// EnvWork returns the first non-empty value among GROK_WORK_<suffix>, then any
// extra keys (in order). Empty if none set.
func EnvWork(suffix string, extra ...string) string {
	suffix = strings.TrimSpace(suffix)
	keys := make([]string, 0, 1+len(extra))
	if suffix != "" {
		keys = append(keys, "GROK_WORK_"+suffix)
	}
	keys = append(keys, extra...)
	return firstEnv(keys...)
}
