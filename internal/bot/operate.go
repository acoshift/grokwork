package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/agentapi"
	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/deploy"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

// Operate runs deploys, Actions dispatches, and cherry-picks through the same
// engines as the web UI. The coding CLI is file-read only: shell and GH_TOKEN
// stay off so a prompt cannot bypass cherry-pick allowlists, Actions branch
// locks, or a gated deploy's expectSha check.

const (
	opsWorkflowCap = 40
	opsLogDefault  = 15
	opsLogMax      = 30
)

// SetDeploys installs the web server's deploy engine. Host methods read it
// at call time; initAgentPlane runs before the engine exists.
func (b *Bot) SetDeploys(e *deploy.Engine) {
	if b == nil {
		return
	}
	b.deploys = e
}

func (b *Bot) OpsCatalog(ctx context.Context, cred agentauth.Cred, owner, repo string) (agentapi.OpsCatalogResult, error) {
	if b == nil || b.cfg == nil {
		return agentapi.OpsCatalogResult{}, fmt.Errorf("operations are not available")
	}
	project := cred.Project
	repoPath, slug, gotOwner, gotRepo, err := b.operateRepo(ctx, project, owner, repo)
	if err != nil {
		return agentapi.OpsCatalogResult{}, b.scrubProject(project, err)
	}
	caps := b.cfg.ResolveCapabilities(project, cred.ActorID)
	out := agentapi.OpsCatalogResult{
		Project: project,
		Repo:    agentapi.OpsRepo{Owner: gotOwner, Repo: gotRepo, Slug: slug},
		Deploy: agentapi.OpsDeployInfo{
			Feature: b.cfg.FeatureDeploy(),
			Enabled: b.cfg.ProjectDeployEnabled(project),
		},
		CherryPick: agentapi.OpsCherryInfo{
			Targets:   b.operateCherryTargets(ctx, project, repoPath),
			ForcePush: b.cfg.ProjectForcePushTargets(project),
		},
		Actions: agentapi.OpsActionsInfo{Feature: b.cfg.FeatureGitHubWrites()},
	}
	if catalog, cErr := b.cfg.ProjectRepoCatalogWith(ctx, project, nil); cErr == nil {
		for _, r := range catalog {
			out.Repos = append(out.Repos, agentapi.OpsRepo{Owner: r.Owner, Repo: r.Repo, Slug: r.Slug()})
		}
	}
	for _, name := range b.cfg.ProjectDeployEnvNames(project) {
		env, ok := b.cfg.ProjectDeployEnv(project, name)
		if !ok || env == nil {
			continue
		}
		out.Deploy.Environments = append(out.Deploy.Environments, agentapi.OpsEnv{
			Name:         name,
			Gated:        strings.TrimSpace(env.RequireCapability) != "",
			AllowedRefs:  slices.Clone(env.AllowedRefs),
			ActorAllowed: deploy.CapabilityAllows(caps, env.RequireCapability),
		})
	}
	pref := b.cfg.ProjectPrimaryBranch(project)
	ref := gitworktree.PrimaryStartRef(ctx, repoPath, pref)
	sha, shaErr := gitworktree.ResolveRefSHA(ctx, repoPath, ref)
	if shaErr == nil && sha != "" {
		out.Tip = agentapi.OpsTip{Ref: ref, SHA: sha}
		m, lerr := deploy.LoadAt(ctx, b.deployRunner(), repoPath, sha, b.cfg.ProjectDeployManifestPath(project))
		if lerr == nil && m != nil {
			out.Deploy.Services = slices.Sorted(maps.Keys(m.Services))
		}
	}
	return out, nil
}

