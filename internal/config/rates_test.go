package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func rate(v float64) *float64 { return &v }

// The whole point of the pointer: an unset rate reports tokens and no dollars.
// Pricing it at zero would report a cost that is confidently too low.
func TestUnsetRateYieldsNoDollarFigure(t *testing.T) {
	tokens := TokenCounts{Input: 1000, Output: 500}

	// Nothing configured at all.
	if _, ok := (ModelRate{}).Price(tokens); ok {
		t.Fatal("empty rate must not price")
	}
	// Input priced, output missing: the run used output tokens, so the answer is
	// "unknown", not "the input half".
	partial := ModelRate{InputPerMTok: rate(3)}
	if got, ok := partial.Price(tokens); ok {
		t.Fatalf("partial rate priced anyway: %v", got)
	}
	// Both classes present → a real figure.
	full := ModelRate{InputPerMTok: rate(3), OutputPerMTok: rate(15)}
	got, ok := full.Price(tokens)
	if !ok {
		t.Fatal("full rate must price")
	}
	if want := 0.0105; got < want-1e-12 || got > want+1e-12 {
		t.Fatalf("price=%v want %v", got, want)
	}
	// An explicit zero is a rate, not an absence: a free model prices at $0.
	free := ModelRate{InputPerMTok: rate(0), OutputPerMTok: rate(0)}
	if got, ok := free.Price(tokens); !ok || got != 0 {
		t.Fatalf("explicit zero rate: %v %v", got, ok)
	}
}

// A class with no tokens needs no rate, so a grok-only deployment never has to
// fill in the cache columns to see dollars.
func TestPriceIgnoresRatesForUnusedClasses(t *testing.T) {
	r := ModelRate{InputPerMTok: rate(3), OutputPerMTok: rate(15)}
	if _, ok := r.Price(TokenCounts{Input: 10, Output: 1}); !ok {
		t.Fatal("no cache tokens: must price")
	}
	// One cache-read token is enough to need the cache-read rate.
	if _, ok := r.Price(TokenCounts{Input: 10, Output: 1, CacheRead: 1}); ok {
		t.Fatal("cache tokens without a cache rate must not price")
	}
	// Cache creation is priced by the cache-WRITE rate, never silently by input:
	// providers charge a premium for it (1.25x on Anthropic).
	withRead := ModelRate{InputPerMTok: rate(3), OutputPerMTok: rate(15), CacheReadPerMTok: rate(0.3)}
	if _, ok := withRead.Price(TokenCounts{Input: 1, Output: 1, CacheCreation: 1}); ok {
		t.Fatal("cache creation must require the cache-write rate")
	}
	all := withRead
	all.CacheWritePerMTok = rate(3.75)
	price, ok := all.Price(TokenCounts{Input: 1_000_000, CacheRead: 1_000_000, CacheCreation: 1_000_000, Output: 1_000_000})
	if !ok {
		t.Fatal("complete rate must price")
	}
	if want := 3 + 0.3 + 3.75 + 15.0; price != want {
		t.Fatalf("price=%v want %v", price, want)
	}
	// Nothing to price is not an error but is not $0.00 either — there is no run.
	if _, ok := all.Price(TokenCounts{}); !ok {
		t.Fatal("empty bundle under a complete rate should price trivially")
	}
}

func TestPriceTokensLookupIsNameNormalized(t *testing.T) {
	c := &Config{ModelRates: map[string]ModelRate{
		"claude-opus-5": {InputPerMTok: rate(5), OutputPerMTok: rate(25)},
	}}
	tokens := TokenCounts{Input: 1_000_000, Output: 0}
	for _, name := range []string{"claude-opus-5", "Claude-Opus-5", "  claude-opus-5  ", "claude-opus-5-high", "claude-opus-5-xhigh"} {
		got, ok := c.PriceTokens(name, tokens)
		if !ok || got != 5 {
			t.Fatalf("%q → %v %v", name, got, ok)
		}
	}
	// An unnamed model is the case worth being strict about: a session that never
	// pinned one ran on whatever the CLI defaulted to, so pricing it against some
	// other model's rate would invent a number.
	if _, ok := c.PriceTokens("", tokens); ok {
		t.Fatal("empty model name must not price")
	}
	if _, ok := c.PriceTokens("grok-4.5", tokens); ok {
		t.Fatal("unconfigured model must not price")
	}
	// Zero tokens never price — a report showing "$0.00" for a run that reported
	// nothing is a lie about a measurement that never happened.
	if _, ok := c.PriceTokens("claude-opus-5", TokenCounts{}); ok {
		t.Fatal("zero tokens must not price")
	}
	// Nil receiver stays safe: rollups run with whatever config they were handed.
	var nilCfg *Config
	if _, ok := nilCfg.PriceTokens("claude-opus-5", tokens); ok {
		t.Fatal("nil config must not price")
	}
}

