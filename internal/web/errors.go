package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/errsrc/deploys"
	"github.com/acoshift/grokwork/internal/errsrc/gcperr"
	sentrycli "github.com/acoshift/grokwork/internal/errsrc/sentry"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const (
	errorListCap           = 50
	errorSourceDisabled    = "Error source not enabled"
	errorNotFound          = "Error not found"
	errorDeploysLocator    = "location and name are required"
	errorGrokBannerBuilder = "This investigate run is on Grok, which cannot attach grokwork MCP (`--deny MCPTool`). It will not fetch the sample stack. Pick Claude for this session, or use **Fix with Grok** (ship/fix attaches MCP)."
	errorGrokBannerOther   = "This investigate run is on Grok, which cannot attach grokwork MCP. It will not fetch the sample stack. Ask a builder to start this session on Claude, or to Fix it."
)

func (s *Server) deploysClient(project string) *deploys.Client {
	opts := deploys.Options{
		Token: s.cfg.ProjectDeploysAPIToken(project),
	}
	opts.BasicUser, opts.BasicPass = s.cfg.ProjectDeploysBasicAuth(project)
	if s.deploysNew != nil {
		return s.deploysNew(opts)
	}
	return deploys.New(opts)
}

func (s *Server) sentryClient(project string) *sentrycli.Client {
	tok := s.cfg.ProjectSentryAuthToken(project)
	org, proj, base := "", "", ""
	if c := s.cfg.ProjectSentry(project); c != nil {
		org, proj, base = c.Org, c.Project, c.BaseURL
	}
	if s.sentryNew != nil {
		return s.sentryNew(tok, org, proj, base)
	}
	return sentrycli.New(tok, org, proj, base)
}

func (s *Server) gcpClient(project string) *gcperr.Client {
	if s.gcpNew != nil {
		return s.gcpNew(project)
	}
	c := s.cfg.ProjectGCPErrors(project)
	if c == nil {
		return &gcperr.Client{}
	}
	return &gcperr.Client{
		ProjectID: c.ProjectID,
		Service:   c.Service,
		Tokens:    gcperr.TokenSourceFor(c.CredentialsFile),
	}
}

func (s *Server) errorsLanding(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	src := strings.ToLower(strings.TrimSpace(ctx.FormValue("src")))
	tabs := s.errorTabs(project, src)
	if src == "" {
		switch len(tabs) {
		case 0:
			return s.errorsExplainer(ctx, project, "", tabs, "No error source is enabled. Configure one on the project Integrations tab.")
		case 1:
			return ctx.Redirect("/projects/" + url.PathEscape(project) + "/errors?src=" + url.QueryEscape(tabs[0].Src))
		default:
			src = tabs[0].Src
			tabs = s.errorTabs(project, src)
		}
	}
	if !validErrorSrc(src) || !s.errorSrcEnabled(project, src) {
		return ctx.Status(http.StatusNotFound).Error(errorSourceDisabled)
	}
	return s.errorsList(ctx, project, src, tabs)
}

