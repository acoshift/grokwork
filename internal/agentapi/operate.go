package agentapi

import (
	"context"
	"fmt"

	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/audit"
)

// OperateHost runs deploy, Actions dispatch, and cherry-pick for one session.
// The project is the credential's project. Callers do not pass a project name.
type OperateHost interface {
	OpsCatalog(ctx context.Context, cred agentauth.Cred, owner, repo string) (OpsCatalogResult, error)
	OpsWorkflows(ctx context.Context, cred agentauth.Cred, owner, repo string) (OpsWorkflowsResult, error)
	OpsLog(ctx context.Context, cred agentauth.Cred, ref, owner, repo string, limit int) (OpsLogResult, error)
	OpsDeploy(ctx context.Context, cred agentauth.Cred, req OpsDeployRequest) (OpsDeployResult, error)
	OpsDispatch(ctx context.Context, cred agentauth.Cred, req OpsDispatchRequest) (OpsDispatchResult, error)
	OpsCherryPick(ctx context.Context, cred agentauth.Cred, req OpsCherryPickRequest) (OpsCherryPickResult, error)
}

// OpsRepo is one GitHub repo the project can operate on. No local path.
type OpsRepo struct {
	Owner string `json:"owner,omitempty"`
	Repo  string `json:"repo,omitempty"`
	Slug  string `json:"slug,omitempty"`
}

// OpsEnv is one configured deploy environment. Secret env values are never copied.
type OpsEnv struct {
	Name         string   `json:"name"`
	Gated        bool     `json:"gated"`
	AllowedRefs  []string `json:"allowedRefs,omitempty"`
	ActorAllowed bool     `json:"actorAllowed"`
}

// OpsDeployInfo is the deploy half of the catalog.
type OpsDeployInfo struct {
	Feature      bool     `json:"feature"`
	Enabled      bool     `json:"enabled"`
	Environments []OpsEnv `json:"environments,omitempty"`
	Services     []string `json:"services,omitempty"`
}

// OpsCherryInfo is the cherry-pick half of the catalog.
type OpsCherryInfo struct {
	Targets   []string `json:"targets,omitempty"`
	ForcePush bool     `json:"forcePush"`
}

// OpsActionsInfo is whether GitHub Actions dispatch is enabled for this host.
type OpsActionsInfo struct {
	Feature bool `json:"feature"`
}

// OpsTip is the commit a gated deploy should pass as expectSha.
type OpsTip struct {
	Ref string `json:"ref,omitempty"`
	SHA string `json:"sha,omitempty"`
}

// OpsCatalogResult is config plus the primary tip. It does not call gh.
type OpsCatalogResult struct {
	Project    string         `json:"project"`
	Repo       OpsRepo        `json:"repo"`
	Repos      []OpsRepo      `json:"repos,omitempty"`
	Deploy     OpsDeployInfo  `json:"deploy"`
	CherryPick OpsCherryInfo  `json:"cherryPick"`
	Actions    OpsActionsInfo `json:"actions"`
	Tip        OpsTip         `json:"tip,omitzero"`
}