func (b *Bot) OpsWorkflows(ctx context.Context, cred agentauth.Cred, owner, repo string) (agentapi.OpsWorkflowsResult, error) {
	if b == nil || b.cfg == nil {
		return agentapi.OpsWorkflowsResult{}, fmt.Errorf("operations are not available")
	}
	repoPath, _, gotOwner, gotRepo, err := b.operateRepo(ctx, cred.Project, owner, repo)
	if err != nil {
		return agentapi.OpsWorkflowsResult{}, b.scrubProject(cred.Project, err)
	}
	list, err := ghpr.ListWorkflowsWith(ctx, b.ghRun(), repoPath)
	if err != nil {
		return agentapi.OpsWorkflowsResult{}, b.scrubProject(cred.Project, err, repoPath)
	}
	pref := b.cfg.ProjectPrimaryBranch(cred.Project)
	primaryRef, err := ghpr.ResolveOriginPrimaryRef(ctx, b.ghRun(), repoPath, pref)
	if err != nil {
		return agentapi.OpsWorkflowsResult{}, b.scrubProject(cred.Project, err, repoPath)
	}
	var found []agentapi.OpsWorkflow
	for _, w := range list {
		if !workflowRunnable(w.State) {
			continue
		}
		raw, readErr := ghpr.WorkflowFileAtRefWith(ctx, b.ghRun(), repoPath, primaryRef, w.Path)
		if readErr != nil {
			continue
		}
		spec, parseErr := ghpr.ParseWorkflowDispatch(raw)
		if parseErr != nil || !spec.Dispatchable {
			continue
		}
		item := agentapi.OpsWorkflow{ID: w.ID, Name: w.Name, Path: w.Path}
		for _, in := range spec.Inputs {
			item.Inputs = append(item.Inputs, agentapi.OpsWorkflowInput{
				Name: in.Name, Description: in.Description, Required: in.Required,
				Default: in.Default, Type: in.Type, Options: slices.Clone(in.Options),
			})
		}
		if branches, locked := b.cfg.ActionsDispatchBranches(cred.Project, gotOwner, gotRepo, w.Path); locked {
			item.Locked = true
			item.AllowedRefs = branches
		}
		found = append(found, item)
	}
	total := len(found)
	truncated := total > opsWorkflowCap
	if truncated {
		found = found[:opsWorkflowCap]
	}
	if found == nil {
		found = []agentapi.OpsWorkflow{}
	}
	return agentapi.OpsWorkflowsResult{
		Workflows: found, Total: total, Truncated: truncated, PrimaryRef: primaryRef,
	}, nil
}

func (b *Bot) OpsLog(ctx context.Context, cred agentauth.Cred, ref, owner, repo string, limit int) (agentapi.OpsLogResult, error) {
	if b == nil || b.cfg == nil {
		return agentapi.OpsLogResult{}, fmt.Errorf("operations are not available")
	}
	ref = strings.TrimSpace(ref)
	if err := rejectOpsRef(ref); err != nil {
		return agentapi.OpsLogResult{}, err
	}
	repoPath, _, _, _, err := b.operateRepo(ctx, cred.Project, owner, repo)
	if err != nil {
		return agentapi.OpsLogResult{}, b.scrubProject(cred.Project, err)
	}
	if ref == "" {
		ref = gitworktree.PrimaryStartRef(ctx, repoPath, b.cfg.ProjectPrimaryBranch(cred.Project))
	}
	rows, err := ghpr.ListCommitsWith(ctx, b.ghRun(), repoPath, ghpr.CommitListOpts{Ref: ref, Limit: opsLogLimit(limit)})
	if err != nil {
		return agentapi.OpsLogResult{}, b.scrubProject(cred.Project, err, repoPath)
	}
	out := agentapi.OpsLogResult{Ref: ref, Commits: make([]agentapi.OpsCommit, 0, len(rows))}
	for _, c := range rows {
		out.Commits = append(out.Commits, agentapi.OpsCommit{
			SHA: c.SHA, Short: operateShort(c.SHA), Subject: c.Subject,
		})
	}
	return out, nil
}

func (b *Bot) OpsDeploy(ctx context.Context, cred agentauth.Cred, req agentapi.OpsDeployRequest) (agentapi.OpsDeployResult, error) {
	detail := map[string]any{
		"service": strings.TrimSpace(req.Service),
		"env":     strings.TrimSpace(req.Env),
		"ref":     strings.TrimSpace(req.Ref),
	}
	fail := func(err error) (agentapi.OpsDeployResult, error) {
		b.auditOperate("deploy.trigger", cred, err, detail)
		return agentapi.OpsDeployResult{}, b.scrubProject(cred.Project, err)
	}
	if b == nil || b.cfg == nil {
		return fail(fmt.Errorf("operations are not available"))
	}
	if strings.TrimSpace(req.Service) == "" || strings.TrimSpace(req.Env) == "" {
		return fail(fmt.Errorf("service and env are required"))
	}
	if !b.cfg.FeatureDeploy() {
		return fail(fmt.Errorf("deploys are not enabled"))
	}
	if b.deploys == nil {
		return fail(fmt.Errorf("deploys are not available"))
	}
	repoPath, slug, _, _, err := b.operateRepo(ctx, cred.Project, req.Owner, req.Repo)
	if err != nil {
		return fail(err)
	}
	run, err := b.deploys.Trigger(ctx, deploy.TriggerRequest{
		Project:   cred.Project,
		Repo:      slug,
		RepoPath:  repoPath,
		Service:   strings.TrimSpace(req.Service),
		Env:       strings.TrimSpace(req.Env),
		Ref:       strings.TrimSpace(req.Ref),
		ExpectSHA: strings.TrimSpace(req.ExpectSHA),
		Actor:     b.operateActor(cred),
		Caps:      b.cfg.ResolveCapabilities(cred.Project, cred.ActorID),
	})
	if run.SHA != "" {
		detail["sha"] = run.SHA
	}
	if run.ID != "" {
		detail["runId"] = run.ID
	}
	if err != nil {
		return fail(err)
	}
	b.auditOperate("deploy.trigger", cred, nil, detail)
	return agentapi.OpsDeployResult{
		RunID:   run.ID,
		SHA:     run.SHA,
		Short:   run.ShortSHA,
		Service: run.Service,
		Env:     run.Env,
		Ref:     run.Ref,
		URL:     b.operateURL(cred.Project, "/deploys/"+url.PathEscape(run.ID)),
	}, nil
}