func (s *Server) errorsList(ctx *hime.Context, project, src string, tabs []ErrorTab) error {
	d := s.basePage(ctx)
	d.Title = "Errors · " + project
	d.IsErrors = true
	d.Project = project
	d.ErrorSrc = src
	d.ErrorTabs = tabs
	d.ErrorStatus = strings.TrimSpace(ctx.FormValue("status"))
	d.ErrorSort = strings.TrimSpace(ctx.FormValue("sort"))
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}

	switch src {
	case errsrc.ProviderDeploys:
		if !s.cfg.ProjectDeploysErrorsCanResolve(project) {
			d.Error = "deploys.app is enabled but no API token (config or DEPLOYS_API_TOKEN_<PROJECT> — not DEPLOYS_TOKEN) or Basic auth (DEPLOYS_AUTH_USER/PASS)."
			return s.viewPage(ctx, "errors", d)
		}
		cfg := s.cfg.ProjectDeploysErrors(project)
		if cfg == nil || strings.TrimSpace(cfg.Project) == "" {
			d.Error = "deploys.app is enabled but no deploys.app project is configured."
			return s.viewPage(ctx, "errors", d)
		}
		limit := errorListCap
		res, listErr := s.deploysClient(project).List(ctx.Context(), deploys.ListReq{
			Project:  cfg.Project,
			Location: strings.TrimSpace(ctx.FormValue("location")),
			Name:     strings.TrimSpace(ctx.FormValue("name")),
			Status:   d.ErrorStatus,
			Sort:     d.ErrorSort,
			Limit:    limit,
			Cursor:   strings.TrimSpace(ctx.FormValue("cursor")),
		})
		if listErr != nil {
			if d.Error == "" {
				d.Error = listErr.Error()
			}
			return s.viewPage(ctx, "errors", d)
		}
		d.ErrorGroups = res.Groups
		d.ErrorNextCursor = res.NextCursor
		if len(d.ErrorGroups) >= limit || res.NextCursor != "" {
			if len(d.ErrorGroups) > limit {
				d.ErrorGroups = d.ErrorGroups[:limit]
			}
			d.ErrorClipped = true
		}
	case errsrc.ProviderSentry:
		if !s.cfg.ProjectSentryCanResolve(project) {
			d.Error = "Sentry is enabled but no auth token (config or SENTRY_AUTH_TOKEN_<PROJECT>)."
			return s.viewPage(ctx, "errors", d)
		}
		cfg := s.cfg.ProjectSentry(project)
		if cfg == nil || cfg.Org == "" || cfg.Project == "" {
			d.Error = "Sentry is enabled but org/project is not configured."
			return s.viewPage(ctx, "errors", d)
		}
		res, listErr := s.sentryClient(project).List(ctx.Context(), errsrc.ListQuery{
			Status: d.ErrorStatus, Sort: d.ErrorSort, Limit: errorListCap,
			Cursor: strings.TrimSpace(ctx.FormValue("cursor")),
		})
		if listErr != nil {
			d.Error = listErr.Error()
			return s.viewPage(ctx, "errors", d)
		}
		d.ErrorGroups = res.Groups
		d.ErrorNextCursor = res.NextCursor
		if len(d.ErrorGroups) >= errorListCap || res.NextCursor != "" {
			if len(d.ErrorGroups) > errorListCap {
				d.ErrorGroups = d.ErrorGroups[:errorListCap]
			}
			d.ErrorClipped = true
		}
	case errsrc.ProviderGCP:
		if !s.cfg.ProjectGCPErrorsCanResolve(project) {
			d.Error = "GCP Error Reporting is enabled but no project id is configured."
			return s.viewPage(ctx, "errors", d)
		}
		period := strings.TrimSpace(ctx.FormValue("period"))
		res, listErr := s.gcpClient(project).List(ctx.Context(), errsrc.ListQuery{
			TimeRange: period, Sort: d.ErrorSort, Limit: errorListCap,
			Cursor:  strings.TrimSpace(ctx.FormValue("cursor")),
			Service: strings.TrimSpace(ctx.FormValue("service")),
		})
		if listErr != nil {
			d.Error = listErr.Error()
			return s.viewPage(ctx, "errors", d)
		}
		d.ErrorGroups = res.Groups
		d.ErrorNextCursor = res.NextCursor
		if len(d.ErrorGroups) >= errorListCap || res.NextCursor != "" {
			if len(d.ErrorGroups) > errorListCap {
				d.ErrorGroups = d.ErrorGroups[:errorListCap]
			}
			d.ErrorClipped = true
		}
	default:
		d.Error = "This error source is enabled but not yet available on this host."
	}
	return s.viewPage(ctx, "errors", d)
}

func (s *Server) errorsExplainer(ctx *hime.Context, project, src string, tabs []ErrorTab, msg string) error {
	d := s.basePage(ctx)
	d.Title = "Errors · " + project
	d.IsErrors = true
	d.Project = project
	d.ErrorSrc = src
	d.ErrorTabs = tabs
	d.Error = msg
	return s.viewPage(ctx, "errors", d)
}

