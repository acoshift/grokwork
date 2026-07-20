package commitreview

import (
	"strings"
	"testing"
)

func TestParseFindingsClean(t *testing.T) {
	text := `{
  "summary": "Looks risky",
  "findings": [
    {"title": "Nil deref", "body": "x may be nil", "severity": "high", "paths": ["a.go"]},
    {"title": "Missing test", "body": "add test", "severity": "low", "paths": ["a_test.go"]}
  ]
}`
	sum, fs, err := ParseFindings(text)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "Looks risky" || len(fs) != 2 {
		t.Fatalf("%q %+v", sum, fs)
	}
	if fs[0].Severity != "high" || fs[0].Title != "Nil deref" {
		t.Fatalf("sort/high first: %+v", fs[0])
	}
	if fs[0].Fingerprint == "" {
		t.Fatal("fingerprint")
	}
}

func TestParseFindingsFenced(t *testing.T) {
	text := "Here you go:\n```json\n{\"summary\":\"ok\",\"findings\":[]}\n```\n"
	sum, fs, err := ParseFindings(text)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "ok" || len(fs) != 0 {
		t.Fatalf("%q %v", sum, fs)
	}
}

func TestParseFindingsSkipEmptyTitleCap(t *testing.T) {
	var findings []string
	for i := 0; i < 20; i++ {
		findings = append(findings, `{"title":"T`+string(rune('A'+i%26))+`","body":"b","severity":"info"}`)
	}
	text := `{"summary":"s","findings":[` + strings.Join(findings, ",") + `,{"title":"","body":"x","severity":"high"}]}`
	_, fs, err := ParseFindings(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != MaxFindings {
		t.Fatalf("cap want %d got %d", MaxFindings, len(fs))
	}
}

func TestParseFindingsInvalid(t *testing.T) {
	if _, _, err := ParseFindings("no json here"); err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeSeverity(t *testing.T) {
	if normalizeSeverity("HIGH") != "high" {
		t.Fatal()
	}
	if normalizeSeverity("nope") != "medium" {
		t.Fatal()
	}
}
