package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/apitoken"
	"github.com/acoshift/grokwork/internal/audit"
)

// requireTokenAdmin is requireAdmin plus a hard refuse when web auth is off
// (requireAdmin no-ops on open LAN).
func (s *Server) requireTokenAdmin(next http.Handler) http.Handler {
	return s.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg == nil || !s.cfg.WebAuthEnabled() {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) apiTokensPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "API tokens · Config"
	d.IsConfig = true
	d.APIEnabled = s.cfg.APIEnabled()
	d.APIProjects = s.cfg.ProjectNames()
	if s.apiTokens != nil {
		d.APITokens = s.apiTokens.List()
	}
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.viewPage(ctx, "config_api_tokens", d)
}

func (s *Server) postAPITokenMint(ctx *hime.Context) error {
	if s.apiTokens == nil {
		return ctx.Redirect("/config/api-tokens?err=token+store+unavailable")
	}
	label := strings.TrimSpace(ctx.PostFormValue("label"))
	if label == "" {
		label = "token"
	}
	_ = ctx.Request.ParseForm()
	projects := ctx.Request.PostForm["project"]
	caps := apitoken.CapsMask{StartSessions: true}
	if ctx.PostFormValue("githubWrites") == "1" {
		caps.GithubWrites = true
	}
	actor, _ := s.auditActor(ctx)
	wire, rec, err := s.apiTokens.Mint(apitoken.MintOpts{
		Label:     label,
		Projects:  projects,
		Caps:      caps,
		CreatedBy: actor,
	})
	s.auditAction(ctx, audit.ActionAPITokenMint, err, map[string]any{
		"tokenId": rec.ID, "label": label, "projects": rec.Projects,
	})
	if err != nil {
		return ctx.Redirect("/config/api-tokens?err=" + url.QueryEscape(err.Error()))
	}
	d := s.basePage(ctx)
	d.Title = "API tokens · Config"
	d.IsConfig = true
	d.APIEnabled = s.cfg.APIEnabled()
	d.APIProjects = s.cfg.ProjectNames()
	d.APITokens = s.apiTokens.List()
	d.APITokenSecret = wire
	d.Flash = fmt.Sprintf("Minted %s — copy the secret now; it will not be shown again. Add %s to a project team with an explicit capability template.", rec.ID, rec.ActorID)
	return s.viewPage(ctx, "config_api_tokens", d)
}

func (s *Server) postAPITokenRevoke(ctx *hime.Context) error {
	id := strings.TrimSpace(ctx.PostFormValue("id"))
	var err error
	if s.apiTokens == nil {
		err = fmt.Errorf("token store unavailable")
	} else {
		err = s.apiTokens.Revoke(id)
	}
	s.auditAction(ctx, audit.ActionAPITokenRevoke, err, map[string]any{"tokenId": id})
	if err != nil {
		return ctx.Redirect("/config/api-tokens?err=" + url.QueryEscape(err.Error()))
	}
	return ctx.Redirect("/config/api-tokens?ok=Token+revoked")
}
