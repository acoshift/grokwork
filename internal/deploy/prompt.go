package deploy

import (
	"fmt"
	"slices"
	"strings"
)

// maxRequirementRunes bounds the free-text box on the deploys page. Generous for
// real instructions, small enough that it cannot dominate the prompt.
const maxRequirementRunes = 4000

// ManifestPromptOpts is everything the manifest-authoring prompt needs.
type ManifestPromptOpts struct {
	Project string
	// Actor is a display name for the "started from web by" line.
	Actor string
	// ManifestPath is where the file must be written.
	ManifestPath string
	// Environments are the names configured in config.json. The manifest must
	// declare these, because an environment the host does not know is ignored.
	Environments []string
	// EnvKeys maps environment name → variable NAMES available to its steps.
	// Names only: values are credentials and never enter a prompt.
	EnvKeys map[string][]string
	// Existing is the current manifest, when there is one, so the agent edits
	// rather than replaces.
	Existing string
	// Requirements is the operator's free text.
	Requirements string
}

// BuildManifestPrompt is the task body for the "generate the pipeline" session.
//
// The schema block is not optional detail: parsing is strict, so a manifest that
// invents a key or misplaces a list fails to load entirely, and the agent cannot
// discover that by reading the repo. Everything the parser enforces is stated
// here explicitly.
//
// Ship mechanics (branch, commit, push, PR) are deliberately absent — the run
// path prepends the remote-work contract, and repeating it here would let the
// two drift.
func BuildManifestPrompt(opts ManifestPromptOpts) string {
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "web user"
	}
	path := strings.TrimSpace(opts.ManifestPath)
	if path == "" {
		path = DefaultManifestPath
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (started from web by %s)\n", actor)
	fmt.Fprintf(&b, "Write the deploy pipeline for project **%s** at `%s`.\n\n", opts.Project, path)

	if strings.TrimSpace(opts.Existing) != "" {
		b.WriteString("A manifest already exists — **edit it**, preserving what already works.\n")
		b.WriteString("Current contents:\n\n```yaml\n")
		b.WriteString(strings.TrimRight(opts.Existing, "\n"))
		b.WriteString("\n```\n\n")
	} else {
		b.WriteString("There is no manifest yet. Create one.\n\n")
	}

	b.WriteString("### First, work out what this repo actually deploys\n")
	b.WriteString("Do not guess. Inspect the repo and base every service on what you find:\n")
	b.WriteString("- Dockerfiles, Procfiles, k8s/helm manifests, terraform, serverless configs\n")
	b.WriteString("- CI workflows already doing deploys (`.github/workflows/`) — this feature replaces those, so they are the best evidence of the real steps\n")
	b.WriteString("- package.json / go.mod / Makefile targets that build or publish\n")
	b.WriteString("- the directory layout, for a monorepo's service boundaries\n\n")
	b.WriteString("If the repo has an existing deploy workflow, port it faithfully rather than inventing a new shape.\n\n")

	writeManifestSchema(&b)

	b.WriteString("### Environments configured on this host\n")
	if len(opts.Environments) == 0 {
		b.WriteString("**None configured yet.** Declare the environments the repo needs (commonly `dev`, `stag`, `prod`).\n")
		b.WriteString("An operator must then add each one in the project's Deploy settings before it can run;\n")
		b.WriteString("say so in your final reply.\n\n")
	} else {
		fmt.Fprintf(&b, "Declare exactly these in `environments` — an environment the host does not know is ignored:\n")
		for _, env := range opts.Environments {
			fmt.Fprintf(&b, "- `%s`", env)
			if keys := opts.EnvKeys[env]; len(keys) > 0 {
				sorted := append([]string(nil), keys...)
				slices.Sort(sorted)
				fmt.Fprintf(&b, " — variables available to its steps: `$%s`", strings.Join(sorted, "`, `$"))
			} else {
				b.WriteString(" — no variables configured yet")
			}
			b.WriteByte('\n')
		}
		b.WriteString("\nUse those variable names rather than hardcoding registries, namespaces or hosts.\n")
		b.WriteString("Their values are credentials and configuration held on the host; you cannot read them, and must not try.\n")
		b.WriteString("If a step needs a variable that is not listed, use a clear name and note it in your final reply so an operator can add it.\n\n")
	}

	b.WriteString("### Variables the runner injects into every step\n")
	b.WriteString("`$GW_PROJECT` `$GW_SERVICE` `$GW_ENV` `$GW_REF` `$GW_SHA` `$GW_SHORT_SHA` `$GW_RUN_ID` `$GW_STEP` `$GW_ACTOR`\n")
	b.WriteString("Tag images with `$GW_SHA` so a deploy is traceable to its commit. Do not define these yourself — names starting with `GW_` are rejected.\n\n")

	if req := truncateRunes(strings.TrimSpace(opts.Requirements), maxRequirementRunes); req != "" {
		b.WriteString("### Additional requirements from the operator\n")
		b.WriteString("These are specific instructions for this repo. Follow them over any general guidance above.\n\n")
		b.WriteString(req)
		b.WriteString("\n\n")
	}

	b.WriteString("### Rules\n")
	b.WriteString("- Steps run as `sh -c` on the deploy host, in the service's `dir`, against real infrastructure. Write them to be safe to re-run.\n")
	b.WriteString("- Never put a secret in the manifest. Reference variables by name.\n")
	b.WriteString("- Prefer promoting an already-built artifact to production over rebuilding it there — if a build is not reproducible, prod must not be the first place it runs.\n")
	b.WriteString("- Give slow steps a realistic `timeout`; the default is 15m and the ceiling is 1h.\n")
	b.WriteString("- Change only the manifest and, if genuinely needed, deploy scripts it calls. This task is not a refactor.\n")
	b.WriteString("- Re-read the file once written and check it against every rule in the schema above, especially the strict-key rule.\n\n")

	b.WriteString("### In your final reply\n")
	b.WriteString("- List the services and environments you declared, and what evidence in the repo each came from.\n")
	b.WriteString("- Call out anything you had to guess, and any variable an operator still needs to configure.\n")
	b.WriteString("- Say plainly if the repo has no deployable service, rather than inventing one.\n")
	return b.String()
}