// saveLocked must persist the rate table or the next config write from the web UI
// silently wipes every price the operator entered.
func TestSaveLockedPreservesModelRates(t *testing.T) {
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
		"Claude-Opus-5": {InputPerMTok: rate(5), OutputPerMTok: rate(25), CacheReadPerMTok: rate(0.5), CacheWritePerMTok: rate(6.25)},
		// Empty rows are dropped rather than stored as empty objects.
		"grok-4.5": {},
	}); err != nil {
		t.Fatal(err)
	}

	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw2, &again); err != nil {
		t.Fatal(err)
	}
	if len(again.ModelRates) != 1 {
		t.Fatalf("rates=%+v", again.ModelRates)
	}
	// Stored under the normalized key so a lookup by any casing finds it.
	r, ok := again.ModelRates["claude-opus-5"]
	if !ok {
		t.Fatalf("key not normalized: %+v", again.ModelRates)
	}
	if r.InputPerMTok == nil || *r.InputPerMTok != 5 || r.CacheWritePerMTok == nil || *r.CacheWritePerMTok != 6.25 {
		t.Fatalf("rate lost: %+v", r)
	}

	// Clearing the table removes the key entirely rather than leaving {}.
	if err := cfg.SetModelRates(nil); err != nil {
		t.Fatal(err)
	}
	raw3, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw3, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["modelRates"]; ok {
		t.Fatalf("cleared table must be omitted: %s", raw3)
	}
}

func TestParseRate(t *testing.T) {
	// Empty means unset, which is how an operator clears a price.
	if got, err := ParseRate("  "); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	// Pasted straight off a pricing page.
	got, err := ParseRate("$1,250.50")
	if err != nil || got == nil || *got != 1250.5 {
		t.Fatalf("pasted: %v %v", got, err)
	}
	// Refused rather than coerced: a dropped typo reads as "not configured", which
	// silently turns dollars off instead of reporting the mistake.
	if _, err := ParseRate("three"); err == nil {
		t.Fatal("garbage must error")
	}
	if _, err := ParseRate("-1"); err == nil {
		t.Fatal("negative must error")
	}
	if got, err := ParseRate("0"); err != nil || got == nil || *got != 0 {
		t.Fatalf("explicit zero is a rate: %v %v", got, err)
	}
}

func TestModelRateItemsCoverCuratedAndCustomModels(t *testing.T) {
	c := &Config{ModelRates: map[string]ModelRate{
		"claude-opus-5":  {InputPerMTok: rate(5)},
		"my-local-model": {InputPerMTok: rate(0.1), OutputPerMTok: rate(0.2)},
	}}
	items := c.ModelRateItems()
	byModel := map[string]ModelRateItem{}
	for _, it := range items {
		if _, dup := byModel[it.Model]; dup {
			t.Fatalf("duplicate row for %q", it.Model)
		}
		byModel[it.Model] = it
	}
	// Curated models are always offered, priced or not, so the table can be filled
	// in without typing names. Grok/Claude effort aliases collapse to the base id.
	if _, dup := byModel["grok-4.6-xhigh"]; dup {
		t.Fatal("effort-suffixed grok name must not be its own rate row")
	}
	grok, ok := byModel["grok-4.6"]
	if !ok || grok.Configured || grok.Input != "" || !grok.Curated {
		t.Fatalf("curated unpriced row: %+v", grok)
	}
	opus := byModel["claude-opus-5"]
	if !opus.Configured || opus.Input != "5" || opus.Output != "" || !opus.Curated {
		t.Fatalf("curated priced row: %+v", opus)
	}
	// A configured name that is not curated still gets an editable row, or the
	// operator could never change or clear it.
	custom, ok := byModel["my-local-model"]
	if !ok || custom.Curated || !custom.Configured || custom.Output != "0.2" {
		t.Fatalf("custom row: %+v", custom)
	}

	snap := c.Snapshot()
	if snap.ModelRatesSet != 2 {
		t.Fatalf("ModelRatesSet=%d want 2", snap.ModelRatesSet)
	}
	if len(snap.ModelRates) != len(items) {
		t.Fatalf("snapshot rows=%d want %d", len(snap.ModelRates), len(items))
	}
}
