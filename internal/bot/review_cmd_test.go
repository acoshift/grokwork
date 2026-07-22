package bot

import "testing"

func TestParseReviewArgs(t *testing.T) {
	id, rest := parseReviewArgs("/review <@123456> please focus on auth")
	if id != "123456" {
		t.Fatalf("id=%q", id)
	}
	if rest != "please focus on auth" {
		t.Fatalf("rest=%q", rest)
	}
	id, rest = parseReviewArgs("/review <@!99> #42 fix tests")
	if id != "99" || rest != "#42 fix tests" {
		t.Fatalf("id=%q rest=%q", id, rest)
	}
	id, _ = parseReviewArgs("/review nobody")
	if id != "" {
		t.Fatal("expected empty without mention")
	}
}

func TestIsReviewCommand(t *testing.T) {
	if !isReviewCommand("/review @x") {
		t.Fatal("want true")
	}
	if isReviewCommand("review the flaky test") {
		t.Fatal("bare review must stay a task")
	}
}

func TestParseMessageReview(t *testing.T) {
	p := ParseMessage("<@BOT> /review <@111>", "BOT")
	if p.Kind != KindReview {
		t.Fatalf("got %v", p.Kind)
	}
	// Free-form without slash stays task.
	p = ParseMessage("<@BOT> review the flaky CI", "BOT")
	if p.Kind != KindTask {
		t.Fatalf("got %v want task", p.Kind)
	}
}