func (b *Bot) OpsDispatch(ctx context.Context, cred agentauth.Cred, req agentapi.OpsDispatchRequest) (agentapi.OpsDispatchResult, error) {
	workflow := strings.TrimSpace(req.Workflow)
	ref := strings.TrimSpace(req.Ref)
	detail := map[string]any{"workflow": workflow, "ref": ref}
	fail := func(err error) (agentapi.OpsDispatchResult, error) {
		b.auditOperate(audit.ActionActionsDispatch, cred, err, detail)
		return agentapi.OpsDispatchResult{}, b.scrubProject(cred.Project, err)
	}
	if b == nil || b.cfg == nil {
		return fail(fmt.Errorf("operations are not available"))
	}
	if !b.cfg.FeatureGitHubWrites() {
		return fail(fmt.Errorf("github actions dispatch is not enabled"))
	}
	if !b.cfg.ResolveCapabilities(cred.Project, cred.ActorID).GithubWrites {
		return fail(fmt.Errorf("not allowed to dispatch workflows for this project"))
	}
	if workflow == "" {
		return fail(fmt.Errorf("workflow is required"))
	}
	if ref == "" {
		return fail(fmt.Errorf("branch (ref) is required"))
	}
	if err := rejectOpsRef(ref); err != nil {
		return fail(err)
	}
	repoPath, _, gotOwner, gotRepo, err := b.operateRepo(ctx, cred.Project, req.Owner, req.Repo)
	if err != nil {
		return fail(err)
	}
	detail["owner"] = gotOwner
	detail["repo"] = gotRepo
	list, err := ghpr.ListWorkflowsWith(ctx, b.ghRun(), repoPath)
	if err != nil {
		return fail(err)
	}
	found := findWorkflow(list, workflow)
	if found == nil {
		return fail(fmt.Errorf("workflow %q not found", workflow))
	}
	if !workflowRunnable(found.State) {
		return fail(fmt.Errorf("workflow %q is disabled", filepath.Base(found.Path)))
	}
	detail["workflow"] = found.Path
	pref := b.cfg.ProjectPrimaryBranch(cred.Project)
	primaryRef, err := ghpr.ResolveOriginPrimaryRef(ctx, b.ghRun(), repoPath, pref)
	if err != nil {
		return fail(err)
	}
	raw, err := ghpr.WorkflowFileAtRefWith(ctx, b.ghRun(), repoPath, primaryRef, found.Path)
	if err != nil {
		return fail(err)
	}
	spec, parseErr := ghpr.ParseWorkflowDispatch(raw)
	if parseErr != nil || !spec.Dispatchable {
		bad := fmt.Errorf("workflow %q is not dispatchable", filepath.Base(found.Path))
		if parseErr != nil {
			bad = fmt.Errorf("workflow %q is not dispatchable: %w", filepath.Base(found.Path), parseErr)
		}
		return fail(bad)
	}
	if branches, locked := b.cfg.ActionsDispatchBranches(cred.Project, gotOwner, gotRepo, found.Path); locked {
		if !slices.Contains(branches, ref) {
			return fail(fmt.Errorf("ref %q is not allowed for this workflow (allowed: %s)", ref, strings.Join(branches, ", ")))
		}
	}
	inputs, err := normalizeDispatchInputs(spec, req.Inputs)
	if err != nil {
		return fail(err)
	}
	if len(inputs) > 0 {
		detail["inputs"] = len(inputs)
	}
	if err := ghpr.DispatchWorkflowWith(ctx, b.ghRun(), repoPath, gotOwner, gotRepo, found.Path, ref, inputs); err != nil {
		return fail(err)
	}
	b.auditOperate(audit.ActionActionsDispatch, cred, nil, detail)
	return agentapi.OpsDispatchResult{Status: "dispatched", Workflow: found.Path, Ref: ref}, nil
}

