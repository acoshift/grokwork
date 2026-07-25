package deploy

import (
	"bytes"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", kv)
		}
		out[k] = v
	}
	return out
}

// TestEnvAllowlistDropsHostCredentials is the whole reason BuildEnv is an
// allowlist: the operator chose per-environment credentials over inheriting the
// host, so a step must not see the box's cloud keys just because nothing named
// them.
func TestEnvAllowlistDropsHostCredentials(t *testing.T) {
	for _, name := range []string{
		"AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "KUBECONFIG",
		"GH_TOKEN", "ANTHROPIC_API_KEY", "DISCORD_BOT_TOKEN", "NPM_TOKEN",
		"GROK_WORK_CONFIG", "SOME_RANDOM_THING",
	} {
		t.Setenv(name, "leaked-"+name)
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/test")

	env := BuildEnv(RunVars{Project: "p", Service: "s", Env: "prod"}, nil)
	got := envMap(t, env)

	for _, name := range []string{
		"AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "KUBECONFIG",
		"GH_TOKEN", "ANTHROPIC_API_KEY", "DISCORD_BOT_TOKEN", "NPM_TOKEN",
		"GROK_WORK_CONFIG", "SOME_RANDOM_THING",
	} {
		if v, ok := got[name]; ok {
			t.Errorf("host credential %s leaked into the step env as %q", name, v)
		}
	}
	// Without these nothing resolves and the CLIs cannot find their configs.
	if got["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH = %q, want it inherited", got["PATH"])
	}
	if got["HOME"] != "/home/test" {
		t.Errorf("HOME = %q, want it inherited", got["HOME"])
	}
	if got["TERM"] != "dumb" {
		t.Errorf("TERM = %q, want dumb so tools do not emit escape codes", got["TERM"])
	}
}

