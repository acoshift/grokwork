package bot

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/reviewstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

var mentionUserRE = regexp.MustCompile(`<@!?(\d+)>`)

// handleReview implements @Grok /review @user [optional PR selector].
// Gated by project allowlist (not web prReviews flag).
func (b *Bot) handleReview(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /review @user` inside a Grok thread that has a PR.", ref(m)); err != nil {
			log.Printf("error: reply review-not-thread: %v", err)
		}
		return
	}
	if m.Author == nil {
		return
	}
	if b.reviews == nil {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Review store is unavailable.", ref(m)); err != nil {
			log.Printf("error: reply review-store: %v", err)
		}
		return
	}

	e, ok := b.sessions.Get(m.ChannelID)
	if !ok {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "No session for this thread yet.", ref(m)); err != nil {
			log.Printf("error: reply review-no-session: %v", err)
		}
		return
	}
	e.NormalizePRs()
	if !e.HasAnyPR() {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "This thread has no tracked PR. Open or link one first.", ref(m)); err != nil {
			log.Printf("error: reply review-no-pr: %v", err)
		}
		return
	}

	reviewerID, rest := parseReviewArgs(parsed.Prompt)
	if reviewerID == "" {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, reviewHelpText(), ref(m)); err != nil {
			log.Printf("error: reply review-help: %v", err)
		}
		return
	}

	pr, ok := resolveReviewPR(e, rest)
	if !ok {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Could not resolve that PR on this thread. Try `/review @user #42` or a full PR URL.", ref(m)); err != nil {
			log.Printf("error: reply review-pr: %v", err)
		}
		return
	}
	pr.FillOwnerRepoFromURL()
	if pr.Owner == "" || pr.Repo == "" || pr.Number <= 0 {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Tracked PR is missing owner/repo/number.", ref(m)); err != nil {
			log.Printf("error: reply review-pr-id: %v", err)
		}
		return
	}

	// Reviewer should be on the project allowlist (when known).
	if e.Project != "" && b.cfg != nil && !b.cfg.AccessAllowed(e.Project, reviewerID, nil) {
		// Soft allow if allowlist is role-only (we don't have their roles here) — still request.
		// Fail only when project has user list and reviewer is not on it AND no roles configured.
		// AccessAllowed with empty roles fails closed for users not on list — that's intended.
		if _, err := s.ChannelMessageSendReply(m.ChannelID,
			fmt.Sprintf("<@%s> is not on this project's allowlist.", reviewerID), ref(m)); err != nil {
			log.Printf("error: reply review-allow: %v", err)
		}
		return
	}

	note := strings.TrimSpace(rest)
	// Strip resolved PR selectors from note if they led.
	note = stripPRSelectorPrefix(note)

	req, err := b.reviews.RequestReview(reviewstore.Request{
		Owner:         pr.Owner,
		Repo:          pr.Repo,
		Number:        pr.Number,
		Project:       e.Project,
		ThreadID:      m.ChannelID,
		HeadSHA:       pr.HeadSHA,
		RequesterID:   m.Author.ID,
		RequesterName: m.Author.String(),
		ReviewerID:    reviewerID,
		Note:          note,
	})
	if err != nil {
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, "Could not request review: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply review-save: %v", sendErr)
		}
		return
	}

	// Formal GitHub review request when the reviewer is on the Tier A map.
	// Team store is already saved; GH failure is reported without rolling back.
	cwd := b.prViewCwd(e)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ghLogin, ghErr := requestFormalGitHubReview(ctx, nil, b.cfg, cwd, pr.Owner, pr.Repo, pr.Number, reviewerID)

	msg := formatReviewRequestReply(reviewRequestReply{
		ReviewerID:  reviewerID,
		RequesterID: m.Author.ID,
		Owner:       pr.Owner,
		Repo:        pr.Repo,
		Number:      pr.Number,
		Note:        note,
		PRURL:       b.discordPRURL(pr.Owner, pr.Repo, pr.Number, pr.URL),
		TeamOK:      req.ID != "",
		GitHubLogin: ghLogin,
		GitHubErr:   ghErr,
	})
	if _, err := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); err != nil {
		log.Printf("error: reply review-ok: %v", err)
	}
}

// ResolveMappedGitHubLogin returns the bare GitHub login for a Discord user when
// present in the Tier A map; empty string means unmapped (do not invent @login).
func ResolveMappedGitHubLogin(cfg *config.Config, discordUserID string) string {
	if cfg == nil {
		return ""
	}
	id, ok := cfg.LookupGitHubIdentity(discordUserID)
	if !ok {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(id.Login), "@")
}