func (b *Bot) OpsCherryPick(ctx context.Context, cred agentauth.Cred, req agentapi.OpsCherryPickRequest) (agentapi.OpsCherryPickResult, error) {
	target := strings.TrimSpace(req.Target)
	shas := cleanSHAs(req.SHAs)
	detail := map[string]any{"target": target}
	fail := func(err error, extra ...string) (agentapi.OpsCherryPickResult, error) {
		b.auditOperate(audit.ActionGitCherryPick, cred, err, detail)
		return agentapi.OpsCherryPickResult{}, b.scrubProject(cred.Project, err, extra...)
	}
	if b == nil || b.cfg == nil {
		return fail(fmt.Errorf("operations are not available"))
	}
	if target == "" {
		return fail(fmt.Errorf("cherry-pick target is required"))
	}
	if len(shas) == 0 {
		return fail(fmt.Errorf("at least one commit is required"))
	}
	if !b.cfg.ResolveCapabilities(cred.Project, cred.ActorID).CanShip() {
		return fail(fmt.Errorf("not allowed to cherry-pick for this project"))
	}
	// Allowlist before any git call. Primary is dropped after the repo resolves.
	if !slices.Contains(b.cfg.ProjectCherryPickTargets(cred.Project), target) {
		return fail(fmt.Errorf("cherry-pick target is not allowlisted"))
	}
	if b.cfg.ProjectForcePushTargets(cred.Project) {
		return fail(fmt.Errorf("this project force-pushes onto target branches; cherry-pick is refused"))
	}
	repoPath, _, gotOwner, gotRepo, err := b.operateRepo(ctx, cred.Project, req.Owner, req.Repo)
	if err != nil {
		return fail(err)
	}
	detail["owner"] = gotOwner
	detail["repo"] = gotRepo
	if !slices.Contains(b.operateCherryTargets(ctx, cred.Project, repoPath), target) {
		return fail(fmt.Errorf("refuse to cherry-pick onto project primary %q", target), repoPath)
	}
	dataDir := strings.TrimSpace(b.cfg.DataDir)
	if dataDir == "" {
		return fail(fmt.Errorf("operations are not available"), repoPath)
	}
	if open, ok := gitworktree.OpenJobForTarget(dataDir, cred.Project, repoPath, target); ok {
		return agentapi.OpsCherryPickResult{
			Status:   gitworktree.JobStatusConflict,
			Target:   target,
			JobID:    open.ID,
			URL:      b.operateURL(cred.Project, "/cherrypick/"+url.PathEscape(open.ID)),
			Files:    operatePublicFiles(open.Files),
			Existing: true,
		}, nil
	}
	id, err := operateJobID()
	if err != nil {
		return fail(err, repoPath)
	}
	checkout := gitworktree.CherryPickCheckoutPath(dataDir, cred.Project, id)
	res, err := gitworktree.CherryPick(ctx, gitworktree.CherryPickOpts{
		Repo:             repoPath,
		Checkout:         checkout,
		Target:           target,
		SHAs:             shas,
		PreferredPrimary: b.cfg.ProjectPrimaryBranch(cred.Project),
	})
	if res.Picked != nil {
		detail["picked"] = res.Picked
	}
	if res.Skipped != nil {
		detail["skipped"] = res.Skipped
	}
	if res.FromSHA != "" {
		detail["from"] = operateShort(res.FromSHA)
	}
	if res.ToSHA != "" {
		detail["to"] = operateShort(res.ToSHA)
	}
	detail["noop"] = res.Noop
	if cerr, ok := errors.AsType[*gitworktree.ConflictError](err); ok {
		now := time.Now().UTC()
		job := gitworktree.Job{
			ID: id, Project: cred.Project, Owner: gotOwner, Repo: gotRepo,
			RepoPath: repoPath, Checkout: checkout, Target: target,
			FromSHA: res.FromSHA, Picked: res.Picked, Skipped: res.Skipped,
			Current: cerr.SHA, Remaining: cerr.Remaining, Files: cerr.Files,
			Status: gitworktree.JobStatusConflict, ActorID: cred.ActorID,
			CreatedAt: now, UpdatedAt: now,
		}
		detail["job"] = id
		detail["files"] = len(cerr.Files)
		if saveErr := gitworktree.SaveJob(dataDir, job); saveErr != nil {
			_ = gitworktree.AbortCherryPick(ctx, repoPath, checkout)
			return fail(saveErr, repoPath, checkout)
		}
		b.auditOperate(audit.ActionGitCherryPick, cred, nil, detail)
		return agentapi.OpsCherryPickResult{
			Status:  gitworktree.JobStatusConflict,
			Target:  target,
			From:    res.FromSHA,
			To:      res.ToSHA,
			Picked:  slices.Clone(res.Picked),
			Skipped: slices.Clone(res.Skipped),
			JobID:   id,
			URL:     b.operateURL(cred.Project, "/cherrypick/"+url.PathEscape(id)),
			Files:   operatePublicFiles(cerr.Files),
		}, nil
	}
	if err != nil {
		return fail(err, repoPath, checkout)
	}
	b.auditOperate(audit.ActionGitCherryPick, cred, nil, detail)
	status := "pushed"
	if res.Noop {
		status = "noop"
	}
	return agentapi.OpsCherryPickResult{
		Status:  status,
		Target:  target,
		From:    res.FromSHA,
		To:      res.ToSHA,
		Picked:  slices.Clone(res.Picked),
		Skipped: slices.Clone(res.Skipped),
	}, nil
}

