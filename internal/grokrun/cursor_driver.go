package grokrun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// cursorDriver runs the `cursor-agent` CLI (Cursor Agent headless / print mode).
//
// Differences from grok/claude that shape this driver:
//   - The prompt goes on stdin (verified against the CLI). Positional args also
//     work, but stdin survives the same # / ? / & characters --prompt-file
//     protects for grok.
//   - --print is required for non-interactive use. --trust skips the workspace
//     prompt so a headless run cannot hang on a TTY question.
//   - --output-format stream-json needs --stream-partial-output for thinking
//     deltas. Assistant text arrives as whole messages and is emitted twice
//     (once with timestamp_ms, once without); the second copy is skipped.
//   - There is no --tools / --max-turns / --mcp-config / --session-id. A non-nil
//     Tools pointer maps to --mode ask (read-only). Session resume is
//     create-or-attach via --resume <id>. A missing id is not an error.
//   - Usage uses camelCase (inputTokens, cacheReadTokens, cacheWriteTokens).
type cursorDriver struct{}

func (cursorDriver) streamFormat() string { return "stream-json" }
func (cursorDriver) promptOnStdin() bool  { return true }

func (cursorDriver) prebindSession(Options) bool { return false }

func (cursorDriver) childEnv(env []string) []string { return env }

func (cursorDriver) args(in argInput) []string {
	opt := in.opt
	args := []string{
		"--print",
		"--output-format", in.format,
		"--trust",
	}
	if in.format == "stream-json" {
		args = append(args, "--stream-partial-output")
	}
	if cwd := strings.TrimSpace(opt.Cwd); cwd != "" {
		args = append(args, "--workspace", cwd)
	}
	if opt.Yolo {
		args = append(args, "--yolo")
	}
	if opt.Model != "" {
		args = append(args, "--model", opt.Model)
	}
	// --resume is create-or-attach (verified: a never-seen UUID still starts
	// a chat under that id). That is grok's -s, so ForceNewSession still
	// passes the prebound id — omitting it would let the CLI mint a different
	// one and leave the journal pointing at a chat that never ran.
	if sid := strings.TrimSpace(opt.SessionID); sid != "" {
		args = append(args, "--resume", sid)
	}
	if opt.Tools != nil {
		// No --tools allowlist. ask is read-only Q&A and is the only headless
		// mode that will not prompt for approvals or open a write path.
		args = append(args, "--mode", "ask")
	}
	return append(args, opt.ExtraArgs...)
}

func (cursorDriver) watchActivity(context.Context, string, string, func(string)) {}

func (cursorDriver) enrich(*Result, string) {}

// sessionAlreadyExists / sessionMissing are always false: --resume is
// create-or-attach, so neither refusal is worth retrying.
func (cursorDriver) sessionAlreadyExists(Result) bool { return false }
func (cursorDriver) sessionMissing(Result) bool       { return false }

type cursorEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	// TimestampMS is set on the first copy of an assistant/thinking event.
	// The CLI then repeats the same assistant message without it.
	TimestampMS int64 `json:"timestamp_ms"`

	Message *struct {
		Content []cursorContentBlock `json:"content"`
	} `json:"message"`

	Usage *cursorUsage `json:"usage"`
}

type cursorContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type cursorUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

func (u *cursorUsage) toUsage() *Usage {
	if u == nil {
		return nil
	}
	return &Usage{
		InputTokens:              u.InputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
		OutputTokens:             u.OutputTokens,
		TotalTokens:              u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens,
	}
}

func (u *cursorUsage) contextTokens() int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens
}

func (cursorDriver) decodeLine(line []byte, acc *streamAccum) {
	var ev cursorEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		acc.note(err)
		return
	}
	acc.session(ev.SessionID)

	switch ev.Type {
	case "thinking":
		if ev.Subtype == "delta" {
			acc.thought(ev.Text)
		}
	case "assistant":
		if ev.Message == nil {
			return
		}
		for _, blk := range ev.Message.Content {
			switch blk.Type {
			case "text":
				// The CLI emits the same assistant message twice; taking both
				// would duplicate the reply. A later distinct message still
				// lands because the buffer no longer equals that text alone.
				if acc.sawDelta {
					continue
				}
				if strings.TrimSpace(acc.b.String()) == strings.TrimSpace(blk.Text) {
					continue
				}
				acc.separate()
				acc.text(blk.Text)
			case "tool_use", "tool_call":
				acc.activity(cursorToolActivity(blk, acc.cb.cwd))
			}
		}
	case "tool", "tool_call", "tool_use", "tool_start":
		if ev.Message != nil {
			for _, blk := range ev.Message.Content {
				acc.activity(cursorToolActivity(blk, acc.cb.cwd))
			}
			break
		}
		if ev.Text != "" {
			acc.activity(truncate(ev.Text, 80))
		}
	case "result":
		if u := ev.Usage.toUsage(); u != nil {
			acc.usage(u)
			acc.context(ev.Usage.contextTokens(), 0)
		}
		switch {
		case ev.IsError:
			acc.errorText(strings.TrimSpace(ev.Result))
		default:
			if acc.b.Len() == 0 {
				acc.text(strings.TrimSpace(ev.Result))
			}
		}
	}
}

func cursorToolActivity(blk cursorContentBlock, cwd string) string {
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

func (cursorDriver) decodeFinal(stdout []byte) (finalOut, bool) {
	var ev cursorEvent
	if err := json.Unmarshal(stdout, &ev); err != nil {
		return finalOut{}, false
	}
	if ev.Type != "" && ev.Type != "result" {
		return finalOut{}, false
	}
	return finalOut{
		Text:      strings.TrimSpace(ev.Result),
		SessionID: ev.SessionID,
		Usage:     ev.Usage.toUsage(),
	}, true
}
