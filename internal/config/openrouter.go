package config

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/grokrun"
)

const (
	defaultOpenRouterModelsURL = "https://openrouter.ai/api/v1/models"
	openRouterUserAgent        = "grokwork"
	maxOpenRouterBody          = 4 << 20
	openRouterHTTPTimeout      = 15 * time.Second
)

// OpenRouterCatalog is list prices from OpenRouter's public models API, keyed by
// lowercase OpenRouter id (x-ai/grok-4.6). Prices are already dollars per million
// tokens, matching ModelRate.
type OpenRouterCatalog struct {
	rates map[string]ModelRate
}

// OpenRouterFillResult is what FillModelRatesFromOpenRouter changed.
type OpenRouterFillResult struct {
	Filled    []string
	Unmatched []string
	Skipped   []string
}

type openRouterModels struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID string `json:"id"`
	// Pricing is raw so a single model with array/tiered pricing cannot fail
	// the whole catalog parse — that model is skipped, the rest still fill.
	Pricing jsontext.Value `json:"pricing"`
}

type openRouterPricing struct {
	Prompt          string `json:"prompt"`
	Completion      string `json:"completion"`
	InputCacheRead  string `json:"input_cache_read"`
	InputCacheWrite string `json:"input_cache_write"`
}

// ParseOpenRouterCatalog reads a GET /api/v1/models body. Variant ids (":batch",
// ":free") and "~" aliases are dropped so a lookup for anthropic/claude-opus-5
// cannot pick the batch discount.
func ParseOpenRouterCatalog(body []byte) (OpenRouterCatalog, error) {
	var parsed openRouterModels
	if err := json.Unmarshal(body, &parsed); err != nil {
		return OpenRouterCatalog{}, fmt.Errorf("openrouter models: decode: %w", err)
	}
	rates := make(map[string]ModelRate, len(parsed.Data))
	for _, m := range parsed.Data {
		id := strings.ToLower(strings.TrimSpace(m.ID))
		if id == "" || strings.HasPrefix(id, "~") || strings.Contains(id, ":") {
			continue
		}
		r, ok := rateFromOpenRouterPricing(m.Pricing)
		if !ok {
			continue
		}
		rates[id] = r
	}
	if len(rates) == 0 {
		return OpenRouterCatalog{}, fmt.Errorf("openrouter models: empty catalog")
	}
	return OpenRouterCatalog{rates: rates}, nil
}

