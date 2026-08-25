package deploys

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGenerateTokenArgsAndParse(t *testing.T) {
	t.Parallel()
	var gotName string
	var gotArgs []string
	exp := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = slices.Clone(args)
		return []byte(`{
    "token": "minted-secret",
    "expiresAt": "2026-08-25T12:00:00Z",
    "project": "acme",
    "permissions": ["error.list", "error.get"]
}`), nil
	}
	tok, err := GenerateToken(t.Context(), run, " acme ")
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "deploys" {
		t.Fatalf("bin=%q", gotName)
	}
	wantArgs := []string{
		"me", "generate-token",
		"-project", "acme",
		"-permissions", "error.list,error.get",
		"-ttl", "3600",
		"-label", "grokwork:errors",
		"-output", "json",
	}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Fatalf("args=%q\nwant=%q", gotArgs, wantArgs)
	}
	if tok.Value != "minted-secret" || !tok.ExpiresAt.Equal(exp) || tok.Project != "acme" {
		t.Fatalf("token=%+v", tok)
	}
}

func TestGenerateTokenRequiresProject(t *testing.T) {
	t.Parallel()
	called := false
	run := func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	_, err := GenerateToken(t.Context(), run, "  ")
	if err == nil || !strings.Contains(err.Error(), "project is required") {
		t.Fatalf("err=%v", err)
	}
	if called {
		t.Fatal("must not exec without a project")
	}
}

func TestGenerateTokenDoesNotLeakStdout(t *testing.T) {
	t.Parallel()
	const secret = "LEAKED-TOKEN-VALUE"
	run := func(context.Context, string, ...string) ([]byte, error) {
		return fmt.Appendf(nil, "not json %s", secret), nil
	}
	_, err := GenerateToken(t.Context(), run, "acme")
	if err == nil {
		t.Fatal("want unexpected output")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("leaked token through error: %v", err)
	}

	run = func(context.Context, string, ...string) ([]byte, error) {
		return fmt.Appendf(nil, `{"token":%q}`, secret), fmt.Errorf("forbidden: no error.list")
	}
	_, err = GenerateToken(t.Context(), run, "acme")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("leaked token through failed-run error: %v", err)
	}
}

func TestGenerateTokenEmptyToken(t *testing.T) {
	t.Parallel()
	run := func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"token":"  ","expiresAt":"2026-08-25T12:00:00Z"}`), nil
	}
	_, err := GenerateToken(t.Context(), run, "acme")
	if err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("err=%v", err)
	}
}

func TestGenerateTokenCLIError(t *testing.T) {
	t.Parallel()
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("deploys me generate-token: permission error.list cannot be delegated")
	}
	_, err := GenerateToken(t.Context(), run, "acme")
	if err == nil || !strings.Contains(err.Error(), "cannot be delegated") {
		t.Fatalf("err=%v", err)
	}
}
