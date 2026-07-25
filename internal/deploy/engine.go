package deploy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

// StopTimeout bounds how long shutdown waits for in-flight steps to end.
const StopTimeout = 10 * time.Second

// dedupeWindow suppresses a duplicate trigger of the same commit on the same
// lane. Keyed on the SHA alone, not the actor: the motivating case is two
// operators clicking the same prod cell within seconds of each other.
const dedupeWindow = 10 * time.Second

var (
	// ErrShuttingDown mirrors the bot's claim gate.
	ErrShuttingDown = errors.New("deploy: shutting down")
	// ErrNotEnabled reports deploys off for the project.
	ErrNotEnabled = errors.New("deploy: deploys are not enabled for this project")
	// ErrForbidden reports a failed capability gate.
	ErrForbidden = errors.New("deploy: you do not have permission to deploy to this environment")
	// ErrTooManyRunning reports the host-wide concurrency cap.
	ErrTooManyRunning = errors.New("deploy: too many deploys are already running")
)

// Actor identifies who triggered a deploy.
type Actor struct {
	ID   string
	Name string
}

// MaxQueuePerLane bounds pending deploys behind an active one, mirroring the
// bot's per-thread follow-up cap.
const MaxQueuePerLane = 5

// ErrQueueFull reports the per-lane pending cap.
var ErrQueueFull = errors.New("deploy: too many deploys are already queued for this service and environment")

// laneState is one service+environment's in-memory claim.
//
// queued holds run ids waiting behind activeID, FIFO. Unlike the bot's social
// queue this never replaces an entry by author: two deploys of one service at
// different commits are not interchangeable.
type laneState struct {
	activeID string
	cancel   context.CancelFunc
	queued   []string
}

// Engine owns deploy execution: policy, lane claims, and the run goroutines.
type Engine struct {
	cfg   *config.Config
	store *Store
	git   Runner
	root  string // deploys root (checkouts live under it)
	host  string

	notifier   Notifier
	publicBase string

	mu    sync.Mutex
	lanes map[string]*laneState
	// laneRepo remembers the checkout a lane deploys from, so a promoted run
	// does not have to re-resolve it.
	laneRepo map[string]string

	// rev advances on every durable transition so the SSE fingerprint changes
	// even when a run starts and finishes inside one 2s tick — otherwise a
	// passive viewer's board never refreshes for that deploy.
	rev      atomic.Uint64
	stopping atomic.Bool
	wg       sync.WaitGroup
}

// NewEngine builds an Engine over dataDir.
func NewEngine(cfg *config.Config, dataDir string) (*Engine, error) {
	store, err := NewStore(dataDir)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	return &Engine{
		cfg:      cfg,
		store:    store,
		root:     filepath.Join(dataDir, "deploys"),
		host:     host,
		lanes:    map[string]*laneState{},
		laneRepo: map[string]string{},
	}, nil
}

// Store exposes the run store for read surfaces.
func (e *Engine) Store() *Store { return e.store }

// Rev is the monotonic revision, for live fingerprints.
func (e *Engine) Rev() uint64 { return e.rev.Load() }

func (e *Engine) bump() { e.rev.Add(1) }

// SetGitRunner injects a git runner (tests).
func (e *Engine) SetGitRunner(r Runner) { e.git = r }

// TriggerRequest is one deploy request.
type TriggerRequest struct {
	Project string
	// Repo is the "owner/repo" slug, or empty for a single-checkout project.
	Repo     string
	RepoPath string // resolved local checkout
	Service  string
	Env      string
	Ref      string
	Actor    Actor
	Caps     config.Capabilities
	// RedeployOf, when set, replays that run's frozen steps at its SHA and
	// waives the ref check (see CheckRefAllowed).
	RedeployOf string
}

