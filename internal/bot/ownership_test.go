package bot

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestCanControlThreadSoftUnowned(t *testing.T) {
	b := testBot(t)
	m := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author:    &discordgo.User{ID: "u1", Username: "alice"},
		ChannelID: "t1",
	}}
	// No session owner → anyone may control (soft open / legacy).
	if !b.canControlThread(m, sessionstore.Entry{}) {
		t.Fatal("unowned should allow control")
	}
}

func TestCanControlThreadOwnerAndCoOwner(t *testing.T) {
	b := testBot(t)
	e := sessionstore.Entry{OwnerID: "owner", OwnerName: "O"}
	e.AddCoOwner("co")

	ownerMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "owner"}, ChannelID: "t1",
	}}
	coMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "co"}, ChannelID: "t1",
	}}
	otherMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "other"}, ChannelID: "t1",
	}}

	if !b.canControlThread(ownerMsg, e) {
		t.Fatal("owner should control")
	}
	if !b.canControlThread(coMsg, e) {
		t.Fatal("co-owner should control")
	}
	// testBot has no projects, so the entry's project grants nobody adminProject
	// → a third party is denied.
	if b.canControlThread(otherMsg, e) {
		t.Fatal("other should not control without project admin caps")
	}
}

// Authority for cancel/reset is owner, co-owner, or a member of a team whose
// capability template grants adminProject. Discord channel permissions
// (Administrator / Manage Messages / Manage Threads) no longer grant anything —
// there is no Session here at all, and the admin path still works.
func TestCanControlThreadProjectAdminOverrides(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir: dir,
		Projects: config.ProjectsMap{
			"app": {
				Path: dir,
				Teams: map[string]config.TeamConfig{
					"leads": {Label: "Leads", Members: []string{"admin1"}, Capabilities: "admin"},
				},
			},
			// Same person, builder team: may ship, may not seize a thread.
			"other": {
				Path: dir,
				Teams: map[string]config.TeamConfig{
					"eng": {Label: "Eng", Members: []string{"admin1"}, Capabilities: "builder"},
				},
			},
			// Member of no team: access only, so no adminProject either.
			"direct": {Path: dir, AllowedUserIDs: []string{"admin1"}},
		},
	}
	b := New(cfg, store, hist)

	adminMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "admin1"}, ChannelID: "t1",
	}}
	strangerMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "nobody"}, ChannelID: "t1",
	}}

	owned := sessionstore.Entry{Project: "app", OwnerID: "owner", OwnerName: "O"}
	if !b.canControlThread(adminMsg, owned) {
		t.Fatal("admin-team member should control a thread they do not own")
	}
	if b.canControlThread(strangerMsg, owned) {
		t.Fatal("non-member should not control")
	}

	// Builder caps are not admin caps.
	builderOwned := sessionstore.Entry{Project: "other", OwnerID: "owner", OwnerName: "O"}
	if b.canControlThread(adminMsg, builderOwned) {
		t.Fatal("builder team must not grant cancel/reset over another owner")
	}

	// A plain project member (allowedUserIds, no team) is not an admin. With
	// SafeTeamMode off they resolve to builder, which is deliberately not enough.
	directOwned := sessionstore.Entry{Project: "direct", OwnerID: "owner", OwnerName: "O"}
	if b.canControlThread(adminMsg, directOwned) {
		t.Fatal("plain project member must not control another owner's thread")
	}

	// A /claim shell with no project resolves to owner/co-owner only.
	noProject := sessionstore.Entry{OwnerID: "owner", OwnerName: "O"}
	if b.canControlThread(adminMsg, noProject) {
		t.Fatal("empty project must fail closed")
	}
}

