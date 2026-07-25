package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

// monorepoManifest is the worked example from docs/design-deploy-pipeline.md.
// Keeping the doc and the test on the same bytes means a schema change that
// breaks the documented example cannot pass.
const monorepoManifest = `
version: 1
environments: [dev, stag, prod]

x-anchors:
  build:   &s_build   { name: build,   run: "docker build -t $IMAGE_REPO/api:$GW_SHA -f services/api/Dockerfile .", timeout: 20m }
  push:    &s_push    { name: push,    run: "docker push $IMAGE_REPO/api:$GW_SHA" }
  migrate: &s_migrate { name: migrate, run: "./scripts/migrate.sh --url \"$DB_MIGRATE_URL\"", timeout: 10m }
  smoke:   &s_smoke   { name: smoke,   run: "./scripts/smoke.sh https://$API_HOST/healthz" }

services:
  api:
    dir: services/api
    steps:
      - *s_build
      - *s_push
      - name: apply
        run: kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA
    pipelines:
      stag:
        - *s_build
        - *s_push
        - *s_migrate
        - { name: rollout, run: "kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA" }
        - { name: wait,    run: "kubectl -n $K8S_NAMESPACE rollout status deploy/api --timeout=5m" }
        - *s_smoke
      prod:
        - { name: verify-image, run: "docker manifest inspect $IMAGE_REPO/api:$GW_SHA > /dev/null" }
        - { name: backup-db,    run: "./scripts/backup.sh --tag pre-$GW_SHORT_SHA", timeout: 30m }
        - *s_migrate
        - { name: canary,       run: "./scripts/canary.sh api $GW_SHA 10" }
        - { name: canary-wait,  run: "./scripts/canary_check.sh api --for 5m", timeout: 8m }
        - { name: rollout,      run: "kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA" }
        - { name: wait,         run: "kubectl -n $K8S_NAMESPACE rollout status deploy/api --timeout=10m" }
        - *s_smoke

  web:
    dir: apps/web
    envs: [dev, prod]
    steps:
      - { name: build,      run: "npm ci && npm run build", timeout: 15m }
      - { name: sync,       run: "gsutil -m rsync -d -r dist gs://$WEB_BUCKET" }
      - { name: invalidate, run: "gcloud compute url-maps invalidate-cdn-cache $CDN_URL_MAP --path '/*'", envs: [prod] }
`

