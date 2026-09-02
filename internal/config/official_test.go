package config

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const xaiFixture = `#### Key Information
# Models
### Text API Pricing

| Model | Context | Input / 1M tokens | Cached input / 1M tokens | Output / 1M tokens |
| --- | --- | --- | --- | --- |
| grok-4.6 (< 200k prompt tokens) | 500k | $2.00 | $0.50 | $6.00 |
| grok-4.6 (≥ 200k prompt tokens) | 500k | $4.00 | $1.00 | $12.00 |
| grok-4.5 (< 200k prompt tokens) | 500k | $2.00 | $0.30 | $6.00 |
| grok-4.5 (≥ 200k prompt tokens) | 500k | $4.00 | $0.60 | $12.00 |

*Prices shown per million tokens.*
`

const anthropicFixture = `## Model pricing

| Model | Base Input Tokens | 5m Cache Writes | 1h Cache Writes | Cache Hits & Refreshes | Output Tokens |
| --- | --- | --- | --- | --- | --- |
| Claude Fable 5.1 | $10 / MTok | $12.50 / MTok | $20 / MTok | $0.25 / MTok | $50 / MTok |
| Claude Mythos 5.1 (limited availability) | $10 / MTok | $12.50 / MTok | $20 / MTok | $0.25 / MTok | $50 / MTok |
| Claude Fable 5 | $10 / MTok | $12.50 / MTok | $20 / MTok | $1 / MTok | $50 / MTok |
| Claude Mythos 5 (limited availability) | $10 / MTok | $12.50 / MTok | $20 / MTok | $1 / MTok | $50 / MTok |
| Claude Opus 5 | $5 / MTok | $6.25 / MTok | $10 / MTok | $0.50 / MTok | $25 / MTok |
| Claude Opus 4.8 | $5 / MTok | $6.25 / MTok | $10 / MTok | $0.50 / MTok | $25 / MTok |
| Claude Sonnet 5 | $2 / MTok | $2.50 / MTok | $4 / MTok | $0.20 / MTok | $10 / MTok |
| Claude Haiku 4.5 | $1 / MTok | $1.25 / MTok | $2 / MTok | $0.10 / MTok | $5 / MTok |
| Claude Opus 4.1 (retired, except on Bedrock) | $15 / MTok | $18.75 / MTok | $30 / MTok | $1.50 / MTok | $75 / MTok |

## Batch processing

| Model | Batch input | Batch output |
| --- | --- | --- |
| Claude Opus 5 | $2.50 / MTok | $12.50 / MTok |
`

const cursorFixture = `## Cursor Models

| Model | Provider | Input | Cache write | Cache read | Output | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| Grok 4.6 | Cursor | $2 | - | $0.5 | $6 | Jointly trained |
| Grok 4.6 (Fast) | Cursor | $4 | - | $1 | $12 | Jointly trained |
| [Composer 2.5](https://cursor.com/blog/composer-2-5) | Cursor | $0.5 | - | $0.2 | $2.5 | - |
| [Composer 2.5 (Fast)](https://cursor.com/blog/composer-2-5) | Cursor | $3 | - | $0.5 | $15 | - |

## Other Models

| Model | Provider | Input | Cache write | Cache read | Output | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| [Claude Fable 5.1](https://www.anthropic.com/claude) | Anthropic | $10 | $12.5 | $0.25 | $50 | - |
| [Claude Opus 5](https://www.anthropic.com/claude/opus) | Anthropic | $5 | $6.25 | $0.5 | $25 | Requires Max Mode |
| [Claude Sonnet 5](https://www.anthropic.com/claude/sonnet) | Anthropic | $2 | $2.5 | $0.2 | $10 | - |
| [Claude Fable 5](https://www.anthropic.com/claude) | Anthropic | $10 | $12.5 | $1 | $50 | - |
| [GPT-5.6 Sol](https://openai.com/) | OpenAI | $4 | $5 | $0.4 | $20 | - |
| [Gemini 3.7 Flash](https://ai.google.dev/) | Google | $0.75 | - | $0.075 | $3.5 | - |
| [Gemini 3.8 Flash](https://ai.google.dev/) | Google | $0.75 | - | $0.075 | $3.5 | - |
| [GLM 5.2](https://z.ai) | Z.ai | $1.4 | - | $0.26 | $4.4 | Hidden by default |
| Kimi K2.7 Code | Moonshot | $0.95 | - | $0.19 | $4 | Hidden by default |
| [Kimi K3](https://www.moonshot.ai) | Moonshot | $3 | - | $0.3 | $15 | Hidden by default |
`

