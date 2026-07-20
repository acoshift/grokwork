package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/runjournal"
)

// RecoverActiveRuns rehydrates durable journals after process start.
// Must run while ready=false (gate enabled). May use Discord() for announce.
func (b *Bot) RecoverActiveRuns(ctx context.Context) error {
	if b == nil || b.runs == nil {
		return nil
	}
	if !b.resumeEnabled() {
		return b.purgeLeftoverJournals(ctx)
	}
	host, _ := os.Hostname()
	b.hostname = host
	b.bootGen = uint64(time.Now().UnixNano())

	list, err := b.runs.List()
	if err != nil {
		return err
	}
	log.Printf("resume: recovering %d journal(s) generation=%d host=%s", len(list), b.bootGen, host)

	for i := range list {
		j := list[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		b.recoverOne(ctx, j)
	}
	return nil
}

func (b *Bot) purgeLeftoverJournals(ctx context.Context) error {
	list, err := b.runs.List()
	if err != nil {
		return err
	}
	n := 0
	for _, j := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		if j.GrokPID != 0 {
			if grokrun.ProcessAlive(j.GrokPID) && runjournal.LooksLikeGrokCLI(j.GrokPID, b.cfg.GrokBin) {
				grokrun.KillProcessGroup(j.GrokPID)
			}
		}
		_ = b.runs.Delete(j.ThreadID)
		n++
	}
	if n > 0 {
		log.Printf("resume: flag off; purged %d leftover journal(s)", n)
	}
	return nil
}

