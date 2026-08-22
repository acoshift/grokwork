package grokrun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// grokDriver runs the `grok` CLI (Grok Build headless).
type grokDriver struct{}

func (grokDriver) streamFormat() string { return "streaming-json" }
func (grokDriver) promptOnStdin() bool  { return false }

// prebindSession is true when activity is wanted: streaming-json carries no tool
// events, so the session id must be known up front to tail updates.jsonl.
func (grokDriver) prebindSession(opt Options) bool { return opt.OnActivity != nil }

func (grokDriver) childEnv(env []string) []string { return env }

func (grokDriver) args(in argInput) []string {
	opt := in.opt
	args := []string{
		"--prompt-file", in.promptPath,
		"--verbatim",
		"--cwd", opt.Cwd,
		"--output-format", in.format,
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
	} else if in.prebound && in.runSessionID != "" {
		args = append(args, "-s", in.runSessionID)
	}
	args = append(args, toolFlags(opt.Tools, opt.AllowMCP)...)
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
	return append(args, opt.ExtraArgs...)
}

func (grokDriver) watchActivity(ctx context.Context, cwd, sessionID string, onActivity func(string)) {
	watchSessionTools(ctx, cwd, sessionID, onActivity)
}

func (grokDriver) enrich(res *Result, cwd string) { enrichContext(res, cwd) }

// sessionAlreadyExists and sessionMissing are always false: grok's -s is
// create-or-attach, so neither state is an error worth retrying.
func (grokDriver) sessionAlreadyExists(Result) bool { return false }
func (grokDriver) sessionMissing(Result) bool       { return false }

func (grokDriver) decodeLine(line []byte, acc *streamAccum) {
	var ev streamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		acc.note(err)
		return
	}
	switch strings.ToLower(ev.Type) {
	case "text":
		acc.delta(firstNonEmpty(ev.Data, ev.Text))
	case "thought":
		acc.thought(firstNonEmpty(ev.Data, ev.Text))
	case "tool", "tool_call", "tool_use", "tool_start", "status":
		acc.activity(activityLine(ev))
		acc.session(ev.SessionID)
	case "max_turns_reached":
		acc.maxTurns()
		acc.session(ev.SessionID)
		acc.usage(ev.Usage)
		acc.turns(ev.NumTurns)
	case "end":
		acc.session(ev.SessionID)
		acc.usage(ev.Usage)
		acc.turns(ev.NumTurns)
	case "error":
		acc.errorText(firstNonEmpty(ev.Message, ev.Data, ev.Text))
		acc.usage(ev.Usage)
		acc.turns(ev.NumTurns)
		acc.session(ev.SessionID)
	default:
		// Soft-show unknown events that look like tool activity.
		if strings.Contains(strings.ToLower(ev.Type), "tool") {
			acc.activity(activityLine(ev))
		}
		acc.session(ev.SessionID)
	}
}

func (grokDriver) decodeFinal(stdout []byte) (finalOut, bool) {
	var parsed jsonOut
	if err := json.Unmarshal(stdout, &parsed); err != nil {
		return finalOut{}, false
	}
	out := finalOut{
		SessionID: parsed.SessionID,
		Usage:     parsed.Usage,
		NumTurns:  parsed.NumTurns,
	}
	switch {
	case parsed.Type == "error":
		out.Text = parsed.Message
	default:
		out.Text = parsed.Text
	}
	return out, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
