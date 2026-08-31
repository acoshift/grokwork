package config

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const openRouterFixture = `{
  "data": [
    {
      "id": "x-ai/grok-4.6",
      "pricing": {"prompt": "0.000002", "completion": "0.000006", "input_cache_read": "0.0000005"}
    },
    {
      "id": "x-ai/grok-4.5",
      "pricing": {"prompt": "0.000002", "completion": "0.000006", "input_cache_read": "0.0000003"}
    },
    {
      "id": "anthropic/claude-opus-5",
      "extra": "ignored",
      "pricing": {
        "prompt": "0.000005",
        "completion": "0.000025",
        "input_cache_read": "0.0000005",
        "input_cache_write": "0.00000625",
        "request": "0"
      }
    },
    {
      "id": "anthropic/claude-opus-5:batch",
      "pricing": {"prompt": "0.0000025", "completion": "0.0000125"}
    },
    {
      "id": "~anthropic/claude-opus-latest",
      "pricing": {"prompt": "0.000009", "completion": "0.000009"}
    },
    {
      "id": "anthropic/claude-opus-4.8",
      "pricing": {
        "prompt": "0.000005",
        "completion": "0.000025",
        "input_cache_read": "0.0000005",
        "input_cache_write": "0.00000625"
      }
    },
    {
      "id": "anthropic/claude-haiku-4.5",
      "pricing": {
        "prompt": "0.000001",
        "completion": "0.000005",
        "input_cache_read": "0.0000001",
        "input_cache_write": "0.00000125"
      }
    },
    {
      "id": "example/tiered",
      "pricing": [
        {"prompt": "0.000001", "completion": "0.000002"},
        {"prompt": "0.000003", "completion": "0.000004"}
      ]
    }
  ]
}`

func TestParseOpenRouterCatalogMapsPerTokenToPerMillion(t *testing.T) {
	cat, err := ParseOpenRouterCatalog([]byte(openRouterFixture))
	if err != nil {
		t.Fatal(err)
	}
	r, id, ok := cat.RateFor("claude-opus-5")
	if !ok || id != "anthropic/claude-opus-5" {
		t.Fatalf("opus-5: ok=%v id=%q", ok, id)
	}
	assertRate(t, r, 5, 25, 0.5, 6.25)

	// The :batch row is cheaper; matching must still use the default id.
	if r.InputPerMTok == nil || *r.InputPerMTok != 5 {
		t.Fatalf("must not pick the batch price: %+v", r)
	}

	r, id, ok = cat.RateFor("grok-4.6-xhigh")
	if !ok || id != "x-ai/grok-4.6" {
		t.Fatalf("grok-4.6-xhigh: ok=%v id=%q", ok, id)
	}
	assertRate(t, r, 2, 6, 0.5, -1)

	r, id, ok = cat.RateFor("claude-opus-4-8")
	if !ok || id != "anthropic/claude-opus-4.8" {
		t.Fatalf("opus-4-8: ok=%v id=%q", ok, id)
	}
	assertRate(t, r, 5, 25, 0.5, 6.25)

	r, _, ok = cat.RateFor("claude-haiku-4-5")
	if !ok {
		t.Fatal("haiku-4-5 must map to claude-haiku-4.5")
	}
	assertRate(t, r, 1, 5, 0.1, 1.25)

	if _, _, ok := cat.RateFor("example/tiered"); ok {
		t.Fatal("array pricing must be skipped, not fail the catalog")
	}
	if _, _, ok := cat.RateFor("composer-2.5"); ok {
		t.Fatal("unmapped name must not invent a rate")
	}
	if _, _, ok := cat.RateFor(""); ok {
		t.Fatal("empty name must not match")
	}
	// A name that is already an OpenRouter id matches exactly.
	if _, id, ok := cat.RateFor("x-ai/grok-4.5"); !ok || id != "x-ai/grok-4.5" {
		t.Fatalf("exact id: ok=%v id=%q", ok, id)
	}
}

func TestParseOpenRouterCatalogRejectsEmpty(t *testing.T) {
	if _, err := ParseOpenRouterCatalog([]byte(`{"data":[]}`)); err == nil {
		t.Fatal("empty catalog must error")
	}
	if _, err := ParseOpenRouterCatalog([]byte(`{`)); err == nil {
		t.Fatal("truncated JSON must error")
	}
}

