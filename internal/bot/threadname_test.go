package bot

import "testing"

func TestThreadNameFromPrompt(t *testing.T) {
	got := threadNameFromPrompt("  investigate flaky payment test  ", "alice")
	if got != "investigate flaky payment test" {
		t.Fatalf("got %q", got)
	}

	got = threadNameFromPrompt("please fix the race in commission", "alice")
	if got != "fix the race in commission" {
		t.Fatalf("got %q", got)
	}

	got = threadNameFromPrompt("", "bob")
	if got != "task from bob" {
		t.Fatalf("got %q", got)
	}

	long := stringsRepeat("word ", 40)
	got = threadNameFromPrompt(long, "alice")
	if len(got) > 100 {
		t.Fatalf("len=%d got %q", len(got), got)
	}
	if !stringsHasSuffix(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
}

func stringsRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func stringsHasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
