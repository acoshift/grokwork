package bot

import "testing"

func TestParseMessage(t *testing.T) {
	p := ParseMessage("<@123> project:app fix bug", "123")
	// Users cannot switch project; the whole string is the prompt.
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
}