// normalizeDispatchInputs matches the web form: unknown names are dropped,
// required inputs with no value and no default fail, booleans accept true/1/on,
// and a choice must be one of its options.
func normalizeDispatchInputs(spec ghpr.DispatchSpec, raw map[string]string) (map[string]string, error) {
	if len(spec.Inputs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(spec.Inputs))
	for _, inp := range spec.Inputs {
		rawVal, present := "", false
		if raw != nil {
			rawVal, present = raw[inp.Name]
		}
		typ := strings.ToLower(strings.TrimSpace(inp.Type))
		if typ == "boolean" {
			if !present || strings.TrimSpace(rawVal) == "" {
				if inp.Default != "" {
					out[inp.Name] = inp.Default
				} else {
					out[inp.Name] = "false"
				}
				continue
			}
			v := strings.TrimSpace(rawVal)
			if v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on") {
				out[inp.Name] = "true"
			} else {
				out[inp.Name] = "false"
			}
			continue
		}
		val := strings.TrimSpace(rawVal)
		if !present || val == "" {
			if inp.Default != "" {
				out[inp.Name] = inp.Default
				continue
			}
			if inp.Required {
				return nil, fmt.Errorf("input %q is required", inp.Name)
			}
			continue
		}
		if typ == "choice" && len(inp.Options) > 0 && !slices.Contains(inp.Options, val) {
			return nil, fmt.Errorf("input %q must be one of %s", inp.Name, strings.Join(inp.Options, ", "))
		}
		out[inp.Name] = val
	}
	return out, nil
}

func (b *Bot) operateRepo(ctx context.Context, project, owner, repo string) (path, slug, gotOwner, gotRepo string, err error) {
	if b == nil || b.cfg == nil {
		return "", "", "", "", fmt.Errorf("operations are not available")
	}
	project = strings.TrimSpace(project)
	root, ok := b.cfg.ProjectPath(project)
	if !ok || strings.TrimSpace(root) == "" {
		return "", "", "", "", fmt.Errorf("unknown project %q", project)
	}
	catalog, cErr := b.cfg.ProjectRepoCatalogWith(ctx, project, nil)
	if cErr != nil || len(catalog) == 0 {
		if !gitworktree.IsRepo(root) {
			return "", "", "", "", fmt.Errorf("project path is not a git repository")
		}
		return root, "", "", "", nil
	}
	active, pErr := config.ResolveRepoPicker(catalog, owner, repo)
	if pErr != nil {
		return "", "", "", "", pErr
	}
	resolved, rErr := gitworktree.ResolveLocalRepo(ctx, root, active.Owner, active.Repo)
	if rErr != nil {
		return "", "", "", "", rErr
	}
	return resolved, active.Slug(), active.Owner, active.Repo, nil
}

