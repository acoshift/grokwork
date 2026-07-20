package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// preserveLabelFields copies lifecycle label when session Set overwrites the entry.
func preserveLabelFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if next.Label == "" {
		next.Label = prev.Label
		next.LabelManual = prev.LabelManual
	}
}

func (b *Bot) handleLabel(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /label` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply label-not-thread: %v", err)
		}
		return
	}
	threadID := m.ChannelID
	arg := parseLabelArg(parsed.Prompt)

	e, ok := b.sessions.Get(threadID)
	if !ok {
		// Shell so label sticks before first task.
		parentID := parentChannelID(s, threadID)
		projName := ""
		if p, err := b.resolveProject(parentID); err == nil {
			projName = p.Name
		}
		e = sessionstore.Entry{Project: projName}
		if m.Author != nil {
			ensureSessionOwner(&e, m.Author.ID, m.Author.String())
		}
	}

	switch {
	case arg == "":
		msg := formatLabelStatus(e)
		if _, err := s.ChannelMessageSendReply(threadID, msg, ref(m)); err != nil {
			log.Printf("error: reply label-status: %v", err)
		}
		return
	case arg == "auto":
		e.ClearLabelManual()
		if err := b.sessions.Set(threadID, e); err != nil {
			if _, sendErr := s.ChannelMessageSendReply(threadID, "Could not save label: "+err.Error(), ref(m)); sendErr != nil {
				log.Printf("error: reply label-save: %v", sendErr)
			}
			return
		}
		msg := fmt.Sprintf("Label auto-enabled → **%s**.", sessionstore.DisplayLabel(e.EffectiveLabel()))
		if _, err := s.ChannelMessageSendReply(threadID, msg, ref(m)); err != nil {
			log.Printf("error: reply label-auto: %v", err)
		}
		b.maybeRefreshBriefLabel(s, threadID)
		return
	case arg == "help" || arg == "?":
		if _, err := s.ChannelMessageSendReply(threadID, labelHelpText(), ref(m)); err != nil {
			log.Printf("error: reply label-help: %v", err)
		}
		return
	}

	lab, okLab := sessionstore.ParseLabel(arg)
	if !okLab {
		if _, err := s.ChannelMessageSendReply(threadID,
			fmt.Sprintf("Unknown label `%s`. %s", arg, labelHelpText()), ref(m)); err != nil {
			log.Printf("error: reply label-unknown: %v", err)
		}
		return
	}
	if err := e.SetLabelManual(lab); err != nil {
		if _, sendErr := s.ChannelMessageSendReply(threadID, err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply label-set: %v", sendErr)
		}
		return
	}
	if m.Author != nil {
		ensureSessionOwner(&e, m.Author.ID, m.Author.String())
	}
	if e.Project == "" {
		parentID := parentChannelID(s, threadID)
		if p, err := b.resolveProject(parentID); err == nil {
			e.Project = p.Name
		}
	}
	if err := b.sessions.Set(threadID, e); err != nil {
		if _, sendErr := s.ChannelMessageSendReply(threadID, "Could not save label: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply label-save: %v", sendErr)
		}
		return
	}
	msg := fmt.Sprintf("Label set to **%s** (manual — auto paused until `@Grok /label auto`).", sessionstore.DisplayLabel(lab))
	if _, err := s.ChannelMessageSendReply(threadID, msg, ref(m)); err != nil {
		log.Printf("error: reply label-ok: %v", err)
	}
	b.maybeRefreshBriefLabel(s, threadID)
}

func parseLabelArg(prompt string) string {
	text := strings.TrimSpace(prompt)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	for _, prefix := range []string{"/label", "label"} {
		if lower == prefix {
			return ""
		}
		if strings.HasPrefix(lower, prefix+" ") {
			return strings.TrimSpace(text[len(prefix):])
		}
	}
	return strings.TrimSpace(text)
}

func formatLabelStatus(e sessionstore.Entry) string {
	lab := sessionstore.DisplayLabel(e.EffectiveLabel())
	mode := "auto"
	if e.LabelManual {
		mode = "manual"
	}
	return fmt.Sprintf("**label:** %s (%s)\n%s", lab, mode, labelHelpText())
}

func labelHelpText() string {
	return "Set: `@Grok /label <open|in_progress|blocked|needs_review|done|abandoned>` · `@Grok /label auto` · aliases: wip, ready, review, close"
}

func (b *Bot) maybeRefreshBriefLabel(s *discordgo.Session, threadID string) {
	if b == nil || s == nil || threadID == "" {
		return
	}
	e, ok := b.sessions.Get(threadID)
	if !ok || e.BriefMsgID == "" {
		return
	}
	if _, err := b.refreshBriefCard(s, threadID, ""); err != nil {
		log.Printf("label: brief refresh thread=%s: %v", threadID, err)
	}
}

// applyAutoLabelOnRunStart promotes open → in_progress when a task starts.
func (b *Bot) applyAutoLabelOnRunStart(threadID, project string, actor Actor) {
	if b == nil || b.sessions == nil || threadID == "" {
		return
	}
	if _, ok, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		if ent.Project == "" {
			ent.Project = project
		}
		if actor.ID != "" {
			ensureSessionOwner(ent, actor.ID, actor.String())
		}
		ent.ApplyAutoLabelOnRunStart()
	}); err != nil {
		log.Printf("label: run-start thread=%s: %v", threadID, err)
	} else if !ok {
		e := sessionstore.Entry{Project: project, Label: sessionstore.LabelInProgress}
		if actor.ID != "" {
			ensureSessionOwner(&e, actor.ID, actor.String())
		}
		if err := b.sessions.Set(threadID, e); err != nil {
			log.Printf("label: create on run-start thread=%s: %v", threadID, err)
		}
	}
}
