package bot

import (
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Related cases — "this looks like WEBAPP-9 in another module".
//
// A link is stored on one side only, on the case that noticed the resemblance.
// The other side is derived at read time (RelatedCaseLinks scans for cases
// pointing back), so the pair can never fall out of sync and nobody has to hold
// edit rights on both cases to record the connection.

// MaxRelatedCases caps the list. It is a cross-reference, not a tag cloud; a
// case that resembles a dozen others is really one root cause nobody has
// written down yet.
const MaxRelatedCases = 12

// CaseLink is one end of a relation, resolved for display.
type CaseLink struct {
	CaseKey  string
	ThreadID string // empty when the key resolves to nothing (case abandoned)
	Project  string
	Title    string
	Phase    string
	// Inbound is true when the *other* case points at this one, rather than
	// this one at it. The UI says so: "referenced by" is not the same claim as
	// "references", and only the outbound half is this case's to remove.
	Inbound bool
}

// FindCaseByKey resolves a case key ("WEBAPP-14", any casing) to its thread.
func (b *Bot) FindCaseByKey(key string) (string, sessionstore.Entry, bool) {
	if b == nil || b.sessions == nil {
		return "", sessionstore.Entry{}, false
	}
	id, e, ok := b.sessions.FindByCaseKey(key)
	if !ok || !e.IsCase() {
		return "", sessionstore.Entry{}, false
	}
	return id, e, true
}

// RelatedCaseLinks returns this case's outbound references followed by the
// inbound ones, each resolved to a title where the target still exists.
//
// Both halves are restricted to the case's own project. The session page has
// no per-link authorization — it renders whatever this returns — so a link
// reaching across projects would publish another project's case key, title and
// phase to anyone who can read this one. LinkCase refuses to create such a
// link; this refuses to *render* one, so hand-edited or legacy data cannot leak
// either.
func (b *Bot) RelatedCaseLinks(threadID string) []CaseLink {
	if b == nil || b.sessions == nil {
		return nil
	}
	self, ok := b.sessions.Get(threadID)
	if !ok || !self.IsCase() {
		return nil
	}
	out := []CaseLink{}
	outbound := map[string]struct{}{}
	for _, key := range self.RelatedCaseKeys() {
		outbound[key] = struct{}{}
		out = append(out, b.resolveCaseLink(key, self.Project))
	}
	if selfKey := strings.TrimSpace(self.CaseKey); selfKey != "" {
		for _, l := range b.sessions.List() {
			if l.ThreadID == threadID || !l.IsCase() || l.Project != self.Project {
				continue
			}
			otherKey := strings.TrimSpace(l.CaseKey)
			if otherKey == "" {
				continue
			}
			// Skip a case this one already points at: the relation is one edge,
			// and listing it twice would offer a Remove that does not apply to
			// the half being shown.
			if _, dup := outbound[otherKey]; dup {
				continue
			}
			for _, ref := range l.RelatedCaseKeys() {
				if ref != selfKey {
					continue
				}
				out = append(out, CaseLink{
					CaseKey:  otherKey,
					ThreadID: l.ThreadID,
					Project:  l.Project,
					Title:    caseRowTitle(l.Entry),
					Phase:    l.CasePhase(),
					Inbound:  true,
				})
				break
			}
		}
	}
	return out
}

// resolveCaseLink fills in a stored reference. A target outside inProject stays
// unresolved — key only, no thread, title or phase — so it renders as a dead
// reference rather than as a window into another project.
func (b *Bot) resolveCaseLink(key, inProject string) CaseLink {
	link := CaseLink{CaseKey: key}
	id, e, ok := b.FindCaseByKey(key)
	if !ok || e.Project != inProject {
		return link
	}
	link.ThreadID = id
	link.Project = e.Project
	link.Title = caseRowTitle(e)
	link.Phase = e.CasePhase()
	return link
}

// LinkCase records that threadID's case relates to key. The key must resolve to
// an existing case: a reference to nothing is a typo, and storing it would show
// up on the board as a dead link with no way to tell the two apart later.
func (b *Bot) LinkCase(threadID, key string) error {
	if b == nil || b.sessions == nil {
		return fmt.Errorf("no session store")
	}
	norm := sessionstore.NormalizeCaseKey(key)
	if norm == "" {
		return fmt.Errorf("%q is not a case id — expected something like WEBAPP-14", strings.TrimSpace(key))
	}
	self, ok := b.sessions.Get(threadID)
	if !ok || !self.IsCase() {
		return fmt.Errorf("this work unit is not a case")
	}
	if norm == strings.TrimSpace(self.CaseKey) {
		return fmt.Errorf("a case cannot reference itself")
	}
	// Same project only. The session page renders a link's title and phase with
	// no further authorization, so a cross-project reference would be a read
	// primitive for any project whose case keys you can guess. Answering
	// "another project" rather than "no such case" is safe here: the actor can
	// already see this project's board, and the message names no other case.
	_, targetEnt, found := b.FindCaseByKey(norm)
	if !found {
		return fmt.Errorf("no case %s", norm)
	}
	if targetEnt.Project != self.Project {
		return fmt.Errorf("%s belongs to another project — cases can only reference cases in %s", norm, self.Project)
	}
	// Decided before Patch, not inside it: Patch always stamps UpdatedAt, and a
	// re-submit of a link that already exists would otherwise reorder the case
	// to the top of every recently-updated list without changing anything.
	existing := self.RelatedCaseKeys()
	for _, k := range existing {
		if k == norm {
			return nil
		}
	}
	if len(existing) >= MaxRelatedCases {
		return fmt.Errorf("a case may reference at most %d others", MaxRelatedCases)
	}
	// The closure re-derives from the entry Patch loaded, so a link added
	// concurrently between the read above and this write is preserved rather
	// than clobbered — at worst the cap is exceeded by one.
	_, ok, err := b.sessions.Patch(threadID, func(e *sessionstore.Entry) {
		for _, k := range e.RelatedCaseKeys() {
			if k == norm {
				return
			}
		}
		e.RelatedCases = append(e.RelatedCaseKeys(), norm)
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown work unit")
	}
	return nil
}

// UnlinkCase drops an outbound reference. Inbound ones belong to the other
// case and are not removable from here.
func (b *Bot) UnlinkCase(threadID, key string) error {
	if b == nil || b.sessions == nil {
		return fmt.Errorf("no session store")
	}
	norm := sessionstore.NormalizeCaseKey(key)
	if norm == "" {
		return fmt.Errorf("not a case id")
	}
	_, ok, err := b.sessions.Patch(threadID, func(e *sessionstore.Entry) {
		kept := make([]string, 0, len(e.RelatedCases))
		for _, k := range e.RelatedCaseKeys() {
			if k != norm {
				kept = append(kept, k)
			}
		}
		e.RelatedCases = kept
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown work unit")
	}
	return nil
}

// caseRowTitle is the board's title rule, reused so a link and the row it
// points at never disagree about what the case is called.
func caseRowTitle(e sessionstore.Entry) string {
	if t := strings.TrimSpace(e.CustomerTitle); t != "" {
		return t
	}
	if g := strings.TrimSpace(e.Goal); g != "" {
		return g
	}
	return "(untitled case)"
}