func (b *Bot) operateCherryTargets(ctx context.Context, project, repoPath string) []string {
	if b == nil || b.cfg == nil {
		return nil
	}
	targets := slices.Clone(b.cfg.ProjectCherryPickTargets(project))
	if repoPath != "" && gitworktree.IsRepo(repoPath) {
		if name, _, err := gitworktree.ResolvePrimaryBranch(ctx, repoPath, b.cfg.ProjectPrimaryBranch(project)); err == nil && name != "" {
			targets = slices.DeleteFunc(targets, func(t string) bool { return t == name })
		}
	}
	return targets
}

func (b *Bot) operateActor(cred agentauth.Cred) deploy.Actor {
	name := ""
	if b != nil && b.sessions != nil {
		if e, ok := b.sessions.Get(cred.ThreadID); ok && e.CreatedBy == cred.ActorID {
			name = e.CreatedByName
		}
	}
	return deploy.Actor{ID: cred.ActorID, Name: name}
}

func (b *Bot) deployRunner() deploy.Runner {
	if r := b.ghRun(); r != nil {
		return deploy.Runner(r)
	}
	return nil
}

func (b *Bot) operateURL(project, rest string) string {
	rel := "/projects/" + url.PathEscape(project) + rest
	base := ""
	if b != nil && b.cfg != nil {
		base = strings.TrimRight(strings.TrimSpace(b.cfg.WebPublicBaseURLValue()), "/")
	}
	if base == "" {
		return rel
	}
	return base + rel
}

func (b *Bot) auditOperate(action string, cred agentauth.Cred, err error, detail map[string]any) {
	if b == nil || b.audit == nil {
		return
	}
	d := make(map[string]any, len(detail)+3)
	for k, v := range detail {
		if v == nil || v == "" {
			continue
		}
		d[k] = v
	}
	d["source"] = "agent"
	if cred.ThreadID != "" {
		d["threadId"] = cred.ThreadID
	}
	if cred.Project != "" {
		d["project"] = cred.Project
	}
	ev := audit.Event{Action: action, Actor: cred.ActorID, Detail: d, OK: err == nil}
	if err != nil {
		ev.Error = err.Error()
		ev.OK = false
	}
	_ = b.audit.Append(ev)
}

func (b *Bot) scrubProject(project string, err error, extra ...string) error {
	if err == nil {
		return nil
	}
	if b != nil && b.cfg != nil {
		if root, ok := b.cfg.ProjectPath(project); ok {
			extra = append(extra, root)
		}
	}
	return b.scrubOps(err, extra...)
}

func (b *Bot) scrubOps(err error, extra ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	var roots []string
	if b != nil && b.cfg != nil {
		if d := strings.TrimSpace(b.cfg.DataDir); d != "" {
			roots = append(roots, d)
		}
	}
	roots = append(roots, extra...)
	changed := false
	for _, p := range roots {
		p = strings.TrimSpace(p)
		if len(p) < 2 {
			continue
		}
		if strings.Contains(msg, p) {
			msg = strings.ReplaceAll(msg, p, "[path]")
			changed = true
		}
	}
	if !changed {
		return err
	}
	return errors.New(msg)
}

func findWorkflow(list []ghpr.WorkflowInfo, workflow string) *ghpr.WorkflowInfo {
	workflow = strings.TrimSpace(workflow)
	if workflow == "" {
		return nil
	}
	base := filepath.Base(workflow)
	for i := range list {
		w := &list[i]
		if w.Path == workflow || filepath.Base(w.Path) == base ||
			strings.EqualFold(filepath.Base(w.Path), base) ||
			strconv.FormatInt(w.ID, 10) == workflow {
			return w
		}
	}
	return nil
}

func workflowRunnable(state string) bool {
	state = strings.TrimSpace(state)
	return state == "" || strings.EqualFold(state, "active")
}

// rejectOpsRef refuses a revision git or gh would parse as a flag.
// ops_log and workflow dispatch pass the ref as its own argv.
func rejectOpsRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") || strings.ContainsAny(ref, " \t\r\n\x00") {
		return fmt.Errorf("invalid ref %q", ref)
	}
	return nil
}

func cleanSHAs(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func operateJobID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "cp_" + hex.EncodeToString(buf[:]), nil
}

func operateShort(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

func operatePublicFiles(files []string) []string {
	var out []string
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" || filepath.IsAbs(f) || strings.Contains(f, "..") {
			continue
		}
		out = append(out, filepath.ToSlash(f))
	}
	return out
}

func opsLogLimit(n int) int {
	if n <= 0 {
		return opsLogDefault
	}
	return min(n, opsLogMax)
}
