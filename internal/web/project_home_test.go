package web

import (
	"testing"
	"time"
)

func TestRelativeAge(t *testing.T) {
	if got := relativeAge(""); got != "" {
		t.Fatalf("empty string: %q", got)
	}
	if got := relativeAge(time.Time{}); got != "" {
		t.Fatalf("zero time: %q", got)
	}
	var nilT *time.Time
	if got := relativeAge(nilT); got != "" {
		t.Fatalf("nil *time.Time: %q", got)
	}
	if got := relativeAge("not-a-time"); got != "not-a-time" {
		t.Fatalf("unparseable: %q", got)
	}

	now := time.Now()
	cases := []struct {
		in   any
		want string
	}{
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(time.Minute), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-2 * time.Hour), "2h ago"},
		{now.Add(-3 * 24 * time.Hour), "3d ago"},
		{now.Add(-2 * time.Hour).UTC().Format(time.RFC3339), "2h ago"},
	}
	for _, tc := range cases {
		if got := relativeAge(tc.in); got != tc.want {
			t.Errorf("relativeAge(%v)=%q want %q", tc.in, got, tc.want)
		}
	}
}
