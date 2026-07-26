package deploy

import (
	"strings"
	"testing"
)

func promptOpts() ManifestPromptOpts {
	return ManifestPromptOpts{
		Project:      "shop",
		Actor:        "Alice",
		ManifestPath: DefaultManifestPath,
		Environments: []string{"dev", "prod"},
		EnvKeys: map[string][]string{
			"dev":  {"K8S_NAMESPACE", "IMAGE_REPO"},
			"prod": {"IMAGE_REPO", "DB_URL", "K8S_NAMESPACE"},
		},
	}
}

// TestManifestPromptStatesEveryParserRule is the load-bearing one. The agent
// cannot discover strict parsing by reading the repo, so a rule the parser
// enforces but the prompt omits produces manifests that fail to load with no
// hint why.
func TestManifestPromptStatesEveryParserRule(t *testing.T) {
	got := BuildManifestPrompt(promptOpts())
	for _, want := range []string{
		// Strict decoding, and the reserved key it forces to exist.
		"Unknown keys are a hard error",
		"x-anchors",
		// The composition mechanisms, which are the whole point of the schema.
		"pipelines",
		"fully replaces",
		"envs",
		// Structural rules.
		"`version` must be `1`",
		"never escaping it",
		"unique within its list",
		// Every limit the parser enforces.
		"16 environments", "20 services", "30 steps", "4000 bytes", "64 KiB",
		// The resolution order a reader needs to predict behaviour.
		"if present, else `steps`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt omits parser rule %q", want)
		}
	}
}

// TestManifestPromptExampleParses pins that the worked example handed to the
// agent actually loads. Shipping an example that fails our own parser would
// teach exactly the wrong shape.
func TestManifestPromptExampleParses(t *testing.T) {
	got := BuildManifestPrompt(promptOpts())
	_, after, ok := strings.Cut(got, "Worked example")
	if !ok {
		t.Fatal("no worked example in the prompt")
	}
	_, body, ok := strings.Cut(after, "```yaml\n")
	if !ok {
		t.Fatal("example is not a yaml block")
	}
	yaml, _, ok := strings.Cut(body, "```")
	if !ok {
		t.Fatal("unterminated yaml block")
	}
	m, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("the example handed to the agent does not parse: %v\n%s", err, yaml)
	}
	// And it demonstrates the thing it claims to: prod differs from the default.
	dev, err := m.Resolve("api", "dev")
	if err != nil {
		t.Fatal(err)
	}
	prod, err := m.Resolve("api", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(dev) == len(prod) {
		t.Fatal("the example does not actually show a different prod pipeline")
	}
	// And the step-level envs filter.
	webDev, _ := m.Resolve("web", "dev")
	webProd, _ := m.Resolve("web", "prod")
	if len(webProd) != len(webDev)+1 {
		t.Fatalf("the example does not show the envs filter: dev=%d prod=%d", len(webDev), len(webProd))
	}
}

// TestManifestPromptCarriesEnvNamesNotValues: variable names help the agent
// write correct steps; values are credentials and must never enter a prompt.
func TestManifestPromptCarriesEnvNamesNotValues(t *testing.T) {
	got := BuildManifestPrompt(promptOpts())
	for _, name := range []string{"K8S_NAMESPACE", "IMAGE_REPO", "DB_URL"} {
		if !strings.Contains(got, name) {
			t.Errorf("prompt omits available variable %q", name)
		}
	}
	if !strings.Contains(got, "cannot read them, and must not try") {
		t.Error("prompt does not tell the agent values are off limits")
	}
	// The opts type has no field for a value, which is the real guarantee; this
	// pins that no caller can smuggle one through EnvKeys either.
	opts := promptOpts()
	opts.EnvKeys["prod"] = append(opts.EnvKeys["prod"], "TOKEN")
	if strings.Contains(BuildManifestPrompt(opts), "postgres://") {
		t.Error("a value reached the prompt")
	}
}