// Trigger validates and starts a deploy, returning the created run.
func (e *Engine) Trigger(ctx context.Context, req TriggerRequest) (Run, error) {
	if e.stopping.Load() {
		return Run{}, ErrShuttingDown
	}
	if !e.cfg.ProjectDeployEnabled(req.Project) {
		return Run{}, ErrNotEnabled
	}
	envCfg, ok := e.cfg.ProjectDeployEnv(req.Project, req.Env)
	if !ok {
		return Run{}, fmt.Errorf("deploy: environment %q is not configured for %s", req.Env, req.Project)
	}
	if !CapabilityAllows(req.Caps, envCfg.RequireCapability) {
		return Run{}, ErrForbidden
	}

	var (
		sha     string
		steps   []ResolvedStep
		dir     string
		waived  bool
		srcRun  Run
		hasSrc  bool
		refUsed = strings.TrimSpace(req.Ref)
	)

	if req.RedeployOf != "" {
		var err error
		srcRun, hasSrc, err = e.store.Get(req.RedeployOf)
		if err != nil || !hasSrc {
			return Run{}, fmt.Errorf("deploy: cannot redeploy unknown run %q", req.RedeployOf)
		}
		// Replay exactly what ran, at the same commit. Re-resolving the manifest
		// would defeat the point: the branch may have moved on.
		sha, refUsed = srcRun.SHA, srcRun.Ref
		steps = make([]ResolvedStep, 0, len(srcRun.Steps))
		for _, s := range srcRun.Steps {
			steps = append(steps, ResolvedStep{Name: s.Name, Command: s.Command, TimeoutMs: s.TimeoutMs})
		}
		waived = srcRun.Lane() == LaneKey(req.Project, req.Repo, req.Service, req.Env) && srcRun.Status == StatusSucceeded
		if !waived {
			if err := e.checkRef(ctx, req, refUsed); err != nil {
				return Run{}, err
			}
		}
	} else {
		if refUsed == "" {
			refUsed = gitworktree.PrimaryStartRef(ctx, req.RepoPath)
		}
		if err := e.checkRef(ctx, req, refUsed); err != nil {
			return Run{}, err
		}
		var err error
		sha, err = gitworktree.ResolveRefSHA(ctx, req.RepoPath, refUsed)
		if err != nil {
			return Run{}, fmt.Errorf("deploy: cannot resolve %s: %w", refUsed, err)
		}
		// The manifest is read straight from the commit — no checkout needed to
		// know what would run, so a rejected request costs nothing on disk.
		m, err := LoadAt(ctx, e.gitRunner(), req.RepoPath, sha, e.cfg.ProjectDeployManifestPath(req.Project))
		if err != nil {
			return Run{}, err
		}
		if !slices.Contains(m.Environments, req.Env) {
			return Run{}, fmt.Errorf("deploy: the manifest at %s does not declare environment %q", shortRev(sha), req.Env)
		}
		steps, err = m.Resolve(req.Service, req.Env)
		if err != nil {
			return Run{}, err
		}
		dir = m.Dir(req.Service)
	}

	if len(steps) == 0 {
		return Run{}, fmt.Errorf("deploy: nothing to run for %s/%s", req.Service, req.Env)
	}
	// Honour the environment's ceiling over whatever the manifest asked for.
	ceiling := envCfg.StepTimeoutMaxMsValue()
	for i := range steps {
		if steps[i].TimeoutMs > ceiling {
			steps[i].TimeoutMs = ceiling
		}
	}

	lane := LaneKey(req.Project, req.Repo, req.Service, req.Env)
	if dup, ok := e.recentDuplicate(lane, sha); ok {
		return dup, nil
	}

	run := Run{
		ID:        NewRunID(),
		Project:   req.Project,
		Repo:      req.Repo,
		Service:   req.Service,
		Env:       req.Env,
		Ref:       refUsed,
		SHA:       sha,
		ShortSHA:  shortSHAOf(sha),
		Status:    StatusPending,
		ActorID:   req.Actor.ID,
		ActorName: req.Actor.Name,
		QueuedAt:  nowStamp(),
		Host:      e.host,
	}
	if waived {
		run.RefCheck = "waived_redeploy"
	}
	if req.RedeployOf != "" {
		run.RedeployOf = req.RedeployOf
		if dir == "" {
			dir = srcRun.serviceDir()
		}
	}
	run.ServiceDir = dir
	for _, s := range steps {
		run.Steps = append(run.Steps, StepRecord{
			Name: s.Name, Command: s.Command, TimeoutMs: s.TimeoutMs, Status: StatusPending,
		})
	}

	runCtx, cancel := context.WithCancel(context.Background())
	claimed, err := e.claimOrQueue(lane, run, cancel, req.RepoPath)
	if err != nil {
		cancel()
		return Run{}, err
	}
	if !claimed {
		// Queued: it will be promoted when the active run finishes. The cancel
		// belongs to the promotion, not to this call.
		cancel()
		return run, nil
	}

	e.wg.Add(1)
	go e.execute(runCtx, run, lane, req.RepoPath)
	return run, nil
}