// FetchOpenRouterCatalog GETs OpenRouter's public models list. modelsURL empty
// uses the public endpoint; client nil uses a 15s timeout client. No API key —
// the catalog is public, and grokwork does not bill through OpenRouter.
func FetchOpenRouterCatalog(ctx context.Context, client *http.Client, modelsURL string) (OpenRouterCatalog, error) {
	modelsURL = cmp.Or(strings.TrimSpace(modelsURL), defaultOpenRouterModelsURL)
	if client == nil {
		client = &http.Client{Timeout: openRouterHTTPTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return OpenRouterCatalog{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", openRouterUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return OpenRouterCatalog{}, fmt.Errorf("openrouter models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return OpenRouterCatalog{}, fmt.Errorf("openrouter models: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOpenRouterBody+1))
	if err != nil {
		return OpenRouterCatalog{}, fmt.Errorf("openrouter models: read: %w", err)
	}
	if len(body) > maxOpenRouterBody {
		return OpenRouterCatalog{}, fmt.Errorf("openrouter models: response too large")
	}
	return ParseOpenRouterCatalog(body)
}

// RateFor maps a grokwork model name onto an OpenRouter list price.
func (c OpenRouterCatalog) RateFor(model string) (ModelRate, string, bool) {
	if len(c.rates) == 0 {
		return ModelRate{}, "", false
	}
	for _, id := range openRouterIDs(model) {
		if r, ok := c.rates[id]; ok && !r.IsEmpty() {
			return r, id, true
		}
	}
	return ModelRate{}, "", false
}

// FillModelRatesFromOpenRouter writes catalog prices into empty rate cells.
// Already-set figures are left alone (a negotiated rate must not be overwritten
// by a public list price), and Cursor models are skipped — Cursor does not bill
// at OpenRouter/vendor list rates.
func (c *Config) FillModelRatesFromOpenRouter(cat OpenRouterCatalog) (OpenRouterFillResult, error) {
	if c == nil {
		return OpenRouterFillResult{}, fmt.Errorf("nil config")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	merged := cloneModelRates(c.ModelRates)
	if merged == nil {
		merged = map[string]ModelRate{}
	}
	var res OpenRouterFillResult
	seen := map[string]bool{}
	consider := func(name string) {
		key := ModelRateKey(name)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		if skipOpenRouterFill(name) {
			res.Skipped = append(res.Skipped, name)
			return
		}
		incoming, _, ok := cat.RateFor(name)
		if !ok {
			res.Unmatched = append(res.Unmatched, name)
			return
		}
		next, n := merged[key].fillBlanks(incoming)
		if n == 0 {
			return
		}
		merged[key] = next
		res.Filled = append(res.Filled, name)
	}
	for _, opt := range grokrun.ModelOptions() {
		consider(opt.Value)
	}
	for _, key := range slices.Sorted(maps.Keys(merged)) {
		consider(key)
	}
	if len(res.Filled) == 0 {
		return res, nil
	}
	c.ModelRates = merged
	return res, c.saveLocked()
}

func skipOpenRouterFill(model string) bool {
	if strings.Contains(model, "/") {
		return false
	}
	a, ok := grokrun.AgentForModel(model)
	return ok && a == grokrun.AgentCursor
}

func openRouterIDs(model string) []string {
	n := ModelRateKey(model)
	if n == "" {
		return nil
	}
	if strings.Contains(n, "/") {
		return []string{n}
	}
	var ids []string
	add := func(id string) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	switch {
	case strings.HasPrefix(n, "grok-"):
		base, stripped := strings.CutSuffix(n, "-xhigh")
		add("x-ai/" + n)
		if stripped {
			add("x-ai/" + base)
		}
	case strings.HasPrefix(n, "claude-"):
		add("anthropic/" + openRouterClaudeSlug(n))
		add("anthropic/" + n)
	}
	return ids
}

// openRouterClaudeSlug turns grokwork's hyphenated patch (claude-opus-4-8) into
// OpenRouter's dotted one (claude-opus-4.8). A name that is already dotted, or
// that has a single trailing version number (claude-opus-5), is unchanged.
func openRouterClaudeSlug(n string) string {
	parts := strings.Split(n, "-")
	if len(parts) < 3 {
		return n
	}
	major, minor := parts[len(parts)-2], parts[len(parts)-1]
	if !allDigits(major) || !allDigits(minor) {
		return n
	}
	return strings.Join(parts[:len(parts)-2], "-") + "-" + major + "." + minor
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (r ModelRate) fillBlanks(src ModelRate) (ModelRate, int) {
	n := 0
	fill := func(dst **float64, src *float64) {
		if *dst != nil || src == nil {
			return
		}
		*dst = new(*src)
		n++
	}
	out := r
	fill(&out.InputPerMTok, src.InputPerMTok)
	fill(&out.OutputPerMTok, src.OutputPerMTok)
	fill(&out.CacheReadPerMTok, src.CacheReadPerMTok)
	fill(&out.CacheWritePerMTok, src.CacheWritePerMTok)
	return out, n
}

// perTokenToPerMillion converts OpenRouter's USD-per-token strings into the
// dollars-per-million-tokens the rest of the rate table uses. A missing or
// unparseable field stays unset; an explicit "0" is a real free rate.
func rateFromOpenRouterPricing(raw jsontext.Value) (ModelRate, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return ModelRate{}, false
	}
	var p openRouterPricing
	if err := json.Unmarshal(raw, &p); err != nil {
		return ModelRate{}, false
	}
	r := ModelRate{
		InputPerMTok:      perTokenToPerMillion(p.Prompt),
		OutputPerMTok:     perTokenToPerMillion(p.Completion),
		CacheReadPerMTok:  perTokenToPerMillion(p.InputCacheRead),
		CacheWritePerMTok: perTokenToPerMillion(p.InputCacheWrite),
	}
	if r.IsEmpty() {
		return ModelRate{}, false
	}
	return r, true
}

func perTokenToPerMillion(s string) *float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return nil
	}
	perM := math.Round(v*1e6*1e8) / 1e8
	return new(perM)
}