func TestManifestPromptListsConfiguredEnvironments(t *testing.T) {
	got := BuildManifestPrompt(promptOpts())
	if !strings.Contains(got, "environment the host does not know is ignored") {
		t.Error("prompt does not explain why the environment list matters")
	}
	// With none configured, it must say so rather than silently omitting the
	// section — otherwise the agent invents names nothing can deploy.
	opts := promptOpts()
	opts.Environments = nil
	opts.EnvKeys = nil
	none := BuildManifestPrompt(opts)
	if !strings.Contains(none, "None configured yet") {
		t.Error("prompt does not flag that no environments are configured")
	}
	if !strings.Contains(none, "Deploy settings") {
		t.Error("prompt does not tell the agent to say what an operator must do next")
	}
}

func TestManifestPromptEditsExistingRatherThanReplacing(t *testing.T) {
	opts := promptOpts()
	opts.Existing = "version: 1\nenvironments: [dev]\nservices:\n  api: {steps: [{name: s, run: 'true'}]}\n"
	got := BuildManifestPrompt(opts)
	if !strings.Contains(got, "**edit it**") {
		t.Error("prompt does not say to edit the existing manifest")
	}
	if !strings.Contains(got, "environments: [dev]") {
		t.Error("prompt does not include the current manifest")
	}
	fresh := BuildManifestPrompt(promptOpts())
	if !strings.Contains(fresh, "no manifest yet") {
		t.Error("prompt does not say the manifest is new")
	}
}

func TestManifestPromptIncludesRequirements(t *testing.T) {
	opts := promptOpts()
	opts.Requirements = "prod must promote the stag image, never rebuild"
	got := BuildManifestPrompt(opts)
	if !strings.Contains(got, "prod must promote the stag image") {
		t.Error("operator requirements missing")
	}
	// They must outrank the generic guidance, or the agent averages them away.
	if !strings.Contains(got, "Follow them over any general guidance above") {
		t.Error("requirements are not given precedence")
	}
	if strings.Contains(BuildManifestPrompt(promptOpts()), "Additional requirements") {
		t.Error("empty requirements still rendered a section")
	}
}

func TestManifestPromptClipsHugeRequirements(t *testing.T) {
	opts := promptOpts()
	opts.Requirements = strings.Repeat("x", maxRequirementRunes*3)
	got := BuildManifestPrompt(opts)
	if len(got) > 40_000 {
		t.Fatalf("prompt grew to %d bytes; a pasted file would dominate it", len(got))
	}
	if !strings.Contains(got, "…") {
		t.Error("clipped requirements are not marked as clipped")
	}
}

// TestManifestPromptOmitsShipMechanics: the run path prepends the remote-work
// contract (branch, commit, push, PR). Repeating it here lets the two drift.
func TestManifestPromptOmitsShipMechanics(t *testing.T) {
	got := BuildManifestPrompt(promptOpts())
	for _, banned := range []string{"gh pr create", "git push", "open a pull request"} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(banned)) {
			t.Errorf("prompt duplicates ship mechanics (%q); the run prefix owns those", banned)
		}
	}
}

func TestManifestPromptTellsAgentToReadTheRepo(t *testing.T) {
	got := BuildManifestPrompt(promptOpts())
	for _, want := range []string{"Dockerfile", ".github/workflows/", "Do not guess"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt omits repo-evidence guidance %q", want)
		}
	}
	if !strings.Contains(got, "GW_SHA") {
		t.Error("prompt omits the injected run variables")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := truncateRunes("hello", 3); got != "hel…" {
		t.Fatalf("got %q", got)
	}
	// Runes, not bytes: clipping mid-character would corrupt the prompt.
	if got := truncateRunes("héllo→", 2); got != "hé…" {
		t.Fatalf("got %q", got)
	}
	if got := truncateRunes("x", 0); got != "" {
		t.Fatalf("got %q", got)
	}
}
