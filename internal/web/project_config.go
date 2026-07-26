package web

import (
	"fmt"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/config"
)

// ── Per-project settings tabs (/config/projects/{name}[/tab]) ────────────
//
// Project settings split into four sub-tab pages, each its own URL under the
// boosted shell: Access (team policy + unified member roster), Workflow
// (shipping + session defaults), Integrations (Discord / GitHub / Linear /
// repo fetch), and Danger (remove project). POSTs land back on their tab via
// projectConfigTabRedirect.

// memberRow is one actor on the Access roster: membership (direct or via a
// team) plus optional explicit capability template, with the effective
// capabilities ResolveCapabilities would grant.
type memberRow struct {
	ID       string
	Name     string   // display name (best-effort)
	Initials string   // avatar fallback
	Template string   // explicit template; "" = default fallback
	Explicit bool     // Template != ""
	Caps     []string // effective capability chips
	// TemplateUnknown: explicit template that resolves to nothing (typo in a
	// hand-edited config). The select shows it as "(unknown)" instead of
	// silently falling back to default; the actor's effective capabilities are
	// none, because ResolveCapabilities fails closed on a broken mapping.
	TemplateUnknown bool
	// Direct: on allowedUserIds, so the row's × removes a direct membership.
	// A row that is not direct is either a live team member or inert, and its ×
	// can only remove the capabilityByUser entry.
	Direct bool
	// ViaTeam: not on allowedUserIds but on one of the project's teams, so the
	// grant is live and the capabilityByUser override is in force. Rendering
	// these as Inert claimed the opposite of the truth.
	ViaTeam bool
	// Via names the teams carrying a ViaTeam row's access ("via team eng").
	Via string
	// Inert: capability map entry with neither a direct membership nor a team
	// (legacy or hand-edited config) — the bot never grants these access, so the
	// roster surfaces them for cleanup instead of hiding them.
	Inert bool
}

// teamRosterRow is one team on the Access tab: the capability template it
// grants, the effective capabilities of that template, and its members.
type teamRosterRow struct {
	Key             string
	Label           string // display; falls back to Key
	Capabilities    string // template name; "" = default fallback
	TemplateUnknown bool
	Caps            []string // effective capability chips for the template
	Members         []memberRow
}

// capMatrixRow is one template line in the "what each role can do" legend.
type capMatrixRow struct {
	Name  string
	Flags []bool // aligned with capColumns labels
}

// capColumns orders capability flags for chips and the legend matrix.
// RequestChange/SafeOps are wave-1 reserved (no command gates) — omitted.
var capColumns = []struct {
	Label string
	Get   func(config.Capabilities) bool
}{
	{"investigate", func(c config.Capabilities) bool { return c.Investigate }},
	{"draft reply", func(c config.Capabilities) bool { return c.DraftCustomerReply }},
	{"escalate", func(c config.Capabilities) bool { return c.FileEscalation }},
	{"sessions", func(c config.Capabilities) bool { return c.StartSessions }},
	{"github", func(c config.Capabilities) bool { return c.GithubWrites }},
	{"merge", func(c config.Capabilities) bool { return c.Merge }},
	{"approve", func(c config.Capabilities) bool { return c.Approve }},
	{"admin", func(c config.Capabilities) bool { return c.AdminProject }},
}

func capChips(c config.Capabilities) []string {
	var out []string
	for _, col := range capColumns {
		if col.Get(c) {
			out = append(out, col.Label)
		}
	}
	return out
}

func capFlags(c config.Capabilities) []bool {
	out := make([]bool, len(capColumns))
	for i, col := range capColumns {
		out[i] = col.Get(c)
	}
	return out
}

func memberInitials(name, id string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		s = strings.TrimSpace(id)
	}
	r := []rune(s)
	if len(r) > 2 {
		r = r[:2]
	}
	return strings.ToLower(string(r))
}

// displayName resolves an actor id to a name. Lookup is keyed by bare Discord
// snowflake, so a namespaced id has to be reduced first; a non-Discord actor has
// no directory to ask, so its row renders the full namespaced id instead.
func displayName(names map[string]string, id string) string {
	if !config.IsDiscordActor(id) {
		return ""
	}
	return names[config.ActorSubject(id)]
}

