package grokrun

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// claudeDriver runs the `claude` CLI (Claude Code headless / print mode).
//
// Differences from grok that shape this driver:
//   - The prompt goes on stdin. There is no --prompt-file/--verbatim pair, and
//     stdin is immune to the same shell/CLI mangling of #, ?, & that --prompt-file
//     protects against for grok.
//   - There is no --cwd; the child inherits cmd.Dir (Run always sets it).
//   - Tool activity, thinking, and usage all arrive on the stream, so neither a
//     session-file tail nor a signals.json read is needed.
type claudeDriver struct{}

func (claudeDriver) streamFormat() string { return "stream-json" }
func (claudeDriver) promptOnStdin() bool  { return true }

// prebindSession is false: the CLI echoes session_id on its init event, and
// activity comes from the stream rather than a file keyed by session id.
func (claudeDriver) prebindSession(Options) bool { return false }

// childEnv disables the self-updater, which is the env-var equivalent of grok's
// --no-auto-update flag.
func (claudeDriver) childEnv(env []string) []string {
	const disableUpdater = "DISABLE_AUTOUPDATER=1"
	for _, kv := range env {
		if name, _, ok := strings.Cut(kv, "="); ok && name == "DISABLE_AUTOUPDATER" {
			return env
		}
	}
	return append(env, disableUpdater)
}

func (claudeDriver) args(in argInput) []string {
	opt := in.opt
	args := []string{
		"--print",
		"--output-format", in.format,
	}
	// An unset MaxTurns must not become "--max-turns 0", which would allow no
	// agentic turn at all.
	if opt.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opt.MaxTurns))
	}
	if in.format == "stream-json" {
		// The CLI requires --verbose for stream-json under --print, and only emits
		// incremental text/thinking deltas with --include-partial-messages. Without
		// them the stream carries whole assistant messages instead.
		args = append(args, "--verbose", "--include-partial-messages")
	}
	if opt.Yolo {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	if opt.Model != "" {
		args = append(args, "--model", opt.Model)
	}
	// Trimmed: the CLI validates the id as a UUID, so a padded value from config or
	// a hand-edited sessions.json would be rejected outright.
	switch sid := strings.TrimSpace(opt.SessionID); {
	case sid != "" && opt.ForceNewSession:
		// ForceNewSession means "start a session under this id", which is grok's
		// -s. The equivalent is --session-id: callers mint the id before the run
		// (so a crash can still find it), so the session does not exist yet and
		// --resume would fail with "No conversation found with session ID".
		// Unlike grok's -s this is not create-or-attach — it errors if the id was
		// already used, which Run recovers from by retrying as a resume.
		args = append(args, "--session-id", sid)
	case sid != "":
		args = append(args, "--resume", sid)
	case in.runSessionID != "":
		args = append(args, "--session-id", in.runSessionID)
	}
	if opt.Tools != nil {
		// An empty value disables every built-in tool, which is the tools-off case;
		// a non-empty value is an allowlist. Either way MCP servers would otherwise
		// still attach their tools (verified against the real CLI), so restricting
		// built-ins without --strict-mcp-config would leave an unrestricted write
		// path open on investigate runs.
		args = append(args, "--tools", *opt.Tools, "--strict-mcp-config")
	}
	if deny := claudeDenyList(opt); deny != "" {
		args = append(args, "--disallowedTools", deny)
	}
	if opt.NoMemory {
		// Closest equivalent: drop CLAUDE.md, skills, plugins, hooks and MCP for
		// this run. --no-plan has no analogue and is ignored.
		args = append(args, "--safe-mode")
	}
	if schema := strings.TrimSpace(opt.JSONSchema); schema != "" {
		args = append(args, "--json-schema", schema)
	}
	return append(args, opt.ExtraArgs...)
}

// claudeDenyList maps the grok-shaped capability switches onto tool denials.
func claudeDenyList(opt Options) string {
	var deny []string
	if opt.NoSubagents {
		deny = append(deny, "Task")
	}
	if opt.DisableWebSearch {
		deny = append(deny, "WebSearch", "WebFetch")
	}
	return strings.Join(deny, ",")
}

// watchActivity is a no-op: tool_use blocks arrive on the stream.
func (claudeDriver) watchActivity(context.Context, string, string, func(string)) {}

// enrich is a no-op: context window and usage come from the result event.
func (claudeDriver) enrich(*Result, string) {}