func TestBuildEnvInjectsRunVars(t *testing.T) {
	got := envMap(t, BuildEnv(RunVars{
		Project: "shop", Service: "api", Env: "prod",
		Ref: "main", SHA: "abcdef1234567890", RunID: "d_1", Step: "rollout", Actor: "Alice",
	}, nil))
	want := map[string]string{
		"GW_PROJECT": "shop", "GW_SERVICE": "api", "GW_ENV": "prod",
		"GW_REF": "main", "GW_SHA": "abcdef1234567890", "GW_SHORT_SHA": "abcdef1",
		"GW_RUN_ID": "d_1", "GW_STEP": "rollout", "GW_ACTOR": "Alice",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestBuildEnvPerEnvironmentWins pins the precedence: an environment may point
// KUBECONFIG or HOME somewhere specific, and the operator who configured it is
// more authoritative than the host default.
func TestBuildEnvPerEnvironmentWins(t *testing.T) {
	t.Setenv("HOME", "/home/host")
	got := envMap(t, BuildEnv(RunVars{Env: "prod"}, map[string]string{
		"HOME":       "/srv/deploy-home",
		"KUBECONFIG": "/etc/grokwork/prod.conf",
	}))
	if got["HOME"] != "/srv/deploy-home" {
		t.Errorf("HOME = %q, want the environment override to win", got["HOME"])
	}
	if got["KUBECONFIG"] != "/etc/grokwork/prod.conf" {
		t.Errorf("KUBECONFIG = %q", got["KUBECONFIG"])
	}
}

func TestBuildEnvIsSorted(t *testing.T) {
	env := BuildEnv(RunVars{Env: "dev"}, map[string]string{"Z": "1", "A": "2"})
	if !slices.IsSorted(env) {
		t.Fatalf("env is not sorted, so a run's environment is not reproducible: %v", env)
	}
}

func TestEnvNames(t *testing.T) {
	got := EnvNames([]string{"B=2", "A=1", "C=has=equals"})
	if strings.Join(got, ",") != "A,B,C" {
		t.Fatalf("EnvNames = %v", got)
	}
}

func TestRedactorMasksAcrossWrites(t *testing.T) {
	const secret = "super-secret-value"
	var buf bytes.Buffer
	r := NewRedactor(&buf, []string{secret})
	// Split the secret across two writes: a redactor that only scans each write
	// in isolation would miss this, which is exactly how a streamed log leaks.
	if _, err := r.Write([]byte("before super-sec")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("ret-value after")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, secret) {
		t.Fatalf("secret survived a split write: %q", got)
	}
	if !strings.Contains(got, "before ") || !strings.Contains(got, " after") {
		t.Fatalf("surrounding output lost: %q", got)
	}
}

// TestRedactCoversDecodedBase64 pins the direction that matters: a credential
// stored base64 is used as `base64 -d`, so the DECODED form is what a tool
// echoes. Redacting the encoding of an already-encoded value matches nothing.
func TestRedactCoversDecodedBase64(t *testing.T) {
	plain := "kubeconfig-body-token"
	encoded := base64.StdEncoding.EncodeToString([]byte(plain))
	var buf bytes.Buffer
	r := NewRedactor(&buf, []string{encoded})
	if _, err := r.Write([]byte("decoded: " + plain + " done")); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if strings.Contains(buf.String(), plain) {
		t.Fatalf("decoded form leaked: %q", buf.String())
	}
}

func TestRedactorIgnoresShortValues(t *testing.T) {
	var buf bytes.Buffer
	// A 1-2 char "secret" would mask everywhere and destroy the log.
	r := NewRedactor(&buf, []string{"a", "ok"})
	if r.SecretsCount() != 0 {
		t.Fatalf("SecretsCount = %d, want short values ignored", r.SecretsCount())
	}
	_, _ = r.Write([]byte("a normal ok line"))
	_ = r.Close()
	if buf.String() != "a normal ok line" {
		t.Fatalf("short values were masked: %q", buf.String())
	}
}

func TestRedactorPrefersLongestOverlappingSecret(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(&buf, []string{"token-abc", "token-abc-extended"})
	_, _ = r.Write([]byte("v=token-abc-extended!"))
	_ = r.Close()
	got := buf.String()
	// Masking the shorter first would leave "-extended" exposed.
	if strings.Contains(got, "extended") {
		t.Fatalf("longer overlapping secret left a tail exposed: %q", got)
	}
}

func TestRedactorPassthroughWithNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(&buf, nil)
	n, err := r.Write([]byte("plain output"))
	if err != nil || n != len("plain output") {
		t.Fatalf("n=%d err=%v", n, err)
	}
	_ = r.Close()
	if buf.String() != "plain output" {
		t.Fatalf("got %q", buf.String())
	}
}

// TestRedactorReportsCallerLength matters because io.Copy treats a short write
// as an error, and the mask changes byte counts.
func TestRedactorReportsCallerLength(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(&buf, []string{"secret-value"})
	in := []byte("x secret-value y")
	n, err := r.Write(in)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(in) {
		t.Fatalf("Write returned %d, want the caller's %d", n, len(in))
	}
}

func TestCapWriterKeepsHeadAndTailOnOverflow(t *testing.T) {
	var buf bytes.Buffer
	c := newCapWriter(&buf, 10, 10)
	head := strings.Repeat("H", 10)
	middle := strings.Repeat("M", 50)
	tail := strings.Repeat("T", 10)
	if _, err := c.Write([]byte(head + middle + tail)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, head) {
		t.Fatalf("head lost: %q", got)
	}
	if !strings.HasSuffix(got, tail) {
		t.Fatalf("tail lost — the bytes that explain a failure: %q", got)
	}
	if !strings.Contains(got, "bytes elided") {
		t.Fatalf("no elision marker: %q", got)
	}
	if !c.Truncated() {
		t.Fatal("Truncated() false after overflow")
	}
}

func TestCapWriterPassesThroughUnderCap(t *testing.T) {
	var buf bytes.Buffer
	c := newCapWriter(&buf, 100, 100)
	_, _ = c.Write([]byte("short output"))
	_ = c.Close()
	if buf.String() != "short output" {
		t.Fatalf("got %q", buf.String())
	}
	if c.Truncated() {
		t.Fatal("Truncated() true without overflow")
	}
	if c.Written() != int64(len("short output")) {
		t.Fatalf("Written = %d", c.Written())
	}
}
