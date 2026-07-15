package bot

import (
	"strings"
	"testing"
	"time"
)

func TestParseMessage(t *testing.T) {
	p := ParseMessage("<@123> project:app fix bug", "123")
	if p.Kind != KindTask || p.Prompt != "project:app fix bug" {
		t.Fatalf("got %+v", p)
	}

	p = ParseMessage("<@123> in api why timeout", "123")
	if p.Kind != KindTask || p.Prompt != "in api why timeout" {
		t.Fatalf("got %+v", p)
	}

	p = ParseMessage("<@123> /help", "123")
	if p.Kind != KindHelp {
		t.Fatalf("got %+v", p)
	}

	p = ParseMessage("<@123> investigate timeout", "123")
	if p.Kind != KindTask || p.Prompt != "investigate timeout" {
		t.Fatalf("got %+v", p)
	}

	for _, in := range []string{"/cancel", "cancel", "/stop", "stop"} {
		p = ParseMessage("<@123> "+in, "123")
		if p.Kind != KindCancel {
			t.Fatalf("%q: got %+v want KindCancel", in, p)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{2 * time.Second, "2s"},
		{65 * time.Second, "1m 5s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h 2m"},
	}
	for _, tc := range cases {
		if got := formatElapsed(tc.d); got != tc.want {
			t.Errorf("formatElapsed(%v)=%q want %q", tc.d, got, tc.want)
		}
	}
}

func TestWorkingStatus(t *testing.T) {
	got := workingStatus("app", 0, "")
	if got != "Working in **app**… · `@Grok /cancel` to stop" {
		t.Fatalf("initial: %q", got)
	}
	got = workingStatus("app", 45*time.Second, "")
	if got != "Working in **app**… · 45s elapsed · `@Grok /cancel` to stop" {
		t.Fatalf("elapsed: %q", got)
	}
	got = workingStatus("app", 45*time.Second, "reading files")
	if !strings.Contains(got, "reading files") {
		t.Fatalf("activity: %q", got)
	}
}
