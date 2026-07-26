package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// handleCheckpoint: @Grok /checkpoint [label]
func (b *Bot) handleCheckpoint(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /checkpoint` inside a Grok thread with a worktree.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok {
		replyText(s, m, "No session for this thread yet.")
		return
	}
	if e.Cwd == "" || e.WorktreeBranch == "" {
		replyText(s, m, "No worktree yet — run a task first so a managed branch exists.")
		return
	}
	if !b.canControlThread(m, e) {
		// builders may also checkpoint
		if !b.actorCanShip(m, e.Project) {
			b.denyControl(s, m, e, "checkpoint")
			return
		}
	}
	label := stripCmdPrefix(parsed.Prompt, "/checkpoint", "checkpoint")
	meta, err := b.createCheckpoint(m.ChannelID, e, ActorFromUser(m.Author), label)
	if err != nil {
		replyText(s, m, "Checkpoint failed: "+err.Error())
		return
	}
	replyText(s, m, fmt.Sprintf("**Checkpoint** `%s` · `%s` · `%s`\n_Restore: `@Grok /restore %s` or `@Grok /undo`_",
		meta.ID, shortSHA(meta.SHA), meta.Label, meta.ID))
}

// handleUndo: @Grok /undo  → restore latest checkpoint (local hard reset).
func (b *Bot) handleUndo(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /undo` inside a Grok thread.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok {
		replyText(s, m, "No session for this thread yet.")
		return
	}
	cp, ok := e.LatestCheckpoint()
	if !ok {
		replyText(s, m, "No checkpoints. Create one with `@Grok /checkpoint [label]`.")
		return
	}
	// Optional: /undo force to skip dirty check — not default.
	force := strings.Contains(strings.ToLower(parsed.Prompt), "force")
	if err := b.restoreCheckpoint(s, m, e, cp, force); err != nil {
		replyText(s, m, "Undo failed: "+err.Error())
		return
	}
	replyText(s, m, fmt.Sprintf("**Restored** checkpoint `%s` → `%s` (local only; not force-pushed).", cp.ID, shortSHA(cp.SHA)))
}

// handleRestore: @Grok /restore <id>
func (b *Bot) handleRestore(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /restore <id>` inside a Grok thread.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok {
		replyText(s, m, "No session for this thread yet.")
		return
	}
	arg := stripCmdPrefix(parsed.Prompt, "/restore", "restore")
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		replyText(s, m, "Usage: `@Grok /restore <checkpoint-id>` (see `/checkpoint` list via status or last create reply).")
		return
	}
	id := fields[0]
	force := false
	for _, f := range fields[1:] {
		if strings.EqualFold(f, "force") {
			force = true
		}
	}
	cp, ok := e.FindCheckpoint(id)
	if !ok {
		// list ids
		var ids []string
		for _, c := range e.Checkpoints {
			ids = append(ids, c.ID)
		}
		msg := fmt.Sprintf("Unknown checkpoint `%s`.", id)
		if len(ids) > 0 {
			msg += " Known: `" + strings.Join(ids, "`, `") + "`"
		}
		replyText(s, m, msg)
		return
	}
	if err := b.restoreCheckpoint(s, m, e, cp, force); err != nil {
		replyText(s, m, "Restore failed: "+err.Error())
		return
	}
	replyText(s, m, fmt.Sprintf("**Restored** checkpoint `%s` → `%s` (local only).", cp.ID, shortSHA(cp.SHA)))
}

func (b *Bot) createCheckpoint(threadID string, e sessionstore.Entry, actor Actor, label string) (sessionstore.CheckpointMeta, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := newCheckpointID()
	sha, ref, err := gitworktree.CreateCheckpointRef(ctx, e.Cwd, threadID, id, "")
	if err != nil {
		log.Printf("git.checkpoint fail thread=%s actor=%s: %v", threadID, actor.ID, err)
		return sessionstore.CheckpointMeta{}, err
	}
	meta := sessionstore.CheckpointMeta{
		ID:        id,
		Ref:       ref,
		SHA:       sha,
		Label:     strings.TrimSpace(label),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		CreatedBy: actor.ID,
	}
	_, _, err = b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.Checkpoints = append(ent.Checkpoints, meta)
		_ = sessionstore.ClampWave2Fields(ent)
	})
	if err != nil {
		return sessionstore.CheckpointMeta{}, err
	}
	log.Printf("git.checkpoint ok thread=%s id=%s sha=%s actor=%s", threadID, id, shortSHA(sha), actor.ID)
	return meta, nil
}

func (b *Bot) restoreCheckpoint(s *discordgo.Session, m *discordgo.MessageCreate, e sessionstore.Entry, cp sessionstore.CheckpointMeta, force bool) error {
	threadID := m.ChannelID
	if !b.canControlThread(m, e) && !b.actorCanApprove(m, e.Project) {
		return fmt.Errorf("only the owner, co-owner, mod, or approver may restore")
	}
	// K8 checklist
	if strings.TrimSpace(e.Cwd) == "" {
		return fmt.Errorf("session has no worktree cwd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	branch := gitworktree.HeadBranchName(ctx, e.Cwd)
	want := strings.TrimSpace(e.WorktreeBranch)
	if want == "" || !gitworktree.IsManagedBranch(want) {
		return fmt.Errorf("session branch is not a managed worktree branch")
	}
	if branch != want {
		return fmt.Errorf("current branch `%s` != session branch `%s`", branch, want)
	}
	if !gitworktree.IsCheckpointRefForThread(cp.Ref, threadID) {
		return fmt.Errorf("checkpoint ref is not scoped to this thread (refused)")
	}
	// Resolve live ref SHA
	liveSHA, err := gitworktree.ResolveRefSHA(ctx, e.Cwd, cp.Ref)
	if err != nil {
		return fmt.Errorf("checkpoint ref missing: %w", err)
	}
	if liveSHA != cp.SHA {
		log.Printf("warn: checkpoint %s meta sha=%s live=%s (using live)", cp.ID, cp.SHA, liveSHA)
	}
	dirty, err := gitworktree.WorkingTreeDirty(ctx, e.Cwd)
	if err != nil {
		return err
	}
	if dirty && !force {
		return fmt.Errorf("working tree is dirty — commit/stash or re-run with `@Grok /restore %s force`", cp.ID)
	}
	if err := gitworktree.HardResetToSHA(ctx, e.Cwd, liveSHA); err != nil {
		log.Printf("git.checkpoint_restore fail thread=%s id=%s: %v", threadID, cp.ID, err)
		return err
	}
	actor := ""
	if m.Author != nil {
		actor = m.Author.ID
	}
	log.Printf("git.checkpoint_restore ok thread=%s id=%s sha=%s actor=%s", threadID, cp.ID, shortSHA(liveSHA), actor)
	return nil
}

func (b *Bot) actorCanShip(m *discordgo.MessageCreate, project string) bool {
	if b.cfg == nil || m == nil || m.Author == nil {
		return false
	}
	caps := b.cfg.ResolveCapabilities(project, m.Author.ID)
	return caps.CanShip()
}

func (b *Bot) actorCanApprove(m *discordgo.MessageCreate, project string) bool {
	if b.cfg == nil || m == nil || m.Author == nil {
		return false
	}
	caps := b.cfg.ResolveCapabilities(project, m.Author.ID)
	return caps.Approve || caps.AdminProject || caps.CanShip()
}

func newCheckpointID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%1e8)
	}
	return hex.EncodeToString(b[:])
}