// requestFormalGitHubReview requests a formal PR review via host gh when mapped.
// Empty login means skip (unmapped); team store is independent. run may be nil.
func requestFormalGitHubReview(ctx context.Context, run ghpr.Runner, cfg *config.Config, cwd, owner, repo string, number int, discordReviewerID string) (login string, err error) {
	login = ResolveMappedGitHubLogin(cfg, discordReviewerID)
	if login == "" {
		return "", nil
	}
	err = ghpr.RequestReviewersWith(ctx, run, cwd, owner, repo, number, login)
	return login, err
}

type reviewRequestReply struct {
	ReviewerID  string
	RequesterID string
	Owner       string
	Repo        string
	Number      int
	Note        string
	PRURL       string
	TeamOK      bool
	GitHubLogin string // bare login if formal request attempted
	GitHubErr   error
}

func formatReviewRequestReply(r reviewRequestReply) string {
	msg := fmt.Sprintf("<@%s> please review **%s/%s#%d** (requested by <@%s>)",
		r.ReviewerID, r.Owner, r.Repo, r.Number, r.RequesterID)
	if r.Note != "" {
		msg += "\n> " + r.Note
	}
	if r.PRURL != "" {
		msg += "\n" + r.PRURL
	}
	if r.TeamOK {
		msg += "\n_Team review request — appears on web **My reviews**._"
	}
	switch {
	case r.GitHubLogin != "" && r.GitHubErr == nil:
		msg += fmt.Sprintf("\n_Also requested formal GitHub review from @%s._", r.GitHubLogin)
	case r.GitHubLogin != "" && r.GitHubErr != nil:
		msg += fmt.Sprintf("\n⚠️ Team request saved, but GitHub review request for @%s failed: %s", r.GitHubLogin, r.GitHubErr.Error())
	default:
		msg += "\n_No GitHub map for this Discord user — team request only (not a formal GitHub review request). Map them under **Config → GitHub map**._"
	}
	return msg
}

func reviewHelpText() string {
	return "Usage: `@Grok /review @user` · optional PR `#42`, URL, or `owner/repo#n` when the thread has multiple PRs. Mapped users also get a formal GitHub review request."
}

func parseReviewArgs(prompt string) (reviewerID, rest string) {
	text := strings.TrimSpace(prompt)
	for _, prefix := range []string{"/review", "review"} {
		if strings.HasPrefix(strings.ToLower(text), prefix) {
			text = strings.TrimSpace(text[len(prefix):])
			break
		}
	}
	m := mentionUserRE.FindStringSubmatch(text)
	if len(m) < 2 {
		return "", text
	}
	reviewerID = m[1]
	// Remove first mention occurrence.
	rest = strings.TrimSpace(mentionUserRE.ReplaceAllString(text, " "))
	// Collapse spaces
	rest = strings.Join(strings.Fields(rest), " ")
	return reviewerID, rest
}

func resolveReviewPR(e sessionstore.Entry, rest string) (sessionstore.TrackedPR, bool) {
	e.NormalizePRs()
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return e.PrimaryPR()
	}
	// Try FindPR with full rest or first token.
	tokens := strings.Fields(rest)
	candidates := []string{rest}
	if len(tokens) > 0 {
		candidates = append(candidates, tokens[0])
	}
	for _, c := range candidates {
		if pr, ok := e.FindPR(c); ok {
			return pr, true
		}
		// Bare number → #N
		if n, err := strconv.Atoi(strings.TrimPrefix(c, "#")); err == nil && n > 0 {
			if pr, ok := e.FindPR("#" + strconv.Itoa(n)); ok {
				return pr, true
			}
			// Match by number alone across PRs.
			for _, pr := range e.PRs {
				if pr.Number == n {
					return pr, true
				}
			}
		}
	}
	// If rest is only a note (no PR selector), use primary.
	if !looksLikePRSelector(rest) {
		return e.PrimaryPR()
	}
	return sessionstore.TrackedPR{}, false
}

func looksLikePRSelector(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.Contains(s, "github.com/") || strings.Contains(s, "/pull/") {
		return true
	}
	if strings.HasPrefix(s, "#") {
		return true
	}
	if strings.Contains(s, "#") && strings.Contains(s, "/") {
		return true
	}
	if _, err := strconv.Atoi(s); err == nil {
		return true
	}
	return false
}

func stripPRSelectorPrefix(note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return ""
	}
	fields := strings.Fields(note)
	if len(fields) == 0 {
		return note
	}
	if looksLikePRSelector(fields[0]) {
		return strings.TrimSpace(strings.TrimPrefix(note, fields[0]))
	}
	return note
}
