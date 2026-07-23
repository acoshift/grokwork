package grokrun

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// Default tools for inspect-only verify command suggestion.
const suggestVerifyTools = "read_file,list_dir,grep"

// SuggestStreamHooks receive live output while SuggestVerifyCommands runs.
// All fields are optional. Callbacks may run on a worker goroutine.
type SuggestStreamHooks struct {
	OnTextDelta func(delta string)
	OnThought   func(delta string)
	OnActivity  func(line string)
}

// SuggestVerifyCommands asks Grok to inspect cwd and propose project verify
// harness lines (name | command [| timeoutMs]). Does not persist config.
// Returns cleaned multi-line text ready for config.ParseVerifyCommandsText.
// When hooks is non-nil, streaming-json is enabled so the UI can show progress.
func SuggestVerifyCommands(ctx context.Context, grokBin, model, cwd string, timeout time.Duration, hooks *SuggestStreamHooks) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", fmt.Errorf("project path is required")
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return "", fmt.Errorf("project path %q is not a directory", cwd)
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	if strings.TrimSpace(grokBin) == "" {
		grokBin = "grok"
	}

	tools := suggestVerifyTools
	prompt := strings.Join([]string{
		"You configure project verify commands for Grok Work.",
		"Inspect this repository (your cwd) and propose shell commands that check the project is healthy:",
		"unit tests, lint, typecheck, build — only what this repo actually supports.",
		"",
		"Reply with ONLY lines in this exact format (no markdown fences, no prose, no numbering):",
		"name | command",
		"name | command | timeoutMs",
		"",
		"Rules:",
		"- name: short token (unit, lint, build, typecheck, fmt, e2e, …)",
		"- command: runs from the repo root; use make/npm/pnpm/yarn/go/cargo/pytest/etc. as the project does",
		"- timeoutMs is optional milliseconds; omit for the default (10 minutes); set only if a command needs longer",
		"- Prefer 1–5 fast local checks; skip deploy, publish, and network-heavy steps unless they are the main check",
		"- Do not invent scripts or targets that do not exist — read Makefile, package.json, go.mod, Cargo.toml,",
		"  pyproject.toml, Taskfile, justfile, .github/workflows, and README first",
		"- Prefer project-standard targets (make test, npm test, go test ./…) over one-off ad-hoc commands",
		"- If you are unsure, prefer fewer, high-confidence commands over speculative ones",
	}, "\n")

	opt := Options{
		GrokBin:          grokBin,
		Prompt:           prompt,
		Cwd:              cwd,
		Yolo:             false,
		Model:            model,
		MaxTurns:         16,
		Timeout:          timeout,
		Tools:            &tools,
		NoSubagents:      true,
		NoPlan:           true,
		NoMemory:         true,
		DisableWebSearch: true,
	}
	if hooks != nil {
		opt.OnTextDelta = hooks.OnTextDelta
		opt.OnThought = hooks.OnThought
		opt.OnActivity = hooks.OnActivity
	}

	result := Run(ctx, opt)
	if result.Cancelled {
		return "", fmt.Errorf("suggest verify commands cancelled or timed out")
	}
	if result.Code != 0 {
		log.Printf("grokrun: suggest verify failed code=%d text=%q stderr=%q",
			result.Code, truncate(result.Text, 200), truncate(result.Stderr, 400))
		msg := strings.TrimSpace(result.Text)
		if msg == "" {
			msg = strings.TrimSpace(result.Stderr)
		}
		if msg == "" {
			msg = fmt.Sprintf("grok exited with code %d", result.Code)
		}
		return "", fmt.Errorf("grok failed: %s", truncate(msg, 240))
	}

	text := ExtractVerifyCommandsText(result.Text)
	if text == "" {
		log.Printf("grokrun: suggest verify empty after clean text=%q", truncate(result.Text, 400))
		return "", fmt.Errorf("grok returned no parseable verify commands")
	}
	log.Printf("grokrun: suggest verify lines=%d", strings.Count(text, "\n")+1)
	return text, nil
}

// ExtractVerifyCommandsText pulls verify harness lines from model output,
// dropping markdown fences and surrounding prose.
func ExtractVerifyCommandsText(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "(empty response)" {
		return ""
	}
	s = stripMarkdownFences(s)

	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Drop common list markers / bold wrappers the model may add.
		line = strings.TrimLeft(line, "-*•")
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "`")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Numbered list: "1. unit | …"
		if i := strings.IndexByte(line, '.'); i > 0 && i < 4 {
			prefix := strings.TrimSpace(line[:i])
			if prefix != "" && isAllDigits(prefix) {
				line = strings.TrimSpace(line[i+1:])
			}
		}
		if looksLikeVerifyLine(line) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func stripMarkdownFences(s string) string {
	if !strings.Contains(s, "```") {
		return s
	}
	var fenced, outside strings.Builder
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		dst := &outside
		if inFence {
			dst = &fenced
		}
		if dst.Len() > 0 {
			dst.WriteByte('\n')
		}
		dst.WriteString(line)
	}
	if out := strings.TrimSpace(fenced.String()); out != "" {
		return out
	}
	// Empty fence body: use surrounding text with markers stripped.
	return strings.TrimSpace(outside.String())
}

func looksLikeVerifyLine(line string) bool {
	if strings.Contains(line, "|") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 {
			return false
		}
		return isVerifyName(strings.TrimSpace(parts[0])) && strings.TrimSpace(parts[1]) != ""
	}
	if idx := strings.Index(line, ":"); idx > 0 {
		name := strings.TrimSpace(line[:idx])
		cmd := strings.TrimSpace(line[idx+1:])
		// Avoid prose ("Note: …") and URLs; colon form is for short tokens only.
		if !isVerifyName(name) || cmd == "" {
			return false
		}
		if strings.EqualFold(name, "http") || strings.EqualFold(name, "https") {
			return false
		}
		return true
	}
	return false
}

// isVerifyName: short shell-label token (unit, lint, typecheck, e2e, CI, …).
func isVerifyName(name string) bool {
	if name == "" || len(name) > 32 || strings.Contains(name, " ") {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	// Reject Title Case prose ("Note", "Here") but allow "unit", "e2e", "CI".
	if name[0] >= 'A' && name[0] <= 'Z' {
		for _, r := range name[1:] {
			if r >= 'a' && r <= 'z' {
				return false
			}
		}
	}
	return true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