func TestParseXAIRatesUsesStandardContextNotLong(t *testing.T) {
	got := parseXAIRates([]byte(xaiFixture))
	r, ok := got["grok-4.6"]
	if !ok {
		t.Fatal("missing grok-4.6")
	}
	assertRate(t, r, 2, 6, 0.5, -1)
	if r, ok := (OfficialRateCatalog{rates: got}).RateFor("grok-4.6-xhigh"); !ok {
		t.Fatal("RateFor must peel grok-4.6-xhigh to grok-4.6")
	} else {
		assertRate(t, r, 2, 6, 0.5, -1)
	}
	if _, ok := got["grok-4.6-xhigh"]; ok {
		t.Fatal("effort aliases must not be stored; lookup peels the suffix")
	}
	if _, ok := got["grok-4.6-high"]; ok {
		t.Fatal("effort aliases must not be stored; lookup peels the suffix")
	}
	r, ok = got["grok-4.5"]
	if !ok {
		t.Fatal("missing grok-4.5")
	}
	assertRate(t, r, 2, 6, 0.3, -1)
	if _, ok := got["grok-4.5-low"]; ok {
		t.Fatal("effort aliases must not be stored; lookup peels the suffix")
	}
}

func TestParseAnthropicRates(t *testing.T) {
	got := parseAnthropicRates([]byte(anthropicFixture))
	assertRate(t, got["claude-opus-5"], 5, 25, 0.5, 6.25)
	assertRate(t, got["claude-opus-4-8"], 5, 25, 0.5, 6.25)
	assertRate(t, got["claude-sonnet-5"], 2, 10, 0.2, 2.5)
	assertRate(t, got["claude-haiku-4-5"], 1, 5, 0.1, 1.25)
	assertRate(t, got["claude-fable-5-1"], 10, 50, 0.25, 12.5)
	if r, ok := (OfficialRateCatalog{rates: got}).RateFor("claude-fable-5-1-xhigh"); !ok {
		t.Fatal("RateFor must peel claude-fable-5-1-xhigh to claude-fable-5-1")
	} else {
		assertRate(t, r, 10, 50, 0.25, 12.5)
	}
	if _, ok := got["claude-fable-5-1-xhigh"]; ok {
		t.Fatal("effort aliases must not be stored; lookup peels the suffix")
	}
	assertRate(t, got["claude-fable-5"], 10, 50, 1, 12.5)
	if _, ok := got["claude-mythos-5"]; ok {
		t.Fatal("mythos must be skipped")
	}
	if _, ok := got["claude-mythos-5-1"]; ok {
		t.Fatal("mythos 5.1 must be skipped")
	}
	if _, ok := got["claude-opus-4-1"]; ok {
		t.Fatal("retired opus 4.1 must be skipped")
	}
}

func TestParseCursorRatesAliasesPickerNames(t *testing.T) {
	got := parseCursorRates([]byte(cursorFixture))
	assertRate(t, got["composer-2.5"], 0.5, 2.5, 0.2, -1)
	assertRate(t, got["composer-2.5-fast"], 3, 15, 0.5, -1)
	assertRate(t, got["cursor-grok-4.6-xhigh"], 2, 6, 0.5, -1)
	assertRate(t, got["cursor-grok-4.6-high"], 2, 6, 0.5, -1)
	assertRate(t, got["claude-fable-5-1-thinking-high"], 10, 50, 0.25, 12.5)
	assertRate(t, got["claude-fable-5-1-thinking-xhigh"], 10, 50, 0.25, 12.5)
	assertRate(t, got["claude-opus-5-thinking-high"], 5, 25, 0.5, 6.25)
	assertRate(t, got["claude-fable-5-thinking-high"], 10, 50, 1, 12.5)
	assertRate(t, got["claude-fable-5-thinking-xhigh"], 10, 50, 1, 12.5)
	if _, ok := got["claude-fable-5.1-thinking-high"]; ok {
		t.Fatal("cursor fable 5.1 must hyphenate like the Claude CLI id")
	}
	assertRate(t, got["gpt-5.6-sol-medium"], 4, 20, 0.4, 5)
	assertRate(t, got["gemini-3.7-flash-high"], 0.75, 3.5, 0.075, -1)
	assertRate(t, got["gemini-3.8-flash-high"], 0.75, 3.5, 0.075, -1)
	assertRate(t, got["glm-5.2-high"], 1.4, 4.4, 0.26, -1)
	assertRate(t, got["glm-5.2-max"], 1.4, 4.4, 0.26, -1)
	assertRate(t, got["kimi-k3-max"], 3, 15, 0.3, -1)
	assertRate(t, got["kimi-k2.7-code"], 0.95, 4, 0.19, -1)
	if _, ok := got["grok-4.6"]; ok {
		t.Fatal("cursor grok-4.6 must not overwrite the xAI grok CLI row")
	}
	if _, ok := got["claude-opus-5"]; ok {
		t.Fatal("cursor claude-opus-5 must not overwrite the Anthropic CLI row")
	}
}

func TestApplyOfficialRatesOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+dir+`", "allowedUserIds": ["u1"]}},
  "channels": {"c1": "app"},
  "grokBin": "grok"
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
	cfg.DataDir = dir
	if err := cfg.SetModelRates(map[string]ModelRate{
		"grok-4.6":       {InputPerMTok: new(9.0)},
		"my-local-model": {InputPerMTok: new(0.1), OutputPerMTok: new(0.2)},
	}); err != nil {
		t.Fatal(err)
	}

	cat := OfficialRateCatalog{rates: map[string]ModelRate{}, sources: []string{"xAI", "Anthropic"}}
	mapsCopy := parseXAIRates([]byte(xaiFixture))
	for k, v := range mapsCopy {
		cat.rates[k] = v
	}
	for k, v := range parseAnthropicRates([]byte(anthropicFixture)) {
		cat.rates[k] = v
	}

	res, err := cfg.ApplyOfficialRates(cat)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Updated, "grok-4.6") || !slices.Contains(res.Updated, "claude-opus-5") {
		t.Fatalf("updated=%v", res.Updated)
	}
	if slices.Contains(res.Updated, "my-local-model") {
		t.Fatalf("custom model overwritten: %v", res.Updated)
	}
	if !slices.Contains(res.Unmatched, "my-local-model") {
		t.Fatalf("unmatched=%v", res.Unmatched)
	}

	grok, ok := cfg.ModelRateFor("grok-4.6")
	if !ok || grok.InputPerMTok == nil || *grok.InputPerMTok != 2 {
		t.Fatalf("existing grok input not replaced: %+v", grok)
	}
	if grok.CacheWritePerMTok != nil {
		t.Fatalf("xAI has no cache write; stale cell must clear: %+v", grok)
	}
	custom, ok := cfg.ModelRateFor("my-local-model")
	if !ok || custom.InputPerMTok == nil || *custom.InputPerMTok != 0.1 {
		t.Fatalf("custom rate lost: %+v", custom)
	}
}

func TestFetchOfficialRatesPartialSourceFailure(t *testing.T) {
	xai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != officialRatesUserAgent {
			t.Errorf("User-Agent=%q", r.Header.Get("User-Agent"))
		}
		io.WriteString(w, xaiFixture)
	}))
	t.Cleanup(xai.Close)
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(fail.Close)
	cursor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, cursorFixture)
	}))
	t.Cleanup(cursor.Close)

	cat, err := FetchOfficialRates(t.Context(), xai.Client(), OfficialRateURLs{
		XAI:       xai.URL,
		Anthropic: fail.URL,
		Cursor:    cursor.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cat.sources, "xAI") || !slices.Contains(cat.sources, "Cursor") {
		t.Fatalf("sources=%v", cat.sources)
	}
	if len(cat.errors) == 0 || !strings.Contains(strings.Join(cat.errors, " "), "Anthropic") {
		t.Fatalf("errors=%v", cat.errors)
	}
	if _, ok := cat.RateFor("grok-4.6"); !ok {
		t.Fatal("xAI rates missing after partial failure")
	}
	if _, ok := cat.RateFor("composer-2.5"); !ok {
		t.Fatal("Cursor rates missing after partial failure")
	}
	if _, ok := cat.RateFor("claude-opus-5"); ok {
		t.Fatal("Anthropic must be absent")
	}

	allFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(allFail.Close)
	if _, err := FetchOfficialRates(t.Context(), allFail.Client(), OfficialRateURLs{
		XAI: allFail.URL, Anthropic: allFail.URL, Cursor: allFail.URL,
	}); err == nil {
		t.Fatal("every source failing must error")
	}
}

func TestParseUSDRate(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"$2.00", 2, true},
		{"$10 / MTok", 10, true},
		{"$12.50 / MTok", 12.5, true},
		{"$0.5", 0.5, true},
		{"$0.25 / MTok1", 0.25, true},
		{"-", 0, false},
		{"", 0, false},
		{"n/a", 0, false},
	} {
		got := parseUSDRate(c.in)
		if !c.ok {
			if got != nil {
				t.Errorf("%q: got %v want unset", c.in, *got)
			}
			continue
		}
		if got == nil || *got != c.want {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

func assertRate(t *testing.T, r ModelRate, in, out, cacheRead, cacheWrite float64) {
	t.Helper()
	check := func(name string, p *float64, want float64) {
		t.Helper()
		if want < 0 {
			if p != nil {
				t.Fatalf("%s set, want unset: %v", name, *p)
			}
			return
		}
		if p == nil || *p != want {
			t.Fatalf("%s=%v want %v", name, p, want)
		}
	}
	check("input", r.InputPerMTok, in)
	check("output", r.OutputPerMTok, out)
	check("cacheRead", r.CacheReadPerMTok, cacheRead)
	check("cacheWrite", r.CacheWritePerMTok, cacheWrite)
}