func (b *Bot) recoverOne(ctx context.Context, j runjournal.Journal) {
	threadID := strings.TrimSpace(j.ThreadID)
	if threadID == "" {
		return
	}
	if j.Host != "" && b.hostname != "" && j.Host != b.hostname {
		log.Printf("resume: skip foreign host thread=%s host=%s", threadID, j.Host)
		return
	}

	projName := ""
	if j.Active != nil {
		projName = j.Active.Project
	} else if len(j.Queue) > 0 {
		projName = j.Queue[0].Project
	}
	cwd, ok := b.cfg.ProjectPath(projName)
	if !ok || cwd == "" {
		log.Printf("resume: missing project %q thread=%s — delete journal", projName, threadID)
		b.notifyResumeFail(threadID, fmt.Sprintf("Could not resume: project `%s` is no longer configured.", projName))
		_ = b.runs.Delete(threadID)
		return
	}
	proj := projectRef{Name: projName, Cwd: cwd}

	// Orphan kill with PID verification (advisor: don't kill non-grok PIDs).
	needsKill := j.GrokPID != 0 || (j.Active != nil && j.Active.Status == runjournal.StatusBlockedOrphan)
	if needsKill {
		if j.GrokPID != 0 {
			if grokrun.ProcessAlive(j.GrokPID) {
				if !runjournal.LooksLikeGrokCLI(j.GrokPID, b.cfg.GrokBin) {
					log.Printf("resume: pid %d not grok CLI; clearing dead/recycled PID thread=%s", j.GrokPID, threadID)
					j.GrokPID = 0
				} else {
					grokrun.KillProcessGroup(j.GrokPID)
					waitPIDExit(j.GrokPID, 3*time.Second)
				}
			} else {
				j.GrokPID = 0
			}
		}
		if j.GrokPID != 0 && grokrun.ProcessAlive(j.GrokPID) {
			_ = b.runs.Update(threadID, func(jj *runjournal.Journal) error {
				if jj.Active != nil {
					jj.Active.Status = runjournal.StatusBlockedOrphan
				}
				jj.BlockedReason = "orphan pid still alive"
				jj.GrokPID = j.GrokPID
				return nil
			})
			b.notifyResumeFail(threadID, fmt.Sprintf(
				"Could not stop previous Grok process (pid %d); not re-driving to avoid double work. Will retry on next restart after the process exits.",
				j.GrokPID,
			))
			return
		}
		_ = b.runs.Update(threadID, func(jj *runjournal.Journal) error {
			jj.GrokPID = 0
			jj.BlockedReason = ""
			if jj.Active != nil && jj.Active.Status == runjournal.StatusBlockedOrphan {
				jj.Active.Status = runjournal.StatusInterrupted
			}
			return nil
		})
		// Reload after update for subsequent logic.
		if reloaded, found, err := b.runs.Load(threadID); err == nil && found {
			j = reloaded
		} else {
			j.GrokPID = 0
			j.BlockedReason = ""
			if j.Active != nil && j.Active.Status == runjournal.StatusBlockedOrphan {
				j.Active.Status = runjournal.StatusInterrupted
			}
		}
	}

	if j.Active == nil && len(j.Queue) == 0 {
		_ = b.runs.Delete(threadID)
		return
	}

	// User cancel intent: drop active, keep queue.
	if j.Active != nil && j.Active.Status == runjournal.StatusCancelling {
		if j.Active.ID != "" {
			b.runs.RemoveTaskFiles(threadID, j.Active.ID)
		}
		dropped := j.Active.ID
		_ = b.runs.Update(threadID, func(jj *runjournal.Journal) error {
			jj.Active = nil
			if len(jj.Queue) == 0 {
				return runjournal.ErrSkipUpdate
			}
			return nil
		})
		if len(j.Queue) == 0 {
			_ = b.runs.Delete(threadID)
			log.Printf("resume: dropped cancelling active=%s thread=%s (empty queue)", dropped, threadID)
			return
		}
		j.Active = nil
		log.Printf("resume: dropped cancelling active=%s thread=%s; draining queue=%d", dropped, threadID, len(j.Queue))
	}

	if j.Active != nil && j.Active.Attempt >= maxResumeAttempts {
		b.notifyResumeFail(threadID, fmt.Sprintf("Gave up resuming after %d attempts.", j.Active.Attempt))
		if j.Active.ID != "" {
			b.runs.RemoveTaskFiles(threadID, j.Active.ID)
		}
		_ = b.runs.Update(threadID, func(jj *runjournal.Journal) error {
			jj.Active = nil
			if len(jj.Queue) == 0 {
				return runjournal.ErrSkipUpdate
			}
			return nil
		})
		if len(j.Queue) == 0 {
			_ = b.runs.Delete(threadID)
			return
		}
		j.Active = nil
	}

	var tasks []runjournal.TaskRecord
	if j.Active != nil {
		switch j.Active.Status {
		case runjournal.StatusRunning, runjournal.StatusInterrupted, runjournal.StatusPending:
			// Heal stuck Working message before clearing StatusMsgID.
			if j.Active.StatusMsgID != "" {
				b.healInterruptedStatus(threadID, j.Active.StatusMsgID, proj.Name)
			}
			j.Active.Attempt++
			j.Active.Status = runjournal.StatusRunning
			j.Active.StatusMsgID = ""
			tasks = append(tasks, *j.Active)
		}
	}
	tasks = append(tasks, j.Queue...)
	if len(tasks) == 0 {
		_ = b.runs.Delete(threadID)
		return
	}

	first := tasks[0]
	_ = b.runs.Update(threadID, func(jj *runjournal.Journal) error {
		a := first
		jj.Active = &a
		jj.Queue = append([]runjournal.TaskRecord(nil), tasks[1:]...)
		jj.GrokPID = 0
		jj.Generation = b.bootGen
		jj.Host = b.hostname
		jj.BlockedReason = ""
		return nil
	})

	items := make([]taskItem, 0, len(tasks))
	for _, rec := range tasks {
		items = append(items, b.rehydrateTaskItem(rec, proj, threadID))
	}

	b.announceResume(threadID, proj.Name, first.Attempt)

	ctxRun, cancel := context.WithCancel(context.Background())
	job := &runJob{cancel: cancel, start: time.Now(), project: proj.Name}
	claimed, _, err := b.claimOrEnqueueInternal(threadID, job, items[0], true)
	if err != nil || !claimed {
		cancel()
		log.Printf("error: resume claim thread=%s claimed=%v err=%v", threadID, claimed, err)
		return
	}
	for i := 1; i < len(items); i++ {
		_, _, qerr := b.claimOrEnqueueInternal(threadID, &runJob{cancel: func() {}}, items[i], true)
		if qerr != nil {
			log.Printf("error: resume enqueue thread=%s: %v", threadID, qerr)
			break
		}
	}
	log.Printf("resume: re-drive thread=%s project=%s tasks=%d attempt=%d", threadID, proj.Name, len(items), first.Attempt)
	b.drainWG.Add(1)
	go b.drainTaskQueue(ctxRun, cancel, items[0], job)
}

