package web

import (
	"testing"

	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestLiveRevsStableAndChange(t *testing.T) {
	srv, cfg, _ := testServer(t)

	a := srv.computeLiveRevs()
	b := srv.computeLiveRevs()
	if a != b {
		t.Fatalf("revs unstable without changes:\n a=%+v\n b=%+v", a, b)
	}
	for _, rev := range []string{a.Dashboard, a.Ship, a.History, a.Worktrees, a.Config} {
		if rev == "" {
			t.Fatal("expected non-empty revs")
		}
	}

	// Config mutation should move Config fingerprint.
	if err := cfg.AddAllowedUser("user-live-rev"); err != nil {
		t.Fatal(err)
	}
	c := srv.computeLiveRevs()
	if c.Config == a.Config {
		t.Fatal("config rev should change after allowlist add")
	}
	if c.Ship != a.Ship {
		t.Fatal("ship rev should not change on allowlist add")
	}

	// History append moves history rev.
	beforeHist := c.History
	if err := srv.history.Append("thread-new", history.Turn{
		User: "bob", Prompt: "do thing", Response: "done", Status: "done", Project: "proj",
	}); err != nil {
		t.Fatal(err)
	}
	d := srv.computeLiveRevs()
	if d.History == beforeHist {
		t.Fatal("history rev should change after new turn")
	}

	// Session with PR moves ship rev.
	beforeShip := d.Ship
	if err := srv.sessions.Set("thread-pr", sessionstore.Entry{
		SessionID: "s1",
		Project:   "proj",
		PRs: []sessionstore.TrackedPR{{
			URL:    "https://github.com/o/r/pull/1",
			Number: 1,
			State:  "OPEN",
			Title:  "feat",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	e := srv.computeLiveRevs()
	if e.Ship == beforeShip {
		t.Fatal("ship rev should change after PR session")
	}
}
