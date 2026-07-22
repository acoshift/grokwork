package grokrun

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Options struct {
	GrokBin   string
	Prompt    string
	Cwd       string
	SessionID string
	// ForceNewSession with non-empty SessionID uses -s instead of --resume.
	ForceNewSession bool
	Yolo            bool
	Model           string
	MaxTurns        int
	Timeout         time.Duration
	ExtraArgs       []string
	// Tools non-nil → --tools allowlist. Pointer to "" means tools-off: the CLI
	// treats an empty --tools value as unrestricted, so we rewrite it to a
	// single non-agentic built-in and deny MCP meta-tools (see toolsOffAllowlist).
	Tools            *string
	NoSubagents      bool
	NoPlan           bool
	NoMemory         bool
	DisableWebSearch bool
	// JSONSchema, when set, passes --json-schema so the model is constrained to
	// that shape (implies --output-format json in the CLI).
	JSONSchema string
	// Env, when non-nil, is used as the child process environment instead of os.Environ().
	// Callers should pass a fully built env (Layer A filter, token omit, etc.).
	Env []string

	// OnTextDelta/OnThought enable streaming-json output.
	OnTextDelta func(delta string)
	OnThought   func(delta string)
	// OnActivity receives tool/status lines when the CLI emits them.
	OnActivity func(line string)
	// OnStartPID is called with the child process id after Start succeeds.
	OnStartPID func(pid int)
}

// toolsOffAllowlist is used when Options.Tools points to "".
// Grok CLI: empty --tools means "no allowlist" (all built-ins), not "zero tools".
// A real allowlist entry that cannot explore the repo gives tools-off behavior;
// MCP meta-tools still attach unless denied separately.
const toolsOffAllowlist = "ask_user_question"

// toolFlags maps Options.Tools to CLI args. nil → no flag; "" → tools-off rewrite.
func toolFlags(tools *string) []string {
	if tools == nil {
		return nil
	}
	t := *tools
	if t == "" {
		// Empty allowlist is unrestricted in the CLI; pin a non-agentic tool
		// and block MCP meta-tools so headless "tools-off" tasks cannot burn
		// max-turns exploring the repo.
		return []string{"--deny", "MCPTool", "--tools", toolsOffAllowlist}
	}
	return []string{"--tools", t}
}

// MaxTurnsUserMessage is posted to Discord when Grok hits --max-turns.
const MaxTurnsUserMessage = "Reached max turns before a final reply. Partial work may exist in the Grok session — send another task to continue."

type Result struct {
	Text                string
	SessionID           string
	Code                int
	Stderr              string
	Cancelled           bool
	MaxTurnsReached     bool
	Usage               *Usage
	NumTurns            int
	ContextTokensUsed   int
	ContextWindowTokens int
}