// sessionAlreadyExists matches the CLI's refusal to create a session id that is
// already taken. Observed shape (exit 1, stderr, empty stdout):
//
//	Error: Session ID 0ee43f01-…-c02046d96498 is already in use.
//
// Matched on the stable part of the sentence rather than the whole line so the id
// and any prefix decoration do not have to be reproduced exactly.
func (claudeDriver) sessionAlreadyExists(res Result) bool {
	if res.Code == 0 {
		return false
	}
	hay := strings.ToLower(res.Stderr + "\n" + res.Text)
	return strings.Contains(hay, "session id") && strings.Contains(hay, "already in use")
}

// sessionMissing matches the opposite refusal — resuming an id the CLI has no
// transcript for. Observed shape (exit 1, stderr, empty stdout):
//
//	No conversation found with session ID: fb69b5a6-…
//
// Two ways in, and neither self-heals without a retry: a first run that died
// before the CLI created the session still persists its prebound id, and
// transcripts are keyed by working directory, so a session recorded under one cwd
// is invisible after the run cwd changes.
func (claudeDriver) sessionMissing(res Result) bool {
	if res.Code == 0 {
		return false
	}
	hay := strings.ToLower(res.Stderr + "\n" + res.Text)
	return strings.Contains(hay, "no conversation found")
}

// claudeEvent covers the envelope shapes the CLI emits under stream-json.
type claudeEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// Status is set on type=system subtype=status (e.g. "requesting" while
	// waiting on the API). Surfaced as activity so the web/Discord "running"
	// chrome is not blank during long model waits.
	Status string `json:"status"`
	// SessionID rides on nearly every event.
	SessionID string `json:"session_id"`
	// ParentToolUseID is non-empty for subagent output, which is not this run's reply.
	ParentToolUseID string `json:"parent_tool_use_id"`

	// type=stream_event
	Event *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	} `json:"event"`

	// type=assistant
	Message *struct {
		Content []claudeContentBlock `json:"content"`
	} `json:"message"`

	// type=result
	//
	// Result carries the reply only on subtype=success. Every error subtype leaves
	// it null and puts the reason in Errors, so reading Result alone loses the only
	// diagnostic the run produced. TerminalReason ("completed", "max_turns",
	// "budget_exhausted", …) is the last resort when neither is set.
	Result         string                 `json:"result"`
	Errors         []string               `json:"errors"`
	TerminalReason string                 `json:"terminal_reason"`
	IsError        bool                   `json:"is_error"`
	NumTurns       int                    `json:"num_turns"`
	Usage          *claudeUsage           `json:"usage"`
	ModelUsage     map[string]claudeModel `json:"modelUsage"`
}

// failureText is the human-readable reason a run failed, preferring the most
// specific source the event carries.
func (e claudeEvent) failureText() string {
	if s := strings.TrimSpace(e.Result); s != "" {
		return s
	}
	var parts []string
	for _, s := range e.Errors {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	if r := strings.TrimSpace(e.TerminalReason); r != "" && r != "completed" {
		return "run ended: " + r
	}
	if s := strings.TrimSpace(e.Subtype); s != "" && s != "success" {
		return s
	}
	return ""
}

type claudeContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// claudeUsage mirrors the CLI's usage object. cache_creation_input_tokens is a
// large share of the prompt on a cold cache, so it must be counted as prompt.
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	// Iterations holds the per-API-call usage, of which only the last describes the
	// live context. See contextTokens.
	Iterations []claudeUsage `json:"iterations"`
}

// contextTokens is how much of the window the conversation occupies at the end of
// the run.
//
// The top-level numbers are a *cumulative* bill across every API call in the run
// (the CLI assigns `usage: this.totalUsage`), and because a cached prefix is
// re-counted on every turn, summing them overstates context roughly in proportion
// to the turn count — a real 2-turn run measured 16,826 cumulative against 8,445
// actually resident. Only the final iteration describes the live context, so use
// it when present and fall back to the totals for a single-call run.
func (u *claudeUsage) contextTokens() int {
	if u == nil {
		return 0
	}
	last := u
	if n := len(u.Iterations); n > 0 {
		last = &u.Iterations[n-1]
	}
	return last.InputTokens + last.CacheReadInputTokens + last.CacheCreationInputTokens + last.OutputTokens
}

func (u *claudeUsage) toUsage() *Usage {
	if u == nil {
		return nil
	}
	return &Usage{
		InputTokens:              u.InputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		OutputTokens:             u.OutputTokens,
		TotalTokens:              u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens,
	}
}

type claudeModel struct {
	ContextWindow int `json:"contextWindow"`
}