// Behaviour change: Discord guild/channel permissions no longer grant
// cancel/reset. A server mod carrying Administrator + Manage Messages +
// Manage Threads who is on no adminProject team is denied — and being on the
// project at all is not enough either.
func TestCanControlThreadDiscordModNoLongerBypasses(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir: dir,
		Projects: config.ProjectsMap{
			"app": {
				Path:           dir,
				AllowedUserIDs: []string{"mod1"},
				Teams: map[string]config.TeamConfig{
					"eng":   {Label: "Eng", Members: []string{"mod1"}, Capabilities: "builder"},
					"leads": {Label: "Leads", Members: []string{"lead1"}, Capabilities: "admin"},
				},
			},
		},
	}
	b := New(cfg, store, hist)

	// Every Discord signal the old bypass consulted, all set.
	modMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author:    &discordgo.User{ID: "mod1", Username: "mod"},
		ChannelID: "t1",
		GuildID:   "g1",
		Member: &discordgo.Member{
			GuildID: "g1",
			Roles:   []string{"role-admin", "role-mod"},
			Permissions: discordgo.PermissionAdministrator |
				discordgo.PermissionManageMessages |
				discordgo.PermissionManageThreads,
		},
	}}
	e := sessionstore.Entry{Project: "app", OwnerID: "owner", OwnerName: "O"}
	if b.canControlThread(modMsg, e) {
		t.Fatal("a Discord mod without an adminProject team must be denied")
	}

	// The replacement authority works on the same project.
	leadMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "lead1"}, ChannelID: "t1",
	}}
	if !b.canControlThread(leadMsg, e) {
		t.Fatal("admin team should control")
	}
}

// safeTeamDefaultTemplate "admin" hands AdminProject to every unmapped actor, so
// actorAdminsProject must gate on membership first or a stranger could cancel
// any thread on the project.
func TestActorAdminsProjectRequiresMembership(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	cfg := &config.Config{
		DataDir: dir,
		Projects: config.ProjectsMap{
			"app": {
				Path:                    dir,
				SafeTeamMode:            &on,
				SafeTeamDefaultTemplate: "admin",
				AllowedUserIDs:          []string{"member1"},
			},
		},
	}
	b := New(cfg, store, hist)

	if !b.actorAdminsProject("app", "member1") {
		t.Fatal("member falls through to the admin default: should be admin")
	}
	if b.actorAdminsProject("app", "stranger") {
		t.Fatal("non-member must never inherit adminProject from the unmapped default")
	}
	if b.actorAdminsProject("", "member1") {
		t.Fatal("empty project must fail closed")
	}
	if b.actorAdminsProject("app", "") {
		t.Fatal("empty actor must fail closed")
	}
}

func TestEnsureSessionOwner(t *testing.T) {
	var e sessionstore.Entry
	ensureSessionOwner(&e, "u1", "alice")
	if e.OwnerID != "u1" || e.OwnerName != "alice" {
		t.Fatalf("got %+v", e)
	}
	ensureSessionOwner(&e, "u2", "bob")
	if e.OwnerID != "u1" {
		t.Fatalf("should not overwrite: %+v", e)
	}
}

func TestBindThreadOwner(t *testing.T) {
	b := testBot(t)
	m := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "u1", Username: "alice"},
	}}
	b.bindThreadOwner("t1", "app", m)
	e, ok := b.sessions.Get("t1")
	if !ok || e.OwnerID != "u1" || e.Project != "app" {
		t.Fatalf("bind new: ok=%v e=%+v", ok, e)
	}
	// Second binder does not steal ownership; keeps project.
	m2 := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "u2", Username: "bob"},
	}}
	b.bindThreadOwner("t1", "other", m2)
	e, _ = b.sessions.Get("t1")
	if e.OwnerID != "u1" || e.Project != "app" {
		t.Fatalf("bind should be no-op when owned: %+v", e)
	}

	// Existing session without owner gets owner, preserves session id.
	if err := b.sessions.Set("t2", sessionstore.Entry{SessionID: "sid", Project: "api"}); err != nil {
		t.Fatal(err)
	}
	b.bindThreadOwner("t2", "api", m)
	e, _ = b.sessions.Get("t2")
	if e.OwnerID != "u1" || e.SessionID != "sid" {
		t.Fatalf("bind existing: %+v", e)
	}
}