type Usage struct {
	InputTokens          int `json:"input_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	ReasoningTokens      int `json:"reasoning_tokens"`
	TotalTokens          int `json:"total_tokens"`
}

func (u *Usage) PromptTokens() int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheReadInputTokens
}

// ContextSummary formats used/size for Discord status (e.g. "4.8k/500k").
func (r Result) ContextSummary() string {
	if r.ContextWindowTokens > 0 {
		return formatTokenCount(r.ContextTokensUsed) + "/" + formatTokenCount(r.ContextWindowTokens)
	}
	if r.Usage != nil {
		if n := r.Usage.PromptTokens(); n > 0 {
			return "~" + formatTokenCount(n)
		}
		if r.Usage.TotalTokens > 0 {
			return "~" + formatTokenCount(r.Usage.TotalTokens)
		}
	}
	return ""
}

func formatTokenCount(n int) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n >= 1_000_000:
		v := float64(n) / 1_000_000
		if v >= 10 || n%1_000_000 == 0 {
			return fmt.Sprintf("%dM", n/1_000_000)
		}
		return fmt.Sprintf("%.1fM", v)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		if n%1000 == 0 {
			return fmt.Sprintf("%dk", n/1000)
		}
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

type jsonOut struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Data      string `json:"data"`
	Message   string `json:"message"`
	SessionID string `json:"sessionId"`
	NumTurns  int    `json:"num_turns"`
	Usage     *Usage `json:"usage"`
}

type streamEvent struct {
	Type       string `json:"type"`
	Data       string `json:"data"`
	Text       string `json:"text"`
	Message    string `json:"message"`
	Name       string `json:"name"`
	Tool       string `json:"tool"`
	SessionID  string `json:"sessionId"`
	StopReason string `json:"stopReason"`
	NumTurns   int    `json:"num_turns"`
	Usage      *Usage `json:"usage"`
}

func Run(ctx context.Context, opt Options) Result {
	if opt.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opt.Timeout)
		defer cancel()
	}

	stream := opt.OnTextDelta != nil || opt.OnThought != nil || opt.OnActivity != nil
	format := "json"
	if stream {
		format = "streaming-json"
	}

	// Pass the prompt via a temp file + --verbatim so characters like #, ?, &,
	// and URL query strings are not mangled by CLI/shell parsing of -p.
	promptPath, cleanupPrompt, err := writePromptFile(opt.Prompt)
	if err != nil {
		return Result{
			Text:      fmt.Sprintf("Failed to write prompt file: %v", err),
			SessionID: opt.SessionID,
			Code:      1,
		}
	}
	defer cleanupPrompt()

	// Know the session id before the process starts so we can tail updates.jsonl
	// for tool activity (streaming-json does not emit tool events).
	runSessionID := strings.TrimSpace(opt.SessionID)
	newSession := false
	if runSessionID == "" && stream && opt.OnActivity != nil {
		runSessionID = NewSessionID()
		newSession = true
	}

	args := []string{
		"--prompt-file", promptPath,
		"--verbatim",
		"--cwd", opt.Cwd,
		"--output-format", format,
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
		if opt.ForceNewSession {
			args = append(args, "-s", opt.SessionID)
		} else {
			args = append(args, "--resume", opt.SessionID)
		}
	} else if newSession {
		args = append(args, "-s", runSessionID)
	}
	args = append(args, toolFlags(opt.Tools)...)
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
	if schema := strings.TrimSpace(opt.JSONSchema); schema != "" {
		args = append(args, "--json-schema", schema)
	}
	args = append(args, opt.ExtraArgs...)

	log.Printf("grokrun: exec bin=%q cwd=%q format=%s promptFile=%s promptLen=%d promptPreview=%q args=%v",
		opt.GrokBin, opt.Cwd, format, promptPath, len(opt.Prompt), truncate(opt.Prompt, 200), args)

	cmd := exec.CommandContext(ctx, opt.GrokBin, args...)
	cmd.Dir = opt.Cwd
	if opt.Env != nil {
		cmd.Env = opt.Env
	} else {
		cmd.Env = os.Environ()
	}
	setProcessGroup(cmd)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if !stream {
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Start(); err != nil {
			return Result{
				Text:      fmt.Sprintf("Failed to start grok: %v", err),
				SessionID: runSessionID,
				Code:      1,
				Stderr:    stderr.String(),
			}
		}
		if opt.OnStartPID != nil && cmd.Process != nil {
			opt.OnStartPID(cmd.Process.Pid)
		}
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				if cmd.Process != nil {
					KillProcessGroup(cmd.Process.Pid)
				}
			case <-done:
			}
		}()
		err := cmd.Wait()
		close(done)
		return finishResult(ctx, opt, err, stdout.Bytes(), stderr.String(), opt.Timeout)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{
			Text:      fmt.Sprintf("Failed to start grok stdout pipe: %v", err),
			SessionID: runSessionID,
			Code:      1,
			Stderr:    stderr.String(),
		}
	}
	if err := cmd.Start(); err != nil {
		return Result{
			Text:      fmt.Sprintf("Failed to start grok: %v", err),
			SessionID: runSessionID,
			Code:      1,
			Stderr:    stderr.String(),
		}
	}
	if opt.OnStartPID != nil && cmd.Process != nil {
		opt.OnStartPID(cmd.Process.Pid)
	}

	watchCtx, stopWatch := context.WithCancel(ctx)
	var watchWG sync.WaitGroup
	if opt.OnActivity != nil && runSessionID != "" {
		watchWG.Add(1)
		go func() {
			defer watchWG.Done()
			watchSessionTools(watchCtx, opt.Cwd, runSessionID, opt.OnActivity)
		}()
	}

	// Kill process group on cancel so grandchildren die too.
	killDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				KillProcessGroup(cmd.Process.Pid)
			}
		case <-killDone:
		}
	}()

	streamed, parseErr := consumeStream(stdout, opt.OnTextDelta, opt.OnThought, opt.OnActivity)
	waitErr := cmd.Wait()
	close(killDone)
	stopWatch()
	watchWG.Wait()

	text := streamed.Text
	sessionID := streamed.SessionID
	if sessionID == "" {
		sessionID = runSessionID
	}
	if sessionID == "" {
		sessionID = opt.SessionID
	}

	if res, ok := contextResult(ctx, opt, stderr.String(), opt.Timeout); ok {
		if text != "" {
			res.Text = text
		}
		if sessionID != "" {
			res.SessionID = sessionID
		}
		res.Usage = streamed.Usage
		res.NumTurns = streamed.NumTurns
		res.MaxTurnsReached = streamed.MaxTurnsReached
		enrichContext(&res, opt.Cwd)
		return res
	}

	code := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
			log.Printf("grokrun: exit code=%d err=%v stderr=%q textLen=%d",
				code, waitErr, truncate(stderr.String(), 1000), len(text))
		} else {
			log.Printf("grokrun: wait failed: %v stderr=%q", waitErr, truncate(stderr.String(), 1000))
			res := Result{
				Text:      fmt.Sprintf("Failed to run grok: %v", waitErr),
				SessionID: sessionID,
				Code:      1,
				Stderr:    stderr.String(),
				Usage:     streamed.Usage,
				NumTurns:  streamed.NumTurns,
			}
			enrichContext(&res, opt.Cwd)
			return res
		}
	} else {
		log.Printf("grokrun: ok stream textLen=%d stderrLen=%d", len(text), stderr.Len())
	}

	if parseErr != nil {
		log.Printf("grokrun: stream parse note: %v", parseErr)
	}

	if text == "" {
		text = strings.TrimSpace(stderr.String())
		if text == "" {
			if code != 0 {
				text = fmt.Sprintf("(grok exited %d with empty stream text)", code)
			} else {
				text = "(empty response)"
			}
		}
	}

	res := Result{
		Text:            text,
		SessionID:       sessionID,
		Code:            code,
		Stderr:          stderr.String(),
		MaxTurnsReached: streamed.MaxTurnsReached,
		Usage:           streamed.Usage,
		NumTurns:        streamed.NumTurns,
	}
	ensureMaxTurnsMessage(&res)
	enrichContext(&res, opt.Cwd)
	return res
}

func finishResult(ctx context.Context, opt Options, err error, stdout []byte, stderr string, timeout time.Duration) Result {
	if res, ok := contextResult(ctx, opt, stderr, timeout); ok && err != nil {
		return res
	}

	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
			log.Printf("grokrun: exit code=%d err=%v stderr=%q stdoutLen=%d",
				code, err, truncate(stderr, 1000), len(stdout))
		} else {
			log.Printf("grokrun: start failed: %v stderr=%q", err, truncate(stderr, 1000))
			return Result{
				Text:      fmt.Sprintf("Failed to start grok: %v", err),
				SessionID: opt.SessionID,
				Code:      1,
				Stderr:    stderr,
			}
		}
	} else {
		log.Printf("grokrun: ok stdoutLen=%d stderrLen=%d", len(stdout), len(stderr))
	}

	out := strings.TrimSpace(string(stdout))
	text := out
	sessionID := opt.SessionID
	var usage *Usage
	var numTurns int

	var parsed jsonOut
	if err := json.Unmarshal(stdout, &parsed); err == nil {
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
		usage = parsed.Usage
		numTurns = parsed.NumTurns
	} else if out == "" {
		text = strings.TrimSpace(stderr)
		if text == "" {
			text = fmt.Sprintf("(grok exited %d with empty stdout)", code)
		}
	}

	if text == "" {
		text = "(empty response)"
	}

	res := Result{
		Text:      text,
		SessionID: sessionID,
		Code:      code,
		Stderr:    stderr,
		Usage:     usage,
		NumTurns:  numTurns,
	}
	ensureMaxTurnsMessage(&res)
	enrichContext(&res, opt.Cwd)
	return res
}

// isMaxTurnsError reports whether stderr indicates the CLI hit --max-turns.
func isMaxTurnsError(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "max turns reached") || strings.Contains(s, "max_turns_reached")
}

// ensureMaxTurnsMessage sets MaxTurnsReached from stderr when needed and
// appends MaxTurnsUserMessage so callers always have user-visible text.
func ensureMaxTurnsMessage(res *Result) {
	if res == nil {
		return
	}
	if !res.MaxTurnsReached && isMaxTurnsError(res.Stderr) {
		res.MaxTurnsReached = true
	}
	if !res.MaxTurnsReached {
		return
	}
	if strings.Contains(res.Text, "Reached max turns") {
		return
	}
	if strings.TrimSpace(res.Text) == "" || isMaxTurnsError(res.Text) {
		res.Text = MaxTurnsUserMessage
		return
	}
	res.Text = strings.TrimRight(res.Text, "\n") + "\n\n" + MaxTurnsUserMessage
}

func contextResult(ctx context.Context, opt Options, stderr string, timeout time.Duration) (Result, bool) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		log.Printf("grokrun: timeout after %s stderr=%q", timeout, truncate(stderr, 1000))
		return Result{
			Text:      fmt.Sprintf("Timed out after %s. Partial work may exist in the Grok session.", timeout),
			SessionID: opt.SessionID,
			Code:      124,
			Stderr:    stderr,
		}, true
	case ctx.Err() != nil:
		log.Printf("grokrun: cancelled stderr=%q", truncate(stderr, 1000))
		return Result{
			Text:      "Cancelled. Partial work may exist in the Grok session.",
			SessionID: opt.SessionID,
			Code:      130,
			Stderr:    stderr,
			Cancelled: true,
		}, true
	default:
		return Result{}, false
	}
}

type streamOut struct {
	Text            string
	SessionID       string
	Usage           *Usage
	NumTurns        int
	MaxTurnsReached bool
}

func consumeStream(r io.Reader, onText, onThought, onActivity func(string)) (out streamOut, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var b strings.Builder
	var parseNotes []string
	maxTurnsNotified := false

	appendText := func(delta string) {
		if delta == "" {
			return
		}
		b.WriteString(delta)
		if onText != nil {
			onText(delta)
		}
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev streamEvent
		if jerr := json.Unmarshal([]byte(line), &ev); jerr != nil {
			parseNotes = append(parseNotes, jerr.Error())
			continue
		}
		switch strings.ToLower(ev.Type) {
		case "text":
			delta := ev.Data
			if delta == "" {
				delta = ev.Text
			}
			appendText(delta)
		case "thought":
			delta := ev.Data
			if delta == "" {
				delta = ev.Text
			}
			if delta != "" && onThought != nil {
				onThought(delta)
			}
		case "tool", "tool_call", "tool_use", "tool_start", "status":
			if line := activityLine(ev); line != "" && onActivity != nil {
				onActivity(line)
			}
			if ev.SessionID != "" {
				out.SessionID = ev.SessionID
			}
		case "max_turns_reached":
			out.MaxTurnsReached = true
			if !maxTurnsNotified {
				maxTurnsNotified = true
				delta := MaxTurnsUserMessage
				if b.Len() > 0 {
					delta = "\n\n" + MaxTurnsUserMessage
				}
				appendText(delta)
			}
			if ev.SessionID != "" {
				out.SessionID = ev.SessionID
			}
			if ev.Usage != nil {
				out.Usage = ev.Usage
			}
			if ev.NumTurns > 0 {
				out.NumTurns = ev.NumTurns
			}
		case "end":
			if ev.SessionID != "" {
				out.SessionID = ev.SessionID
			}
			if ev.Usage != nil {
				out.Usage = ev.Usage
			}
			if ev.NumTurns > 0 {
				out.NumTurns = ev.NumTurns
			}
		case "error":
			msg := ev.Message
			if msg == "" {
				msg = ev.Data
			}
			if msg == "" {
				msg = ev.Text
			}
			if isMaxTurnsError(msg) {
				out.MaxTurnsReached = true
				if !maxTurnsNotified {
					maxTurnsNotified = true
					delta := MaxTurnsUserMessage
					if b.Len() > 0 {
						delta = "\n\n" + MaxTurnsUserMessage
					}
					appendText(delta)
				}
			} else if msg != "" {
				if b.Len() > 0 {
					appendText("\n\n" + msg)
				} else {
					appendText(msg)
				}
			}
			if ev.Usage != nil {
				out.Usage = ev.Usage
			}
			if ev.NumTurns > 0 {
				out.NumTurns = ev.NumTurns
			}
			if ev.SessionID != "" {
				out.SessionID = ev.SessionID
			}
		default:
			if line := activityLine(ev); line != "" && onActivity != nil {
				// Soft-show unknown non-text events when they look like activity.
				if strings.Contains(strings.ToLower(ev.Type), "tool") {
					onActivity(line)
				}
			}
			if ev.SessionID != "" {
				out.SessionID = ev.SessionID
			}
		}
	}
	out.Text = b.String()
	if scanErr := sc.Err(); scanErr != nil {
		err = scanErr
	} else if len(parseNotes) > 0 {
		err = fmt.Errorf("skipped %d malformed lines (e.g. %s)", len(parseNotes), parseNotes[0])
	}
	return out, err
}

func activityLine(ev streamEvent) string {
	name := strings.TrimSpace(ev.Name)
	if name == "" {
		name = strings.TrimSpace(ev.Tool)
	}
	detail := strings.TrimSpace(ev.Data)
	if detail == "" {
		detail = strings.TrimSpace(ev.Text)
	}
	if detail == "" {
		detail = strings.TrimSpace(ev.Message)
	}
	typ := strings.ToLower(strings.TrimSpace(ev.Type))
	switch {
	case name != "" && detail != "":
		return fmt.Sprintf("%s: %s", name, truncate(detail, 60))
	case name != "":
		return "tool " + name
	case detail != "" && (typ == "status" || strings.Contains(typ, "tool")):
		return truncate(detail, 80)
	default:
		return ""
	}
}

func enrichContext(res *Result, cwd string) {
	if res == nil || res.SessionID == "" {
		return
	}
	path, ok := findSignalsPath(cwd, res.SessionID)
	if !ok {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("grokrun: read signals %s: %v", path, err)
		return
	}
	var sig struct {
		ContextTokensUsed   int `json:"contextTokensUsed"`
		ContextWindowTokens int `json:"contextWindowTokens"`
	}
	if err := json.Unmarshal(data, &sig); err != nil {
		log.Printf("grokrun: parse signals %s: %v", path, err)
		return
	}
	if sig.ContextWindowTokens <= 0 && sig.ContextTokensUsed <= 0 {
		return
	}
	res.ContextTokensUsed = sig.ContextTokensUsed
	res.ContextWindowTokens = sig.ContextWindowTokens
	log.Printf("grokrun: context %d/%d session=%s",
		res.ContextTokensUsed, res.ContextWindowTokens, res.SessionID)
}

func findSignalsPath(cwd, sessionID string) (string, bool) {
	home := grokHome()
	if home == "" || sessionID == "" {
		return "", false
	}
	if abs, err := filepath.Abs(cwd); err == nil && abs != "" {
		p := filepath.Join(home, "sessions", encodeSessionDir(abs), sessionID, "signals.json")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", sessionID, "signals.json"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	return matches[0], true
}

func grokHome() string {
	if h := strings.TrimSpace(os.Getenv("GROK_HOME")); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".grok")
}

// encodeSessionDir matches Grok's session dir naming (%2FUsers%2F…).
// writePromptFile stores the user/system prompt for --prompt-file so special
// characters (#, ?, &, quotes, newlines) are delivered verbatim to the CLI.
func writePromptFile(prompt string) (path string, cleanup func(), err error) {
	prompt = strings.ReplaceAll(prompt, "\x00", "")
	f, err := os.CreateTemp("", "grokwork-prompt-*.txt")
	if err != nil {
		return "", func() {}, err
	}
	path = f.Name()
	cleanup = func() {
		_ = os.Remove(path)
	}
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

func encodeSessionDir(abs string) string {
	var b strings.Builder
	b.Grow(len(abs) * 3)
	for i := 0; i < len(abs); i++ {
		c := abs[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

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

// NewSessionID returns a random UUID v4 string for Grok's -s flag.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively impossible; still emit UUID shape.
		now := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(now >> (8 * i))
		}
		for i := 8; i < 16; i++ {
			b[i] = byte(now >> (8 * (i - 8)))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func newSessionID() string { return NewSessionID() }
