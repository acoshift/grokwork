package errsrc

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCapSampleEmptyAndShort(t *testing.T) {
	if CapSample("") != "" {
		t.Fatal("empty")
	}
	if CapSample("  panic  ") != "panic" {
		t.Fatal("trim")
	}
	short := strings.Repeat("a", 10)
	if CapSample(short) != short {
		t.Fatal("short")
	}
}

func TestCapSampleRuneCeiling(t *testing.T) {
	long := strings.Repeat("ä", SampleMaxRunes+20)
	got := CapSample(long)
	if utf8.RuneCountInString(got) != SampleMaxRunes {
		t.Fatalf("runes=%d want %d", utf8.RuneCountInString(got), SampleMaxRunes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("invalid utf8")
	}
}
