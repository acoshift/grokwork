package inbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUnreadWatermark(t *testing.T) {
	st := newStore(t)
	for _, s := range []string{"a", "b", "c"} {
		if _, err := st.Append("oidc:alice", Item{Subject: s}); err != nil {
			t.Fatal(err)
		}
	}
	if n := st.UnreadCount("oidc:alice"); n != 3 {
		t.Fatalf("unread after append = %d, want 3", n)
	}
	if err := st.MarkRead("oidc:alice", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead("oidc:alice", 2); err != nil {
		t.Fatal(err)
	}
	if n := st.UnreadCount("oidc:alice"); n != 1 {
		t.Fatalf("unread after marking 1 then 2 = %d, want 1", n)
	}
	if err := st.MarkAllRead("oidc:alice"); err != nil {
		t.Fatal(err)
	}
	if n := st.UnreadCount("oidc:alice"); n != 0 {
		t.Fatalf("unread after mark-all = %d, want 0", n)
	}
}

func TestMarkReadDoesNotHideNewer(t *testing.T) {
	st := newStore(t)
	for _, s := range []string{"a", "b", "c"} {
		if _, err := st.Append("oidc:alice", Item{Subject: s}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkRead("oidc:alice", 1); err != nil {
		t.Fatal(err)
	}
	if n := st.UnreadCount("oidc:alice"); n != 2 {
		t.Fatalf("unread after marking 1 = %d, want 2 (2 and 3 stay)", n)
	}
	c := st.ReadCursor("oidc:alice")
	if c.Unread(1) {
		t.Fatal("seq 1 should be read")
	}
	if !c.Unread(2) || !c.Unread(3) {
		t.Fatalf("2 and 3 should stay unread: %+v", c)
	}
}

func TestMarkReadCompactsThrough(t *testing.T) {
	st := newStore(t)
	for _, s := range []string{"a", "b", "c"} {
		if _, err := st.Append("oidc:alice", Item{Subject: s}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkRead("oidc:alice", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead("oidc:alice", 2); err != nil {
		t.Fatal(err)
	}
	c := st.ReadCursor("oidc:alice")
	if c.Through != 2 {
		t.Fatalf("through = %d, want 2 after marking 1 then 2", c.Through)
	}
	if len(c.Read) != 0 {
		t.Fatalf("read set should compact away: %+v", c.Read)
	}
}

func TestCursorSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("oidc:alice", Item{Subject: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("oidc:alice", Item{Subject: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("oidc:alice", Item{Subject: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead("oidc:alice", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead("oidc:alice", 2); err != nil {
		t.Fatal(err)
	}
	st2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := st2.UnreadCount("oidc:alice"); n != 1 {
		t.Fatalf("unread after restart = %d, want 1", n)
	}
}

func TestListDoesNotWriteCursor(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("oidc:alice", Item{Subject: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.List("oidc:alice"); err != nil {
		t.Fatal(err)
	}
	if n := st.UnreadCount("oidc:alice"); n != 1 {
		t.Fatalf("List must not mark read, unread=%d", n)
	}
	p, err := st.cursorPath("oidc:alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("cursor file appeared after List: %v", err)
	}
}

func TestUnreadCountEmptyActor(t *testing.T) {
	st := newStore(t)
	if n := st.UnreadCount(""); n != 0 {
		t.Fatalf("empty actor unread = %d, want 0", n)
	}
	if n := st.UnreadCount("   "); n != 0 {
		t.Fatalf("blank actor unread = %d, want 0", n)
	}
	if n := st.LastSeq(""); n != 0 {
		t.Fatalf("empty last seq = %d, want 0", n)
	}
}

func TestCursorJSONOmitsZeroThrough(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("oidc:alice", Item{Subject: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead("oidc:alice", 1); err != nil {
		t.Fatal(err)
	}
	// through compacted to 1; file should exist with through=1.
	p, err := st.cursorPath("oidc:alice")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["through"]; !ok {
		t.Fatalf("through=1 must be present: %s", raw)
	}
}

func TestCorruptCursorIsUnread(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("oidc:alice", Item{Subject: "a"}); err != nil {
		t.Fatal(err)
	}
	p, err := st.cursorPath("oidc:alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := st.UnreadCount("oidc:alice"); n != 1 {
		t.Fatalf("torn cursor must not hide the feed, unread=%d", n)
	}
}