func waitPIDExit(pid int, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !grokrun.ProcessAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (b *Bot) rehydrateTaskItem(rec runjournal.TaskRecord, proj projectRef, threadID string) taskItem {
	if p, ok := b.cfg.ProjectPath(rec.Project); ok && p != "" {
		proj = projectRef{Name: rec.Project, Cwd: p}
	} else if rec.ProjectCwd != "" {
		proj = projectRef{Name: rec.Project, Cwd: rec.ProjectCwd}
	}
	src := rec.Source
	if src == "" {
		src = SourceDiscord
	}
	item := taskItem{
		s:                b.Discord(),
		m:                nil,
		parsed:           Parsed{Kind: KindTask, Prompt: rec.Prompt},
		proj:             proj,
		threadID:         threadID,
		actor:            Actor{ID: rec.Actor.ID, DisplayName: rec.Actor.DisplayName},
		source:           src,
		attachmentPaths:  append([]string(nil), rec.AttachmentPaths...),
		origin:           rec.Origin,
		createdBy:        rec.CreatedBy,
		createdByName:    rec.CreatedByName,
		discordURL:       rec.DiscordURL,
		taskID:           rec.ID,
		attempt:          rec.Attempt,
		referencedPrompt: rec.ReferencedPrompt,
		triggerMsgID:     rec.TriggerMsgID,
	}
	if item.origin == "" {
		item.origin = src
	}
	return item
}

func (b *Bot) healInterruptedStatus(threadID, statusMsgID, project string) {
	s := b.Discord()
	if s == nil || statusMsgID == "" {
		return
	}
	header := fmt.Sprintf("Interrupted · process restarted · **%s**", project)
	if err := discordEditComponents(s, threadID, statusMsgID, header, actionBarDone(threadID), true); err != nil {
		log.Printf("warn: heal status thread=%s: %v", threadID, err)
	}
}

func (b *Bot) announceResume(threadID, project string, attempt int) {
	s := b.Discord()
	if s == nil {
		return
	}
	msg := fmt.Sprintf("%s · **%s**", resumeAnnouncePrefix, project)
	if attempt > 1 {
		msg = fmt.Sprintf("%s · **%s** · attempt %d", resumeAnnouncePrefix, project, attempt)
	}
	if _, err := s.ChannelMessageSend(threadID, msg); err != nil {
		log.Printf("warn: resume announce thread=%s: %v", threadID, err)
	}
}

func (b *Bot) notifyResumeFail(threadID, text string) {
	s := b.Discord()
	if s == nil {
		log.Printf("resume: fail thread=%s: %s", threadID, text)
		return
	}
	if _, err := s.ChannelMessageSend(threadID, text); err != nil {
		log.Printf("warn: resume fail notify thread=%s: %v", threadID, err)
	}
}

// prebindSessionID ensures a durable session id in the journal before grokrun.Run.
// Does NOT write SessionID into sessions.json (post-run save stays as today).
// Returns sessionID and forceNew (-s vs --resume).
func (b *Bot) prebindSessionID(threadID, project string) (sessionID string, forceNew bool) {
	_ = project
	var journalSID string
	if b.runs != nil && b.resumeEnabled() {
		if j, ok, err := b.runs.Load(threadID); err == nil && ok {
			journalSID = strings.TrimSpace(j.SessionID)
		}
	}
	var sessionsSID string
	if e, ok := b.sessions.Get(threadID); ok {
		sessionsSID = strings.TrimSpace(e.SessionID)
	}

	sid := journalSID
	if sid == "" {
		sid = sessionsSID
	}
	if sid == "" {
		sid = grokrun.NewSessionID()
		forceNew = true
	} else if sessionsSID == "" {
		// Journal-only id: cannot verify session exists → -s with same id.
		forceNew = true
	}

	b.patchJournal(threadID, func(j *runjournal.Journal) {
		j.SessionID = sid
	})
	return sid, forceNew
}
