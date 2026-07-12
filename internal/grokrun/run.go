package grokrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Options struct {
	GrokBin   string
	Prompt    string
	Cwd       string
	SessionID string
	Yolo      bool
	Model     string
	MaxTurns  int
	Timeout   time.Duration
	ExtraArgs []string
}

type Result struct {
	Text      string
	SessionID string
	Code      int
	Stderr    string
}

type jsonOut struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Message   string `json:"message"`
	SessionID string `json:"sessionId"`
}

// Run executes one headless Grok Build turn.
func Run(ctx context.Context, opt Options) Result {
	if opt.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opt.Timeout)
		defer cancel()
	}

	args := []string{
		"-p", opt.Prompt,
		"--cwd", opt.Cwd,
		"--output-format", "json",
		"--max-turns", fmt.Sprintf("%d", opt.MaxTurns),
		"--no-auto-update",
	}
	if opt.Yolo {
		args = append(args, "--yolo")
	}
	if opt.Model != "" {
		args = append(args, "-m", opt.Model)
	}
	if opt.SessionID != "" {
		args = append(args, "--resume", opt.SessionID)
	}
	args = append(args, opt.ExtraArgs...)

	cmd := exec.CommandContext(ctx, opt.GrokBin, args...)
	cmd.Dir = opt.Cwd
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Result{
				Text:      fmt.Sprintf("Timed out after %s. Partial work may exist in the Grok session.", opt.Timeout),
				SessionID: opt.SessionID,
				Code:      124,
				Stderr:    stderr.String(),
			}
		}
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return Result{
				Text:      fmt.Sprintf("Failed to start grok: %v", err),
				SessionID: opt.SessionID,
				Code:      1,
				Stderr:    stderr.String(),
			}
		}
	}

	out := strings.TrimSpace(stdout.String())
	text := out
	sessionID := opt.SessionID

	var parsed jsonOut
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err == nil {
		if parsed.Type == "error" {
			text = parsed.Message
			if text == "" {
				text = out
			}
		} else if parsed.Text != "" {
			text = parsed.Text
		}
		if parsed.SessionID != "" {
			sessionID = parsed.SessionID
		}
	} else if out == "" {
		text = strings.TrimSpace(stderr.String())
		if text == "" {
			text = fmt.Sprintf("(grok exited %d with empty stdout)", code)
		}
	}

	if text == "" {
		text = "(empty response)"
	}

	return Result{
		Text:      text,
		SessionID: sessionID,
		Code:      code,
		Stderr:    stderr.String(),
	}
}
