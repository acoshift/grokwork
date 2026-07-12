package grokrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	// Tools, when non-nil, passes --tools (comma-separated allowlist).
	// Use a pointer to empty string to request no tools when the CLI supports it.
	Tools *string
	// NoSubagents / NoPlan / DisableWebSearch add corresponding headless flags.
	NoSubagents      bool
	NoPlan           bool
	NoMemory         bool
	DisableWebSearch bool
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
	if opt.Tools != nil {
		args = append(args, "--tools", *opt.Tools)
	}
	if opt.NoSubagents {
		args = append(args, "--no-subagents")
	}
	if opt.NoPlan {
		args = append(args, "--no-plan")
	}
	if opt.NoMemory {
		args = append(args, "--no-memory")
	}
	if opt.DisableWebSearch {
		args = append(args, "--disable-web-search")
	}
	args = append(args, opt.ExtraArgs...)

	// Log argv without dumping a huge prompt twice if already logged upstream.
	logArgs := make([]string, len(args))
	copy(logArgs, args)
	for i := 0; i+1 < len(logArgs); i++ {
		if logArgs[i] == "-p" && len(logArgs[i+1]) > 200 {
			logArgs[i+1] = logArgs[i+1][:200] + "…"
		}
	}
	log.Printf("grokrun: exec bin=%q cwd=%q args=%v", opt.GrokBin, opt.Cwd, logArgs)

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
			log.Printf("grokrun: timeout after %s stderr=%q", opt.Timeout, truncate(stderr.String(), 1000))
			return Result{
				Text:      fmt.Sprintf("Timed out after %s. Partial work may exist in the Grok session.", opt.Timeout),
				SessionID: opt.SessionID,
				Code:      124,
				Stderr:    stderr.String(),
			}
		}
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
			log.Printf("grokrun: exit code=%d err=%v stderr=%q stdoutLen=%d",
				code, err, truncate(stderr.String(), 1000), stdout.Len())
		} else {
			log.Printf("grokrun: start failed: %v stderr=%q", err, truncate(stderr.String(), 1000))
			return Result{
				Text:      fmt.Sprintf("Failed to start grok: %v", err),
				SessionID: opt.SessionID,
				Code:      1,
				Stderr:    stderr.String(),
			}
		}
	} else {
		log.Printf("grokrun: ok stdoutLen=%d stderrLen=%d", stdout.Len(), stderr.Len())
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

// SummarizeTitle asks Grok for a short Discord thread title (separate one-shot
// session, no resume into the work session). On failure, ok is false.
func SummarizeTitle(ctx context.Context, grokBin, model, taskPrompt, cwd string, timeout time.Duration) (title string, ok bool) {
	if strings.TrimSpace(taskPrompt) == "" {
		return "", false
	}
	if cwd == "" {
		cwd = os.TempDir()
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}

	noTools := ""
	prompt := strings.Join([]string{
		"You name Discord threads for an engineering team.",
		"Given the user task below, reply with ONLY a short thread title.",
		"Rules:",
		"- 3 to 10 words",
		"- under 80 characters",
		"- no quotes, no markdown, no trailing punctuation",
		"- no leading labels like Title:",
		"- describe the task, not the user",
		"",
		"Task:",
		taskPrompt,
	}, "\n")

	result := Run(ctx, Options{
		GrokBin:          grokBin,
		Prompt:           prompt,
		Cwd:              cwd,
		Yolo:             false,
		Model:            model,
		MaxTurns:         1,
		Timeout:          timeout,
		Tools:            &noTools,
		NoSubagents:      true,
		NoPlan:           true,
		NoMemory:         true,
		DisableWebSearch: true,
		ExtraArgs:        []string{"--verbatim"},
	})
	if result.Code != 0 {
		log.Printf("grokrun: summarize failed code=%d text=%q stderr=%q",
			result.Code, truncate(result.Text, 200), truncate(result.Stderr, 400))
		return "", false
	}

	title = cleanTitle(result.Text)
	if title == "" {
		return "", false
	}
	log.Printf("grokrun: summarize title=%q", title)
	return title, true
}

func cleanTitle(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// First non-empty line only.
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		s = line
		break
	}
	s = strings.Trim(s, "\"'`*")
	s = strings.TrimPrefix(s, "Title:")
	s = strings.TrimPrefix(s, "title:")
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || s == "(empty response)" {
		return ""
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
