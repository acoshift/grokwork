package bot

import (
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// importOpenGitHubPRs binds open GitHub PRs that no session tracks yet.
// Called at the start of each PR-status poll so Ship stays a session-store read.
// A shadow shell left by an earlier cycle is removed first: a multi-repo run
// opens the other repos on feature branches, which the importer treats as
// human PRs until the creating session records them.
func (b *Bot) importOpenGitHubPRs() int {
	if b == nil || b.cfg == nil || b.sessions == nil {
		return 0
	}
	ctx := b.bgContext()
	if ctx.Err() != nil {
		return 0
	}
	b.dropShadowImportedPRs()
	created := 0
	for _, project := range b.cfg.ProjectNames() {
		if ctx.Err() != nil {
			break
		}
		created += b.importOpenGitHubPRsForProject(project)
	}
	return created
}

func (b *Bot) importOpenGitHubPRsForProject(project string) int {
	cwd, ok := b.cfg.ProjectPath(project)
	if !ok || strings.TrimSpace(cwd) == "" {
		return 0
	}
	catalog, err := b.cfg.ProjectRepoCatalogWith(b.bgContext(), project, nil)
	if err != nil {
		log.Printf("pr-import: catalog project=%s: %v", project, err)
		return 0
	}
	if len(catalog) == 0 {
		return 0
	}
	created := 0
	for _, ref := range catalog {
		if b.bgContext().Err() != nil {
			break
		}
		list, listErr := ghpr.ListPRsWith(b.bgContext(), b.ghRun(), cwd, ghpr.PRListOpts{
			Owner: ref.Owner,
			Repo:  ref.Repo,
			State: "open",
			Limit: ghpr.DefaultPRListLimit,
		})
		if listErr != nil {
			log.Printf("pr-import: list project=%s repo=%s/%s: %v", project, ref.Owner, ref.Repo, listErr)
			continue
		}
		for _, item := range list {
			if b.shouldSkipPRImport(project, item) {
				continue
			}
			unitID, ok, impErr := b.importTrackedPR(project, item)
			if impErr != nil {
				log.Printf("pr-import: create project=%s pr=%s/%s#%d: %v", project, item.Owner, item.Repo, item.Number, impErr)
				b.auditPRImport("", project, item.Owner, item.Repo, item.Number, impErr)
				continue
			}
			if ok {
				created++
				b.auditPRImport(unitID, project, item.Owner, item.Repo, item.Number, nil)
			}
		}
	}
	return created
}

func (b *Bot) shouldSkipPRImport(project string, item ghpr.PRListItem) bool {
	if item.Number <= 0 || ghpr.IsTerminal(item.State) {
		return true
	}
	if gitworktree.IsManagedBranch(item.HeadRef) {
		return true
	}
	return len(b.FindPRSessions(project, item.Owner, item.Repo, item.Number)) > 0
}

// importTrackedPR allocates a web-native shell for one GitHub PR.
// It does not create a worktree, stamp a CLI, or start a run.
func (b *Bot) importTrackedPR(project string, item ghpr.PRListItem) (unitID string, created bool, err error) {
	if b == nil || b.sessions == nil {
		return "", false, nil
	}
	if len(b.FindPRSessions(project, item.Owner, item.Repo, item.Number)) > 0 {
		return "", false, nil
	}
	unitID = gitworktree.NewWebUnitID()
	pr := sessionstore.TrackedPR{
		URL:     item.URL,
		Number:  item.Number,
		State:   item.State,
		Title:   item.Title,
		Review:  item.ReviewDecision,
		HeadSHA: item.HeadSHA,
		HeadRef: item.HeadRef,
		IsDraft: item.IsDraft,
		Owner:   item.Owner,
		Repo:    item.Repo,
	}
	pr.FillOwnerRepoFromURL()
	e := sessionstore.Entry{
		Project:     project,
		Goal:        clampGoal(item.Title),
		SessionKind: sessionstore.SessionKindImportedPR,
		OwnerName:   strings.TrimSpace(item.Author),
	}
	e.UpsertPR(pr)
	e.StampTurn("")
	if err := b.sessions.Set(unitID, e); err != nil {
		return "", false, err
	}
	log.Printf("pr-import: tracked project=%s pr=%s/%s#%d unit=%s", project, pr.Owner, pr.Repo, pr.Number, unitID)
	return unitID, true, nil
}

func (b *Bot) auditPRImport(unitID, project, owner, repo string, number int, err error) {
	if b == nil || b.audit == nil {
		return
	}
	d := map[string]any{
		"source": "poller",
		"owner":  owner,
		"repo":   repo,
		"number": number,
	}
	if unitID != "" {
		d["threadId"] = unitID
	}
	if project != "" {
		d["project"] = project
	}
	ev := audit.Event{Action: audit.ActionPRImport, Detail: d, OK: err == nil}
	if err != nil {
		ev.Error = err.Error()
	}
	if appendErr := b.audit.Append(ev); appendErr != nil {
		log.Printf("warn: audit %s: %v", audit.ActionPRImport, appendErr)
	}
}

// sessionCreatesPR reports whether e is a work unit that can be the session
// that opened a PR. Imported shells and agent-review units bind a PR without
// being that session.
func sessionCreatesPR(e sessionstore.Entry) bool {
	return !e.IsImportedPR() && !e.IsPRReview() && !e.IsPRAsk()
}

// importedShellUntouched reports whether e is still the importer's empty
// shell. The first real run clears SessionKindImportedPR, so a cwd, branch,
// agent session, or owner means someone has taken the unit over.
func importedShellUntouched(e sessionstore.Entry) bool {
	if !e.IsImportedPR() {
		return false
	}
	if strings.TrimSpace(e.SessionID) != "" || strings.TrimSpace(e.Cwd) != "" || strings.TrimSpace(e.WorktreeBranch) != "" {
		return false
	}
	return strings.TrimSpace(e.OwnerID) == ""
}

// strictSamePR matches owner/repo#n only. SamePR also treats a bare number as
// the same PR, and one project can have that number in two repos (deploys #25
// is both admin and collector). A shadow delete must not use that rule.
func strictSamePR(a, b sessionstore.TrackedPR) bool {
	a.FillOwnerRepoFromURL()
	b.FillOwnerRepoFromURL()
	if a.Number <= 0 || a.Number != b.Number {
		return false
	}
	if a.Owner == "" || b.Owner == "" || a.Repo == "" || b.Repo == "" {
		return false
	}
	return strings.EqualFold(a.Owner, b.Owner) && strings.EqualFold(a.Repo, b.Repo)
}

// creatorBindsPR returns the work unit in project that already tracks pr.
func creatorBindsPR(list []sessionstore.Listed, project string, pr sessionstore.TrackedPR, except string) (string, bool) {
	project = strings.TrimSpace(project)
	if project == "" || pr.Number <= 0 {
		return "", false
	}
	for _, listed := range list {
		if listed.ThreadID == except || !sessionCreatesPR(listed.Entry) {
			continue
		}
		if !strings.EqualFold(listed.Project, project) {
			continue
		}
		e := listed.Entry
		e.NormalizePRs()
		for _, got := range e.PRs {
			if strictSamePR(got, pr) {
				return listed.ThreadID, true
			}
		}
	}
	return "", false
}

// importedPRShadowed reports whether pr on an imported shell is already
// tracked by a work session in the same project. The PR page and the ship
// board then belong to that session.
func importedPRShadowed(list []sessionstore.Listed, shell sessionstore.Listed, pr sessionstore.TrackedPR) bool {
	if !shell.IsImportedPR() {
		return false
	}
	_, ok := creatorBindsPR(list, shell.Project, pr, shell.ThreadID)
	return ok
}

// dropShadowImportedPRs deletes imported shells whose PRs are all tracked by
// a work session in the same project.
//
// The importer skips managed branches (grokwork/<thread>). A session that
// opens PRs in several repos of one project uses ordinary feature branches
// for the other repos, and the poller can import those before
// refreshPRAfterTask records them on the creator. Each PR then shows the
// creator and a shell. The creator is the session that opened it.
func (b *Bot) dropShadowImportedPRs() int {
	if b == nil || b.sessions == nil {
		return 0
	}
	list := b.sessions.List()
	var drop []string
	for _, listed := range list {
		if !importedShellUntouched(listed.Entry) || b.isThreadBusy(listed.ThreadID) {
			continue
		}
		e := listed.Entry
		e.NormalizePRs()
		if len(e.PRs) == 0 {
			continue
		}
		shadowed := true
		for _, pr := range e.PRs {
			if _, ok := creatorBindsPR(list, e.Project, pr, listed.ThreadID); !ok {
				shadowed = false
				break
			}
		}
		if shadowed {
			drop = append(drop, listed.ThreadID)
		}
	}
	n := 0
	for _, id := range drop {
		if b.deleteImportedShadow(id) {
			n++
		}
	}
	return n
}

// dropShadowsIfCreator removes imported duplicates of every PR threadID tracks,
// once that unit is a work session rather than a shell.
func (b *Bot) dropShadowsIfCreator(threadID string) {
	if b == nil || b.sessions == nil || threadID == "" {
		return
	}
	e, ok := b.sessions.Get(threadID)
	if !ok || !sessionCreatesPR(e) {
		return
	}
	b.dropShadowImportedPRs()
}

func (b *Bot) deleteImportedShadow(threadID string) bool {
	if b == nil || b.sessions == nil || threadID == "" {
		return false
	}
	if err := b.sessions.Delete(threadID); err != nil {
		log.Printf("pr-import: drop shadow session %s: %v", threadID, err)
		return false
	}
	if b.history != nil {
		if err := b.history.Delete(threadID); err != nil {
			log.Printf("pr-import: drop shadow history %s: %v", threadID, err)
		}
	}
	if b.events != nil {
		if err := b.events.Delete(threadID); err != nil {
			log.Printf("pr-import: drop shadow timeline %s: %v", threadID, err)
		}
	}
	log.Printf("pr-import: dropped shadow unit=%s", threadID)
	return true
}