// OpsWorkflowInput is one workflow_dispatch input.
type OpsWorkflowInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Type        string   `json:"type,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// OpsWorkflow is one dispatchable workflow.
type OpsWorkflow struct {
	ID          int64              `json:"id,omitempty"`
	Name        string             `json:"name"`
	Path        string             `json:"path"`
	Inputs      []OpsWorkflowInput `json:"inputs,omitempty"`
	AllowedRefs []string           `json:"allowedRefs,omitempty"`
	Locked      bool               `json:"locked,omitempty"`
}

// OpsWorkflowsResult is the dispatchable workflow list.
// Total is the pre-cap count. Truncated is set when the list was cut.
type OpsWorkflowsResult struct {
	Workflows  []OpsWorkflow `json:"workflows"`
	Total      int           `json:"total"`
	Truncated  bool          `json:"truncated,omitempty"`
	PrimaryRef string        `json:"primaryRef,omitempty"`
}

// OpsCommit is one recent commit. Author email is omitted.
type OpsCommit struct {
	SHA     string `json:"sha"`
	Short   string `json:"short,omitempty"`
	Subject string `json:"subject"`
}

// OpsLogResult is a short commit log on one ref.
type OpsLogResult struct {
	Ref     string      `json:"ref"`
	Commits []OpsCommit `json:"commits"`
}

// OpsDeployRequest starts one deploy. Empty Ref means the project primary.
type OpsDeployRequest struct {
	Service   string
	Env       string
	Ref       string
	ExpectSHA string
	Owner     string
	Repo      string
}

// OpsDeployResult is the created run. Checkout paths are not included.
type OpsDeployResult struct {
	RunID   string `json:"runId"`
	SHA     string `json:"sha"`
	Short   string `json:"short,omitempty"`
	Service string `json:"service"`
	Env     string `json:"env"`
	Ref     string `json:"ref"`
	URL     string `json:"url"`
}

// OpsDispatchRequest starts one workflow_dispatch.
type OpsDispatchRequest struct {
	Workflow string
	Ref      string
	Inputs   map[string]string
	Owner    string
	Repo     string
}

// OpsDispatchResult reports that GitHub accepted the dispatch.
// Dispatch is asynchronous and returns no run id.
type OpsDispatchResult struct {
	Status   string `json:"status"`
	Workflow string `json:"workflow"`
	Ref      string `json:"ref"`
}

// OpsCherryPickRequest cherry-picks SHAs onto an allowlisted branch.
type OpsCherryPickRequest struct {
	Target string
	SHAs   []string
	Owner  string
	Repo   string
}

// OpsCherryPickResult is pushed, noop, or a parked conflict.
// URL is a web path or public URL, never a filesystem path.
type OpsCherryPickResult struct {
	Status   string   `json:"status"`
	Target   string   `json:"target"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Picked   []string `json:"picked,omitempty"`
	Skipped  []string `json:"skipped,omitempty"`
	JobID    string   `json:"jobId,omitempty"`
	URL      string   `json:"url,omitempty"`
	Files    []string `json:"files,omitempty"`
	Existing bool     `json:"existing,omitempty"`
}

func (s *Service) operateCred(token, action string, auditDeny bool) (agentauth.Cred, error) {
	cred, err := s.verify(token)
	if err != nil {
		return agentauth.Cred{}, err
	}
	if !cred.Caps.Operate {
		err = fmt.Errorf("forbidden: operate")
		if auditDeny {
			s.deny(cred, action, err, nil)
		}
		return agentauth.Cred{}, err
	}
	if s == nil || s.Operate == nil {
		err = fmt.Errorf("operations are not available")
		if auditDeny {
			s.deny(cred, action, err, nil)
		}
		return agentauth.Cred{}, err
	}
	return cred, nil
}

// OpsCatalog returns environments, cherry-pick targets, repos, and the primary tip.
func (s *Service) OpsCatalog(ctx context.Context, token, owner, repo string) (OpsCatalogResult, error) {
	cred, err := s.operateCred(token, "", false)
	if err != nil {
		return OpsCatalogResult{}, err
	}
	return s.Operate.OpsCatalog(ctx, cred, owner, repo)
}

// OpsWorkflows lists dispatchable workflows.
func (s *Service) OpsWorkflows(ctx context.Context, token, owner, repo string) (OpsWorkflowsResult, error) {
	cred, err := s.operateCred(token, "", false)
	if err != nil {
		return OpsWorkflowsResult{}, err
	}
	return s.Operate.OpsWorkflows(ctx, cred, owner, repo)
}

// OpsLog lists recent commits on a ref.
func (s *Service) OpsLog(ctx context.Context, token, ref, owner, repo string, limit int) (OpsLogResult, error) {
	cred, err := s.operateCred(token, "", false)
	if err != nil {
		return OpsLogResult{}, err
	}
	return s.Operate.OpsLog(ctx, cred, ref, owner, repo, limit)
}

// OpsDeploy triggers a deploy through the host engine.
func (s *Service) OpsDeploy(ctx context.Context, token string, req OpsDeployRequest) (OpsDeployResult, error) {
	cred, err := s.operateCred(token, "deploy.trigger", true)
	if err != nil {
		return OpsDeployResult{}, err
	}
	return s.Operate.OpsDeploy(ctx, cred, req)
}

// OpsDispatch dispatches a GitHub Actions workflow.
func (s *Service) OpsDispatch(ctx context.Context, token string, req OpsDispatchRequest) (OpsDispatchResult, error) {
	cred, err := s.operateCred(token, audit.ActionActionsDispatch, true)
	if err != nil {
		return OpsDispatchResult{}, err
	}
	return s.Operate.OpsDispatch(ctx, cred, req)
}

// OpsCherryPick cherry-picks onto an allowlisted branch.
func (s *Service) OpsCherryPick(ctx context.Context, token string, req OpsCherryPickRequest) (OpsCherryPickResult, error) {
	cred, err := s.operateCred(token, audit.ActionGitCherryPick, true)
	if err != nil {
		return OpsCherryPickResult{}, err
	}
	return s.Operate.OpsCherryPick(ctx, cred, req)
}