// claim takes a lane and persists the run inside the same lock.
//
// The durable write happens inside the RAM lock and a failed write rolls the
// RAM change back, so a claim fails rather than leaving memory and disk
// disagreeing — the same discipline as bot.claimOrEnqueueInternal.
func (e *Engine) claimOrQueue(lane string, run Run, cancel context.CancelFunc, repoPath string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopping.Load() {
		return false, ErrShuttingDown
	}
	if st, ok := e.lanes[lane]; ok && st.activeID != "" {
		if len(st.queued) >= MaxQueuePerLane {
			return false, ErrQueueFull
		}
		st.queued = append(st.queued, run.ID)
		if err := e.store.Create(run); err != nil {
			// Roll the RAM change back so a failed write cannot leave memory and
			// disk disagreeing about what is queued.
			st.queued = st.queued[:len(st.queued)-1]
			return false, err
		}
		e.laneRepo[lane] = repoPath
		e.bump()
		return false, nil
	}
	if cap := e.cfg.MaxConcurrentDeploysValue(); cap > 0 {
		running := 0
		for _, st := range e.lanes {
			if st.activeID != "" {
				running++
			}
		}
		if running >= cap {
			return false, ErrTooManyRunning
		}
	}
	prev := e.lanes[lane]
	st := &laneState{activeID: run.ID, cancel: cancel}
	if prev != nil {
		st.queued = prev.queued
	}
	e.lanes[lane] = st
	if err := e.store.Create(run); err != nil {
		if prev != nil {
			e.lanes[lane] = prev
		} else {
			delete(e.lanes, lane)
		}
		return false, err
	}
	e.laneRepo[lane] = repoPath
	e.bump()
	return true, nil
}

// promoteNext starts the queue head, if any. Called when a run finishes.
func (e *Engine) promoteNext(lane, finishedID string) {
	e.mu.Lock()
	st, ok := e.lanes[lane]
	if !ok || st.activeID != finishedID {
		e.mu.Unlock()
		return
	}
	repoPath := e.laneRepo[lane]
	for {
		if len(st.queued) == 0 || e.stopping.Load() {
			delete(e.lanes, lane)
			delete(e.laneRepo, lane)
			e.mu.Unlock()
			return
		}
		nextID := st.queued[0]
		st.queued = st.queued[1:]
		next, found, err := e.store.Get(nextID)
		if err != nil || !found || next.Status != StatusPending {
			// A queued run cancelled while waiting is simply skipped.
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		st.activeID = nextID
		st.cancel = cancel
		e.mu.Unlock()
		e.wg.Add(1)
		go e.execute(ctx, next, lane, repoPath)
		e.bump()
		return
	}
}

// recentDuplicate returns an in-flight run of the same commit on the same lane.
func (e *Engine) recentDuplicate(lane, sha string) (Run, bool) {
	runs, err := e.store.ListForLane(lane, 5)
	if err != nil {
		return Run{}, false
	}
	cutoff := time.Now().UTC().Add(-dedupeWindow)
	for _, r := range runs {
		if r.SHA != sha || r.Status.Terminal() {
			continue
		}
		if parseTime(r.QueuedAt).After(cutoff) {
			return r, true
		}
	}
	return Run{}, false
}

// checkRef enforces the per-environment ref allowlist. This is the control that
// stops someone pushing a branch that rewrites the pipeline and deploying it to
// production; the manifest limits only bound the blast radius.
func (e *Engine) checkRef(ctx context.Context, req TriggerRequest, ref string) error {
	envCfg, ok := e.cfg.ProjectDeployEnv(req.Project, req.Env)
	if !ok {
		return fmt.Errorf("deploy: environment %q is not configured", req.Env)
	}
	allowed := envCfg.AllowedRefs
	if len(allowed) == 0 {
		// Default: the project primary branch only.
		primary, _, err := gitworktree.ResolvePrimaryBranch(ctx, req.RepoPath)
		if err != nil || primary == "" {
			return fmt.Errorf("deploy: cannot determine the primary branch for %s", req.Project)
		}
		allowed = []string{primary, "origin/" + primary}
	}
	if RefAllowed(ref, allowed) {
		return nil
	}
	return fmt.Errorf("deploy: %q is not an allowed ref for %s (allowed: %s)", ref, req.Env, strings.Join(allowed, ", "))
}

// RefAllowed matches a ref against an allowlist, supporting a trailing "*".
func RefAllowed(ref string, allowed []string) bool {
	ref = strings.TrimSpace(ref)
	bare := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "origin/")
	for _, pat := range allowed {
		pat = strings.TrimSpace(pat)
		if pat == "*" {
			return true
		}
		patBare := strings.TrimPrefix(strings.TrimPrefix(pat, "refs/heads/"), "origin/")
		if strings.HasSuffix(patBare, "*") {
			if strings.HasPrefix(bare, strings.TrimSuffix(patBare, "*")) {
				return true
			}
			continue
		}
		if bare == patBare {
			return true
		}
	}
	return false
}