func TestParseMonorepoManifest(t *testing.T) {
	m, err := Parse([]byte(monorepoManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := m.ServiceNames(), []string{"api", "web"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ServiceNames = %v, want %v", got, want)
	}

	// Each environment resolves to a different pipeline length: dev falls back to
	// the default steps, stag and prod each fully replace them.
	cases := []struct {
		service, env string
		wantSteps    []string
	}{
		{"api", "dev", []string{"build", "push", "apply"}},
		{"api", "stag", []string{"build", "push", "migrate", "rollout", "wait", "smoke"}},
		{"api", "prod", []string{"verify-image", "backup-db", "migrate", "canary", "canary-wait", "rollout", "wait", "smoke"}},
		// Step-level envs filter: dev has no CDN, prod does.
		{"web", "dev", []string{"build", "sync"}},
		{"web", "prod", []string{"build", "sync", "invalidate"}},
	}
	for _, tc := range cases {
		t.Run(tc.service+"/"+tc.env, func(t *testing.T) {
			steps, err := m.Resolve(tc.service, tc.env)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			var names []string
			for _, s := range steps {
				names = append(names, s.Name)
			}
			if strings.Join(names, ",") != strings.Join(tc.wantSteps, ",") {
				t.Fatalf("steps = %v, want %v", names, tc.wantSteps)
			}
			if !m.Deployable(tc.service, tc.env) {
				t.Fatal("Deployable = false, want true")
			}
		})
	}

	// A service narrowed with envs is not deployable elsewhere.
	if _, err := m.Resolve("web", "stag"); err == nil {
		t.Fatal("Resolve(web, stag) = nil error, want refusal")
	}
	if m.Deployable("web", "stag") {
		t.Fatal("Deployable(web, stag) = true, want false")
	}

	// Timeouts: declared values survive, absent ones take the default.
	prod, err := m.Resolve("api", "prod")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	byName := map[string]ResolvedStep{}
	for _, s := range prod {
		byName[s.Name] = s
	}
	if got, want := byName["backup-db"].TimeoutMs, 30*60*1000; got != want {
		t.Fatalf("backup-db timeout = %d, want %d", got, want)
	}
	if got, want := byName["rollout"].TimeoutMs, int(DefaultStepTimeout.Milliseconds()); got != want {
		t.Fatalf("rollout timeout = %d, want default %d", got, want)
	}
	if got, want := m.Dir("api"), "services/api"; got != want {
		t.Fatalf("Dir(api) = %q, want %q", got, want)
	}
}

// TestParseMergeKeyInheritance pins that YAML merge keys survive strict
// decoding, since that is how two similar services share a definition.
func TestParseMergeKeyInheritance(t *testing.T) {
	src := `
version: 1
environments: [dev]
services:
  api: &base
    dir: services/api
    steps:
      - { name: ship, run: "./ship.sh" }
  worker:
    <<: *base
    dir: services/worker
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := m.Dir("worker"), "services/worker"; got != want {
		t.Fatalf("worker dir = %q, want %q (local key must win over the merge)", got, want)
	}
	steps, err := m.Resolve("worker", "dev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(steps) != 1 || steps[0].Name != "ship" {
		t.Fatalf("worker steps = %+v, want the inherited ship step", steps)
	}
}

func TestParseRejects(t *testing.T) {
	// Every case is either malformed or hostile. wantErr is a substring of the
	// message an operator will read on the deploys page.
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{{
		name:    "unknown top-level key",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {dir: ., steps: [{name: s, run: 'true'}]}\nextra: 1\n",
		wantErr: "field extra not found",
	}, {
		// The reason strict decoding is on: this would otherwise drop every step.
		name:    "misspelled steps key",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {dir: ., stpes: []}\n",
		wantErr: "field stpes not found",
	}, {
		name:    "wrong version",
		src:     "version: 2\nenvironments: [dev]\nservices:\n  a: {steps: [{name: s, run: 'true'}]}\n",
		wantErr: "version must be 1",
	}, {
		name:    "no environments",
		src:     "version: 1\nservices:\n  a: {steps: [{name: s, run: 'true'}]}\n",
		wantErr: "environments is required",
	}, {
		name:    "no services",
		src:     "version: 1\nenvironments: [dev]\nservices: {}\n",
		wantErr: "services is required",
	}, {
		name:    "duplicate environment",
		src:     "version: 1\nenvironments: [dev, dev]\nservices:\n  a: {steps: [{name: s, run: 'true'}]}\n",
		wantErr: `duplicate name "dev"`,
	}, {
		name:    "invalid environment name",
		src:     "version: 1\nenvironments: [Prod]\nservices:\n  a: {steps: [{name: s, run: 'true'}]}\n",
		wantErr: "invalid name",
	}, {
		name:    "absolute dir escapes the checkout",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {dir: /etc, steps: [{name: s, run: 'true'}]}\n",
		wantErr: "must be relative",
	}, {
		name:    "parent dir escapes the checkout",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {dir: ../../etc, steps: [{name: s, run: 'true'}]}\n",
		wantErr: "must stay inside the repo",
	}, {
		name:    "service with neither steps nor pipelines",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {dir: .}\n",
		wantErr: "needs steps or at least one pipelines entry",
	}, {
		name:    "pipeline for an unknown environment",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {steps: [{name: s, run: 'true'}], pipelines: {prod: [{name: s, run: 'true'}]}}\n",
		wantErr: `unknown environment "prod"`,
	}, {
		name:    "pipeline contradicts the service envs narrowing",
		src:     "version: 1\nenvironments: [dev, prod]\nservices:\n  a: {envs: [dev], steps: [{name: s, run: 'true'}], pipelines: {prod: [{name: s, run: 'true'}]}}\n",
		wantErr: "excluded by",
	}, {
		name:    "empty pipeline",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {steps: [{name: s, run: 'true'}], pipelines: {dev: []}}\n",
		wantErr: "needs at least one step",
	}, {
		name:    "duplicate step name",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {steps: [{name: s, run: 'true'}, {name: s, run: 'true'}]}\n",
		wantErr: `duplicate step name "s"`,
	}, {
		name:    "empty command",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {steps: [{name: s, run: '  '}]}\n",
		wantErr: "command is required",
	}, {
		name:    "step timeout over the ceiling",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {steps: [{name: s, run: 'true', timeout: 720h}]}\n",
		wantErr: "over the ceiling",
	}, {
		name:    "unparseable step timeout",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {steps: [{name: s, run: 'true', timeout: soon}]}\n",
		wantErr: "invalid duration",
	}, {
		name:    "step restricted to an unknown environment",
		src:     "version: 1\nenvironments: [dev]\nservices:\n  a: {steps: [{name: s, run: 'true', envs: [prod]}]}\n",
		wantErr: `unknown environment "prod"`,
	}, {
		name:    "empty manifest",
		src:     "\n\n",
		wantErr: "manifest is empty",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatal("Parse succeeded, want rejection")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseRejectsTooManySteps(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: 1\nenvironments: [dev]\nservices:\n  a:\n    steps:\n")
	for i := range MaxStepsPerPipeline + 1 {
		fmt.Fprintf(&b, "      - { name: s%d, run: 'true' }\n", i)
	}
	_, err := Parse([]byte(b.String()))
	if err == nil || !strings.Contains(err.Error(), "over the limit") {
		t.Fatalf("error = %v, want a step-count limit refusal", err)
	}
}

func TestParseRejectsOversizeCommand(t *testing.T) {
	src := fmt.Sprintf("version: 1\nenvironments: [dev]\nservices:\n  a:\n    steps:\n      - { name: s, run: '%s' }\n",
		strings.Repeat("x", MaxCommandBytes+1))
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "over the limit") {
		t.Fatalf("error = %v, want a command-length refusal", err)
	}
}

// TestParseRejectsAliasBombWithoutExpanding pins that an alias fan-out cannot
// turn a tiny manifest into unbounded work. YAML has no sequence splat, so
// [*a, *a] is a nested sequence, which fails against the typed step list at the
// first level instead of expanding 9^15 entries.
func TestParseRejectsAliasBombWithoutExpanding(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: 1\nenvironments: [dev]\nx-anchors:\n  a: &a0 [{name: n, run: r}]\n")
	for i := 1; i <= 15; i++ {
		fmt.Fprintf(&b, "  l%d: &a%d [*a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d]\n",
			i, i, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1)
	}
	b.WriteString("services:\n  a:\n    steps: *a15\n")
	src := b.String()
	if len(src) > 4096 {
		t.Fatalf("bomb source grew to %d bytes; the point is that it is tiny", len(src))
	}
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("Parse accepted an alias bomb")
	}
}

func TestValidateDir(t *testing.T) {
	ok := []string{"", ".", "services/api", "a/b/c", "apps/web/"}
	for _, d := range ok {
		if err := validateDir(d); err != nil {
			t.Errorf("validateDir(%q) = %v, want nil", d, err)
		}
	}
	bad := []string{"/abs", "../up", "a/../../up", `a\b`}
	for _, d := range bad {
		if err := validateDir(d); err == nil {
			t.Errorf("validateDir(%q) = nil, want refusal", d)
		}
	}
}

// fakeGit records every git invocation and replays canned stdout.
type fakeGit struct {
	calls []string
	out   map[string]string
	err   map[string]error
}

func (f *fakeGit) run(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if err, ok := f.err[key]; ok {
		return nil, err
	}
	return []byte(f.out[key]), nil
}

const (
	testRev  = "abc1234def5678"
	testBlob = "0f1e2d3c4b5a"
)

func lsTreeKey() string {
	return "git ls-tree -l --full-tree " + testRev + " -- " + DefaultManifestPath
}

func TestLoadAtReadsManifestAtRevision(t *testing.T) {
	body := "version: 1\nenvironments: [dev]\nservices:\n  a: {steps: [{name: s, run: 'true'}]}\n"
	f := &fakeGit{out: map[string]string{
		lsTreeKey():                     fmt.Sprintf("100644 blob %s  %d\t%s", testBlob, len(body), DefaultManifestPath),
		"git cat-file blob " + testBlob: body,
	}}
	m, err := LoadAt(context.Background(), f.run, "/repo", testRev, "")
	if err != nil {
		t.Fatalf("LoadAt: %v", err)
	}
	if !m.Deployable("a", "dev") {
		t.Fatal("parsed manifest is not deployable")
	}
	if len(f.calls) != 2 || !strings.HasPrefix(f.calls[0], "git ls-tree") {
		t.Fatalf("calls = %v, want the size probe first", f.calls)
	}
}

// TestLoadAtRefusesOversizeBlobWithoutReading is the whole point of probing with
// ls-tree: a Runner buffers all of stdout, so checking the cap after reading
// would not be a cap at all.
func TestLoadAtRefusesOversizeBlobWithoutReading(t *testing.T) {
	f := &fakeGit{out: map[string]string{
		lsTreeKey(): fmt.Sprintf("100644 blob %s  %d\t%s", testBlob, MaxManifestBytes+1, DefaultManifestPath),
	}}
	_, err := LoadAt(context.Background(), f.run, "/repo", testRev, "")
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("error = %v, want a size refusal", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "cat-file") {
			t.Fatalf("read the blob anyway: calls = %v", f.calls)
		}
	}
}

func TestLoadAtNoManifestIsNotAnError(t *testing.T) {
	// ls-tree exits 0 with empty output for a path that does not exist, which is
	// why absence is a value here rather than an error string to match on.
	f := &fakeGit{out: map[string]string{lsTreeKey(): ""}}
	_, err := LoadAt(context.Background(), f.run, "/repo", testRev, "")
	if !errors.Is(err, ErrNoManifest) {
		t.Fatalf("error = %v, want ErrNoManifest", err)
	}
}

func TestLoadAtDirectoryAtManifestPathIsNotConfigured(t *testing.T) {
	f := &fakeGit{out: map[string]string{
		lsTreeKey(): fmt.Sprintf("040000 tree %s  -\t%s", testBlob, DefaultManifestPath),
	}}
	_, err := LoadAt(context.Background(), f.run, "/repo", testRev, "")
	if !errors.Is(err, ErrNoManifest) {
		t.Fatalf("error = %v, want ErrNoManifest", err)
	}
}

func TestLoadAtRejectsEscapingManifestPath(t *testing.T) {
	f := &fakeGit{}
	_, err := LoadAt(context.Background(), f.run, "/repo", testRev, "../../etc/passwd")
	if err == nil || !strings.Contains(err.Error(), "must stay inside the repo") {
		t.Fatalf("error = %v, want a containment refusal", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("ran git for a rejected path: %v", f.calls)
	}
}

// TestEnvNameRuleMatchesConfig pins the deliberately duplicated environment-name
// rule. internal/config cannot import this package (this package needs config
// for per-environment credentials, so the dependency only runs one way), and a
// silent drift would let an admin create an environment no manifest can name.
func TestEnvNameRuleMatchesConfig(t *testing.T) {
	if EnvNameRule != config.DeployEnvNameRule {
		t.Fatalf("EnvNameRule = %q, config.DeployEnvNameRule = %q — the duplicated rule drifted",
			EnvNameRule, config.DeployEnvNameRule)
	}
}
