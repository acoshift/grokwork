package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/grokrun"
)

func TestFreshConfigTreatsEveryCuratedModelEnabled(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, opt := range grokrun.ModelOptions() {
		if !cfg.ModelAllowed(opt.Value) {
			t.Errorf("%q disabled on a fresh config", opt.Value)
		}
	}
	if n := len(cfg.DisabledModelNames()); n != 0 {
		t.Fatalf("denylist=%d want empty", n)
	}
	if cfg.Snapshot().DisabledModelCount != 0 {
		t.Fatalf("snapshot count=%d", cfg.Snapshot().DisabledModelCount)
	}
}

func TestDisabledModelsRoundTripThroughLoadAndSave(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("claude-opus-5-high", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("grok-4.5-high", true); err != nil {
		t.Fatal(err)
	}
	if cfg.ModelAllowed("claude-opus-5-high") || cfg.ModelAllowed("grok-4.5-high") {
		t.Fatal("names still allowed after disable")
	}
	if !cfg.ModelAllowed("composer-2.5") {
		t.Fatal("unrelated name was disabled")
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ModelAllowed("claude-opus-5-high") || reloaded.ModelAllowed("grok-4.5-high") {
		t.Fatal("disable did not survive reload")
	}
	if !reloaded.ModelAllowed("composer-2.5") {
		t.Fatal("unrelated name disabled after reload")
	}
}

// saveLocked must persist the denylist or the next unrelated config write from
// the web UI silently re-enables every model the operator turned off.
func TestSaveLockedPreservesDisabledModels(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("claude-opus-5-high", true); err != nil {
		t.Fatal(err)
	}
	// An unrelated save is the trap: dropping disabledModels from the marshal
	// whitelist would wipe the denylist while leaving rates intact.
	if err := cfg.SetModelRates(map[string]ModelRate{
		"grok-4.5": {InputPerMTok: rate(3), OutputPerMTok: rate(15)},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(again.DisabledModels, "claude-opus-5-high") {
		t.Fatalf("denylist lost on unrelated save: %s", raw)
	}
	if _, ok := again.ModelRates["grok-4.5"]; !ok {
		t.Fatal("rates were not saved either")
	}

	if err := cfg.SetDisabledModels(nil); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw2, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["disabledModels"]; ok {
		t.Fatalf("cleared denylist must be omitted: %s", raw2)
	}
}

func TestDisableAllClaudeAgentModels(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetAgentModelsDisabled("claude", true); err != nil {
		t.Fatal(err)
	}
	for _, opt := range grokrun.ModelOptions() {
		allowed := cfg.ModelAllowed(opt.Value)
		if opt.Agent == grokrun.AgentClaude && allowed {
			t.Errorf("claude %q still allowed", opt.Value)
		}
		if opt.Agent != grokrun.AgentClaude && !allowed {
			t.Errorf("%s %q was disabled with Claude", opt.Agent, opt.Value)
		}
	}

	// Individual re-enable of one Claude model after "disable all" works because
	// the store is the expanded list, not a parallel agent flag.
	if err := cfg.SetModelDisabled("claude-haiku-4-5-high", false); err != nil {
		t.Fatal(err)
	}
	if !cfg.ModelAllowed("claude-haiku-4-5-high") {
		t.Fatal("re-enable of one Claude model failed")
	}
	if cfg.ModelAllowed("claude-opus-5-high") {
		t.Fatal("sibling Claude model was re-enabled")
	}
}

func TestDisableCursorGPTFamily(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetFamilyModelsDisabled("cursor", "gpt", true); err != nil {
		t.Fatal(err)
	}
	var gpt, other int
	for _, opt := range grokrun.ModelOptions() {
		if opt.Agent != grokrun.AgentCursor {
			if !cfg.ModelAllowed(opt.Value) {
				t.Errorf("non-cursor %q disabled", opt.Value)
			}
			continue
		}
		fam := grokrun.ModelFamily(opt.Value)
		allowed := cfg.ModelAllowed(opt.Value)
		if fam == "gpt" {
			gpt++
			if allowed {
				t.Errorf("cursor gpt %q still allowed", opt.Value)
			}
		} else if !allowed {
			t.Errorf("cursor %s %q was disabled with GPT", fam, opt.Value)
		} else {
			other++
		}
	}
	if gpt == 0 {
		t.Fatal("catalog has no Cursor GPT names to disable")
	}
	if other == 0 {
		t.Fatal("catalog has no Cursor non-GPT names to keep enabled")
	}
	if cfg.ModelAllowed("gpt-5.6-sol-medium") {
		t.Fatal("gpt-5.6-sol-medium must be disabled")
	}
	if !cfg.ModelAllowed("composer-2.5") || !cfg.ModelAllowed("cursor-grok-4.6-high") {
		t.Fatal("Cursor non-GPT names must stay enabled")
	}
}

func TestPickerModelGroupsOmitsDisabledNames(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("claude-opus-5-high", true); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, g := range cfg.PickerModelGroups("") {
		for _, c := range g.Choices {
			seen[c.Value] = true
		}
	}
	if seen["claude-opus-5-high"] {
		t.Fatal("picker still lists a disabled name")
	}
	if !seen["claude-haiku-4-5-high"] || !seen["grok-4.6-high"] || !seen["composer-2.5"] {
		t.Fatalf("picker dropped an enabled name: %v", seen)
	}
	// Admin default dropdowns keep the full catalog so a stored default stays visible.
	admin := map[string]bool{}
	for _, g := range ModelGroups("") {
		for _, c := range g.Choices {
			admin[c.Value] = true
		}
	}
	if !admin["claude-opus-5-high"] {
		t.Fatal("admin ModelGroups must still list the disabled name")
	}
}

func TestRequestedAgentCLIRejectsDisabledName(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "model": "grok-4.5-high"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("claude-opus-5-high", true); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.RequestedAgentCLI("claude-opus-5-high"); err == nil {
		t.Fatal("disabled curated name must be refused")
	}
	got, err := cfg.RequestedAgentCLI("grok-4.5-high")
	if err != nil || got.Model != "grok-4.5-high" {
		t.Fatalf("enabled name: cli=%+v err=%v", got, err)
	}
}

func TestUnstampedResolutionSkipsDisabledDefault(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "agent": "grok", "model": "claude-opus-5-high"`)
	if err != nil {
		t.Fatal(err)
	}
	before := cfg.ResolveAgentCLI("")
	if before.Model != "claude-opus-5-high" || before.Agent != grokrun.AgentClaude {
		t.Fatalf("before disable cli=%+v", before)
	}
	if err := cfg.SetModelDisabled("claude-opus-5-high", true); err != nil {
		t.Fatal(err)
	}
	got := cfg.ResolveAgentCLI("")
	if got.Model == "claude-opus-5-high" {
		t.Fatalf("unstamped resolution returned the disabled default: %+v", got)
	}
	empty, err := cfg.RequestedAgentCLI("")
	if err != nil {
		t.Fatal(err)
	}
	if empty.Model == "claude-opus-5-high" {
		t.Fatalf("empty request returned the disabled default: %+v", empty)
	}
}

func TestPinnedAgentCLIIgnoresDisable(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "model": "grok-4.5-high"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("claude-opus-5-high", true); err != nil {
		t.Fatal(err)
	}
	got := cfg.PinnedAgentCLI("claude", "claude-opus-5-high")
	if got.Agent != grokrun.AgentClaude || got.Model != "claude-opus-5-high" {
		t.Fatalf("pinned cli=%+v, stamped model must survive a later disable", got)
	}
}

func TestModelAvailabilityGroupsClaudeAndCursorGPT(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetFamilyModelsDisabled("cursor", "gpt", true); err != nil {
		t.Fatal(err)
	}
	var sawClaude, sawCursorGPT, cursorGPTDisabled, composerEnabled bool
	for _, g := range cfg.ModelAvailability() {
		switch g.Agent {
		case "claude":
			sawClaude = true
			if g.ShowFamilies {
				t.Fatal("Claude is one family; the page must not extra-group it")
			}
			if len(g.Families) == 0 || len(g.Families[0].Choices) == 0 {
				t.Fatal("Claude block is empty")
			}
		case "cursor":
			if !g.ShowFamilies {
				t.Fatal("Cursor has several families; the page must group them")
			}
			for _, f := range g.Families {
				if f.Key == "gpt" {
					sawCursorGPT = true
					for _, c := range f.Choices {
						if c.Enabled {
							t.Errorf("cursor gpt %q still enabled in availability", c.Value)
						}
						if c.Value == "gpt-5.6-sol-medium" {
							cursorGPTDisabled = !c.Enabled
						}
					}
				}
				if f.Key == "composer" {
					for _, c := range f.Choices {
						if c.Value == "composer-2.5" && c.Enabled {
							composerEnabled = true
						}
					}
				}
			}
		}
	}
	if !sawClaude || !sawCursorGPT {
		t.Fatalf("availability missing claude=%v cursor-gpt=%v", sawClaude, sawCursorGPT)
	}
	if !cursorGPTDisabled || !composerEnabled {
		t.Fatalf("gpt disabled=%v composer enabled=%v", cursorGPTDisabled, composerEnabled)
	}
}

func TestLoadDropsUnknownDisabledNames(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	raw := `{
		"discordToken": "tok",
		"projects": { "p": "` + proj + `" },
		"channels": { "c1": "p" },
		"disabledModels": ["claude-opus-5-high", "not-a-real-model", "claude-opus-5-high"]
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", path)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("GROK_WORK_HTTP_LISTEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.DisabledModelNames()
	if len(got) != 1 || got[0] != "claude-opus-5-high" {
		t.Fatalf("normalized denylist=%v", got)
	}
}

func TestDisableRejectsUnknownNameAndEmptyAgent(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("gpt-9", true); err == nil {
		t.Fatal("unknown name must be refused")
	}
	if err := cfg.SetAgentModelsDisabled("", true); err == nil {
		t.Fatal("empty agent must be refused (ParseAgent treats empty as grok)")
	}
	if err := cfg.SetFamilyModelsDisabled("cursor", "", true); err == nil {
		t.Fatal("empty family must be refused")
	}
}

func TestEffectiveReviewModelSkipsDisabled(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "model": "grok-4.5-high", "reviewModel": "claude-opus-5-high"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("claude-opus-5-high", true); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveReviewModel(); got != "grok-4.5-high" {
		t.Fatalf("effective review=%q want the task model", got)
	}
	if err := cfg.SetModelDisabled("grok-4.5-high", true); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveReviewModel(); got != "" {
		t.Fatalf("both disabled: effective review=%q want empty", got)
	}
}

func TestEffectiveAskModelSkipsDisabled(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "model": "grok-4.5-high", "reviewModel": "claude-opus-5-high", "askModel": "grok-4.6-high"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("grok-4.6-high", true); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveAskModel(); got != "claude-opus-5-high" {
		t.Fatalf("effective ask=%q want the review model", got)
	}
	cli, err := cfg.AskAgentCLI("")
	if err != nil {
		t.Fatal(err)
	}
	if cli.Model != "claude-opus-5-high" {
		t.Fatalf("ask cli model=%q want the review model", cli.Model)
	}
	if err := cfg.SetModelDisabled("claude-opus-5-high", true); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveAskModel(); got != "grok-4.5-high" {
		t.Fatalf("effective ask=%q want the task model", got)
	}
	if err := cfg.SetModelDisabled("grok-4.5-high", true); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveAskModel(); got != "" {
		t.Fatalf("all disabled: effective ask=%q want empty", got)
	}
}

func TestSnapshotCarriesModelAvailability(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("grok-4.5-high", true); err != nil {
		t.Fatal(err)
	}
	snap := cfg.Snapshot()
	if snap.DisabledModelCount != 1 {
		t.Fatalf("count=%d", snap.DisabledModelCount)
	}
	if len(snap.ModelAvailability) == 0 {
		t.Fatal("snapshot missing availability groups")
	}
	found := false
	for _, g := range snap.ModelAvailability {
		for _, f := range g.Families {
			for _, c := range f.Choices {
				if c.Value == "grok-4.5-high" {
					found = true
					if c.Enabled {
						t.Fatal("snapshot still marks grok-4.5 enabled")
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("snapshot availability missing grok-4.5")
	}
}

func TestRequestedAgentCLIDisabledErrorMatchesUncurated(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetModelDisabled("composer-2.5", true); err != nil {
		t.Fatal(err)
	}
	_, uncurated := cfg.RequestedAgentCLI("gpt-9")
	_, disabled := cfg.RequestedAgentCLI("composer-2.5")
	if uncurated == nil || disabled == nil {
		t.Fatal("both must error")
	}
	if !strings.Contains(uncurated.Error(), "not a known model") ||
		!strings.Contains(disabled.Error(), "not a known model") {
		t.Fatalf("errors must match: uncurated=%v disabled=%v", uncurated, disabled)
	}
}
