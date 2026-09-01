package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoshift/grokwork/internal/grokrun"
)

const (
	defaultXAIRatesURL       = "https://docs.x.ai/developers/models.md"
	defaultAnthropicRatesURL = "https://platform.claude.com/docs/en/about-claude/pricing.md"
	defaultCursorRatesURL    = "https://cursor.com/docs/models-and-pricing.md"
	officialRatesUserAgent   = "grokwork"
	maxOfficialRatesBody     = 2 << 20
	officialRatesHTTPTimeout = 15 * time.Second
)

// OfficialRateURLs overrides the three vendor markdown docs. Empty fields use
// the public defaults. Tests point each field at an httptest.Server.
type OfficialRateURLs struct {
	XAI       string
	Anthropic string
	Cursor    string
}

// OfficialRateCatalog is list prices from each agent's official docs, already
// in dollars per million tokens, keyed by grokwork model name.
type OfficialRateCatalog struct {
	rates   map[string]ModelRate
	sources []string
	errors  []string
}

// OfficialFillResult is what ApplyOfficialRates changed.
type OfficialFillResult struct {
	Updated   []string
	Unmatched []string
	Sources   []string
	Errors    []string
}

type officialSource struct {
	name  string
	url   string
	parse func(body []byte) map[string]ModelRate
}

// FetchOfficialRates GETs xAI, Anthropic, and Cursor markdown pricing pages in
// parallel and maps them onto grokwork model names. One failed source does not
// fail the others; the catalog errors only when every source fails.
func FetchOfficialRates(ctx context.Context, client *http.Client, urls OfficialRateURLs) (OfficialRateCatalog, error) {
	if client == nil {
		client = &http.Client{Timeout: officialRatesHTTPTimeout}
	}
	sources := []officialSource{
		{name: "xAI", url: cmp.Or(strings.TrimSpace(urls.XAI), defaultXAIRatesURL), parse: parseXAIRates},
		{name: "Anthropic", url: cmp.Or(strings.TrimSpace(urls.Anthropic), defaultAnthropicRatesURL), parse: parseAnthropicRates},
		{name: "Cursor", url: cmp.Or(strings.TrimSpace(urls.Cursor), defaultCursorRatesURL), parse: parseCursorRates},
	}
	var (
		mu  sync.Mutex
		cat = OfficialRateCatalog{rates: map[string]ModelRate{}}
		wg  sync.WaitGroup
	)
	for _, src := range sources {
		wg.Go(func() {
			got := fetchOfficialDoc(ctx, client, src.url)
			if got.err != nil {
				mu.Lock()
				cat.errors = append(cat.errors, src.name+": "+got.err.Error())
				mu.Unlock()
				return
			}
			parsed := src.parse(got.body)
			mu.Lock()
			defer mu.Unlock()
			if len(parsed) == 0 {
				cat.errors = append(cat.errors, src.name+": no model prices in page")
				return
			}
			cat.sources = append(cat.sources, src.name)
			maps.Copy(cat.rates, parsed)
		})
	}
	wg.Wait()
	slices.Sort(cat.sources)
	slices.Sort(cat.errors)
	if len(cat.rates) == 0 {
		if len(cat.errors) == 0 {
			return OfficialRateCatalog{}, fmt.Errorf("official rates: empty catalog")
		}
		return OfficialRateCatalog{}, fmt.Errorf("official rates: %s", strings.Join(cat.errors, "; "))
	}
	return cat, nil
}

type fetchResult struct {
	body []byte
	err  error
}

func fetchOfficialDoc(ctx context.Context, client *http.Client, rawURL string) fetchResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fetchResult{err: err}
	}
	req.Header.Set("User-Agent", officialRatesUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return fetchResult{err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fetchResult{err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOfficialRatesBody+1))
	if err != nil {
		return fetchResult{err: err}
	}
	if len(body) > maxOfficialRatesBody {
		return fetchResult{err: errors.New("response too large")}
	}
	return fetchResult{body: body}
}

// RateFor returns the official list price for a grokwork model name.
func (c OfficialRateCatalog) RateFor(model string) (ModelRate, bool) {
	r, ok := c.rates[ModelRateKey(model)]
	if !ok || r.IsEmpty() {
		return ModelRate{}, false
	}
	return r, true
}

