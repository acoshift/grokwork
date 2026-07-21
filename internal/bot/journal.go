package bot

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/runjournal"
)

// Resume / interruption Discord + prompt constants (K13).
const (
	resumeAnnouncePrefix   = "Resumed after restart"
	interruptionPromptNote = "Previous attempt was interrupted by process restart; continue without duplicating completed steps.\n\n"
	maxResumeAttempts      = 3
	defaultShutdownTimeout = 15 * time.Second
)

// ErrNotReady is returned when the bot has not finished startup recovery.
var ErrNotReady = fmt.Errorf("bot is starting up; try again in a moment")

// ErrShuttingDown is returned when Stop has begun.
var ErrShuttingDown = fmt.Errorf("bot is shutting down")

func (b *Bot) resumeEnabled() bool {
	if b == nil || b.cfg == nil {
		return false
	}
	return b.cfg.ResumeActiveRunsEnabled()
}

// Ready reports whether new task claims are accepted.
// When the ready gate is not enabled (tests / simple embeds), always true.
func (b *Bot) Ready() bool {
	if b == nil {
		return false
	}
	if !b.gateReady.Load() {
		return true
	}
	return b.ready.Load()
}

// EnableReadyGate requires SetReady(true) before claims succeed (production boot).
func (b *Bot) EnableReadyGate() {
	if b == nil {
		return
	}
	b.gateReady.Store(true)
	b.ready.Store(false)
}

// SetReady marks the bot ready for new claims after recovery.
func (b *Bot) SetReady(v bool) {
	if b == nil {
		return
	}
	b.ready.Store(v)
}

// ShutdownTimeout returns how long Stop waits for drains.
func (b *Bot) ShutdownTimeout() time.Duration {
	if b == nil || b.cfg == nil {
		return defaultShutdownTimeout
	}
	if ms := b.cfg.ShutdownTimeoutMsValue(); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return defaultShutdownTimeout
}

// Runs returns the run journal store (nil when New failed to open it).
func (b *Bot) Runs() *runjournal.Store {
	if b == nil {
		return nil
	}
	return b.runs
}

// materializeTaskFiles downloads/copies attachments into the durable run journal tree
// and resolves ReferencedPrompt. Does not hold threadState.mu (K11).
func (b *Bot) materializeTaskFiles(ctx context.Context, threadID, taskID string, m *discordgo.MessageCreate, webPaths []string, related *discordgo.Message) (paths []string, refPrompt string, err error) {
	if b == nil || b.runs == nil || !b.resumeEnabled() {
		// No durable store / flag off: keep web paths as-is; Discord downloads later in executeTask.
		return append([]string(nil), webPaths...), "", nil
	}
	destDir := b.runs.TaskFilesDir(threadID, taskID)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, "", err
	}

	if len(webPaths) > 0 {
		for _, p := range webPaths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			name := filepath.Base(p)
			dst := filepath.Join(destDir, name)
			if err := copyFile(p, dst); err != nil {
				_ = os.RemoveAll(destDir)
				return nil, "", fmt.Errorf("copy attachment %q: %w", name, err)
			}
			paths = append(paths, dst)
		}
	}

	if m != nil {
		atts := collectAttachments(m.Attachments, related)
		if len(atts) > 0 {
			saved, dlErr := downloadAttachments(ctx, atts, destDir)
			if dlErr != nil {
				_ = os.RemoveAll(destDir)
				return nil, "", dlErr
			}
			for _, s := range saved {
				paths = append(paths, s.Path)
			}
		}
		if related != nil {
			refPrompt = formatReferencedPrompt(related)
		}
	}
	return paths, refPrompt, nil
}

