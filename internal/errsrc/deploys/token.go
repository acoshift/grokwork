package deploys

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	generateTokenBin     = "deploys"
	generateTokenPerms   = "error.list,error.get"
	generateTokenTTL     = 365 * 24 * 60 * 60 // 1 year; deploys.app MaxGenerateTokenTTLSeconds
	generateTokenLabel   = "grokwork:errors"
	generateTokenTimeout = 20 * time.Second
)

// GeneratedToken is the once-returned value from `deploys me generate-token`.
type GeneratedToken struct {
	Value     string
	ExpiresAt time.Time
	Project   string
}

// CLIRunner execs the deploys CLI. Tests inject fakes. name is the binary.
// On failure return a non-nil error and do not put stdout in it — stdout is
// the minted token.
type CLIRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// GenerateToken mints a 1-year error.list+error.get bearer via
// `deploys me generate-token` using the host CLI login. The value is returned
// once; callers must store it. stdout from a failed run is discarded so the
// token cannot leak through an error string.
func GenerateToken(ctx context.Context, run CLIRunner, project string) (GeneratedToken, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return GeneratedToken{}, fmt.Errorf("deploys.app project is required to generate a token")
	}
	if run == nil {
		run = execCLI
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, generateTokenTimeout)
		defer cancel()
	}
	out, err := run(ctx, generateTokenBin,
		"me", "generate-token",
		"-project", project,
		"-permissions", generateTokenPerms,
		"-ttl", strconv.Itoa(generateTokenTTL),
		"-label", generateTokenLabel,
		"-output", "json",
	)
	if err != nil {
		return GeneratedToken{}, err
	}
	var parsed struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
		Project   string    `json:"project"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &parsed); err != nil {
		// out is the token; never quote it.
		return GeneratedToken{}, fmt.Errorf("deploys me generate-token: unexpected output")
	}
	token := strings.TrimSpace(parsed.Token)
	if token == "" {
		return GeneratedToken{}, fmt.Errorf("deploys me generate-token: empty token")
	}
	return GeneratedToken{
		Value:     token,
		ExpiresAt: parsed.ExpiresAt,
		Project:   cmp.Or(strings.TrimSpace(parsed.Project), project),
	}, nil
}

func execCLI(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("deploys CLI not found on PATH")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("deploys me generate-token: %s", truncate(msg, 200))
	}
	return stdout.Bytes(), nil
}