// buildMemberRoster merges the direct allowlist and the capabilityByUser map
// into one roster: direct members in config order, then map-only rows.
// Team members are NOT listed for their team grant — that lives on the team
// roster, because removing it means leaving the team. A team member *with* a
// capabilityByUser override does get a row, because the override is editable
// only here; it is a live grant, not an inert one, so it renders its effective
// capabilities rather than the "cannot use Grok" warning. This page is the audit
// surface for adminProject, so a real grant shown as dead is worse than noise.
func (s *Server) buildMemberRoster(item *config.ProjectItem, names map[string]string) []memberRow {
	tplByUser := make(map[string]string, len(item.CapabilityByUser))
	for _, m := range item.CapabilityByUser {
		tplByUser[m.ID] = m.Template
	}
	known := make(map[string]bool, len(item.CapabilityTemplateNames))
	for _, n := range item.CapabilityTemplateNames {
		known[n] = true
	}
	// Teams carrying each actor, in the snapshot's key order.
	teamsByActor := make(map[string][]string)
	for _, t := range item.Teams {
		for _, id := range t.Members {
			n := config.NormalizeActorID(id)
			teamsByActor[n] = append(teamsByActor[n], t.Key)
		}
	}
	var rows []memberRow
	member := make(map[string]bool, len(item.AllowedUserIDs))
	for _, id := range item.AllowedUserIDs {
		member[config.NormalizeActorID(id)] = true
		tpl := tplByUser[id]
		name := displayName(names, id)
		rows = append(rows, memberRow{
			ID:              id,
			Name:            name,
			Initials:        memberInitials(name, id),
			Template:        tpl,
			Explicit:        tpl != "",
			TemplateUnknown: tpl != "" && !known[tpl],
			Direct:          true,
			Caps:            capChips(s.cfg.ResolveCapabilities(item.Name, id)),
		})
	}
	for _, m := range item.CapabilityByUser {
		id := config.NormalizeActorID(m.ID)
		if member[id] {
			continue
		}
		teams := teamsByActor[id]
		row := memberRow{
			ID: m.ID, Name: displayName(names, m.ID), Initials: "?",
			Template: m.Template, Explicit: true,
			TemplateUnknown: m.Template != "" && !known[m.Template],
			ViaTeam:         len(teams) > 0,
			Inert:           len(teams) == 0,
		}
		if row.ViaTeam {
			row.Initials = memberInitials(row.Name, config.ActorSubject(m.ID))
			row.Via = "via team " + strings.Join(teams, ", ")
			row.Caps = capChips(s.cfg.ResolveCapabilities(item.Name, m.ID))
		}
		rows = append(rows, row)
	}
	return rows
}

// defaultRoleFallback is the template members without an explicit role resolve
// to: the policy default under safe team, else builder (backward compat).
func defaultRoleFallback(item *config.ProjectItem) string {
	if item.SafeTeamMode {
		return item.SafeTeamDefaultTemplate
	}
	return "builder"
}

// buildTeamRoster renders one row per team with the capabilities its template
// grants and its members. A team naming no template shows the policy fallback's
// capabilities, which is what its members actually get. A team naming an
// *unknown* template shows no capabilities at all, because that is what
// ResolveCapabilities gives them — a broken mapping fails closed instead of
// falling through to the unmapped default.
func (s *Server) buildTeamRoster(item *config.ProjectItem, names map[string]string) []teamRosterRow {
	if len(item.Teams) == 0 {
		return nil
	}
	fallback := defaultRoleFallback(item)
	rows := make([]teamRosterRow, 0, len(item.Teams))
	for _, t := range item.Teams {
		var caps config.Capabilities
		if !t.TemplateUnknown {
			tpl := t.Capabilities
			if tpl == "" {
				tpl = fallback
			}
			caps, _ = s.cfg.ResolveTemplate(item.Name, tpl)
		}
		row := teamRosterRow{
			Key:             t.Key,
			Label:           t.Label,
			Capabilities:    t.Capabilities,
			TemplateUnknown: t.TemplateUnknown,
			Caps:            capChips(caps),
		}
		if row.Label == "" {
			row.Label = t.Key
		}
		for _, id := range t.Members {
			name := displayName(names, id)
			row.Members = append(row.Members, memberRow{
				// Normalized for display: a hand-written config may spell the
				// same person "123" and "discord:123", and the roster should not
				// suggest those are different kinds of member when every match
				// goes through SameActor. Any write normalizes anyway.
				ID:       config.NormalizeActorID(id),
				Name:     name,
				Initials: memberInitials(name, config.ActorSubject(id)),
			})
		}
		rows = append(rows, row)
	}
	return rows
}

