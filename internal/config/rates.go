package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/acoshift/grokwork/internal/grokrun"
)

// Agent runs cost real money, and the only thing the CLIs report is tokens. A
// rate table turns those tokens into dollars — per model, because that is the
// granularity every provider prices at, and a deployment that mixes a cheap title
// model with an expensive task model is the normal case, not the exception.
//
// Rates live in config rather than in code on purpose: published prices change,
// deployments negotiate their own, and a stale number compiled into the binary is
// worse than no number at all — it looks authoritative.

// ModelRate prices one model in dollars per million tokens.
//
// Every field is a pointer because "unset" and "free" are different claims. A
// missing rate must yield no dollar figure at all; rendering $0.00 for it would
// read as a measured cost of nothing, and a spend report that under-reports is
// the one failure mode nobody catches.
type ModelRate struct {
	InputPerMTok  *float64 `json:"inputPerMTok,omitempty"`
	OutputPerMTok *float64 `json:"outputPerMTok,omitempty"`
	// CacheReadPerMTok prices tokens served from the prompt cache (~0.1x input on
	// Anthropic). Every agent turn after the first re-reads the whole prefix, so on
	// a long session this class dominates the token count.
	CacheReadPerMTok *float64 `json:"cacheReadPerMTok,omitempty"`
	// CacheWritePerMTok prices tokens written into the cache (~1.25x input on
	// Anthropic). Deliberately not defaulted to the input rate: guessing 1.0x when
	// the real multiplier is 1.25x is a wrong number wearing a right number's
	// clothes, and this is a class claude reports on nearly every run.
	CacheWritePerMTok *float64 `json:"cacheWritePerMTok,omitempty"`
}

// TokenCounts is the billable token classes of one run, or of a summed rollup.
type TokenCounts struct {
	Input         int
	CacheRead     int
	CacheCreation int
	Output        int
}

// Total is every billable token in the bundle.
func (t TokenCounts) Total() int {
	return t.Input + t.CacheRead + t.CacheCreation + t.Output
}

// IsZero reports a bundle with nothing to price.
func (t TokenCounts) IsZero() bool { return t.Total() == 0 }

// Add accumulates o into t.
func (t *TokenCounts) Add(o TokenCounts) {
	t.Input += o.Input
	t.CacheRead += o.CacheRead
	t.CacheCreation += o.CacheCreation
	t.Output += o.Output
}

// Price is the dollar cost of t under this rate, and whether it could be priced.
//
// ok is false when any class with a non-zero count has no rate: pricing those
// tokens at zero would report a cost that is confidently too low, which is worse
// than reporting tokens and no cost. A class with zero tokens needs no rate, so a
// grok-only deployment never has to fill in the cache columns, and an empty rate
// prices an empty bundle (nothing to charge for).
func (r ModelRate) Price(t TokenCounts) (float64, bool) {
	total := 0.0
	for _, part := range []struct {
		tokens int
		rate   *float64
	}{
		{t.Input, r.InputPerMTok},
		{t.CacheRead, r.CacheReadPerMTok},
		{t.CacheCreation, r.CacheWritePerMTok},
		{t.Output, r.OutputPerMTok},
	} {
		if part.tokens == 0 {
			continue
		}
		if part.rate == nil {
			return 0, false
		}
		total += float64(part.tokens) * *part.rate / 1_000_000
	}
	return total, true
}

// IsEmpty reports a rate with no figure set at all — the shape a model gets when
// an operator saved the form without filling anything in.
func (r ModelRate) IsEmpty() bool {
	return r.InputPerMTok == nil && r.OutputPerMTok == nil &&
		r.CacheReadPerMTok == nil && r.CacheWritePerMTok == nil
}