// CapabilityAllows checks an actor against an environment's required capability.
// Empty means builder-class, matching every other money/risk gate in the product.
func CapabilityAllows(caps config.Capabilities, required string) bool {
	switch strings.TrimSpace(required) {
	case "":
		return caps.CanShip()
	case "approve":
		return caps.Approve
	case "adminProject":
		return caps.AdminProject
	case "safeOps":
		return caps.SafeOps
	case "merge":
		return caps.Merge
	}
	// An unknown requirement fails closed rather than silently degrading to the
	// builder gate.
	return false
}

// Cancel stops a running deploy.
func (e *Engine) Cancel(runID string) error {
	// Stamp the intent first, then cancel — the same ordering as the bot's
	// cancel, so a crash between the two still reads as "was being cancelled".
	err := e.store.Update(runID, func(r *Run) error {
		if r.Status.Terminal() {
			return ErrSkipUpdate
		}
		r.Status = StatusCancelling
		return nil
	})
	if err != nil {
		return err
	}
	e.mu.Lock()
	for lane, st := range e.lanes {
		if st.activeID == runID && st.cancel != nil {
			st.cancel()
		}
		// A queued run has no goroutine to cancel: drop it from the lane and
		// mark it terminal, or promoteNext would later start something the
		// operator already stopped.
		if i := slices.Index(st.queued, runID); i >= 0 {
			st.queued = slices.Delete(st.queued, i, i+1)
			e.lanes[lane] = st
			e.mu.Unlock()
			_ = e.store.Update(runID, func(r *Run) error {
				if r.Status.Terminal() {
					return ErrSkipUpdate
				}
				r.Status = StatusCancelled
				r.Error = "cancelled before it started"
				r.EndedAt = nowStamp()
				return nil
			})
			e.bump()
			return nil
		}
	}
	e.mu.Unlock()
	e.bump()
	return nil
}

