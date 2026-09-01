package web

import (
	"fmt"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
)

// updateDisabledModels persists per-model and bulk enable/disable of curated
// names. Bulk expands onto the same denylist as a per-model toggle, so
// re-enabling one Claude model after "disable all Claude" is a single name
// removal rather than fighting a second flag.
func (s *Server) updateDisabledModels(ctx *hime.Context) error {
	action := strings.TrimSpace(ctx.PostFormValue("action"))
	model := strings.TrimSpace(ctx.PostFormValue("model"))
	agent := strings.TrimSpace(ctx.PostFormValue("agent"))
	family := strings.TrimSpace(ctx.PostFormValue("family"))

	var (
		err error
		msg string
	)
	switch action {
	case "disable":
		err = s.cfg.SetModelDisabled(model, true)
		msg = fmt.Sprintf("Disabled %s", model)
	case "enable":
		err = s.cfg.SetModelDisabled(model, false)
		msg = fmt.Sprintf("Enabled %s", model)
	case "disable-agent":
		err = s.cfg.SetAgentModelsDisabled(agent, true)
		msg = fmt.Sprintf("Disabled all %s models", agent)
	case "enable-agent":
		err = s.cfg.SetAgentModelsDisabled(agent, false)
		msg = fmt.Sprintf("Enabled all %s models", agent)
	case "disable-family":
		err = s.cfg.SetFamilyModelsDisabled(agent, family, true)
		msg = fmt.Sprintf("Disabled %s %s models", agent, family)
	case "enable-family":
		err = s.cfg.SetFamilyModelsDisabled(agent, family, false)
		msg = fmt.Sprintf("Enabled %s %s models", agent, family)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	detail := map[string]any{"section": "disabledModels", "action": action}
	if model != "" {
		detail["model"] = model
	}
	if agent != "" {
		detail["agent"] = agent
	}
	if family != "" {
		detail["family"] = family
	}
	s.auditAction(ctx, audit.ActionConfigSettings, err, detail)
	if err != nil {
		return s.configPageRedirect(ctx, "config.models", "", err)
	}
	return s.configPageRedirect(ctx, "config.models", msg, nil)
}
