package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// formatMsDur formats a millisecond duration as a compact unit-suffixed string
// suitable for form display and time.ParseDuration (e.g. "30m", "1h", "45s").
func formatMsDur(ms int) string {
	if ms <= 0 {
		return "0s"
	}
	d := time.Duration(ms) * time.Millisecond
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return fmt.Sprintf("%dms", ms)
}

// parseTimeoutMs accepts a duration with unit suffix (Go time.ParseDuration:
// "1h", "30m", "45s", "1h30m", "900000ms") or a bare integer as milliseconds
// (backward compatible with the old config form).
func parseTimeoutMs(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("timeout is required")
	}
	if ms, err := strconv.Atoi(s); err == nil {
		return ms, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("timeout must be a duration like 30m, 1h, or milliseconds")
	}
	if d < 0 {
		return 0, fmt.Errorf("timeout must be positive")
	}
	ms := int(d / time.Millisecond)
	if time.Duration(ms)*time.Millisecond != d {
		// Sub-millisecond remainder (e.g. 500µs) — not useful for run limits.
		return 0, fmt.Errorf("timeout must be at least 1ms resolution")
	}
	return ms, nil
}
