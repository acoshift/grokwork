// Package agentapi is the cap-checked control plane for in-session agents.
// Transports (MCP, markers) call here; they do not authorize themselves.
package agentapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/clickup"
	"github.com/acoshift/grokwork/internal/errsrc/deploys"
	"github.com/acoshift/grokwork/internal/errsrc/gcperr"
	"github.com/acoshift/grokwork/internal/errsrc/sentry"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/projstore"
	"github.com/acoshift/grokwork/internal/reviewstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// SoftAbandoner is the bot soft-abandon path (no worktree delete).
type SoftAbandoner interface {
	SoftAbandonSession(threadID, who string) (string, error)
	SetSessionLabel(threadID, label string) error
}

// SessionFileSink stores an agent-sent file on the bound session.
type SessionFileSink interface {
	ReceiveSessionFileBytes(threadID, name, contentType, rel string, content []byte) (history.Attachment, error)
	ReceiveSessionFileFromWorktree(threadID, path string) (history.Attachment, error)
}

// Service is the host control plane.
type Service struct {
	Auth     *agentauth.Store
	Sessions *sessionstore.Store
	Reviews  *reviewstore.Store
	Storage  *projstore.Store
	Bot      SoftAbandoner
	Files    SessionFileSink
	Audit    *audit.Logger
	// EligibleReviewer mirrors web canRequestReviewer / eligibleReviewer.
	EligibleReviewer func(project, reviewerID string) bool
	// ListEligibleReviewers returns canonical member ids that pass EligibleReviewer.
	ListEligibleReviewers func(project string) []ReviewerRow
	// DisplayName resolves an actor id for review request attribution.
	DisplayName func(actorID string) string
	// OnReviewRequested is best-effort notify after a successful team review request.
	OnReviewRequested func(req reviewstore.Request)
	// RepoDir resolves a project checkout path for gh issue list.
	RepoDir func(project string) string
	// GH runner optional.
	GH ghpr.Runner

	ClickUpEnabled     func(project string) bool
	ClickUpAPIKey      func(project string) string
	ClickUpWorkspaceID func(project string) string
	ClickUpListID      func(project string) string
	ClickUpNew         func(apiKey string) *clickup.Client

	LinearEnabled func(project string) bool
	LinearAPIKey  func(project string) string
	LinearTeamKey func(project string) string
	LinearNew     func(apiKey string) *linear.Client

	DeploysErrorsEnabled func(project string) bool
	DeploysAPIToken      func(project string) string
	DeploysBasicUser     func(project string) string
	DeploysBasicPass     func(project string) string
	DeploysProject       func(project string) string
	DeploysLocation      func(project string) string
	DeploysDeployment    func(project string) string
	DeploysNew           func(opts deploys.Options) *deploys.Client

	SentryEnabled   func(project string) bool
	SentryAuthToken func(project string) string
	SentryOrg       func(project string) string
	SentryProject   func(project string) string
	SentryBaseURL   func(project string) string
	SentryNew       func(token, org, project, baseURL string) *sentry.Client

	GCPErrorsEnabled   func(project string) bool
	GCPProjectID       func(project string) string
	GCPProjectNumber   func(project string) string
	GCPService         func(project string) string
	GCPCredentialsFile func(project string) string
	GCPNew             func(project string) *gcperr.Client
}

// SessionInfo is a compact self snapshot.
type SessionInfo struct {
	ThreadID      string                      `json:"threadId"`
	Project       string                      `json:"project"`
	Goal          string                      `json:"goal,omitempty"`
	Label         string                      `json:"label"`
	Mode          string                      `json:"mode,omitempty"`
	Phase         string                      `json:"phase,omitempty"`
	Branch        string                      `json:"branch,omitempty"`
	CaseKey       string                      `json:"caseKey,omitempty"`
	ShipMode      string                      `json:"shipMode,omitempty"`
	OwnerID       string                      `json:"ownerId,omitempty"`
	OwnerName     string                      `json:"ownerName,omitempty"`
	EngineerID    string                      `json:"engineerId,omitempty"`
	EngineerName  string                      `json:"engineerName,omitempty"`
	Severity      string                      `json:"severity,omitempty"`
	CustomerRef   string                      `json:"customerRef,omitempty"`
	RelatedCases  []string                    `json:"relatedCases,omitempty"`
	OpenQuestions []sessionstore.OpenQuestion `json:"openQuestions,omitempty"`
	PRs           []sessionstore.TrackedPR    `json:"prs,omitempty"`
	Issues        []sessionstore.TrackedIssue `json:"issues,omitempty"`
	Errors        []sessionstore.TrackedError `json:"errors,omitempty"`
}