func (s *Server) errorDetail(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	src := strings.ToLower(strings.TrimSpace(ctx.PathValue("src")))
	id := strings.TrimSpace(ctx.PathValue("id"))
	if !validErrorSrc(src) || !s.errorSrcEnabled(project, src) {
		return ctx.Status(http.StatusNotFound).Error(errorSourceDisabled)
	}
	if id == "" {
		return ctx.Status(http.StatusNotFound).Error(errorNotFound)
	}

	d := s.basePage(ctx)
	d.Title = id + " · " + project
	d.IsErrors = true
	d.Project = project
	d.ErrorSrc = src
	d.ErrorTabs = s.errorTabs(project, src)
	d.ErrorLocation = strings.TrimSpace(ctx.FormValue("location"))
	d.ErrorName = strings.TrimSpace(ctx.FormValue("name"))
	d.ErrorDetail.ID = id
	d.ErrorDetail.Location = d.ErrorLocation
	d.ErrorDetail.Resource = d.ErrorName
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	d.ShowFixPicker = ctx.FormValue("picker") == "1"
	d.CanStartSession = s.canStartSession(d)
	d.CanStartFixMode = d.CanStartSession && s.cfg.ResolveCapabilities(project, d.UserID).CanShip()

	switch src {
	case errsrc.ProviderDeploys:
		if d.ErrorLocation == "" || d.ErrorName == "" {
			return ctx.Status(http.StatusBadRequest).Error(errorDeploysLocator)
		}
		if !s.cfg.ProjectDeploysErrorsCanResolve(project) {
			d.Error = "deploys.app is enabled but no API token or Basic auth."
			s.attachErrorSessions(&d, project, src, id, d.ErrorLocation, d.ErrorName)
			s.attachModelPicker(&d, project, s.cfg.TaskModel())
			s.attachErrorGrokBanner(&d, project, "")
			return s.viewPage(ctx, "error_detail", d)
		}
		cfg := s.cfg.ProjectDeploysErrors(project)
		depProject := ""
		if cfg != nil {
			depProject = cfg.Project
		}
		if depProject == "" {
			d.Error = "deploys.app is enabled but no deploys.app project is configured."
			s.attachErrorSessions(&d, project, src, id, d.ErrorLocation, d.ErrorName)
			s.attachModelPicker(&d, project, s.cfg.TaskModel())
			s.attachErrorGrokBanner(&d, project, "")
			return s.viewPage(ctx, "error_detail", d)
		}
		got, err := s.deploysClient(project).Get(ctx.Context(), deploys.GetReq{
			Project:  depProject,
			Location: d.ErrorLocation,
			Name:     d.ErrorName,
			ID:       id,
		})
		if err != nil {
			return ctx.Status(http.StatusNotFound).Error(errorNotFound)
		}
		d.ErrorDetail = got
		d.Title = firstNonEmpty(got.Title, id) + " · " + project
	case errsrc.ProviderSentry:
		if !s.cfg.ProjectSentryCanResolve(project) {
			d.Error = "Sentry is enabled but no auth token."
			s.attachErrorSessions(&d, project, src, id, "", "")
			s.attachModelPicker(&d, project, s.cfg.TaskModel())
			s.attachErrorGrokBanner(&d, project, "")
			return s.viewPage(ctx, "error_detail", d)
		}
		got, slug, err := s.sentryClient(project).Get(ctx.Context(), id)
		if err != nil {
			return ctx.Status(http.StatusNotFound).Error(errorNotFound)
		}
		cfg := s.cfg.ProjectSentry(project)
		want := ""
		if cfg != nil {
			want = cfg.Project
		}
		if !sentrycli.MatchProject(slug, want) {
			return ctx.Status(http.StatusNotFound).Error(errorNotFound)
		}
		d.ErrorDetail = got
		d.Title = firstNonEmpty(got.Title, got.ShortID, id) + " · " + project
	case errsrc.ProviderGCP:
		if !s.cfg.ProjectGCPErrorsCanResolve(project) {
			d.Error = "GCP Error Reporting is enabled but no project id is configured."
			s.attachErrorSessions(&d, project, src, id, "", "")
			s.attachModelPicker(&d, project, s.cfg.TaskModel())
			s.attachErrorGrokBanner(&d, project, "")
			return s.viewPage(ctx, "error_detail", d)
		}
		period := strings.TrimSpace(ctx.FormValue("period"))
		got, err := s.gcpClient(project).Get(ctx.Context(), id, period)
		if err != nil {
			return ctx.Status(http.StatusNotFound).Error(errorNotFound)
		}
		d.ErrorDetail = got
		d.Title = firstNonEmpty(got.Title, id) + " · " + project
	default:
		d.Error = "This error source is enabled but not yet available on this host."
	}

	s.attachErrorSessions(&d, project, src, id, d.ErrorLocation, d.ErrorName)
	if d.ShowFixPicker || len(d.FixHits) > 1 {
		d.ShowFixPicker = true
	}
	s.attachModelPicker(&d, project, s.cfg.TaskModel())
	reuse := ""
	if len(d.FixHits) == 1 {
		reuse = d.FixHits[0].ThreadID
	}
	s.attachErrorGrokBanner(&d, project, reuse)
	return s.viewPage(ctx, "error_detail", d)
}

func (s *Server) postErrorInvestigate(ctx *hime.Context) error {
	return s.postErrorStart(ctx, bot.ErrorIntentInvestigate)
}

func (s *Server) postErrorFix(ctx *hime.Context) error {
	return s.postErrorStart(ctx, bot.ErrorIntentFix)
}