func TestOpenRouterIDs(t *testing.T) {
	for _, c := range []struct {
		name string
		want []string
	}{
		{"grok-4.6", []string{"x-ai/grok-4.6"}},
		{"GROK-4.6-xhigh", []string{"x-ai/grok-4.6-xhigh", "x-ai/grok-4.6"}},
		{"claude-opus-5", []string{"anthropic/claude-opus-5"}},
		{"claude-opus-4-8", []string{"anthropic/claude-opus-4.8", "anthropic/claude-opus-4-8"}},
		{"claude-haiku-4-5", []string{"anthropic/claude-haiku-4.5", "anthropic/claude-haiku-4-5"}},
		{"anthropic/claude-opus-5", []string{"anthropic/claude-opus-5"}},
		{"composer-2.5", nil},
		{"", nil},
	} {
		got := openRouterIDs(c.name)
		if !slices.Equal(got, c.want) {
			t.Errorf("%q → %v want %v", c.name, got, c.want)
		}
	}
}

func TestFillModelRatesFromOpenRouterFillsBlanksOnly(t *testing.T) {
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

	cat, err := ParseOpenRouterCatalog([]byte(openRouterFixture))
	if err != nil {
		t.Fatal(err)
	}

	// A negotiated grok input must survive; empty output is the cell the fill is for.
	if err := cfg.SetModelRates(map[string]ModelRate{
		"grok-4.6":       {InputPerMTok: new(9.0)},
		"my-local-model": {InputPerMTok: new(0.1), OutputPerMTok: new(0.2)},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := cfg.FillModelRatesFromOpenRouter(cat)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Filled, "grok-4.6") || !slices.Contains(res.Filled, "claude-opus-5") {
		t.Fatalf("filled=%v", res.Filled)
	}
	if slices.Contains(res.Filled, "composer-2.5") || slices.Contains(res.Filled, "my-local-model") {
		t.Fatalf("must not fill cursor or custom unmatched: %v", res.Filled)
	}
	if !slices.Contains(res.Skipped, "composer-2.5") {
		t.Fatalf("cursor must be skipped: %v", res.Skipped)
	}
	if !slices.Contains(res.Unmatched, "my-local-model") {
		t.Fatalf("custom unmatched: %v", res.Unmatched)
	}

	grok, ok := cfg.ModelRateFor("grok-4.6")
	if !ok || grok.InputPerMTok == nil || *grok.InputPerMTok != 9 {
		t.Fatalf("negotiated input overwritten: %+v", grok)
	}
	if grok.OutputPerMTok == nil || *grok.OutputPerMTok != 6 {
		t.Fatalf("blank output not filled: %+v", grok)
	}
	opus, ok := cfg.ModelRateFor("claude-opus-5")
	if !ok {
		t.Fatal("opus-5 not filled")
	}
	assertRate(t, opus, 5, 25, 0.5, 6.25)
	if _, ok := cfg.ModelRateFor("composer-2.5"); ok {
		t.Fatal("cursor model must stay unpriced")
	}

	// A second fill is a no-op save-wise: every matched cell is already set.
	again, err := cfg.FillModelRatesFromOpenRouter(cat)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Filled) != 0 {
		t.Fatalf("second fill must not rewrite: %v", again.Filled)
	}

	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		ModelRates map[string]ModelRate `json:"modelRates"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.ModelRates["grok-4.6"].InputPerMTok == nil || *file.ModelRates["grok-4.6"].InputPerMTok != 9 {
		t.Fatalf("persisted negotiated input lost: %+v", file.ModelRates["grok-4.6"])
	}
}

func TestFetchOpenRouterCatalog(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		if r.Method != http.MethodGet {
			t.Errorf("method=%s", r.Method)
		}
		io.WriteString(w, openRouterFixture)
	}))
	t.Cleanup(srv.Close)

	cat, err := FetchOpenRouterCatalog(t.Context(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if ua != openRouterUserAgent {
		t.Fatalf("User-Agent=%q", ua)
	}
	if _, _, ok := cat.RateFor("claude-opus-5"); !ok {
		t.Fatal("fetched catalog missing opus-5")
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(fail.Close)
	if _, err := FetchOpenRouterCatalog(t.Context(), fail.Client(), fail.URL); err == nil {
		t.Fatal("HTTP 502 must error")
	}
}

func TestPerTokenToPerMillion(t *testing.T) {
	if perTokenToPerMillion("") != nil || perTokenToPerMillion("nope") != nil || perTokenToPerMillion("-1") != nil {
		t.Fatal("invalid must be unset")
	}
	z := perTokenToPerMillion("0")
	if z == nil || *z != 0 {
		t.Fatalf("explicit zero: %v", z)
	}
	got := perTokenToPerMillion("0.000005")
	if got == nil || *got != 5 {
		t.Fatalf("0.000005 → %v want 5", got)
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