// writeManifestSchema states everything the parser enforces.
//
// Kept next to the parser on purpose: a rule added to manifest.go that is not
// stated here produces manifests that fail to load with no hint why.
func writeManifestSchema(b *strings.Builder) {
	b.WriteString("### Manifest schema (read carefully — parsing is strict)\n")
	fmt.Fprintf(b, "- **Unknown keys are a hard error.** Every key must be one of those below; a typo fails the whole file rather than being ignored.\n")
	fmt.Fprintf(b, "- `version` must be `1`.\n")
	fmt.Fprintf(b, "- `environments` is a list of names matching `%s`.\n", EnvNameRule)
	fmt.Fprintf(b, "- `services` maps a service name (`[a-z0-9][a-z0-9._-]*`) to its definition:\n")
	b.WriteString("  - `dir` — working directory for its steps, relative to the repo root, never escaping it. Omit for the root.\n")
	b.WriteString("  - `envs` — optional list narrowing which environments this service deploys to. Default: all.\n")
	b.WriteString("  - `steps` — the default pipeline.\n")
	b.WriteString("  - `pipelines` — optional map of environment name → a step list that **fully replaces** `steps` for that environment.\n")
	b.WriteString("- A step is `{ name, run, timeout?, envs? }`:\n")
	b.WriteString("  - `name` — unique within its list.\n")
	b.WriteString("  - `run` — the shell command.\n")
	b.WriteString("  - `timeout` — a duration string like `20m`. Must be quoted-free scalar, not a number.\n")
	b.WriteString("  - `envs` — optional list restricting the step to those environments (for \"same pipeline, one extra step in prod\").\n")
	b.WriteString("- `x-anchors` is the **only** place YAML anchor definitions may live. Strict parsing rejects any other top-level key, so anchors have nowhere else to go. Nothing inside it runs.\n")
	fmt.Fprintf(b, "- Limits: %d environments, %d services, %d steps per pipeline, %d bytes per command, %d KiB total file.\n",
		MaxEnvironments, MaxServices, MaxStepsPerPipeline, MaxCommandBytes, MaxManifestBytes/1024)
	b.WriteString("\nHow an environment resolves for a service: `pipelines.<env>` if present, else `steps`; then any step whose `envs` excludes that environment is dropped. A service with neither is not deployable there.\n")
	b.WriteString("\nWorked example — note prod runs a *different* pipeline, not the same one with a variable swapped:\n\n")
	b.WriteString("```yaml\n")
	b.WriteString(`version: 1
environments: [dev, stag, prod]

x-anchors:
  build: &s_build { name: build, run: "docker build -t $IMAGE_REPO/api:$GW_SHA -f services/api/Dockerfile .", timeout: 20m }
  push:  &s_push  { name: push,  run: "docker push $IMAGE_REPO/api:$GW_SHA" }
  smoke: &s_smoke { name: smoke, run: "./scripts/smoke.sh https://$API_HOST/healthz" }

services:
  api:
    dir: services/api
    steps:
      - *s_build
      - *s_push
      - { name: apply, run: "kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA" }
    pipelines:
      prod:
        - { name: verify-image, run: "docker manifest inspect $IMAGE_REPO/api:$GW_SHA" }
        - { name: rollout,      run: "kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA" }
        - { name: wait,         run: "kubectl -n $K8S_NAMESPACE rollout status deploy/api --timeout=10m", timeout: 12m }
        - *s_smoke

  web:
    dir: apps/web
    envs: [dev, prod]
    steps:
      - { name: build,      run: "npm ci && npm run build", timeout: 15m }
      - { name: sync,       run: "gsutil -m rsync -d -r dist gs://$WEB_BUCKET" }
      - { name: invalidate, run: "gcloud compute url-maps invalidate-cdn-cache $CDN_URL_MAP --path '/*'", envs: [prod] }
`)
	b.WriteString("```\n\n")
}

// truncateRunes clips text to n runes, adding an ellipsis when it clips.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
