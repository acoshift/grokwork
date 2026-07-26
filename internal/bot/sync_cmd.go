package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

// handleSync: @Grok /sync — fetch origin and merge origin/<primary> into the session branch.
func (b *Bot) handleSync(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /sync` inside a Grok thread.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok {
		replyText(s, m, "No session for this thread yet.")
		return
	}
	if !b.actorCanShip(m, e.Project) && !b.canControlThread(m, e) {
		b.auditCmdMsg(audit.ActionGitSync, m, e.Project, errAuditDeniedCapability, nil)
		replyText(s, m, "You're not allowed to `/sync` (need builder caps or thread control).")
		return
	}
	cwd := strings.TrimSpace(e.Cwd)
	if cwd == "" {
		replyText(s, m, "No worktree — run a task first.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	branch := gitworktree.HeadBranchName(ctx, cwd)
	want := strings.TrimSpace(e.WorktreeBranch)
	if want != "" && branch != "" && branch != want {
		replyText(s, m, fmt.Sprintf("Current branch `%s` != session `%s` — aborting.", branch, want))
		return
	}
	if want != "" && !gitworktree.IsManagedBranch(want) {
		replyText(s, m, "Session branch is not managed — aborting.")
		return
	}

	// Optional checkpoint before sync when dirty or always recommended.
	if _, ok := e.LatestCheckpoint(); !ok {
		// soft tip only
	}

	if err := gitworktree.FetchOrigin(ctx, cwd); err != nil {
		// fetch may fail offline; still try local origin/* if present
		b.auditCmdMsg(audit.ActionGitSync, m, e.Project, fmt.Errorf("fetch: %w", err), map[string]any{
			"branch": e.WorktreeBranch,
		})
		replyText(s, m, "git fetch failed: "+err.Error())
		return
	}

	base := strings.TrimSpace(e.PrimaryBranch)
	if base == "" {
		mainRepo := e.MainCwd
		if mainRepo == "" && b.cfg != nil {
			if p, ok := b.cfg.ProjectPath(e.Project); ok {
				mainRepo = p
			}
		}
		if mainRepo == "" {
			mainRepo = cwd
		}
		name, _, err := gitworktree.ResolvePrimaryBranch(ctx, mainRepo)
		if err != nil {
			replyText(s, m, "Could not resolve primary branch: "+err.Error())
			return
		}
		base = name
	}

	if err := gitworktree.MergeOriginBase(ctx, cwd, base); err != nil {
		files, _ := gitworktree.ConflictedFiles(ctx, cwd)
		b.auditCmdMsg(audit.ActionGitSync, m, e.Project, fmt.Errorf("merge: %w", err), map[string]any{
			"branch":    e.WorktreeBranch,
			"base":      base,
			"conflicts": len(files),
		})
		msg := "Merge conflict with `origin/" + base + "`: " + err.Error()
		if len(files) > 0 {
			max := 15
			if len(files) < max {
				max = len(files)
			}
			msg += "\nConflicted files:\n• `" + strings.Join(files[:max], "`\n• `") + "`"
			if len(files) > max {
				msg += fmt.Sprintf("\n…and %d more", len(files)-max)
			}
			msg += "\n_Resolve conflicts in the worktree, or `@Grok /undo` to a checkpoint. Conflict clinic is not built yet._"
		}
		// leave conflicted state for user/model; do not auto-abort
		replyText(s, m, msg)
		return
	}
	head, _ := gitworktree.HeadSHA(ctx, cwd)
	b.auditCmdMsg(audit.ActionGitSync, m, e.Project, nil, map[string]any{
		"branch": e.WorktreeBranch,
		"base":   base,
		"head":   shortSHA(head),
	})
	replyText(s, m, fmt.Sprintf("**Synced** with `origin/%s` · HEAD `%s`", base, shortSHA(head)))
}
