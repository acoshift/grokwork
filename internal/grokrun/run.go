package grokrun

import (
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
	// Agent selects the coding CLI. Zero value is AgentGrok.
	Agent Agent
	// Bin is the CLI binary (path or PATH name) for Agent.
	Bin       string
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
	Tools *string
	// AllowMCP, when true with a non-empty Tools allowlist, omits --deny MCPTool
	// so Grok can attach user/repo MCP servers (including grokwork). Trusted
	// projects only: repo .grok/config.toml MCP is otherwise RCE on investigate.
	// Tools-off (pointer to "") still denies MCP even when this is set.
	AllowMCP         bool
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
	// MCPConfigPath, when set, is passed to claude as --mcp-config (and
	// --strict-mcp-config so only that file's servers attach).
	MCPConfigPath string
	// AgentToken is re-admitted into the child env as GROKWORK_AGENT_TOKEN when
	// IncludeAgentToken is set on ChildEnvPolicy (or appended by the caller).
	AgentToken string

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

// toolFlags maps Options.Tools to CLI args. nil → no flag (unrestricted built-ins
// and MCP meta-tools may attach from user/repo config). Non-nil denies MCPTool
// unless allowMCP is set with a non-empty allowlist: empty string is tools-off
// (pin a non-agentic built-in; MCP stays denied), and a non-empty allowlist is
// investigate-style. Without --deny MCPTool, Grok still attaches MCP meta-tools
// and repo .grok/config.toml servers, which would be RCE on "read-only"
// investigate runs unless the project opted into AgentMCPAlways.
func toolFlags(tools *string, allowMCP bool) []string {
	if tools == nil {
		return nil
	}
	t := *tools
	if t == "" {
		// Empty allowlist is unrestricted in the CLI; pin a non-agentic tool
		// so headless "tools-off" tasks cannot burn max-turns exploring the repo.
		t = toolsOffAllowlist
		allowMCP = false
	}
	if allowMCP {
		return []string{"--tools", t}
	}
	return []string{"--deny", "MCPTool", "--tools", t}
}

// MaxTurnsUserMessage is posted to Discord when the agent hits --max-turns.
const MaxTurnsUserMessage = "Reached max turns before a final reply. Partial work may exist in the agent session — send another task to continue."

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
	// CacheCreationInputTokens is reported by claude and is part of the prompt:
	// on a cold cache it dwarfs input_tokens. grok omits it (stays zero).
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens"`
	ReasoningTokens          int `json:"reasoning_tokens"`
	TotalTokens              int `json:"total_tokens"`
}

func (u *Usage) PromptTokens() int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
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

// Run executes one agent turn.
//
// The retries exist because callers mint a session id *before* the run (so a crash
// can still find the session), which means neither the caller nor Run can know
// whether a given id has been created yet — and claude rejects the wrong guess in
// either direction: creating an existing id fails with "Session ID … is already in
// use", resuming a missing one with "No conversation found". Both were verified
// against the CLI (exit 1, message on stderr, empty stdout), as was the fact that
// resuming after the first error recovers the session with its context intact.
// So run the caller's intent, then flip the guess exactly once on that signal.
// grok needs none of this: its -s is create-or-attach.
func Run(ctx context.Context, opt Options) Result {
	res := runOnce(ctx, opt)
	if strings.TrimSpace(opt.SessionID) == "" || res.Cancelled {
		return res
	}
	d := opt.Agent.driver()
	switch {
	case opt.ForceNewSession && d.sessionAlreadyExists(res):
		log.Printf("grokrun: session %s already exists, retrying as resume", opt.SessionID)
		retry := opt
		retry.ForceNewSession = false
		return runOnce(ctx, retry)
	case !opt.ForceNewSession && d.sessionMissing(res):
		// The stored id names no transcript — the run that minted it died before the
		// CLI created it, or the run cwd changed and transcripts are cwd-keyed. Start
		// the session under that same id instead of failing every run from here on.
		// Prior context is lost either way; a working thread beats a stuck one.
		log.Printf("grokrun: session %s not found, starting it instead", opt.SessionID)
		retry := opt
		retry.ForceNewSession = true
		return runOnce(ctx, retry)
	}
	return res
}

