package spend

import (
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
)

// threads is an in-memory ThreadSource so the fold is testable without a store.
type threads []history.Thread

func (t threads) Walk(fn func(history.Thread) error) error {
	for _, th := range t {
		if err := fn(th); err != nil {
			return err
		}
	}
	return nil
}

func rate(v float64) *float64 { return &v }

func pricer(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{ModelRates: map[string]config.ModelRate{
		"claude-opus-5": {
			InputPerMTok:      rate(5),
			OutputPerMTok:     rate(25),
			CacheReadPerMTok:  rate(0.5),
			CacheWritePerMTok: rate(6.25),
		},
	}}
}

func turn(project, userID, user, model string, u *history.Usage, at string) history.Turn {
	return history.Turn{
		At: at, User: user, UserID: userID, Prompt: "do the thing",
		Status: "done", Project: project, Model: model, Agent: "claude", Usage: u,
	}
}

func usage(in, cacheRead, cacheWrite, out int) *history.Usage {
	return &history.Usage{
		InputTokens: in, CacheReadTokens: cacheRead,
		CacheCreationTokens: cacheWrite, OutputTokens: out,
	}
}

func rowByKey(t *testing.T, rows []Row, key string) Row {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("row %q missing from %+v", key, rows)
	return Row{}
}

func TestBuildRollsUpByProjectActorAndSession(t *testing.T) {
	src := threads{{
		ThreadID: "t1", Project: "app",
		Turns: []history.Turn{
			turn("app", "u1", "alice", "claude-opus-5", usage(1_000_000, 0, 0, 0), "2026-07-01T00:00:00Z"),
			turn("app", "u2", "bob", "claude-opus-5", usage(0, 0, 0, 1_000_000), "2026-07-02T00:00:00Z"),
		},
	}, {
		ThreadID: "t2", Project: "api",
		Turns: []history.Turn{
			turn("api", "u1", "alice", "claude-opus-5", usage(0, 2_000_000, 0, 0), "2026-07-03T00:00:00Z"),
		},
	}}
	rep, err := Build(src, pricer(t), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Turns != 3 || rep.Total.TotalTokens() != 4_000_000 {
		t.Fatalf("total=%+v", rep.Total)
	}
	// $5 (1M input) + $25 (1M output) + $1 (2M cache read).
	if got := rep.Total.Dollars; got < 30.999 || got > 31.001 {
		t.Fatalf("total dollars=%v want 31", got)
	}
	if rep.Total.Priced != 3 || rep.Total.Unpriced != 0 || rep.Total.Partial() {
		t.Fatalf("total pricing=%+v", rep.Total)
	}
	if rep.Total.LastAt != "2026-07-03T00:00:00Z" {
		t.Fatalf("lastAt=%q", rep.Total.LastAt)
	}

	// Ordered by cost: app ($30) before api ($1).
	if len(rep.ByProject) != 2 || rep.ByProject[0].Key != "app" || rep.ByProject[1].Key != "api" {
		t.Fatalf("byProject=%+v", rep.ByProject)
	}
	// Actors span projects: alice ran the 1M input turn in app and the 2M cache
	// read in api.
	alice := rowByKey(t, rep.ByActor, "u1")
	if alice.Turns != 2 || alice.Label != "alice" {
		t.Fatalf("alice=%+v", alice)
	}
	if got := alice.Dollars; got < 5.999 || got > 6.001 {
		t.Fatalf("alice dollars=%v want 6", got)
	}
	bob := rowByKey(t, rep.ByActor, "u2")
	if bob.Turns != 1 || bob.Dollars < 24.999 || bob.Dollars > 25.001 {
		t.Fatalf("bob=%+v", bob)
	}
	// Session rows carry their project so the page can link into the workspace.
	s1 := rowByKey(t, rep.BySession, "t1")
	if s1.Turns != 2 || s1.Project != "app" || s1.Label == "" {
		t.Fatalf("session t1=%+v", s1)
	}
}

// An unset rate must report tokens and no dollars. Reporting $0 would make an
// unconfigured model look like the cheapest thing in the fleet.
func TestUnpricedModelReportsTokensNotZeroDollars(t *testing.T) {
	src := threads{{
		ThreadID: "t1", Project: "app",
		Turns: []history.Turn{
			// grok-4.5 has no rate configured.
			turn("app", "u1", "alice", "grok-4.5", usage(500_000, 0, 0, 10_000), "2026-07-01T00:00:00Z"),
		},
	}}
	rep, err := Build(src, pricer(t), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.TotalTokens() != 510_000 {
		t.Fatalf("tokens=%d", rep.Total.TotalTokens())
	}
	if rep.Total.HasCost() || rep.Total.Dollars != 0 || rep.Total.Priced != 0 || rep.Total.Unpriced != 1 {
		t.Fatalf("unpriced turn must not produce a cost: %+v", rep.Total)
	}
	if len(rep.UnpricedModels) != 1 || rep.UnpricedModels[0] != "grok-4.5" {
		t.Fatalf("unpricedModels=%v", rep.UnpricedModels)
	}

	// Mixing a priced and an unpriced turn yields a floor, flagged as partial —
	// never a total that silently omits one of them.
	src[0].Turns = append(src[0].Turns,
		turn("app", "u1", "alice", "claude-opus-5", usage(1_000_000, 0, 0, 0), "2026-07-02T00:00:00Z"))
	rep, err = Build(src, pricer(t), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Total.Partial() || rep.Total.Priced != 1 || rep.Total.Unpriced != 1 {
		t.Fatalf("mixed pricing=%+v", rep.Total)
	}
	if got := rep.Total.Dollars; got < 4.999 || got > 5.001 {
		t.Fatalf("partial dollars=%v want 5", got)
	}

	// A model whose rate covers only some of the classes it used is unpriced too —
	// the cache-write column is empty here.
	half := &config.Config{ModelRates: map[string]config.ModelRate{
		"claude-opus-5": {InputPerMTok: rate(5), OutputPerMTok: rate(25)},
	}}
	rep, err = Build(threads{{ThreadID: "t3", Project: "app", Turns: []history.Turn{
		turn("app", "u1", "alice", "claude-opus-5", usage(10, 0, 5_000, 20), "2026-07-01T00:00:00Z"),
	}}}, half, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.HasCost() {
		t.Fatalf("incomplete rate must not price: %+v", rep.Total)
	}
}

// Turns with no token record are not zero-cost turns — they are unmeasured, and
// counting them would dilute every per-turn figure on the page.
func TestBuildSkipsTurnsWithoutUsage(t *testing.T) {
	src := threads{{
		ThreadID: "t1", Project: "app",
		Turns: []history.Turn{
			{At: "2026-07-01T00:00:00Z", Prompt: "old record", Project: "app", Status: "done"},
			turn("app", "u1", "alice", "claude-opus-5", usage(1_000_000, 0, 0, 0), "2026-07-02T00:00:00Z"),
		},
	}}
	rep, err := Build(src, pricer(t), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Turns != 1 {
		t.Fatalf("turns=%d want 1", rep.Total.Turns)
	}
	if len(rep.BySession) != 1 || rep.BySession[0].Turns != 1 {
		t.Fatalf("bySession=%+v", rep.BySession)
	}
}

// Visibility is applied per turn, so a project the viewer may not see cannot leak
// through the project, actor, session, or total rows.
func TestBuildFiltersByVisibility(t *testing.T) {
	src := threads{{
		ThreadID: "t1", Project: "app",
		Turns: []history.Turn{
			turn("app", "u1", "alice", "claude-opus-5", usage(1_000_000, 0, 0, 0), "2026-07-01T00:00:00Z"),
		},
	}, {
		ThreadID: "t2", Project: "secret",
		Turns: []history.Turn{
			turn("secret", "u1", "alice", "claude-opus-5", usage(0, 0, 0, 4_000_000), "2026-07-02T00:00:00Z"),
		},
	}}
	rep, err := Build(src, pricer(t), Query{Visible: func(p string) bool { return p == "app" }})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ByProject) != 1 || rep.ByProject[0].Key != "app" {
		t.Fatalf("byProject leaked: %+v", rep.ByProject)
	}
	if len(rep.BySession) != 1 || rep.BySession[0].Key != "t1" {
		t.Fatalf("bySession leaked: %+v", rep.BySession)
	}
	// alice appears, but only with her visible spend — the $100 of hidden output
	// tokens must not reach her actor row or the total.
	alice := rowByKey(t, rep.ByActor, "u1")
	if alice.Turns != 1 || alice.Dollars < 4.999 || alice.Dollars > 5.001 {
		t.Fatalf("actor row leaked hidden spend: %+v", alice)
	}
	if rep.Total.Turns != 1 || rep.Total.TotalTokens() != 1_000_000 {
		t.Fatalf("total leaked: %+v", rep.Total)
	}

	// A turn whose project cannot be determined is dropped when a filter is set:
	// an unattributable turn must not become a hole in the ACL.
	orphan := threads{{ThreadID: "t3", Turns: []history.Turn{
		turn("", "u1", "alice", "claude-opus-5", usage(9_000_000, 0, 0, 0), "2026-07-03T00:00:00Z"),
	}}}
	rep, err = Build(orphan, pricer(t), Query{Visible: func(p string) bool { return p == "app" }})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Turns != 0 {
		t.Fatalf("unattributable turn counted: %+v", rep.Total)
	}
	// With no filter (admin / auth off) it is counted, since nothing is hidden.
	rep, err = Build(orphan, pricer(t), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Turns != 1 {
		t.Fatalf("admin view dropped the turn: %+v", rep.Total)
	}
}

// Months are UTC calendar buckets, newest first, and they obey the same gates
// as every other row. An offset that is still the previous UTC day stays in
// that month; a missing month is not invented; an unreadable timestamp is its
// own row so the month rows still sum to the total.
func TestBuildSplitsCostByMonth(t *testing.T) {
	src := threads{{
		ThreadID: "t1", Project: "app",
		Turns: []history.Turn{
			// 1M input = $5, July.
			turn("app", "u1", "alice", "claude-opus-5", usage(1_000_000, 0, 0, 0), "2026-07-31T23:00:00Z"),
			// 00:30 +07 on 1 Aug is 17:30 UTC on 31 Jul — still July. 1M output = $25.
			turn("app", "u2", "bob", "claude-opus-5", usage(0, 0, 0, 1_000_000), "2026-08-01T00:30:00+07:00"),
			// No usage: must not open a month of its own.
			{At: "2026-06-01T00:00:00Z", Prompt: "old", Project: "app", Status: "done"},
		},
	}, {
		ThreadID: "t2", Project: "api",
		Turns: []history.Turn{
			// Fractional seconds. 2M cache read = $1, September.
			turn("api", "u1", "alice", "claude-opus-5", usage(0, 2_000_000, 0, 0), "2026-09-15T12:00:00.5Z"),
			// Unreadable stamp, still $5 of input. Its own row, not September.
			turn("api", "u1", "alice", "claude-opus-5", usage(1_000_000, 0, 0, 0), "not-a-time"),
			// August has nothing. 1M output on an unpriced model, October: tokens
			// count, dollars do not, and the row is not a $0.
			turn("api", "u2", "bob", "grok-4.5", usage(0, 0, 0, 1_000_000), "2026-10-02T00:00:00Z"),
		},
	}}
	rep, err := Build(src, pricer(t), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ByMonth) != 4 {
		t.Fatalf("months=%+v", rep.ByMonth)
	}
	wantKeys := []string{"2026-10", "2026-09", "2026-07", ""}
	for i, key := range wantKeys {
		if rep.ByMonth[i].Key != key {
			t.Fatalf("month[%d]=%q want %q (full=%+v)", i, rep.ByMonth[i].Key, key, rep.ByMonth)
		}
	}
	// June never had a priced turn, and August had none at all.
	for _, r := range rep.ByMonth {
		if r.Key == "2026-06" || r.Key == "2026-08" {
			t.Fatalf("invented empty month: %+v", r)
		}
	}

	july := rowByKey(t, rep.ByMonth, "2026-07")
	if july.Turns != 2 || july.Project != "" {
		t.Fatalf("july=%+v", july)
	}
	if july.Dollars < 29.999 || july.Dollars > 30.001 || july.Partial() {
		t.Fatalf("july cost=%+v", july)
	}
	sep := rowByKey(t, rep.ByMonth, "2026-09")
	if sep.Turns != 1 || sep.Dollars < 0.999 || sep.Dollars > 1.001 {
		t.Fatalf("september=%+v", sep)
	}
	unknown := rowByKey(t, rep.ByMonth, "")
	if unknown.Turns != 1 || unknown.Dollars < 4.999 || unknown.Dollars > 5.001 {
		t.Fatalf("unknown month=%+v", unknown)
	}
	oct := rowByKey(t, rep.ByMonth, "2026-10")
	if oct.HasCost() || oct.Dollars != 0 || oct.Unpriced != 1 || oct.TotalTokens() != 1_000_000 {
		t.Fatalf("unpriced month must not read as $0: %+v", oct)
	}

	var turns int
	var dollars float64
	for _, r := range rep.ByMonth {
		turns += r.Turns
		dollars += r.Dollars
	}
	if turns != rep.Total.Turns || dollars < rep.Total.Dollars-0.001 || dollars > rep.Total.Dollars+0.001 {
		t.Fatalf("months do not sum to total: months turns=%d $%v total=%+v", turns, dollars, rep.Total)
	}

	// A hidden project's month must not appear, and must not inflate a month
	// the viewer can otherwise see.
	rep, err = Build(src, pricer(t), Query{Visible: func(p string) bool { return p == "app" }})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ByMonth) != 1 || rep.ByMonth[0].Key != "2026-07" || rep.ByMonth[0].Turns != 2 {
		t.Fatalf("visibility leaked into months: %+v", rep.ByMonth)
	}

	// Actor scope drops the other person's July turn ($25) and every other month.
	rep, err = Build(src, pricer(t), Query{ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ByMonth) != 3 {
		t.Fatalf("actor months=%+v", rep.ByMonth)
	}
	july = rowByKey(t, rep.ByMonth, "2026-07")
	if july.Turns != 1 || july.Dollars < 4.999 || july.Dollars > 5.001 {
		t.Fatalf("actor july=%+v", july)
	}
	if _, ok := monthPresent(rep.ByMonth, "2026-10"); ok {
		t.Fatalf("bob's october leaked into alice's months: %+v", rep.ByMonth)
	}
}

func monthPresent(rows []Row, key string) (Row, bool) {
	for _, r := range rows {
		if r.Key == key {
			return r, true
		}
	}
	return Row{}, false
}

func TestBuildScopesByProjectAndActor(t *testing.T) {
	src := threads{{
		ThreadID: "t1", Project: "app",
		Turns: []history.Turn{
			turn("app", "u1", "alice", "claude-opus-5", usage(1_000_000, 0, 0, 0), "2026-07-01T00:00:00Z"),
			turn("app", "u2", "bob", "claude-opus-5", usage(2_000_000, 0, 0, 0), "2026-07-02T00:00:00Z"),
		},
	}, {
		ThreadID: "t2", Project: "api",
		Turns: []history.Turn{
			turn("api", "u1", "alice", "claude-opus-5", usage(4_000_000, 0, 0, 0), "2026-07-03T00:00:00Z"),
		},
	}}
	rep, err := Build(src, pricer(t), Query{Project: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Turns != 2 || len(rep.ByProject) != 1 || rep.ByProject[0].Key != "app" {
		t.Fatalf("project scope=%+v", rep)
	}
	rep, err = Build(src, pricer(t), Query{ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Turns != 2 || rep.Total.TotalTokens() != 5_000_000 {
		t.Fatalf("actor scope=%+v", rep.Total)
	}
}

func TestForThreadSumsOneSession(t *testing.T) {
	th := history.Thread{ThreadID: "t1", Project: "app", Turns: []history.Turn{
		turn("app", "u1", "alice", "claude-opus-5", usage(1_000_000, 0, 0, 0), "2026-07-01T00:00:00Z"),
		turn("app", "u1", "alice", "claude-opus-5", usage(0, 0, 0, 1_000_000), "2026-07-02T00:00:00Z"),
		{At: "2026-07-03T00:00:00Z", Prompt: "no usage", Project: "app"},
	}}
	row := ForThread(th, pricer(t))
	if row.Turns != 2 || row.TotalTokens() != 2_000_000 {
		t.Fatalf("row=%+v", row)
	}
	if got := row.Dollars; got < 29.999 || got > 30.001 {
		t.Fatalf("dollars=%v want 30", got)
	}
	if row.Key != "t1" || row.Project != "app" {
		t.Fatalf("identity=%+v", row)
	}
	// No pricer at all (nobody configured rates): tokens still report, cost does not.
	bare := ForThread(th, nil)
	if bare.TotalTokens() != 2_000_000 || bare.HasCost() {
		t.Fatalf("nil pricer=%+v", bare)
	}
}

// A model that never stamped a name still has to appear, or its tokens vanish
// from every total. It cannot be priced — there is nothing to look up.
func TestUnnamedModelCountsTokensAsUnpriced(t *testing.T) {
	src := threads{{ThreadID: "t1", Project: "app", Turns: []history.Turn{
		turn("app", "u1", "alice", "", usage(1000, 0, 0, 10), "2026-07-01T00:00:00Z"),
	}}}
	rep, err := Build(src, pricer(t), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.TotalTokens() != 1010 || rep.Total.Unpriced != 1 {
		t.Fatalf("total=%+v", rep.Total)
	}
	if len(rep.Total.Models) != 1 || rep.Total.Models[0] != "" {
		t.Fatalf("models=%q", rep.Total.Models)
	}
	if len(rep.UnpricedModels) != 1 || rep.UnpricedModels[0] != "" {
		t.Fatalf("unpricedModels=%q", rep.UnpricedModels)
	}
}

// A CLI that reports only a run total still contributes its tokens.
func TestTotalOnlyUsageStillCountsTokens(t *testing.T) {
	src := threads{{ThreadID: "t1", Project: "app", Turns: []history.Turn{
		{
			At: "2026-07-01T00:00:00Z", Project: "app", Prompt: "p", UserID: "u1",
			Model: "grok-4.5", Usage: &history.Usage{TotalTokens: 350},
		},
	}}}
	rep, err := Build(src, pricer(t), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.TotalTokens() != 350 || rep.Total.Turns != 1 {
		t.Fatalf("total=%+v", rep.Total)
	}
}