// buildCapMatrix renders each known template (builtin + project overlays)
// against the capability columns for the Access legend.
func (s *Server) buildCapMatrix(item *config.ProjectItem) ([]capMatrixRow, []string) {
	names := make([]string, len(capColumns))
	for i, col := range capColumns {
		names[i] = col.Label
	}
	rows := make([]capMatrixRow, 0, len(item.CapabilityTemplateNames))
	for _, tpl := range item.CapabilityTemplateNames {
		caps, ok := s.cfg.ResolveTemplate(item.Name, tpl)
		if !ok {
			continue
		}
		rows = append(rows, capMatrixRow{Name: tpl, Flags: capFlags(caps)})
	}
	return rows, names
}

// projectConfigTab locates the project, fills the shared settings chrome for
// one tab, then renders. Unknown project → config hub with err.
func (s *Server) projectConfigTab(ctx *hime.Context, tab, tmpl string, fill func(d *pageData)) error {
	name := ctx.PathValue("name")
	snap := s.cfg.Snapshot()
	var item *config.ProjectItem
	for i := range snap.Projects {
		if snap.Projects[i].Name == name {
			item = &snap.Projects[i]
			break
		}
	}
	if item == nil {
		return ctx.RedirectTo("config", map[string]string{"err": fmt.Sprintf("unknown project %q", name)})
	}
	d := s.basePage(ctx)
	d.Title = item.Name + " · Config"
	d.IsConfig = true
	d.Config = snap
	d.Project = item.Name
	d.ProjectItem = *item
	d.ProjectTab = tab
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	if fill != nil {
		fill(&d)
	}
	return s.viewPage(ctx, tmpl, d)
}

// projectConfigPage is the Access tab (default): team policy, teams, and the
// direct-member roster.
func (s *Server) projectConfigPage(ctx *hime.Context) error {
	return s.projectConfigTab(ctx, "access", "project_config", func(d *pageData) {
		item := &d.ProjectItem
		// The name directory is keyed by bare Discord snowflake, so every id
		// asked about is reduced to its subject and non-Discord actors are not
		// asked about at all.
		var nameIDs []string
		want := func(id string) {
			if config.IsDiscordActor(id) {
				nameIDs = append(nameIDs, config.ActorSubject(id))
			}
		}
		for _, id := range item.MemberIDs {
			want(id)
		}
		for _, m := range item.CapabilityByUser {
			want(m.ID)
		}
		names := s.resolveDiscordUserNames(nameIDs)
		d.DiscordUserNames = names
		d.Members = s.buildMemberRoster(item, names)
		d.TeamRoster = s.buildTeamRoster(item, names)
		d.CapMatrix, d.CapNames = s.buildCapMatrix(item)
		// Effective role for members without an explicit one: safe team off
		// falls back to builder (backward compat), on → the default template.
		d.DefaultRoleFallback = defaultRoleFallback(item)
	})
}

func (s *Server) projectConfigWorkflowPage(ctx *hime.Context) error {
	return s.projectConfigTab(ctx, "workflow", "project_config_workflow", func(d *pageData) {
		// Pending Grok draft overrides the saved textarea until Save (or a new Suggest).
		if draft := s.peekVerifyDraft(d.ProjectItem.Name); draft != "" {
			d.ProjectItem.VerifyCommandsText = draft
		}
	})
}

func (s *Server) projectConfigIntegrationsPage(ctx *hime.Context) error {
	return s.projectConfigTab(ctx, "integrations", "project_config_integrations", nil)
}

func (s *Server) projectConfigDangerPage(ctx *hime.Context) error {
	return s.projectConfigTab(ctx, "danger", "project_config_danger", nil)
}