// ModelRateKey normalizes a model name for rate lookup. Names reach us from
// config, from a session stamp, and from a curated dropdown, so case and padding
// must not decide whether a run is priced.
func ModelRateKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// ModelRateFor returns the configured rate for a model.
//
// ok is false for an unknown or unnamed model. An unnamed model is the common
// case worth being careful about: a session that never pinned one ran on whatever
// default the CLI chose, and pricing it against some other model's rate would
// invent a number.
func (c *Config) ModelRateFor(model string) (ModelRate, bool) {
	key := ModelRateKey(model)
	if key == "" || c == nil {
		return ModelRate{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.ModelRates[key]
	if !ok {
		// Tolerate a hand-edited config that wrote the name with its original
		// casing rather than the normalized key.
		for k, v := range c.ModelRates {
			if ModelRateKey(k) == key {
				r, ok = v, true
				break
			}
		}
	}
	if !ok || r.IsEmpty() {
		return ModelRate{}, false
	}
	return r, true
}

// PriceTokens prices a model's tokens. ok is false when the model has no rate, or
// has one that does not cover a class the run actually used.
func (c *Config) PriceTokens(model string, t TokenCounts) (float64, bool) {
	if t.IsZero() {
		return 0, false
	}
	r, ok := c.ModelRateFor(model)
	if !ok {
		return 0, false
	}
	return r.Price(t)
}

// SetModelRates replaces the whole rate table and persists it. The table is
// replaced rather than merged because the config form submits every row it
// rendered: a merge would make clearing a rate impossible.
func (c *Config) SetModelRates(rates map[string]ModelRate) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	clean := map[string]ModelRate{}
	for name, r := range rates {
		key := ModelRateKey(name)
		if key == "" || r.IsEmpty() {
			continue
		}
		clean[key] = r
	}
	c.mu.Lock()
	if len(clean) == 0 {
		c.ModelRates = nil
	} else {
		c.ModelRates = clean
	}
	err := c.saveLocked()
	c.mu.Unlock()
	return err
}

// ModelRateItem is one row of the rate table for the config UI. Values are
// pre-formatted strings so an unset rate renders as an empty input rather than
// "0", which the operator would then save as a real zero rate.
type ModelRateItem struct {
	Model      string
	Agent      string
	Input      string
	Output     string
	CacheRead  string
	CacheWrite string
	// Configured is true when this model has at least one rate set.
	Configured bool
	// Curated is false for a row that exists only because config names it — a
	// custom or self-hosted model id, which must stay editable but is not offered
	// as a choice anywhere else.
	Curated bool
}

// ModelRateItems lists the curated models first (so the common table can be
// filled in without typing a name), then any configured model that is not
// curated.
func (c *Config) ModelRateItems() []ModelRateItem {
	var rates map[string]ModelRate
	if c != nil {
		c.mu.RLock()
		rates = maps.Clone(c.ModelRates)
		c.mu.RUnlock()
	}
	return modelRateItemsFrom(rates)
}

// modelRateItemsFrom is the lock-free half, so Snapshot (which already holds the
// read lock) can build the same rows without a nested RLock — an RWMutex stops
// admitting readers once a writer is queued, so re-entering would deadlock
// against a concurrent config save from the web UI.
func modelRateItemsFrom(rates map[string]ModelRate) []ModelRateItem {
	if rates == nil {
		rates = map[string]ModelRate{}
	}
	seen := map[string]bool{}
	var out []ModelRateItem
	add := func(model, agent string, curated bool) {
		key := ModelRateKey(model)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		r := rates[key]
		out = append(out, ModelRateItem{
			Model:      model,
			Agent:      agent,
			Input:      formatRate(r.InputPerMTok),
			Output:     formatRate(r.OutputPerMTok),
			CacheRead:  formatRate(r.CacheReadPerMTok),
			CacheWrite: formatRate(r.CacheWritePerMTok),
			Configured: !r.IsEmpty(),
			Curated:    curated,
		})
	}
	for _, opt := range grokrun.ModelOptions() {
		add(opt.Value, opt.Agent.Label(), true)
	}
	for _, key := range slices.Sorted(maps.Keys(rates)) {
		agent := ""
		if a, ok := grokrun.AgentForModel(key); ok {
			agent = a.Label()
		}
		add(key, agent, false)
	}
	return out
}

// formatRate renders a rate for a form input. Unset stays empty — the whole point
// of the pointer.
func formatRate(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// ParseRate reads one rate form field. An empty field means unset (nil), which is
// how an operator clears a rate; a negative or unparseable value is refused
// rather than coerced, since a silently dropped rate reads as "not configured"
// and hides a typo.
func ParseRate(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// Operators paste prices from pricing pages, which write them with a dollar
	// sign and thousands separators.
	s = strings.TrimPrefix(s, "$")
	s = strings.ReplaceAll(s, ",", "")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("not a number: %q", s)
	}
	if v < 0 {
		return nil, fmt.Errorf("rate must not be negative: %q", s)
	}
	return &v, nil
}

func cloneModelRates(in map[string]ModelRate) map[string]ModelRate {
	if in == nil {
		return nil
	}
	out := make(map[string]ModelRate, len(in))
	for k, v := range in {
		out[k] = ModelRate{
			InputPerMTok:      cloneFloatPtr(v.InputPerMTok),
			OutputPerMTok:     cloneFloatPtr(v.OutputPerMTok),
			CacheReadPerMTok:  cloneFloatPtr(v.CacheReadPerMTok),
			CacheWritePerMTok: cloneFloatPtr(v.CacheWritePerMTok),
		}
	}
	return out
}

func cloneFloatPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
