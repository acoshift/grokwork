package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/acoshift/grokwork/internal/apitoken"
	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const apiJSONLimit = 1 << 20

type apiCtxKey int

const apiPrincipalKey apiCtxKey = 1

type apiError struct {
	status  int
	code    string
	message string
}

func (e apiError) Error() string { return e.message }

func errAPI(status int, code, msg string) apiError {
	return apiError{status: status, code: code, message: msg}
}

var (
	errAPIDisabled  = errAPI(http.StatusNotFound, "not_found", "not found")
	errUnauthorized = errAPI(http.StatusUnauthorized, "unauthorized", "unauthorized")
	errRateLimited  = errAPI(http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
)

func writeAPIError(w http.ResponseWriter, err error) {
	var ae apiError
	if errors.As(err, &ae) {
		writeAPIJSON(w, ae.status, map[string]any{
			"error": map[string]string{"code": ae.code, "message": ae.message},
		})
		return
	}
	if errors.Is(err, apitoken.ErrUnauthorized) {
		writeAPIError(w, errUnauthorized)
		return
	}
	if errors.Is(err, apitoken.ErrIdempotencyConflict) {
		writeAPIError(w, errAPI(http.StatusConflict, "conflict", "idempotency key reused with a different body"))
		return
	}
	if errors.Is(err, bot.ErrQueueFull) {
		writeAPIError(w, errAPI(http.StatusConflict, "conflict", err.Error()))
		return
	}
	if errors.Is(err, bot.ErrUnknownThread) {
		writeAPIError(w, errAPI(http.StatusNotFound, "not_found", "not found"))
		return
	}
	if errors.Is(err, bot.ErrEmptyPrompt) {
		writeAPIError(w, errAPI(http.StatusBadRequest, "validation", "prompt is required"))
		return
	}
	if errors.Is(err, bot.ErrCannotStartFix) || errors.Is(err, bot.ErrCannotSelectModel) {
		writeAPIError(w, errAPI(http.StatusForbidden, "forbidden", err.Error()))
		return
	}
	writeAPIJSON(w, http.StatusInternalServerError, map[string]any{
		"error": map[string]string{"code": "internal", "message": "internal error"},
	})
}

func marshalAPIJSON(v any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func writeAPIJSON(w http.ResponseWriter, status int, v any) {
	raw, err := marshalAPIJSON(v)
	if err != nil {
		http.Error(w, `{"error":{"code":"internal","message":"internal error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func (s *Server) apiWritesEnabled() bool {
	return s != nil && s.cfg != nil && s.cfg.APIEnabled()
}

func (s *Server) registerAPI(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/health", http.HandlerFunc(s.apiHealth))
	mux.Handle("GET /api/v1/whoami", s.requireAPIToken(http.HandlerFunc(s.apiWhoami)))
	mux.Handle("GET /api/v1/projects", s.requireAPIToken(http.HandlerFunc(s.apiProjects)))
	mux.Handle("GET /api/v1/sessions/{sessionId}", s.requireAPIToken(http.HandlerFunc(s.apiSessionGet)))
	mux.Handle("POST /api/v1/projects/{project}/sessions", s.requireAPIToken(http.HandlerFunc(s.apiStartSession)))
	mux.Handle("POST /api/v1/projects/{project}/issues", s.requireAPIToken(http.HandlerFunc(s.apiCreateIssue)))
	mux.Handle("POST /api/v1/sessions/{sessionId}/continue", s.requireAPIToken(http.HandlerFunc(s.apiContinue)))
	mux.Handle("POST /api/v1/sessions/{sessionId}/cancel", s.requireAPIToken(http.HandlerFunc(s.apiCancel)))
}

func (s *Server) apiHealth(w http.ResponseWriter, r *http.Request) {
	writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "api": s.apiWritesEnabled()})
}

func (s *Server) requireAPIToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.apiWritesEnabled() {
			writeAPIError(w, errAPIDisabled)
			return
		}
		if s.apiTokens == nil {
			writeAPIError(w, errUnauthorized)
			return
		}
		hdr := r.Header.Get("Authorization")
		scheme, rest, ok := strings.Cut(hdr, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(rest) == "" {
			s.auditAPI(nil, audit.ActionAccessDeny, errUnauthorized, map[string]any{"reason": "bad_token"})
			writeAPIError(w, errUnauthorized)
			return
		}
		rec, err := s.apiTokens.Authenticate(strings.TrimSpace(rest))
		if err != nil {
			s.auditAPI(nil, audit.ActionAccessDeny, errUnauthorized, map[string]any{"reason": "bad_token"})
			writeAPIError(w, errUnauthorized)
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			r.Body = http.MaxBytesReader(w, r.Body, apiJSONLimit)
		}
		ctx := context.WithValue(r.Context(), apiPrincipalKey, rec)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func apiPrincipal(ctx context.Context) apitoken.Record {
	rec, _ := ctx.Value(apiPrincipalKey).(apitoken.Record)
	return rec
}

func (s *Server) auditAPI(rec *apitoken.Record, action string, err error, detail map[string]any) {
	if s == nil || s.audit == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["source"] = "api"
	actor := audit.ActorAnonymous
	if rec != nil && rec.ActorID != "" {
		actor = rec.ActorID
		detail["tokenId"] = rec.ID
		detail["tokenLabel"] = rec.Label
	}
	ev := audit.Event{
		Action: action,
		Actor:  actor,
		Role:   "api",
		Detail: detail,
		OK:     err == nil,
	}
	if err != nil {
		ev.Error = err.Error()
	}
	_ = s.audit.Append(ev)
}

func (s *Server) apiEffective(rec apitoken.Record, project string) (config.Capabilities, error) {
	project = strings.TrimSpace(project)
	if project == "" || !slices.Contains(rec.Projects, project) {
		return config.Capabilities{}, errAPI(http.StatusNotFound, "not_found", "not found")
	}
	if !s.cfg.AccessAllowed(project, rec.ActorID) {
		return config.Capabilities{}, errAPI(http.StatusNotFound, "not_found", "not found")
	}
	return apitoken.Intersect(s.cfg.ResolveCapabilities(project, rec.ActorID), rec.Caps), nil
}

func (s *Server) tokenProjects(rec apitoken.Record) []string {
	var out []string
	for _, p := range rec.Projects {
		if s.cfg.AccessAllowed(p, rec.ActorID) {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) apiWhoami(w http.ResponseWriter, r *http.Request) {
	rec := apiPrincipal(r.Context())
	caps := map[string]any{}
	projects := s.tokenProjects(rec)
	for _, p := range projects {
		c := apitoken.Intersect(s.cfg.ResolveCapabilities(p, rec.ActorID), rec.Caps)
		caps[p] = map[string]bool{
			"investigate":   c.Investigate,
			"startSessions": c.StartSessions,
			"githubWrites":  c.GithubWrites,
		}
	}
	out := map[string]any{
		"actorId":      rec.ActorID,
		"label":        rec.Label,
		"projects":     projects,
		"capabilities": caps,
	}
	if !rec.ExpiresAt.IsZero() {
		out["expiresAt"] = rec.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	writeAPIJSON(w, http.StatusOK, out)
}

func (s *Server) apiProjects(w http.ResponseWriter, r *http.Request) {
	rec := apiPrincipal(r.Context())
	type row struct {
		Name string `json:"name"`
	}
	var list []row
	for _, p := range s.tokenProjects(rec) {
		list = append(list, row{Name: p})
	}
	if list == nil {
		list = []row{}
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"projects": list})
}

func (s *Server) loadOwnedSession(rec apitoken.Record, sessionID string) (sessionstore.Entry, error) {
	if s.sessions == nil {
		return sessionstore.Entry{}, errAPI(http.StatusNotFound, "not_found", "not found")
	}
	ent, ok := s.sessions.Get(sessionID)
	if !ok {
		return sessionstore.Entry{}, errAPI(http.StatusNotFound, "not_found", "not found")
	}
	if !slices.Contains(rec.Projects, ent.Project) || !s.cfg.AccessAllowed(ent.Project, rec.ActorID) {
		return sessionstore.Entry{}, errAPI(http.StatusNotFound, "not_found", "not found")
	}
	if !ent.CanControl(rec.ActorID) {
		return sessionstore.Entry{}, errAPI(http.StatusNotFound, "not_found", "not found")
	}
	ent.NormalizePRs()
	return ent, nil
}

func (s *Server) sessionPublicURL(id string) string {
	base := strings.TrimRight(s.cfg.WebPublicBaseURLValue(), "/")
	path := "/sessions/" + id
	if base == "" {
		return path
	}
	return base + path
}

func (s *Server) sessionJSON(id string, ent sessionstore.Entry) map[string]any {
	rt := s.bot.SessionRuntime(id)
	var prs []map[string]any
	for _, p := range ent.PRs {
		prs = append(prs, map[string]any{
			"number": p.Number,
			"url":    p.URL,
			"state":  p.State,
		})
	}
	if prs == nil {
		prs = []map[string]any{}
	}
	return map[string]any{
		"sessionId": id,
		"project":   ent.Project,
		"goal":      ent.Goal,
		"mode":      ent.Mode,
		"label":     ent.Label,
		"ownerId":   ent.OwnerID,
		"running":   rt.Running,
		"queueLen":  rt.QueueLen,
		"activity":  rt.Activity,
		"prs":       prs,
		"url":       s.sessionPublicURL(id),
	}
}

func (s *Server) apiSessionGet(w http.ResponseWriter, r *http.Request) {
	rec := apiPrincipal(r.Context())
	id := strings.TrimSpace(r.PathValue("sessionId"))
	ent, err := s.loadOwnedSession(rec, id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, s.sessionJSON(id, ent))
}

func (s *Server) checkStartRateActor(actorID string) error {
	if !s.startLimiter().AllowN(actorID, 1) {
		return errRateLimited
	}
	return nil
}

func decodeAPIBody(r *http.Request, dst any) (raw []byte, err error) {
	raw, err = io.ReadAll(r.Body)
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			return nil, errAPI(http.StatusBadRequest, "invalid_request", "body too large")
		}
		return nil, errAPI(http.StatusBadRequest, "invalid_request", "invalid body")
	}
	if len(bytesTrimSpace(raw)) == 0 {
		return raw, errAPI(http.StatusBadRequest, "invalid_request", "body required")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return raw, errAPI(http.StatusBadRequest, "invalid_request", "invalid json")
	}
	return raw, nil
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func (s *Server) idempotencyReplay(w http.ResponseWriter, rec apitoken.Record, key, bodyHash string) bool {
	if s.apiTokens == nil || strings.TrimSpace(key) == "" {
		return false
	}
	got, ok, err := s.apiTokens.IdempotencyGet(rec.ID, key, bodyHash)
	if errors.Is(err, apitoken.ErrIdempotencyConflict) {
		writeAPIError(w, err)
		return true
	}
	if err != nil || !ok {
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(got.Status)
	_, _ = w.Write(got.Response)
	return true
}

func (s *Server) idempotencyStore(rec apitoken.Record, key, bodyHash string, status int, v any) {
	if s.apiTokens == nil || strings.TrimSpace(key) == "" {
		return
	}
	raw, err := marshalAPIJSON(v)
	if err != nil {
		return
	}
	_ = s.apiTokens.IdempotencyPut(rec.ID, key, apitoken.IdemRecord{
		BodyHash: bodyHash, Status: status, Response: raw,
	})
}

type apiStartBody struct {
	Prompt string `json:"prompt"`
	Title  string `json:"title"`
	Mode   string `json:"mode"`
	Model  string `json:"model"`
}

func (s *Server) apiStartSession(w http.ResponseWriter, r *http.Request) {
	rec := apiPrincipal(r.Context())
	project := strings.TrimSpace(r.PathValue("project"))
	effective, err := s.apiEffective(rec, project)
	if err != nil {
		s.auditAPI(&rec, audit.ActionSessionStart, err, map[string]any{"project": project})
		writeAPIError(w, err)
		return
	}
	var body apiStartBody
	raw, err := decodeAPIBody(r, &body)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	hash := apitoken.BodyHash(raw)
	if s.idempotencyReplay(w, rec, key, hash) {
		return
	}
	if !effective.StartSessions {
		err := errAPI(http.StatusForbidden, "forbidden", "not allowed to start sessions for this project")
		s.auditAPI(&rec, audit.ActionSessionStart, err, map[string]any{"project": project})
		writeAPIError(w, err)
		return
	}
	if bot.WantsFixStartMode(body.Mode, s.cfg.ProjectDefaultMode(project)) && !effective.CanShip() {
		err := errAPI(http.StatusForbidden, "forbidden", "fix requires ship-class effective caps")
		s.auditAPI(&rec, audit.ActionSessionStart, err, map[string]any{"project": project, "mode": body.Mode})
		writeAPIError(w, err)
		return
	}
	if bot.WantsPlanStartMode(body.Mode) && !effective.CanShip() {
		err := errAPI(http.StatusForbidden, "forbidden", "plan requires ship-class effective caps")
		s.auditAPI(&rec, audit.ActionSessionStart, err, map[string]any{"project": project, "mode": body.Mode})
		writeAPIError(w, err)
		return
	}
	if strings.TrimSpace(body.Model) != "" && !effective.CanShip() {
		err := errAPI(http.StatusForbidden, "forbidden", "model selection requires ship-class effective caps")
		s.auditAPI(&rec, audit.ActionSessionStart, err, map[string]any{"project": project})
		writeAPIError(w, err)
		return
	}
	if err := s.checkStartRateActor(rec.ActorID); err != nil {
		writeAPIError(w, err)
		return
	}
	res, startErr := s.bot.StartWebTask(bot.StartWebTaskOpts{
		Project:   project,
		Prompt:    body.Prompt,
		Title:     body.Title,
		Mode:      body.Mode,
		Model:     body.Model,
		Actor:     bot.Actor{ID: rec.ActorID, DisplayName: rec.Label},
		WebNative: true,
	})
	detail := map[string]any{"project": project, "mode": body.Mode, "origin": "api"}
	if startErr == nil {
		detail["threadId"] = res.ThreadID
		detail["status"] = string(res.Status)
		detail["created"] = res.Created
	}
	s.auditAPI(&rec, audit.ActionSessionStart, startErr, detail)
	if startErr != nil {
		writeAPIError(w, startErr)
		return
	}
	out := map[string]any{
		"sessionId":  res.ThreadID,
		"status":     string(res.Status),
		"queuePos":   res.QueuePos,
		"created":    res.Created,
		"url":        s.sessionPublicURL(res.ThreadID),
		"discordUrl": "",
	}
	s.idempotencyStore(rec, key, hash, http.StatusCreated, out)
	writeAPIJSON(w, http.StatusCreated, out)
}

type apiIssueBody struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Kind  string `json:"kind"`
}

func (s *Server) apiCreateIssue(w http.ResponseWriter, r *http.Request) {
	rec := apiPrincipal(r.Context())
	project := strings.TrimSpace(r.PathValue("project"))
	effective, err := s.apiEffective(rec, project)
	if err != nil {
		s.auditAPI(&rec, audit.ActionIssueCreate, err, map[string]any{"project": project})
		writeAPIError(w, err)
		return
	}
	var body apiIssueBody
	raw, err := decodeAPIBody(r, &body)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	hash := apitoken.BodyHash(raw)
	if s.idempotencyReplay(w, rec, key, hash) {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	if kind != "feature" && kind != "bug" {
		writeAPIError(w, errAPI(http.StatusBadRequest, "validation", "kind must be feature or bug"))
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeAPIError(w, errAPI(http.StatusBadRequest, "validation", "title is required"))
		return
	}
	if !effective.GithubWrites {
		err := errAPI(http.StatusForbidden, "forbidden", "not allowed to create GitHub issues for this project")
		s.auditAPI(&rec, audit.ActionIssueCreate, err, map[string]any{"project": project})
		writeAPIError(w, err)
		return
	}
	ref, cwd, resErr := s.ResolveProjectRepo(r.Context(), project, body.Owner, body.Repo, rec.ActorID)
	if resErr != nil {
		writeAPIError(w, resErr)
		return
	}
	issueBody := body.Body
	if rec.Label != "" {
		issueBody = "On behalf of automation \"" + rec.Label + "\"\n\n" + issueBody
	}
	n, url, createErr := ghpr.CreateIssueWith(r.Context(), s.ghRun(), cwd, ref.Owner, ref.Repo, ghpr.CreateIssueOpts{
		Title:  body.Title,
		Body:   issueBody,
		Labels: []string{kind},
	})
	s.auditAPI(&rec, audit.ActionIssueCreate, createErr, map[string]any{
		"project": project, "owner": ref.Owner, "repo": ref.Repo, "kind": kind, "number": n,
	})
	if createErr != nil {
		writeAPIError(w, errAPI(http.StatusBadGateway, "internal", "failed to create issue"))
		return
	}
	out := map[string]any{"number": n, "url": url, "owner": ref.Owner, "repo": ref.Repo}
	s.idempotencyStore(rec, key, hash, http.StatusCreated, out)
	writeAPIJSON(w, http.StatusCreated, out)
}

type apiContinueBody struct {
	Prompt string `json:"prompt"`
}

func (s *Server) apiContinue(w http.ResponseWriter, r *http.Request) {
	rec := apiPrincipal(r.Context())
	id := strings.TrimSpace(r.PathValue("sessionId"))
	ent, err := s.loadOwnedSession(rec, id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	effective, err := s.apiEffective(rec, ent.Project)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	var body apiContinueBody
	raw, err := decodeAPIBody(r, &body)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	hash := apitoken.BodyHash(raw)
	if s.idempotencyReplay(w, rec, key, hash) {
		return
	}
	if !effective.StartSessions {
		err := errAPI(http.StatusForbidden, "forbidden", "not allowed to start sessions for this project")
		s.auditAPI(&rec, audit.ActionSessionStart, err, map[string]any{"kind": "continue", "threadId": id})
		writeAPIError(w, err)
		return
	}
	if ent.IsCaseClosed() {
		writeAPIError(w, errAPI(http.StatusConflict, "conflict", "case is closed — reopen first"))
		return
	}
	if err := s.checkStartRateActor(rec.ActorID); err != nil {
		writeAPIError(w, err)
		return
	}
	res, startErr := s.bot.StartContinue(bot.ContinueOpts{
		ThreadID: id, Project: ent.Project, Prompt: body.Prompt,
		Actor: bot.Actor{ID: rec.ActorID, DisplayName: rec.Label},
	})
	s.auditAPI(&rec, audit.ActionSessionStart, startErr, map[string]any{
		"kind": "continue", "threadId": id, "project": ent.Project,
	})
	if startErr != nil {
		writeAPIError(w, startErr)
		return
	}
	out := map[string]any{
		"sessionId": id,
		"status":    string(res.Status),
		"queuePos":  res.QueuePos,
		"url":       s.sessionPublicURL(id),
	}
	s.idempotencyStore(rec, key, hash, http.StatusOK, out)
	writeAPIJSON(w, http.StatusOK, out)
}

func (s *Server) apiCancel(w http.ResponseWriter, r *http.Request) {
	rec := apiPrincipal(r.Context())
	id := strings.TrimSpace(r.PathValue("sessionId"))
	if _, err := s.loadOwnedSession(rec, id); err != nil {
		writeAPIError(w, err)
		return
	}
	msg, _ := s.bot.CancelRun(id, rec.Label)
	s.auditAPI(&rec, audit.ActionSessionCancel, nil, map[string]any{"threadId": id})
	writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
}
