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

func TestParseFindingsPrefersLastObject(t *testing.T) {
	// Intermediate status object then final review (seen with tools-on reviews).
	text := `{
  "summary": "Reviewing the Playtech provider init commit…",
  "findings": []
}{
  "summary": "Real review",
  "findings": [
    {"title": "Money bug", "body": "uses Abs", "severity": "high", "paths": ["cb.go"]}
  ]
}`
	sum, fs, err := ParseFindings(text)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "Real review" || len(fs) != 1 || fs[0].Title != "Money bug" {
		t.Fatalf("sum=%q fs=%+v", sum, fs)
	}
}

func TestParseFindingsBodyWithMarkdownFence(t *testing.T) {
	// Finding bodies often include ```go samples; naive fence strip must not run.
	fence := "```"
	text := "{\n" +
		`  "summary": "ok",` + "\n" +
		`  "findings": [` + "\n" +
		`    {` + "\n" +
		`      "title": "Cancel math",` + "\n" +
		`      "body": "Bug in cancel path:\n\n` + fence + `go\nr.TurnOver.Sub(tx.Money.Amount)\n` + fence + `\n\nShould use Neg.",` + "\n" +
		`      "severity": "high",` + "\n" +
		`      "paths": ["cb_transaction.go"]` + "\n" +
		`    }` + "\n" +
		`  ]` + "\n" +
		`}`
	sum, fs, err := ParseFindings(text)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "ok" || len(fs) != 1 {
		t.Fatalf("%q %+v", sum, fs)
	}
	if !strings.Contains(fs[0].Body, fence+"go") {
		t.Fatalf("body lost fence: %q", fs[0].Body)
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