func runOnce(ctx context.Context, opt Options) Result {
	if opt.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opt.Timeout)
		defer cancel()
	}

	d := opt.Agent.driver()

	stream := opt.OnTextDelta != nil || opt.OnThought != nil || opt.OnActivity != nil
	format := "json"
	if stream {
		format = d.streamFormat()
	}

	// grok takes the prompt as a file (+ --verbatim) so characters like #, ?, &
	// and URL query strings survive CLI parsing; claude takes it on stdin, which
	// is unparsed and needs no file.
	promptPath := ""
	if !d.promptOnStdin() {
		path, cleanupPrompt, err := writePromptFile(opt.Prompt)
		if err != nil {
			return Result{
				Text:      fmt.Sprintf("Failed to write prompt file: %v", err),
				SessionID: opt.SessionID,
				Code:      1,
			}
		}
		defer cleanupPrompt()
		promptPath = path
	}

	// Some drivers must know the session id before the process starts (grok tails
	// updates.jsonl for tool activity, keyed by that id).
	runSessionID := strings.TrimSpace(opt.SessionID)
	prebound := false
	if runSessionID == "" && stream && d.prebindSession(opt) {
		runSessionID = NewSessionID()
		prebound = true
	}

	args := d.args(argInput{
		opt:          opt,
		promptPath:   promptPath,
		format:       format,
		runSessionID: runSessionID,
		prebound:     prebound,
	})

	log.Printf("grokrun: exec agent=%s bin=%q cwd=%q format=%s promptFile=%s promptLen=%d promptPreview=%q args=%v",
		opt.Agent, opt.Bin, opt.Cwd, format, promptPath, len(opt.Prompt), truncate(opt.Prompt, 200), args)

	cmd := exec.CommandContext(ctx, opt.Bin, args...)
	cmd.Dir = opt.Cwd
	if opt.Env != nil {
		cmd.Env = opt.Env
	} else {
		cmd.Env = os.Environ()
	}
	cmd.Env = d.childEnv(cmd.Env)
	if d.promptOnStdin() {
		cmd.Stdin = strings.NewReader(sanitizePrompt(opt.Prompt))
	}
	setProcessGroup(cmd)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if !stream {
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Start(); err != nil {
			return Result{
				Text:      fmt.Sprintf("Failed to start %s: %v", opt.Agent, err),
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
		return finishResult(ctx, opt, d, err, stdout.Bytes(), stderr.String(), opt.Timeout)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{
			Text:      fmt.Sprintf("Failed to open %s stdout pipe: %v", opt.Agent, err),
			SessionID: runSessionID,
			Code:      1,
			Stderr:    stderr.String(),
		}
	}
	if err := cmd.Start(); err != nil {
		return Result{
			Text:      fmt.Sprintf("Failed to start %s: %v", opt.Agent, err),
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
	if d.prebindSession(opt) && runSessionID != "" {
		watchWG.Add(1)
		go func() {
			defer watchWG.Done()
			d.watchActivity(watchCtx, opt.Cwd, runSessionID, opt.OnActivity)
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

	streamed, parseErr := decodeStream(stdout, d, streamCallbacks{
		onText:     opt.OnTextDelta,
		onThought:  opt.OnThought,
		onActivity: opt.OnActivity,
		cwd:        opt.Cwd,
	})
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
		applyStreamContext(&res, streamed)
		d.enrich(&res, opt.Cwd)
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
				Text:      fmt.Sprintf("Failed to run %s: %v", opt.Agent, waitErr),
				SessionID: sessionID,
				Code:      1,
				Stderr:    stderr.String(),
				Usage:     streamed.Usage,
				NumTurns:  streamed.NumTurns,
			}
			applyStreamContext(&res, streamed)
			d.enrich(&res, opt.Cwd)
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
				text = fmt.Sprintf("(%s exited %d with empty stream text)", opt.Agent, code)
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
	applyStreamContext(&res, streamed)
	d.enrich(&res, opt.Cwd)
	return res
}

// applyStreamContext carries context-window numbers that arrived on the stream.
// Drivers with an out-of-band source (grok's signals.json) refine these in enrich.
func applyStreamContext(res *Result, streamed streamOut) {
	if res.ContextTokensUsed == 0 {
		res.ContextTokensUsed = streamed.ContextTokensUsed
	}
	if res.ContextWindowTokens == 0 {
		res.ContextWindowTokens = streamed.ContextWindowTokens
	}
}

func finishResult(ctx context.Context, opt Options, d driver, err error, stdout []byte, stderr string, timeout time.Duration) Result {
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
				Text:      fmt.Sprintf("Failed to start %s: %v", opt.Agent, err),
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
	maxTurns := false

	if parsed, ok := d.decodeFinal(stdout); ok {
		if parsed.Text != "" {
			text = parsed.Text
		}
		if parsed.SessionID != "" {
			sessionID = parsed.SessionID
		}
		usage = parsed.Usage
		numTurns = parsed.NumTurns
		maxTurns = parsed.MaxTurnsReached
	} else if out == "" {
		text = strings.TrimSpace(stderr)
		if text == "" {
			text = fmt.Sprintf("(%s exited %d with empty stdout)", opt.Agent, code)
		}
	}

	if text == "" {
		text = "(empty response)"
	}

	res := Result{
		Text:            text,
		SessionID:       sessionID,
		Code:            code,
		Stderr:          stderr,
		Usage:           usage,
		NumTurns:        numTurns,
		MaxTurnsReached: maxTurns,
	}
	ensureMaxTurnsMessage(&res)
	d.enrich(&res, opt.Cwd)
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
			Text:      fmt.Sprintf("Timed out after %s. Partial work may exist in the %s.", timeout, opt.Agent.SessionLabel()),
			SessionID: opt.SessionID,
			Code:      124,
			Stderr:    stderr,
		}, true
	case ctx.Err() != nil:
		log.Printf("grokrun: cancelled stderr=%q", truncate(stderr, 1000))
		return Result{
			Text:      "Cancelled. Partial work may exist in the " + opt.Agent.SessionLabel() + ".",
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
	// Context fields are set by drivers that report the window on the stream
	// (claude); grok fills them from signals.json in enrich instead.
	ContextTokensUsed   int
	ContextWindowTokens int
}

// consumeStream decodes grok streaming-json. Kept as a named helper because the
// grok event vocabulary is what the stream tests pin.
func consumeStream(r io.Reader, onText, onThought, onActivity func(string)) (streamOut, error) {
	return decodeStream(r, grokDriver{}, streamCallbacks{
		onText:     onText,
		onThought:  onThought,
		onActivity: onActivity,
	})
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
// sanitizePrompt strips NULs, which no CLI accepts on stdin or in a prompt file.
func sanitizePrompt(prompt string) string {
	return strings.ReplaceAll(prompt, "\x00", "")
}

func writePromptFile(prompt string) (path string, cleanup func(), err error) {
	prompt = sanitizePrompt(prompt)
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
	for i := range len(abs) {
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

func SummarizeTitle(ctx context.Context, cli CLI, taskPrompt, cwd string, timeout time.Duration) (title string, ok bool) {
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

	cli = cli.Resolved()
	result := Run(ctx, Options{
		Agent:            cli.Agent,
		Bin:              cli.Bin,
		Prompt:           prompt,
		Cwd:              cwd,
		Yolo:             false,
		Model:            cli.Model,
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
	for line := range strings.SplitSeq(s, "\n") {
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
		for i := range 8 {
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