func (claudeDriver) decodeLine(line []byte, acc *streamAccum) {
	var ev claudeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		acc.note(err)
		return
	}
	acc.session(ev.SessionID)

	switch ev.Type {
	case "system":
		if ev.Subtype == "status" {
			acc.activity(claudeStatusActivity(ev.Status))
		}
	case "stream_event":
		// Subagent deltas are not this run's answer.
		if ev.ParentToolUseID != "" || ev.Event == nil {
			return
		}
		// A run is several assistant messages (tool preamble, then the answer). Their
		// text is separate prose, so start a paragraph rather than gluing the next
		// message onto the last word of the previous one.
		if ev.Event.Type == "message_start" {
			acc.separate()
			return
		}
		if ev.Event.Delta == nil {
			return
		}
		switch ev.Event.Delta.Type {
		case "text_delta":
			acc.delta(ev.Event.Delta.Text)
		case "thinking_delta":
			acc.thought(ev.Event.Delta.Thinking)
		}
	case "assistant":
		if ev.ParentToolUseID != "" || ev.Message == nil {
			return
		}
		for _, blk := range ev.Message.Content {
			switch blk.Type {
			case "text":
				// With --include-partial-messages the same text already streamed as
				// deltas; taking it again would duplicate the whole reply.
				if !acc.sawDelta {
					acc.separate()
					acc.text(blk.Text)
				}
			case "tool_use":
				acc.activity(claudeToolActivity(blk, acc.cb.cwd))
			}
		}
	case "result":
		acc.turns(ev.NumTurns)
		if u := ev.Usage.toUsage(); u != nil {
			acc.usage(u)
			acc.context(ev.Usage.contextTokens(), claudeContextWindow(ev.ModelUsage))
		}
		switch {
		case ev.Subtype == "error_max_turns":
			acc.maxTurns()
		case ev.IsError:
			acc.errorText(ev.failureText())
		default:
			// Only a fallback: a normal streaming run already has the full text.
			if acc.b.Len() == 0 {
				acc.text(strings.TrimSpace(ev.Result))
			}
		}
	}
}

// claudeContextWindow picks the largest advertised window. A run may bill several
// models (a small model handles side tasks like title generation), and the main
// model is the one with the widest context.
func claudeContextWindow(m map[string]claudeModel) int {
	window := 0
	for _, v := range m {
		if v.ContextWindow > window {
			window = v.ContextWindow
		}
	}
	return window
}

// claudeStatusActivity turns a CLI system/status value into a short activity line.
func claudeStatusActivity(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return ""
	}
	// Known values: requesting (waiting on the model). Keep the raw token for
	// anything else so a new CLI status still surfaces rather than being dropped.
	switch status {
	case "requesting":
		return "waiting on model"
	default:
		return status
	}
}

func claudeToolActivity(blk claudeContentBlock, cwd string) string {
	name := strings.TrimSpace(blk.Name)
	detail := relativizeToolDetail(toolDetailFromRawInput(blk.Input), cwd)
	switch {
	case name != "" && detail != "":
		return fmt.Sprintf("%s: %s", name, truncate(detail, 60))
	case name != "":
		return "tool " + name
	case detail != "":
		return truncate(detail, 80)
	default:
		return ""
	}
}

// relativizeToolDetail rewrites paths under cwd to be relative to it.
//
// Two reasons, both load-bearing. Activity lines are rendered into Discord, and a
// local path must never appear there (CLAUDE.md) — claude's file tools take
// absolute paths, so its raw detail is the private worktree path. And the detail is
// then truncated to its first 60 characters, so an absolute path spent the whole
// budget on a prefix that is identical for every file in the repo and dropped the
// filename, which is the only part worth showing.
func relativizeToolDetail(detail, cwd string) string {
	detail = strings.TrimSpace(detail)
	cwd = strings.TrimSpace(cwd)
	if detail == "" || cwd == "" {
		return detail
	}
	// Also match the cwd's resolved form: macOS /tmp is a symlink to /private/tmp,
	// so the child reports paths under a prefix that does not match cwd verbatim.
	prefixes := []string{cwd}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil && resolved != cwd {
		prefixes = append(prefixes, resolved)
	}
	for _, p := range prefixes {
		p = strings.TrimSuffix(p, string(filepath.Separator))
		if detail == p {
			return "."
		}
		if rest, ok := strings.CutPrefix(detail, p+string(filepath.Separator)); ok {
			return rest
		}
	}
	return detail
}

// decodeFinal parses --output-format json, whose body is the same result object
// the stream ends with.
func (claudeDriver) decodeFinal(stdout []byte) (finalOut, bool) {
	var ev claudeEvent
	if err := json.Unmarshal(stdout, &ev); err != nil {
		return finalOut{}, false
	}
	out := finalOut{
		Text:      strings.TrimSpace(ev.Result),
		SessionID: ev.SessionID,
		Usage:     ev.Usage.toUsage(),
		NumTurns:  ev.NumTurns,
	}
	if ev.Subtype == "error_max_turns" {
		out.MaxTurnsReached = true
	}
	return out, true
}
