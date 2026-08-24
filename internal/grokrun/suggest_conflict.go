package grokrun

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

func suggestConflictTools(a Agent) string {
	switch a.Resolve() {
	case AgentClaude:
		return "Read,Glob,Grep,Edit,Write"
	case AgentCursor:
		return "Read,Glob,Grep,Edit,Write"
	default:
		return "read_file,list_dir,grep,write_file"
	}
}

// SuggestConflictResolution asks the coding CLI to resolve conflict markers in
// cwd (a parked cherry-pick checkout). It does not run git, continue, commit,
// or push. SessionID is empty — this is not a grokwork session.
func SuggestConflictResolution(ctx context.Context, cli CLI, cwd string, timeout time.Duration, files []string, target, sha string, hooks *SuggestStreamHooks) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", fmt.Errorf("checkout path is required")
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return "", fmt.Errorf("checkout is not a directory")
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	cli = cli.Resolved()
	list := strings.Join(files, "\n- ")
	if list != "" {
		list = "- " + list
	}
	prompt := strings.Join([]string{
		"You are resolving a git cherry-pick conflict in this working tree.",
		"HEAD / ours is the target branch " + strings.TrimSpace(target) + ".",
		"Theirs is the commit being cherry-picked: " + strings.TrimSpace(sha) + ".",
		"Unmerged files:",
		list,
		"",
		"Edit those files in place: remove conflict markers and produce a coherent result.",
		"Prefer keeping the target-branch intent while landing the picked change.",
		"",
		"Rules:",
		"- Do not run git. Do not cherry-pick --continue. Do not commit. Do not push.",
		"- Do not edit files that are not in the unmerged list.",
		"- Do not create extra files.",
		"- Reply with a short summary of what you changed (plain text).",
	}, "\n")

	tools := suggestConflictTools(cli.Agent)
	opt := Options{
		Agent:            cli.Agent,
		Bin:              cli.Bin,
		Prompt:           prompt,
		Cwd:              cwd,
		Yolo:             true,
		Model:            cli.Model,
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
		return "", fmt.Errorf("suggest conflict resolution cancelled or timed out")
	}
	if result.Code != 0 {
		log.Printf("grokrun: suggest conflict failed code=%d text=%q", result.Code, truncate(result.Text, 200))
		msg := strings.TrimSpace(result.Text)
		if msg == "" {
			msg = strings.TrimSpace(result.Stderr)
		}
		if msg == "" {
			msg = fmt.Sprintf("grok exited with code %d", result.Code)
		}
		return "", fmt.Errorf("grok failed: %s", truncate(msg, 240))
	}
	return strings.TrimSpace(result.Text), nil
}