// ReviewerRow is one team-review-eligible project member.
type ReviewerRow struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// PRRow is a compact PR listing row.
type PRRow struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
}

// IssueRow is a compact issue listing row.
type IssueRow struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	URL    string   `json:"url,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

func (s *Service) verify(token string) (agentauth.Cred, error) {
	if s == nil || s.Auth == nil {
		return agentauth.Cred{}, fmt.Errorf("agent auth unavailable")
	}
	return s.Auth.Verify(token)
}

func (s *Service) deny(cred agentauth.Cred, action string, err error, detail map[string]any) {
	if s == nil || s.Audit == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["source"] = "agent"
	detail["threadId"] = cred.ThreadID
	detail["project"] = cred.Project
	detail["credId"] = cred.ID
	ev := audit.Event{Action: action, Actor: cred.ActorID, Detail: detail, OK: err == nil}
	if err != nil {
		ev.Error = err.Error()
		ev.OK = false
	}
	_ = s.Audit.Append(ev)
}

// SessionGet returns the bound session only.
func (s *Service) SessionGet(token string) (SessionInfo, error) {
	cred, err := s.verify(token)
	if err != nil {
		return SessionInfo{}, err
	}
	if !cred.Caps.SessionRead {
		return SessionInfo{}, fmt.Errorf("forbidden: session read")
	}
	if s.Sessions == nil {
		return SessionInfo{}, fmt.Errorf("no session store")
	}
	ent, ok := s.Sessions.Get(cred.ThreadID)
	if !ok {
		return SessionInfo{}, fmt.Errorf("no session")
	}
	// Refuse if entry project disagrees with token (hand-edited data).
	if ent.Project != "" && ent.Project != cred.Project {
		return SessionInfo{}, fmt.Errorf("project mismatch")
	}
	ent.NormalizePRs()
	return SessionInfo{
		ThreadID:      cred.ThreadID,
		Project:       cred.Project,
		Goal:          ent.Goal,
		Label:         ent.EffectiveLabel(),
		Mode:          ent.Mode,
		Phase:         ent.Phase,
		Branch:        ent.WorktreeBranch,
		CaseKey:       ent.CaseKey,
		ShipMode:      ent.ShipMode,
		OwnerID:       ent.OwnerID,
		OwnerName:     ent.OwnerName,
		EngineerID:    ent.EngineerID,
		EngineerName:  ent.EngineerName,
		Severity:      ent.Severity,
		CustomerRef:   ent.CustomerRef,
		RelatedCases:  ent.RelatedCaseKeys(),
		OpenQuestions: ent.OpenQuestions,
		PRs:           ent.PRs,
		Issues:        ent.Issues,
		Errors:        ent.Errors,
	}, nil
}

// SessionDone sets manual done on the bound thread.
func (s *Service) SessionDone(token string) error {
	cred, err := s.verify(token)
	if err != nil {
		return err
	}
	if !cred.Caps.SessionDone {
		err = fmt.Errorf("forbidden: session done")
		s.deny(cred, audit.ActionAgentSessionDone, err, nil)
		return err
	}
	if s.Bot == nil {
		return fmt.Errorf("bot unavailable")
	}
	err = s.Bot.SetSessionLabel(cred.ThreadID, sessionstore.LabelDone)
	s.deny(cred, audit.ActionAgentSessionDone, err, map[string]any{"label": sessionstore.LabelDone})
	return err
}

// SessionAbandon soft-abandons the bound thread (no worktree delete).
func (s *Service) SessionAbandon(token, reason string) error {
	cred, err := s.verify(token)
	if err != nil {
		return err
	}
	if !cred.Caps.SessionAbandon {
		err = fmt.Errorf("forbidden: session abandon")
		s.deny(cred, audit.ActionAgentSessionAbandon, err, nil)
		return err
	}
	if s.Bot == nil {
		return fmt.Errorf("bot unavailable")
	}
	_, err = s.Bot.SoftAbandonSession(cred.ThreadID, cred.ActorID)
	detail := map[string]any{"label": sessionstore.LabelAbandoned}
	if strings.TrimSpace(reason) != "" {
		detail["hasReason"] = true
	}
	s.deny(cred, audit.ActionAgentSessionAbandon, err, detail)
	return err
}

// ListPRs lists tracked PRs for the bound session (scope=session) or all
// sessions in the project that have tracked PRs (scope=project).
func (s *Service) ListPRs(token, scope string) ([]PRRow, error) {
	cred, err := s.verify(token)
	if err != nil {
		return nil, err
	}
	if !cred.Caps.PRsList {
		return nil, fmt.Errorf("forbidden: prs list")
	}
	if s.Sessions == nil {
		return nil, fmt.Errorf("no session store")
	}
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		scope = "session"
	}
	var rows []PRRow
	switch scope {
	case "session":
		ent, ok := s.Sessions.Get(cred.ThreadID)
		if !ok {
			return nil, nil
		}
		ent.NormalizePRs()
		for _, pr := range ent.PRs {
			rows = append(rows, PRRow{
				Owner: pr.Owner, Repo: pr.Repo, Number: pr.Number,
				Title: pr.Title, URL: pr.URL, State: pr.State, Draft: pr.IsDraft,
			})
		}
	case "project":
		for _, item := range s.Sessions.List() {
			e := item.Entry
			if e.Project != cred.Project {
				continue
			}
			e.NormalizePRs()
			for _, pr := range e.PRs {
				rows = append(rows, PRRow{
					Owner: pr.Owner, Repo: pr.Repo, Number: pr.Number,
					Title: pr.Title, URL: pr.URL, State: pr.State, Draft: pr.IsDraft,
				})
			}
		}
	default:
		return nil, fmt.Errorf("unknown scope %q", scope)
	}
	return rows, nil
}

// ListIssues lists GitHub issues for the bound project repo.
func (s *Service) ListIssues(ctx context.Context, token string, state string, limit int, labels []string) ([]IssueRow, error) {
	cred, err := s.verify(token)
	if err != nil {
		return nil, err
	}
	if !cred.Caps.IssuesList {
		return nil, fmt.Errorf("forbidden: issues list")
	}
	if s.RepoDir == nil {
		return nil, fmt.Errorf("repo resolver unavailable")
	}
	dir := s.RepoDir(cred.Project)
	if dir == "" {
		return nil, fmt.Errorf("no checkout for project")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	run := s.GH
	if run == nil {
		run = nil // ListIssuesWith uses default
	}
	infos, err := ghpr.ListIssuesWith(ctx, run, dir, ghpr.IssueListOpts{
		State:  state,
		Limit:  limit,
		Labels: labels,
	})
	if err != nil {
		return nil, err
	}
	out := make([]IssueRow, 0, len(infos))
	for _, i := range infos {
		out = append(out, IssueRow{
			Number: i.Number, Title: i.Title, State: i.State, URL: i.URL, Labels: i.Labels,
		})
	}
	return out, nil
}

// RequestTeamReview creates a team review request (not GitHub formal).
func (s *Service) RequestTeamReview(token, owner, repo string, number int, reviewerID, note, headSHA string) (reviewstore.Request, error) {
	cred, err := s.verify(token)
	if err != nil {
		return reviewstore.Request{}, err
	}
	if !cred.Caps.ReviewRequest {
		err = fmt.Errorf("forbidden: review request")
		s.deny(cred, audit.ActionAgentReviewRequest, err, nil)
		return reviewstore.Request{}, err
	}
	if s.Reviews == nil {
		return reviewstore.Request{}, fmt.Errorf("review store unavailable")
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	reviewerID = strings.TrimSpace(reviewerID)
	if owner == "" || repo == "" || number <= 0 || reviewerID == "" {
		err = fmt.Errorf("owner, repo, number, reviewer required")
		s.deny(cred, audit.ActionAgentReviewRequest, err, nil)
		return reviewstore.Request{}, err
	}
	if s.EligibleReviewer == nil || !s.EligibleReviewer(cred.Project, reviewerID) {
		err = fmt.Errorf("reviewer is not eligible (builder-class required)")
		s.deny(cred, audit.ActionAgentReviewRequest, err, map[string]any{
			"reviewerId": reviewerID, "owner": owner, "repo": repo, "number": number,
		})
		return reviewstore.Request{}, err
	}
	reviewerName := reviewerID
	if s.DisplayName != nil {
		if n := s.DisplayName(reviewerID); n != "" {
			reviewerName = n
		}
	}
	reqName := cred.ActorID
	if s.DisplayName != nil {
		if n := s.DisplayName(cred.ActorID); n != "" {
			reqName = n + " (agent)"
		}
	} else if reqName != "" {
		reqName = reqName + " (agent)"
	} else {
		reqName = "agent"
	}
	req, err := s.Reviews.RequestReview(reviewstore.Request{
		Owner:         owner,
		Repo:          repo,
		Number:        number,
		Project:       cred.Project,
		ThreadID:      cred.ThreadID,
		HeadSHA:       headSHA,
		RequesterID:   cred.ActorID,
		RequesterName: reqName,
		ReviewerID:    reviewerID,
		ReviewerName:  reviewerName,
		Note:          note,
	})
	s.deny(cred, audit.ActionAgentReviewRequest, err, map[string]any{
		"owner": owner, "repo": repo, "number": number, "reviewerId": reviewerID,
	})
	if err == nil && s.OnReviewRequested != nil {
		s.OnReviewRequested(req)
	}
	return req, err
}

// ListReviewers lists team-review-eligible members of the bound project.
func (s *Service) ListReviewers(token string) ([]ReviewerRow, error) {
	cred, err := s.verify(token)
	if err != nil {
		return nil, err
	}
	if !cred.Caps.ReviewRequest {
		return nil, fmt.Errorf("forbidden: review request")
	}
	if s.ListEligibleReviewers == nil {
		return nil, nil
	}
	return s.ListEligibleReviewers(cred.Project), nil
}

// StoragePut writes a project blob.
// encoding is "text" (default) or "base64". Never guess: short strings that
// happen to be valid base64 must not be silently decoded.
func (s *Service) StoragePut(token, key, content, contentType, encoding string) (projstore.Meta, error) {
	cred, err := s.verify(token)
	if err != nil {
		return projstore.Meta{}, err
	}
	if !cred.Caps.StorageWrite {
		err = fmt.Errorf("forbidden: storage write")
		s.deny(cred, audit.ActionAgentStoragePut, err, map[string]any{"key": key})
		return projstore.Meta{}, err
	}
	if s.Storage == nil {
		return projstore.Meta{}, fmt.Errorf("storage unavailable")
	}
	var raw []byte
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "text", "utf8", "utf-8":
		raw = []byte(content)
	case "base64", "b64":
		raw, err = base64.StdEncoding.DecodeString(content)
		if err != nil {
			return projstore.Meta{}, fmt.Errorf("invalid base64 content: %w", err)
		}
	default:
		return projstore.Meta{}, fmt.Errorf("unknown encoding %q (use text or base64)", encoding)
	}
	meta, err := s.Storage.Put(cred.Project, key, raw, contentType, cred.ThreadID, cred.ActorID)
	s.deny(cred, audit.ActionAgentStoragePut, err, map[string]any{"key": key, "size": meta.Size})
	return meta, err
}

// SessionFile is the compact result of session_send_file.
type SessionFile struct {
	Name string `json:"name"`
	Rel  string `json:"rel,omitempty"`
	Size int64  `json:"size,omitzero"`
	Type string `json:"contentType,omitempty"`
}

// SessionSendFile stores a deliverable on the bound session.
// Pass a worktree path, or name+content (encoding text or base64). Never both
// with disagreement: a path is preferred so large files do not travel through JSON.
func (s *Service) SessionSendFile(token, path, name, content, contentType, encoding string) (SessionFile, error) {
	cred, err := s.verify(token)
	if err != nil {
		return SessionFile{}, err
	}
	if !cred.Caps.SessionFiles {
		err = fmt.Errorf("forbidden: session files")
		s.deny(cred, audit.ActionAgentSessionFile, err, map[string]any{"name": name})
		return SessionFile{}, err
	}
	if s.Files == nil {
		return SessionFile{}, fmt.Errorf("session files unavailable")
	}
	path = strings.TrimSpace(path)
	name = strings.TrimSpace(name)
	var att history.Attachment
	if path != "" {
		att, err = s.Files.ReceiveSessionFileFromWorktree(cred.ThreadID, path)
	} else {
		if name == "" {
			err = fmt.Errorf("name is required when content is sent")
			s.deny(cred, audit.ActionAgentSessionFile, err, nil)
			return SessionFile{}, err
		}
		var raw []byte
		switch strings.ToLower(strings.TrimSpace(encoding)) {
		case "", "text", "utf8", "utf-8":
			raw = []byte(content)
		case "base64", "b64":
			raw, err = base64.StdEncoding.DecodeString(content)
			if err != nil {
				return SessionFile{}, fmt.Errorf("invalid base64 content: %w", err)
			}
		default:
			return SessionFile{}, fmt.Errorf("unknown encoding %q (use text or base64)", encoding)
		}
		att, err = s.Files.ReceiveSessionFileBytes(cred.ThreadID, name, contentType, "", raw)
	}
	s.deny(cred, audit.ActionAgentSessionFile, err, map[string]any{"name": att.Name, "size": att.Size})
	if err != nil {
		return SessionFile{}, err
	}
	return SessionFile{Name: att.Name, Rel: att.Rel, Size: att.Size, Type: att.ContentType}, nil
}

// StorageGet reads a project blob (content base64).
func (s *Service) StorageGet(token, key string) (contentB64 string, meta projstore.Meta, err error) {
	cred, err := s.verify(token)
	if err != nil {
		return "", projstore.Meta{}, err
	}
	if !cred.Caps.StorageRead {
		return "", projstore.Meta{}, fmt.Errorf("forbidden: storage read")
	}
	if s.Storage == nil {
		return "", projstore.Meta{}, fmt.Errorf("storage unavailable")
	}
	data, meta, err := s.Storage.Get(cred.Project, key)
	if err != nil {
		return "", projstore.Meta{}, err
	}
	return base64.StdEncoding.EncodeToString(data), meta, nil
}

// StorageList lists keys for the bound project.
func (s *Service) StorageList(token, prefix string, limit int) ([]projstore.Meta, error) {
	cred, err := s.verify(token)
	if err != nil {
		return nil, err
	}
	if !cred.Caps.StorageRead {
		return nil, fmt.Errorf("forbidden: storage read")
	}
	if s.Storage == nil {
		return nil, fmt.Errorf("storage unavailable")
	}
	return s.Storage.List(cred.Project, prefix, limit)
}

// StorageDelete deletes a key.
func (s *Service) StorageDelete(token, key string) error {
	cred, err := s.verify(token)
	if err != nil {
		return err
	}
	if !cred.Caps.StorageWrite {
		err = fmt.Errorf("forbidden: storage write")
		s.deny(cred, audit.ActionAgentStorageDelete, err, map[string]any{"key": key})
		return err
	}
	if s.Storage == nil {
		return fmt.Errorf("storage unavailable")
	}
	err = s.Storage.Delete(cred.Project, key)
	s.deny(cred, audit.ActionAgentStorageDelete, err, map[string]any{"key": key})
	return err
}