func (s *Server) postErrorStart(ctx *hime.Context, intent bot.ErrorIntent) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	src := strings.ToLower(strings.TrimSpace(ctx.PathValue("src")))
	id := strings.TrimSpace(ctx.PathValue("id"))
	if !validErrorSrc(src) || !s.errorSrcEnabled(project, src) {
		return ctx.Status(http.StatusNotFound).Error(errorSourceDisabled)
	}
	if id == "" {
		return ctx.Status(http.StatusNotFound).Error(errorNotFound)
	}
	loc := strings.TrimSpace(ctx.PostFormValue("location"))
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	if src == errsrc.ProviderDeploys && (loc == "" || name == "") {
		return ctx.Status(http.StatusBadRequest).Error(errorDeploysLocator)
	}
	if s.bot == nil {
		return ctx.Status(http.StatusServiceUnavailable).Error("bot unavailable")
	}
	if err := s.checkStartRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"project": project, "provider": src, "errorId": id, "intent": string(intent),
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	opts := bot.ErrorStartOpts{
		Provider: src,
		Intent:   intent,
		Project:  project,
		Actor:    s.fixActor(ctx),
		ForceNew: formBool(ctx.PostFormValue("force_new")),
		ThreadID: strings.TrimSpace(ctx.PostFormValue("thread_id")),
		ID:       id,
		Location: loc,
		Resource: name,
		Model:    strings.TrimSpace(ctx.PostFormValue("model")),
	}
	if err := s.fillErrorStartFromProvider(ctx, project, src, &opts); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"project": project, "provider": src, "errorId": id, "intent": string(intent),
		})
		q := url.Values{"err": {err.Error()}}
		if loc != "" {
			q.Set("location", loc)
		}
		if name != "" {
			q.Set("name", name)
		}
		return ctx.Redirect(errorDetailPath(project, src, id, q))
	}

	res, startErr := s.bot.StartError(opts)
	detail := map[string]any{
		"project": project, "provider": src, "errorId": id, "intent": string(intent),
		"threadId": res.ThreadID, "status": string(res.Status),
		"queuePos": res.QueuePos, "created": res.Created,
	}
	if errors.Is(startErr, bot.ErrPickerRequired) {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		q := url.Values{"picker": {"1"}, "err": {"Multiple sessions bind this error — pick one or force a new thread."}}
		if loc != "" {
			q.Set("location", loc)
		}
		if name != "" {
			q.Set("name", name)
		}
		return ctx.Redirect(errorDetailPath(project, src, id, q))
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		if errors.Is(startErr, bot.ErrCannotStartFix) || errors.Is(startErr, bot.ErrCannotSelectModel) {
			return ctx.Status(http.StatusForbidden).Error(startErr.Error())
		}
		q := url.Values{"err": {startErr.Error()}}
		if loc != "" {
			q.Set("location", loc)
		}
		if name != "" {
			q.Set("name", name)
		}
		return ctx.Redirect(errorDetailPath(project, src, id, q))
	}
	s.auditAction(ctx, audit.ActionSessionStart, nil, detail)
	ok := fixStatusFlash(res.Status)
	if res.DiscordOffline {
		ok = ok + "&discord=offline"
	}
	return s.sessionRedirect(ctx, res.ThreadID, ok, "")
}

func (s *Server) fillErrorStartFromProvider(ctx *hime.Context, project, src string, opts *bot.ErrorStartOpts) error {
	switch src {
	case errsrc.ProviderDeploys:
		if !s.cfg.ProjectDeploysErrorsCanResolve(project) {
			return nil
		}
		cfg := s.cfg.ProjectDeploysErrors(project)
		if cfg == nil || strings.TrimSpace(cfg.Project) == "" {
			return nil
		}
		got, err := s.deploysClient(project).Get(ctx.Context(), deploys.GetReq{
			Project: cfg.Project, Location: opts.Location, Name: opts.Resource, ID: opts.ID,
		})
		if err != nil {
			return err
		}
		fillErrorOptsFromGroup(opts, got)
	case errsrc.ProviderSentry:
		if !s.cfg.ProjectSentryCanResolve(project) {
			return nil
		}
		got, slug, err := s.sentryClient(project).Get(ctx.Context(), opts.ID)
		if err != nil {
			return err
		}
		cfg := s.cfg.ProjectSentry(project)
		want := ""
		if cfg != nil {
			want = cfg.Project
		}
		if !sentrycli.MatchProject(slug, want) {
			return fmt.Errorf(errorNotFound)
		}
		fillErrorOptsFromGroup(opts, got)
		opts.ShortID = got.ShortID
	case errsrc.ProviderGCP:
		if !s.cfg.ProjectGCPErrorsCanResolve(project) {
			return nil
		}
		got, err := s.gcpClient(project).Get(ctx.Context(), opts.ID, strings.TrimSpace(ctx.PostFormValue("period")))
		if err != nil {
			return err
		}
		fillErrorOptsFromGroup(opts, got)
	}
	return nil
}