// ApplyOfficialRates overwrites matched rows with official list prices. Custom
// unmatched names are left alone. Unlike the old OpenRouter fill, a cell that
// already has a figure is replaced — refreshing published prices is the point.
func (c *Config) ApplyOfficialRates(cat OfficialRateCatalog) (OfficialFillResult, error) {
	if c == nil {
		return OfficialFillResult{}, fmt.Errorf("nil config")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	merged := cloneModelRates(c.ModelRates)
	if merged == nil {
		merged = map[string]ModelRate{}
	}
	res := OfficialFillResult{Sources: slices.Clone(cat.sources), Errors: slices.Clone(cat.errors)}
	seen := map[string]bool{}
	consider := func(name string) {
		key := ModelRateKey(name)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		incoming, ok := cat.RateFor(name)
		if !ok {
			res.Unmatched = append(res.Unmatched, name)
			return
		}
		merged[key] = incoming
		res.Updated = append(res.Updated, name)
	}
	for _, opt := range grokrun.ModelOptions() {
		consider(opt.Value)
	}
	for _, key := range slices.Sorted(maps.Keys(merged)) {
		consider(key)
	}
	if len(res.Updated) == 0 {
		return res, nil
	}
	c.ModelRates = merged
	return res, c.saveLocked()
}

func parseXAIRates(body []byte) map[string]ModelRate {
	out := map[string]ModelRate{}
	for _, row := range pricedRows(string(body)) {
		name := xaiModelKey(row["model"])
		if name == "" {
			continue
		}
		if r, ok := rateFromCells(row); ok {
			out[name] = r
			out[name+"-xhigh"] = r
			out[name+"-low"] = r
		}
	}
	return out
}

func parseAnthropicRates(body []byte) map[string]ModelRate {
	out := map[string]ModelRate{}
	for _, row := range pricedRows(string(body)) {
		name := anthropicModelKey(row["model"])
		if name == "" {
			continue
		}
		if r, ok := rateFromCells(row); ok {
			out[name] = r
		}
	}
	return out
}

func parseCursorRates(body []byte) map[string]ModelRate {
	out := map[string]ModelRate{}
	for _, row := range pricedRows(string(body)) {
		name := cursorModelKey(row["model"])
		if name == "" {
			continue
		}
		r, ok := rateFromCells(row)
		if !ok {
			continue
		}
		aliases := cursorAliases(name)
		if len(aliases) == 0 {
			out[name] = r
			continue
		}
		for _, alias := range aliases {
			out[alias] = r
		}
	}
	return out
}

func rateFromCells(row map[string]string) (ModelRate, bool) {
	r := ModelRate{
		InputPerMTok:      parseUSDRate(row["input"]),
		OutputPerMTok:     parseUSDRate(row["output"]),
		CacheReadPerMTok:  parseUSDRate(row["cacheRead"]),
		CacheWritePerMTok: parseUSDRate(row["cacheWrite"]),
	}
	if r.IsEmpty() {
		return ModelRate{}, false
	}
	return r, true
}

// parseUSDRate reads a table cell that is already dollars per million tokens
// ($2.00, $10 / MTok, $0.5, "-"). Dash/empty is unset, not free.
func parseUSDRate(s string) *float64 {
	s = strings.TrimSpace(stripMDLink(s))
	s = strings.ReplaceAll(s, "\u00a0", " ")
	if s == "" || s == "-" || strings.EqualFold(s, "n/a") {
		return nil
	}
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimSpace(s)
	end := 0
	for i := range len(s) {
		c := s[i]
		if c >= '0' && c <= '9' || c == '.' {
			end = i + 1
			continue
		}
		break
	}
	if end == 0 {
		return nil
	}
	v, err := ParseRate(s[:end])
	if err != nil {
		return nil
	}
	return v
}

func xaiModelKey(cell string) string {
	cell = strings.TrimSpace(stripMDLink(cell))
	if cell == "" {
		return ""
	}
	if strings.Contains(cell, "≥") || strings.Contains(cell, ">=") {
		return ""
	}
	cell, _, _ = strings.Cut(cell, "(")
	key := strings.ToLower(strings.TrimSpace(cell))
	if !strings.HasPrefix(key, "grok-") {
		return ""
	}
	return key
}

func anthropicModelKey(cell string) string {
	cell = strings.TrimSpace(stripMDLink(cell))
	lower := strings.ToLower(cell)
	if cell == "" || strings.Contains(lower, "retired") || strings.Contains(lower, "mythos") {
		return ""
	}
	cell, _, _ = strings.Cut(cell, "(")
	cell = strings.ToLower(strings.TrimSpace(cell))
	if cell == "" {
		return ""
	}
	cell = strings.ReplaceAll(cell, ".", "-")
	fields := strings.Fields(cell)
	if len(fields) == 0 {
		return ""
	}
	key := strings.Join(fields, "-")
	if !strings.HasPrefix(key, "claude-") {
		return ""
	}
	return key
}

func cursorModelKey(cell string) string {
	cell = strings.TrimSpace(stripMDLink(cell))
	if cell == "" {
		return ""
	}
	fast := strings.Contains(strings.ToLower(cell), "(fast)")
	// Drop other parentheticals after capturing Fast.
	for {
		start := strings.Index(cell, "(")
		end := strings.Index(cell, ")")
		if start < 0 || end < 0 || end < start {
			break
		}
		cell = strings.TrimSpace(cell[:start] + cell[end+1:])
	}
	fields := strings.Fields(strings.ToLower(cell))
	if len(fields) == 0 {
		return ""
	}
	key := strings.Join(fields, "-")
	if fast {
		key += "-fast"
	}
	return key
}

// cursorAliases maps a Cursor docs slug onto the grokwork picker names that
// bill at that price (effort suffixes, cursor- prefix).
func cursorAliases(key string) []string {
	switch {
	case key == "grok-4.6" || key == "grok-4.5":
		return []string{"cursor-" + key + "-high", "cursor-" + key + "-xhigh"}
	case strings.HasPrefix(key, "claude-"):
		// Docs write "Claude Fable 5.1"; picker ids hyphenate the minor
		// (claude-fable-5-1-thinking-high), matching the Claude CLI id.
		return []string{strings.ReplaceAll(key, ".", "-") + "-thinking-high"}
	case key == "gpt-5.6-sol":
		return []string{"gpt-5.6-sol-medium"}
	case key == "gemini-3.7-flash":
		return []string{"gemini-3.7-flash-high"}
	case key == "glm-5.2":
		return []string{"glm-5.2-high", "glm-5.2-max"}
	case key == "kimi-k3":
		return []string{"kimi-k3-max"}
	}
	return nil
}

func stripMDLink(s string) string {
	for {
		mid := strings.Index(s, "](")
		if mid < 0 {
			return s
		}
		lb := strings.LastIndex(s[:mid], "[")
		rest := s[mid+2:]
		rb := strings.Index(rest, ")")
		if lb < 0 || rb < 0 {
			return s
		}
		s = s[:lb] + s[lb+1:mid] + rest[rb+1:]
	}
}

func pricedRows(md string) []map[string]string {
	var out []map[string]string
	for _, table := range markdownTables(md) {
		if len(table) < 2 {
			continue
		}
		col := map[int]string{}
		for i, h := range table[0] {
			if role := classifyRateHeader(h); role != "" {
				col[i] = role
			}
		}
		if !headerHas(col, "model") || !headerHas(col, "input") || !headerHas(col, "output") {
			continue
		}
		// Batch and fast-mode tables list only input/output and would overwrite
		// the standard rate with a discount or surcharge.
		if !headerHas(col, "cacheRead") && !headerHas(col, "cacheWrite") {
			continue
		}
		for _, cells := range table[1:] {
			row := map[string]string{}
			for i, role := range col {
				if i < len(cells) {
					row[role] = cells[i]
				}
			}
			if strings.TrimSpace(row["model"]) != "" {
				out = append(out, row)
			}
		}
	}
	return out
}

func headerHas(col map[int]string, role string) bool {
	for _, r := range col {
		if r == role {
			return true
		}
	}
	return false
}

func classifyRateHeader(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.ReplaceAll(h, "/", " ")
	switch {
	case strings.Contains(h, "model"):
		return "model"
	case strings.Contains(h, "1h") && strings.Contains(h, "cache"):
		return ""
	case strings.Contains(h, "cache") && strings.Contains(h, "write"):
		return "cacheWrite"
	case strings.Contains(h, "cached") || (strings.Contains(h, "cache") && (strings.Contains(h, "read") || strings.Contains(h, "hit"))):
		return "cacheRead"
	case strings.Contains(h, "output"):
		return "output"
	case strings.Contains(h, "input"):
		return "input"
	default:
		return ""
	}
}

func markdownTables(md string) [][][]string {
	var tables [][][]string
	var cur [][]string
	flush := func() {
		if len(cur) > 0 {
			tables = append(tables, cur)
			cur = nil
		}
	}
	for line := range strings.SplitSeq(md, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			flush()
			continue
		}
		cells := splitPipeRow(line)
		if len(cells) == 0 || isTableSep(cells) {
			continue
		}
		cur = append(cur, cells)
	}
	flush()
	return tables
}

func splitPipeRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	raw := strings.Split(line, "|")
	out := make([]string, len(raw))
	for i, c := range raw {
		out[i] = strings.TrimSpace(c)
	}
	return out
}

func isTableSep(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSpace(c)
		c = strings.Trim(c, ":-")
		if c != "" {
			return false
		}
	}
	return true
}
