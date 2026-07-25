package web

import (
	"strings"

	"github.com/acoshift/grokwork/internal/deploy"
)

// deployNotifier adapts the bot's Discord surface and the inbox to the
// engine's Notifier seam, so internal/deploy needs no Discord dependency.
type deployNotifier struct{ s *Server }

// SendChannel posts to the project's configured Discord channel. It returns an
// error when there is no channel, which is the engine's signal to fall back.
func (n deployNotifier) SendChannel(project, content string) error {
	return n.s.bot.SendProjectMessage(project, content)
}

// SendInbox appends to one actor's feed.
func (n deployNotifier) SendInbox(actorID, subject, body, url, project string) error {
	return n.s.bot.AppendInbox(actorID, "deploy.done", subject, body, url, project)
}

// wireDeployNotifier installs the notifier and the run-link base.
func (s *Server) wireDeployNotifier() {
	if s.bot == nil || s.deploys == nil {
		return
	}
	s.deploys.SetNotifier(deployNotifier{s: s})
	s.deploys.SetPublicBaseURL(strings.TrimSpace(s.cfg.WebPublicBaseURLValue()))
}

var _ deploy.Notifier = deployNotifier{}
