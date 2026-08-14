package agentauth

import (
	"testing"
	"time"
)

func TestMintVerifyRevoke(t *testing.T) {
	s := NewStore()
	raw, cred, err := s.Mint("t1", "app", "actor1", "run1", DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || cred.ThreadID != "t1" || cred.Project != "app" {
		t.Fatalf("cred=%+v raw empty=%v", cred, raw == "")
	}
	got, err := s.Verify(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != cred.ID || got.ThreadID != "t1" {
		t.Fatalf("%+v", got)
	}
	// Cannot act as another thread via token — binding is fixed.
	if got.ThreadID == "other" {
		t.Fatal("impossible")
	}
	s.Revoke(cred.ID)
	if _, err := s.Verify(raw); err == nil {
		t.Fatal("expected revoke")
	}
}

func TestVerifyRejectsForeignAndExpired(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	raw, _, err := s.Mint("t1", "app", "a", "", DefaultShipCaps(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify("not-a-token"); err == nil {
		t.Fatal("expected invalid")
	}
	s.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := s.Verify(raw); err == nil {
		t.Fatal("expected expired")
	}
}

func TestDefaultShipCapsIncludeLinearRead(t *testing.T) {
	if !DefaultShipCaps().LinearRead {
		t.Fatal("LinearRead")
	}
}

func TestDefaultInvestigateCapsAreReadOnly(t *testing.T) {
	c := DefaultInvestigateCaps()
	if !c.SessionRead || !c.PRsList || !c.IssuesList || !c.StorageRead || !c.ClickUpRead || !c.LinearRead {
		t.Fatalf("missing reads: %+v", c)
	}
	if c.SessionDone || c.SessionAbandon || c.ReviewRequest || c.StorageWrite {
		t.Fatalf("writes must be off: %+v", c)
	}
}

func TestRevokeThread(t *testing.T) {
	s := NewStore()
	raw, _, err := s.Mint("t1", "app", "a", "", DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s.RevokeThread("t1")
	if _, err := s.Verify(raw); err == nil {
		t.Fatal("expected revoked")
	}
}
