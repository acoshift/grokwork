package deploy

import (
	"os"
	"slices"
	"strings"
)

// baseEnvNames are the host variables a step inherits, by name.
//
// This is an allowlist, not a denylist, and that is the whole point. The
// operator chose per-environment credentials over inheriting the host
// environment, so a step must not see the box's cloud credentials just because
// nobody thought to name them. grokrun.FilterChildEnv takes the opposite
// approach for agent children — a denylist — which still passes anything it has
// not heard of. Do not "unify" the two.
//
// Everything here is needed for a command to run at all: without PATH nothing
// resolves, and without HOME docker, kubectl and gcloud cannot find their own
// configuration.
var baseEnvNames = []string{
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"SHELL",
	"LANG",
	"LC_ALL",
	"TZ",
	"TMPDIR",
	// Deploys that push over ssh (git, rsync) need the agent socket.
	"SSH_AUTH_SOCK",
}

// RunVars are the identifying values injected into every step.
type RunVars struct {
	Project string
	Service string
	Env     string
	Ref     string
	SHA     string
	RunID   string
	Step    string
	// Actor is a display name. Never a Discord snowflake: step output can reach
	// a log an operator reads, and an internal id there is noise at best.
	Actor string
}

// injected returns the GW_* pairs for a step.
func (v RunVars) injected() map[string]string {
	short := v.SHA
	if len(short) > 7 {
		short = short[:7]
	}
	return map[string]string{
		"GW_PROJECT":   v.Project,
		"GW_SERVICE":   v.Service,
		"GW_ENV":       v.Env,
		"GW_REF":       v.Ref,
		"GW_SHA":       v.SHA,
		"GW_SHORT_SHA": short,
		"GW_RUN_ID":    v.RunID,
		"GW_STEP":      v.Step,
		"GW_ACTOR":     v.Actor,
	}
}

// BuildEnv assembles a step's environment from three layers, in increasing
// precedence: the host base allowlist, the injected run vars, then the
// project's per-environment map.
//
// The per-environment map wins last on purpose: an environment may need to
// point HOME or KUBECONFIG somewhere specific, and the operator who configured
// it is more authoritative than this default list. It cannot forge the injected
// identity, because config rejects GW_*/GROK_WORK_* keys on the way in.
func BuildEnv(vars RunVars, envMap map[string]string) []string {
	merged := make(map[string]string, len(baseEnvNames)+len(envMap)+9)
	for _, name := range baseEnvNames {
		if v, ok := os.LookupEnv(name); ok {
			merged[name] = v
		}
	}
	// Not inherited from the host: a deploy step is not interactive, and a
	// leftover TERM makes tools emit escape codes into the log.
	merged["TERM"] = "dumb"
	for k, v := range vars.injected() {
		merged[k] = v
	}
	for k, v := range envMap {
		merged[k] = v
	}

	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	// Sorted so a run's environment is reproducible and diffable.
	slices.Sort(out)
	return out
}

// EnvNames returns just the names in a built environment, for logging what a
// step was given without logging what it was given.
func EnvNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