// execute runs every step of a deploy, then cleans up.
func (e *Engine) execute(ctx context.Context, run Run, lane, repoPath string) {
	defer e.wg.Done()
	defer e.promoteNext(lane, run.ID)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("error: panic in deploy %s: %v", run.ID, r)
			_ = e.store.Update(run.ID, func(rec *Run) error {
				rec.Status = StatusFailed
				rec.Error = fmt.Sprintf("internal error: %v", r)
				rec.EndedAt = nowStamp()
				return nil
			})
			e.bump()
		}
	}()

	checkout := filepath.Join(e.root, "checkouts", sanitizeSegment(run.Project), run.ID)
	// Context state first, exactly as in RunStep: a cancelled deploy kills
	// whatever git or shell command was in flight, and reporting that command's
	// failure would tell the operator the deploy broke when they stopped it.
	cleaned := false
	cleanup := func() {
		if !cleaned {
			cleaned = true
			e.cleanupCheckout(repoPath, checkout)
		}
	}
	// Cleanup runs before the terminal status is written, so a run is never
	// observable as finished while its checkout is still on disk.
	done := func(status Status, msg string) {
		cleanup()
		e.finishRun(run.ID, status, msg)
	}
	fail := func(msg string) {
		if ctx.Err() != nil {
			done(StatusCancelled, "cancelled")
			return
		}
		done(StatusFailed, msg)
	}

	_ = e.store.Update(run.ID, func(rec *Run) error {
		rec.Status = StatusRunning
		rec.StartedAt = nowStamp()
		rec.CheckoutPath = checkout
		return nil
	})
	e.bump()

	// A fresh detached checkout per run: the shared main checkout is routinely
	// dirty with other agents' edits, and a per-lane checkout reused across runs
	// would carry stale build output into a retry (AddDetached reuses a
	// matching-HEAD worktree with no cleanliness check).
	if err := gitworktree.AddDetached(ctx, repoPath, checkout, run.SHA); err != nil {
		e.skipFrom(run.ID, 0)
		fail("checkout " + shortRev(run.SHA) + ": " + err.Error())
		return
	}
	// Net for the panic path; the normal paths clean up via done/fail.
	defer cleanup()

	stepDir, err := StepDir(checkout, run.ServiceDir)
	if err != nil {
		e.skipFrom(run.ID, 0)
		fail(err.Error())
		return
	}

	envCfg, _ := e.cfg.ProjectDeployEnv(run.Project, run.Env)
	secrets := envCfg.SecretValues()
	envMap := map[string]string{}
	if envCfg != nil {
		envMap = envCfg.Env
	}

	for i := range run.Steps {
		step := run.Steps[i]
		_ = e.store.Update(run.ID, func(rec *Run) error {
			rec.CurrentStep = i
			rec.Steps[i].Status = StatusRunning
			rec.Steps[i].StartedAt = nowStamp()
			return nil
		})
		e.bump()

		logFile, err := e.store.CreateStepLog(run.ID, i, step.Name)
		if err != nil {
			fail("open step log: " + err.Error())
			return
		}
		vars := RunVars{
			Project: run.Project, Service: run.Service, Env: run.Env,
			Ref: run.Ref, SHA: run.SHA, RunID: run.ID, Step: step.Name, Actor: run.ActorName,
		}
		res := RunStep(ctx, StepSpec{
			Name:      step.Name,
			Command:   step.Command,
			Dir:       stepDir,
			Env:       BuildEnv(vars, envMap),
			TimeoutMs: step.TimeoutMs,
		}, logFile, secrets, func(pid int) {
			_ = e.store.Update(run.ID, func(rec *Run) error { rec.PID = pid; return nil })
		})
		_ = logFile.Close()

		status := StatusSucceeded
		if !res.OK() {
			status = StatusFailed
		}
		_ = e.store.Update(run.ID, func(rec *Run) error {
			rec.PID = 0
			rec.Steps[i].Status = status
			rec.Steps[i].ExitCode = res.ExitCode
			rec.Steps[i].EndedAt = nowStamp()
			rec.Steps[i].LogBytes = res.LogBytes
			rec.Steps[i].LogTrunc = res.LogTrunc
			rec.Steps[i].Error = res.Err
			return nil
		})
		e.bump()

		if !res.OK() {
			// Fail fast, but record the untouched steps as skipped: a run whose
			// tail is still "pending" reads as though it might yet continue.
			e.skipFrom(run.ID, i+1)
			if res.Cancelled {
				done(StatusCancelled, "cancelled")
			} else {
				done(StatusFailed, fmt.Sprintf("step %q failed (exit %d)", step.Name, res.ExitCode))
			}
			return
		}
	}
	done(StatusSucceeded, "")
}

func (e *Engine) finishRun(runID string, status Status, msg string) {
	_ = e.store.Update(runID, func(rec *Run) error {
		rec.Status = status
		rec.Error = msg
		rec.EndedAt = nowStamp()
		rec.PID = 0
		return nil
	})
	if status == StatusSucceeded {
		e.markSuperseded(runID)
	}
	e.bump()
	// One notice per finished run, from the single place a run reaches a
	// terminal status. A delivery failure never fails the deploy.
	if r, ok, err := e.store.Get(runID); err == nil && ok {
		e.notifyFinished(r)
	}
}

// skipFrom marks every step from idx onward as skipped.
func (e *Engine) skipFrom(runID string, idx int) {
	_ = e.store.Update(runID, func(rec *Run) error {
		for i := idx; i < len(rec.Steps); i++ {
			if rec.Steps[i].Status == StatusPending {
				rec.Steps[i].Status = StatusSkipped
			}
		}
		return nil
	})
}

// markSuperseded flags older successful runs on the lane, so the board can show
// which commit is actually live.
func (e *Engine) markSuperseded(runID string) {
	run, ok, err := e.store.Get(runID)
	if err != nil || !ok {
		return
	}
	prior, err := e.store.ListForLane(run.Lane(), 0)
	if err != nil {
		return
	}
	for _, p := range prior {
		if p.ID == runID || p.Status != StatusSucceeded || p.SupersededBy != "" {
			continue
		}
		_ = e.store.Update(p.ID, func(rec *Run) error {
			rec.SupersededBy = runID
			return nil
		})
	}
}

