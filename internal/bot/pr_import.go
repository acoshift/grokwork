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
func (b *Bot) importOpenGitHubPRs() int {
	if b == nil || b.cfg == nil || b.sessions == nil {
		return 0
	}
	ctx := b.bgContext()
	if ctx.Err() != nil {
		return 0
	}
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