func fillErrorOptsFromGroup(opts *bot.ErrorStartOpts, got errsrc.GroupDetail) {
	if opts == nil {
		return
	}
	opts.Title = got.Title
	opts.URL = got.URL
	opts.Status = got.Status
	opts.Fingerprint = got.Fingerprint
	opts.Count = got.Count
	opts.LastSeen = got.LastSeen
	if got.Location != "" {
		opts.Location = got.Location
	}
	if got.Resource != "" {
		opts.Resource = got.Resource
	}
}

func (s *Server) attachErrorSessions(d *pageData, project, src, id, location, resource string) {
	if d == nil || s.bot == nil {
		return
	}
	d.FixHits = s.bot.FindByError(project, src, id, location, resource)
	d.IssueSessions = d.FixHits
}

func (s *Server) attachErrorGrokBanner(d *pageData, project, reuseThread string) {
	if d == nil || !s.errorInvestigateOnGrok(project, reuseThread) {
		return
	}
	d.ErrorGrokBanner = true
	d.ErrorGrokBannerCopy = errorGrokBannerCopy(s.cfg.ResolveCapabilities(project, d.UserID).CanShip())
}

func (s *Server) errorInvestigateOnGrok(project, reuseThread string) bool {
	if reuseThread != "" && s.bot != nil {
		return s.bot.ThreadAgent(reuseThread) == grokrun.AgentGrok
	}
	if s.cfg == nil {
		return true
	}
	return s.cfg.ResolveAgentCLI("").Agent.Resolve() == grokrun.AgentGrok
}

func errorGrokBannerCopy(canShip bool) string {
	if canShip {
		return errorGrokBannerBuilder
	}
	return errorGrokBannerOther
}

func (s *Server) attachSessionErrorBanner(d *pageData, ent sessionstore.Entry, threadID string) {
	if d == nil || len(ent.Errors) == 0 || ent.Mode != bot.ModeInvestigate {
		return
	}
	if s.bot == nil || s.bot.ThreadAgent(threadID) != grokrun.AgentGrok {
		return
	}
	d.ErrorGrokBanner = true
	d.ErrorGrokBannerCopy = errorGrokBannerCopy(s.cfg.ResolveCapabilities(ent.Project, d.UserID).CanShip())
}

type ErrorTab struct {
	Src    string
	Label  string
	Active bool
}

func (s *Server) errorTabs(project, active string) []ErrorTab {
	var tabs []ErrorTab
	if s.cfg.ProjectDeploysErrorsEnabled(project) {
		tabs = append(tabs, ErrorTab{Src: errsrc.ProviderDeploys, Label: "deploys.app", Active: active == errsrc.ProviderDeploys})
	}
	if s.cfg.ProjectSentryEnabled(project) {
		tabs = append(tabs, ErrorTab{Src: errsrc.ProviderSentry, Label: "Sentry", Active: active == errsrc.ProviderSentry})
	}
	if s.cfg.ProjectGCPErrorsEnabled(project) {
		tabs = append(tabs, ErrorTab{Src: errsrc.ProviderGCP, Label: "GCP", Active: active == errsrc.ProviderGCP})
	}
	return tabs
}

func (s *Server) errorSrcEnabled(project, src string) bool {
	switch src {
	case errsrc.ProviderDeploys:
		return s.cfg.ProjectDeploysErrorsEnabled(project)
	case errsrc.ProviderSentry:
		return s.cfg.ProjectSentryEnabled(project)
	case errsrc.ProviderGCP:
		return s.cfg.ProjectGCPErrorsEnabled(project)
	}
	return false
}

func validErrorSrc(src string) bool {
	switch src {
	case errsrc.ProviderDeploys, errsrc.ProviderSentry, errsrc.ProviderGCP:
		return true
	}
	return false
}

func errorDetailPath(project, src, id string, q url.Values) string {
	loc := fmt.Sprintf("/projects/%s/errors/%s/%s", url.PathEscape(project), url.PathEscape(src), url.PathEscape(id))
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	return loc
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