func formatReferencedPrompt(related *discordgo.Message) string {
	if related == nil {
		return ""
	}
	// Folded reply block only (user Prompt stays separate).
	return promptWithReferenced("", related)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func taskRecordFromItem(item taskItem, status runjournal.Status) runjournal.TaskRecord {
	now := time.Now().UTC().Format(time.RFC3339)
	id := item.taskID
	if id == "" {
		id = runjournal.NewTaskID()
	}
	attempt := item.attempt
	if attempt <= 0 {
		attempt = 1
	}
	trigger := item.triggerMsgID
	if trigger == "" && item.m != nil {
		trigger = item.m.ID
	}
	return runjournal.TaskRecord{
		ID:               id,
		Status:           status,
		Prompt:           item.parsed.Prompt,
		Project:          item.proj.Name,
		ProjectCwd:       item.proj.Cwd,
		Source:           item.source,
		Origin:           item.origin,
		Actor:            runjournal.Actor{ID: item.actor.ID, DisplayName: item.actor.DisplayName},
		CreatedBy:        item.createdBy,
		CreatedByName:    item.createdByName,
		DiscordURL:       item.discordURL,
		TriggerMsgID:     trigger,
		StatusMsgID:      item.statusMsgID,
		AttachmentPaths:  append([]string(nil), item.attachmentPaths...),
		ReferencedPrompt: item.referencedPrompt,
		CreatedAt:        now,
		StartedAt:        now,
		Attempt:          attempt,
	}
}

// saveJournalFromState rebuilds journal queue from RAM under one Update (RMW-safe).
// When hasActive is true, Active is replaced from activeItem; otherwise existing Active
// is preserved (enqueue path). Never writes a placeholder Active.
func (b *Bot) saveJournalFromState(threadID string, st *threadState, activeItem taskItem, hasActive bool) error {
	if b == nil || b.runs == nil || !b.resumeEnabled() {
		return nil
	}
	var empty bool
	err := b.runs.Update(threadID, func(j *runjournal.Journal) error {
		j.Generation = b.bootGen
		j.Host = b.hostname
		if st.job != nil {
			if hasActive {
				rec := taskRecordFromItem(activeItem, runjournal.StatusRunning)
				if j.Active != nil && j.Active.ID == rec.ID {
					// Prefer RAM/item status id; keep journal value if item left it empty.
					if rec.StatusMsgID == "" {
						rec.StatusMsgID = j.Active.StatusMsgID
					}
					if j.Active.StartedAt != "" {
						rec.StartedAt = j.Active.StartedAt
					}
					if j.Active.Status == runjournal.StatusCancelling {
						rec.Status = runjournal.StatusCancelling
					}
				}
				j.Active = &rec
			}
			// enqueue: leave Active as-is (may be nil — no placeholder).
		} else {
			j.Active = nil
		}
		j.Queue = j.Queue[:0]
		for _, it := range st.queue {
			j.Queue = append(j.Queue, taskRecordFromItem(it, runjournal.StatusPending))
		}
		if j.Active == nil && len(j.Queue) == 0 {
			empty = true
			return runjournal.ErrSkipUpdate
		}
		return nil
	})
	if err != nil {
		return err
	}
	if empty {
		return b.runs.Delete(threadID)
	}
	return nil
}

func (b *Bot) patchJournal(threadID string, fn func(*runjournal.Journal)) {
	if b == nil || b.runs == nil || !b.resumeEnabled() {
		return
	}
	if err := b.runs.Update(threadID, func(j *runjournal.Journal) error {
		fn(j)
		j.Generation = b.bootGen
		return nil
	}); err != nil {
		log.Printf("warn: journal patch thread=%s: %v", threadID, err)
	}
}

func (b *Bot) deleteJournal(threadID string) {
	if b == nil || b.runs == nil {
		return
	}
	if err := b.runs.Delete(threadID); err != nil {
		log.Printf("warn: journal delete thread=%s: %v", threadID, err)
	}
}

// checkpointInterruptedLocked marks Active interrupted (unless cancelling) and
// rebuilds Queue from st.queue. Caller holds st.mu.
func (b *Bot) checkpointInterruptedLocked(threadID string, st *threadState) {
	if b == nil || b.runs == nil || !b.resumeEnabled() {
		return
	}
	if err := b.runs.Update(threadID, func(j *runjournal.Journal) error {
		if j.Active != nil && j.Active.Status != runjournal.StatusCancelling {
			j.Active.Status = runjournal.StatusInterrupted
		}
		j.Queue = j.Queue[:0]
		for _, it := range st.queue {
			j.Queue = append(j.Queue, taskRecordFromItem(it, runjournal.StatusPending))
		}
		j.GrokPID = 0
		j.BlockedReason = ""
		j.Generation = b.bootGen
		j.Host = b.hostname
		if j.Active == nil && len(j.Queue) == 0 {
			return runjournal.ErrSkipUpdate
		}
		return nil
	}); err != nil {
		log.Printf("warn: checkpoint interrupted thread=%s: %v", threadID, err)
	}
}