// cleanupCheckout removes the ephemeral worktree.
//
// The checkout is detached, so it has no branch and gitworktree's managed-branch
// deletion guard never applies; it also lives outside worktreesRoot so the idle
// sweep cannot see it.
func (e *Engine) cleanupCheckout(repoPath, checkout string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := gitworktree.Remove(ctx, repoPath, checkout, ""); err != nil {
		log.Printf("warn: deploy checkout remove %s: %v", checkout, err)
	}
	// Unconditional: Remove is built around managed branches and a detached
	// checkout has none, so it can return nil having left the directory behind.
	if err := os.RemoveAll(checkout); err != nil {
		log.Printf("warn: deploy checkout rmdir %s: %v", checkout, err)
	}
}

// Stop cancels in-flight deploys and waits for them, then marks whatever is
// still non-terminal as interrupted.
//
// The stopping flag is set first so a trigger landing mid-shutdown is refused
// rather than starting work nothing waits on — the same ordering as Bot.Stop.
func (e *Engine) Stop(ctx context.Context) {
	e.stopping.Store(true)
	e.mu.Lock()
	for _, st := range e.lanes {
		if st.cancel != nil {
			st.cancel()
		}
	}
	e.mu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); e.wg.Wait() }()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("warn: deploy shutdown timed out with runs in flight")
	}
	e.markInterrupted()
}

// markInterrupted stamps non-terminal runs of this host. A deploy is never
// auto-resumed: shell steps are not idempotent, so recovery is an explicit
// human redeploy with the step index visible.
func (e *Engine) markInterrupted() {
	runs, err := e.store.List()
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.Status.Terminal() || (r.Host != "" && r.Host != e.host) {
			continue
		}
		_ = e.store.Update(r.ID, func(rec *Run) error {
			if rec.Status.Terminal() {
				return ErrSkipUpdate
			}
			if rec.Status == StatusPending {
				rec.Status = StatusCancelled
				rec.Error = "cancelled: process restarted before this run started"
			} else {
				rec.Status = StatusInterrupted
				rec.Error = "interrupted by process restart"
			}
			rec.EndedAt = nowStamp()
			rec.PID = 0
			return nil
		})
	}
	e.bump()
}

func (e *Engine) gitRunner() Runner {
	if e.git != nil {
		return e.git
	}
	return execRunner
}

// ActiveRun reports the running run id for a lane, if any.
func (e *Engine) ActiveRun(lane string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.lanes[lane]
	if !ok || st.activeID == "" {
		return "", false
	}
	return st.activeID, true
}

// QueueLen reports how many runs are waiting on a lane.
func (e *Engine) QueueLen(lane string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st, ok := e.lanes[lane]; ok {
		return len(st.queued)
	}
	return 0
}

// ActiveCount reports how many deploys are running.
func (e *Engine) ActiveCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, st := range e.lanes {
		if st.activeID != "" {
			n++
		}
	}
	return n
}

func shortSHAOf(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// sanitizeSegment makes a project name safe as a path segment.
func sanitizeSegment(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
	if out == "" || out == "." || out == ".." {
		return "_unknown"
	}
	return out
}

// SeedRunForTest inserts a run record without executing anything.
//
// Used by tests and by the preview server (GROKWORK_WEB_PREVIEW) so the run
// page, live tail and confirm modal can be reviewed without running a command.
func (e *Engine) SeedRunForTest(r Run, stepLogs map[int]string) (Run, error) {
	if r.ID == "" {
		r.ID = NewRunID()
	}
	if r.Host == "" {
		r.Host = e.host
	}
	if err := e.store.Create(r); err != nil {
		return Run{}, err
	}
	for idx, body := range stepLogs {
		name := ""
		if idx >= 0 && idx < len(r.Steps) {
			name = r.Steps[idx].Name
		}
		f, err := e.store.CreateStepLog(r.ID, idx, name)
		if err != nil {
			return Run{}, err
		}
		if _, err := f.WriteString(body); err != nil {
			_ = f.Close()
			return Run{}, err
		}
		_ = f.Close()
	}
	e.bump()
	return r, nil
}
