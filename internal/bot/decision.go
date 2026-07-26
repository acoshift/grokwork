package bot

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// DECISION:
// id: q1
// prompt: Should we bump the API timeout to 30s?
// options: Yes|No|Need more data

var (
	decisionBlockRE = regexp.MustCompile(`(?is)DECISION:\s*\n((?:[^\n]*\n){1,12})`)
	decisionFieldRE = regexp.MustCompile(`(?im)^\s*(id|prompt|options)\s*:\s*(.+?)\s*$`)
)

type decisionSpec struct {
	ID      string
	Prompt  string
	Options []string
}

// parseDecisionBlocks extracts DECISION blocks from model output.
func parseDecisionBlocks(text string) []decisionSpec {
	matches := decisionBlockRE.FindAllStringSubmatch(text, 5)
	if len(matches) == 0 {
		return nil
	}
	var out []decisionSpec
	for _, m := range matches {
		body := m[1]
		var d decisionSpec
		for _, fm := range decisionFieldRE.FindAllStringSubmatch(body, -1) {
			key := strings.ToLower(fm[1])
			val := strings.TrimSpace(fm[2])
			switch key {
			case "id":
				d.ID = sanitizeDecisionID(val)
			case "prompt":
				d.Prompt = val
			case "options":
				for _, opt := range strings.Split(val, "|") {
					opt = strings.TrimSpace(opt)
					if opt != "" {
						d.Options = append(d.Options, opt)
					}
				}
			}
		}
		if d.Prompt == "" {
			continue
		}
		if d.ID == "" {
			d.ID = fmt.Sprintf("q%d", len(out)+1)
		}
		if len(d.Options) == 0 {
			d.Options = []string{"Yes", "No"}
		}
		if len(d.Options) > 5 {
			d.Options = d.Options[:5]
		}
		out = append(out, d)
	}
	return out
}

func sanitizeDecisionID(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// postDecisionCards posts Discord buttons for each decision and stores OpenQuestions.
func (b *Bot) postDecisionCards(s *discordgo.Session, threadID string, specs []decisionSpec) {
	if s == nil || len(specs) == 0 {
		return
	}
	for _, d := range specs {
		// Cap open questions
		if e, ok := b.sessions.Get(threadID); ok && len(e.OpenQuestions) >= sessionstore.MaxOpenQuestions {
			sendChunks(s, threadID, "Too many open questions — answer or dismiss some before new decisions.")
			return
		}
		q := sessionstore.OpenQuestion{
			ID:      d.ID,
			Text:    d.Prompt,
			Status:  "open",
			AskedAt: time.Now().UTC().Format(time.RFC3339),
			Options: append([]string(nil), d.Options...),
		}
		_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
			// replace same id if re-asked
			found := false
			for i := range ent.OpenQuestions {
				if ent.OpenQuestions[i].ID == q.ID {
					ent.OpenQuestions[i] = q
					found = true
					break
				}
			}
			if !found {
				if len(ent.OpenQuestions) >= sessionstore.MaxOpenQuestions {
					return
				}
				ent.OpenQuestions = append(ent.OpenQuestions, q)
			}
			_ = sessionstore.ClampWave2Fields(ent)
		})
		if err != nil {
			log.Printf("decision: patch: %v", err)
			continue
		}
		content := fmt.Sprintf("**Decision** `%s`\n%s", d.ID, d.Prompt)
		comps := decisionButtons(threadID, d)
		if _, err := s.ChannelMessageSendComplex(threadID, &discordgo.MessageSend{
			Content:    sanitizeDiscordContent(content),
			Components: comps,
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		}); err != nil {
			log.Printf("error: decision card: %v", err)
		}
	}
}

// decision custom_id: gd:d:<threadID>:<qid>:<optIndex>
const decisionPrefix = "gd:d:"

func decisionButtons(threadID string, d decisionSpec) []discordgo.MessageComponent {
	var row []discordgo.MessageComponent
	for i, opt := range d.Options {
		label := opt
		if len([]rune(label)) > 80 {
			label = string([]rune(label)[:77]) + "…"
		}
		row = append(row, discordgo.Button{
			Label:    label,
			Style:    discordgo.SecondaryButton,
			CustomID: fmt.Sprintf("%s%s:%s:%d", decisionPrefix, threadID, d.ID, i),
		})
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: row},
	}
}

func parseDecisionCustomID(id string) (threadID, qid string, optIdx int, ok bool) {
	if !strings.HasPrefix(id, decisionPrefix) {
		return "", "", 0, false
	}
	rest := strings.TrimPrefix(id, decisionPrefix)
	// threadID:qid:idx — threadIDs are numeric snowflakes (no colons)
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return "", "", 0, false
	}
	threadID, qid = parts[0], parts[1]
	if threadID == "" || qid == "" {
		return "", "", 0, false
	}
	n := 0
	for _, r := range parts[2] {
		if r < '0' || r > '9' {
			return "", "", 0, false
		}
		n = n*10 + int(r-'0')
	}
	return threadID, qid, n, true
}

func (b *Bot) handleDecisionClick(s *discordgo.Session, i *discordgo.InteractionCreate, threadID, qid string, optIdx int, user *discordgo.User) {
	e, ok := b.sessions.Get(threadID)
	if !ok {
		respondEphemeral(s, i, "No session for this thread.")
		return
	}
	var answer string
	var prompt string
	found := false
	for _, q := range e.OpenQuestions {
		if q.ID != qid {
			continue
		}
		found = true
		prompt = q.Text
		if q.Status == "answered" {
			respondEphemeral(s, i, "Already answered: "+q.Answer)
			return
		}
		if optIdx < 0 || optIdx >= len(q.Options) {
			respondEphemeral(s, i, "Unknown option.")
			return
		}
		answer = q.Options[optIdx]
		break
	}
	if !found {
		respondEphemeral(s, i, "Decision not found (may have expired).")
		return
	}
	_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		for i := range ent.OpenQuestions {
			if ent.OpenQuestions[i].ID == qid {
				ent.OpenQuestions[i].Status = "answered"
				ent.OpenQuestions[i].Answer = answer
				break
			}
		}
	})
	if err != nil {
		respondEphemeral(s, i, "Could not save answer: "+err.Error())
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("Recorded **%s** for `%s`.", answer, qid))

	// Queue a follow-up so the model sees the decision (builder freeform).
	follow := fmt.Sprintf(
		"Human answered decision `%s`.\nQuestion: %s\nAnswer: %s\nContinue with this choice.",
		qid, prompt, answer,
	)
	actor := ActorFromUser(user)
	parentID := parentChannelID(s, threadID)
	proj, err := b.resolveProject(parentID)
	if err != nil {
		// fall back to session project
		if e.Project != "" {
			if path, ok := b.cfg.ProjectPath(e.Project); ok {
				proj = projectRef{Name: e.Project, Cwd: path}
				err = nil
			}
		}
	}
	if err != nil {
		log.Printf("decision: project: %v", err)
		return
	}
	_, startErr := b.StartContinue(ContinueOpts{
		ThreadID: threadID,
		Project:  proj.Name,
		Prompt:   follow,
		Actor:    actor,
	})
	// A decision click queues a model run like any other dispatch, so it is a
	// session.start. The question id is recorded, never the option label — the model
	// wrote those and they can quote a customer.
	b.auditCmd(audit.ActionSessionStart, actor, threadID, proj.Name, startErr, map[string]any{
		"origin": auditOriginDecision, "kind": "task", "questionId": qid, "via": "button",
	})
}