func TestPreserveOwnershipFields(t *testing.T) {
	prev := sessionstore.Entry{OwnerID: "o1", OwnerName: "A", CoOwnerIDs: []string{"c1"}}
	next := sessionstore.Entry{SessionID: "s"}
	preserveOwnershipFields(&next, prev)
	if next.OwnerID != "o1" || next.OwnerName != "A" || len(next.CoOwnerIDs) != 1 || next.CoOwnerIDs[0] != "c1" {
		t.Fatalf("got %+v", next)
	}
	// Do not clobber explicit next owner.
	next2 := sessionstore.Entry{OwnerID: "o2", OwnerName: "B"}
	preserveOwnershipFields(&next2, prev)
	if next2.OwnerID != "o2" {
		t.Fatalf("clobbered owner: %+v", next2)
	}
}

func TestPreservePRFieldsKeepsOwnership(t *testing.T) {
	prev := sessionstore.Entry{
		OwnerID: "o1", OwnerName: "A", CoOwnerIDs: []string{"c1"},
		PRNumber: 9, PRURL: "https://github.com/o/r/pull/9",
		Issues: []sessionstore.TrackedIssue{{Number: 42, Keyword: sessionstore.IssueKeywordFixes}},
	}
	next := sessionstore.Entry{SessionID: "s", Project: "p"}
	preservePRFields(&next, prev)
	if next.OwnerID != "o1" || next.PRNumber != 9 {
		t.Fatalf("got %+v", next)
	}
	if len(next.Issues) != 1 || next.Issues[0].Number != 42 {
		t.Fatalf("issues not preserved: %+v", next.Issues)
	}
}

func TestFirstMentionedUserSkipsBot(t *testing.T) {
	s := &discordgo.Session{State: &discordgo.State{}}
	s.State.User = &discordgo.User{ID: "bot"}
	m := &discordgo.MessageCreate{Message: &discordgo.Message{
		Mentions: []*discordgo.User{
			{ID: "bot", Username: "Grok"},
			{ID: "u9", Username: "bob"},
		},
	}}
	u := firstMentionedUser(s, m)
	if u == nil || u.ID != "u9" {
		t.Fatalf("got %+v", u)
	}
}

func TestFormatHandOffCard(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := hist.Append("t1", history.Turn{
		User: "alice", Prompt: "fix the flaky payment timeout in checkout", Status: "done", Project: "app",
	}); err != nil {
		t.Fatal(err)
	}
	b := New(&config.Config{DataDir: dir}, store, hist)
	e := sessionstore.Entry{
		Project: "app", WorktreeBranch: "grok/discord/t1",
		OwnerID: "u2", OwnerName: "bob",
	}
	from := &discordgo.User{ID: "u1", Username: "alice"}
	to := &discordgo.User{ID: "u2", Username: "bob"}
	card := b.formatHandOffCard("t1", e, from, to)
	for _, want := range []string{"Hand-off", "app", "fix the flaky", "grok/discord/t1", "owns cancel/reset"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card missing %q:\n%s", want, card)
		}
	}
}

func TestLastPromptPreview(t *testing.T) {
	dir := t.TempDir()
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(&config.Config{DataDir: dir}, store, hist)
	if got := b.lastPromptPreview("missing"); got != "" {
		t.Fatalf("empty: %q", got)
	}
	long := strings.Repeat("word ", 50)
	if err := hist.Append("t1", history.Turn{Prompt: long, Status: "done"}); err != nil {
		t.Fatal(err)
	}
	got := b.lastPromptPreview("t1")
	if got == "" || len(got) > 170 {
		t.Fatalf("preview=%q len=%d", got, len(got))
	}
}

func testBot(t *testing.T) *Bot {
	t.Helper()
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(&config.Config{DataDir: dir}, store, hist)
}
