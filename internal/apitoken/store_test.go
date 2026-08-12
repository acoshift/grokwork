package apitoken

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMintAuthenticateRevoke(t *testing.T) {
	s := newStore(t)
	wire, rec, err := s.Mint(MintOpts{
		Label:     "cloud-vm",
		Projects:  []string{"app", " app ", "app"},
		Caps:      CapsMask{StartSessions: true},
		CreatedBy: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ActorID != config.NormalizeActorID("token:"+rec.ID) {
		t.Fatalf("actor %q", rec.ActorID)
	}
	if rec.TokenHash != "" {
		t.Fatal("public copy leaked hash")
	}
	if len(rec.Projects) != 1 || rec.Projects[0] != "app" {
		t.Fatalf("projects=%v", rec.Projects)
	}
	if _, _, ok := parseWire(wire); !ok {
		t.Fatalf("minted wire unparseable: %s", wire)
	}

	got, err := s.Authenticate(wire)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.ID != rec.ID || got.ActorID != rec.ActorID {
		t.Fatalf("auth rec = %+v", got)
	}

	if _, err := s.Authenticate("gw_zzzzzzzzzz_nope"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := s.Authenticate(""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty: %v", err)
	}

	if err := s.Revoke(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(wire); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked still authenticates: %v", err)
	}
}

func TestExpiredTokenUnauthorized(t *testing.T) {
	s := newStore(t)
	s.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	wire, rec, err := s.Mint(MintOpts{ExpiresAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(wire); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) }
	if _, err := s.Authenticate(wire); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("after expiry: %v", err)
	}
	if _, ok := s.Get(rec.ID); !ok {
		t.Fatal("expired row should remain")
	}
}

func TestMalformedFileIsHardError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("malformed JSON must refuse to boot")
	}
}

func TestBootstrapInsertThenRevokeThenEnvDoesNotResurrect(t *testing.T) {
	s := newStore(t)
	const (
		id   = "k7m2p9qxab"
		wire = "gw_" + id + "_" + "supersecretbootstrapvalue"
	)
	inserted, err := s.bootstrap(wire, "app,api")
	if err != nil || !inserted {
		t.Fatalf("first bootstrap inserted=%v err=%v", inserted, err)
	}
	if _, err := s.Authenticate(wire); err != nil {
		t.Fatalf("bootstrap auth: %v", err)
	}
	if err := s.Revoke(id); err != nil {
		t.Fatal(err)
	}
	inserted, err = s.bootstrap(wire, "app")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("revoked publicId must not re-insert")
	}
	if _, err := s.Authenticate(wire); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked bootstrap still authenticates: %v", err)
	}
	if !s.HasPublicID(id) {
		t.Fatal("tombstone missing")
	}
	listed := s.List()
	if len(listed) != 1 || listed[0].RevokedAt.IsZero() {
		t.Fatalf("want one revoked row, got %+v", listed)
	}
}

func TestIntersectMask(t *testing.T) {
	team := config.BuiltinCapabilityTemplates["builder"]
	got := Intersect(team, CapsMask{StartSessions: true})
	if !got.StartSessions || got.GithubWrites || got.CanShip() || got.Merge || got.AdminProject {
		t.Fatalf("mask leak: %+v", got)
	}
	if !got.Investigate {
		t.Fatal("investigate is informational from the team")
	}
	zero := Intersect(config.Capabilities{}, CapsMask{StartSessions: true, GithubWrites: true})
	if zero.StartSessions || zero.GithubWrites {
		t.Fatalf("empty team must stay zero: %+v", zero)
	}
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	s := newStore(t)
	_, rec, err := s.Mint(MintOpts{Label: "x"})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"prompt":"a"}`)
	h := BodyHash(body)
	if _, ok, err := s.IdempotencyGet(rec.ID, "k1", h); err != nil || ok {
		t.Fatalf("empty get: ok=%v err=%v", ok, err)
	}
	if err := s.IdempotencyPut(rec.ID, "k1", IdemRecord{
		BodyHash: h, Status: 201, Response: []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.IdempotencyGet(rec.ID, "k1", h)
	if err != nil || !ok || got.Status != 201 || string(got.Response) != `{"ok":true}` {
		t.Fatalf("replay = %+v ok=%v err=%v", got, ok, err)
	}
	_, _, err = s.IdempotencyGet(rec.ID, "k1", BodyHash([]byte(`{"prompt":"b"}`)))
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestIdempotencyPruneTTLThenCap(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	_, rec, err := s.Mint(MintOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// one expired, one fresh
	s.now = func() time.Time { return base.Add(-30 * time.Hour) }
	if err := s.IdempotencyPut(rec.ID, "old", IdemRecord{BodyHash: "a", Status: 201, Response: []byte("1")}); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base }
	if err := s.IdempotencyPut(rec.ID, "new", IdemRecord{BodyHash: "b", Status: 201, Response: []byte("2")}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.IdempotencyGet(rec.ID, "old", "a"); err != nil || ok {
		t.Fatalf("expired key should be pruned: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.IdempotencyGet(rec.ID, "new", "b"); err != nil || !ok {
		t.Fatalf("fresh key missing: ok=%v err=%v", ok, err)
	}
}

func TestMintForTestRoundTrip(t *testing.T) {
	s := newStore(t)
	const wire = "gw_testhashid_secretvalue"
	if err := s.MintForTest("testhashid", wire, Record{
		Label:    "fixture",
		Projects: []string{"app"},
		Caps:     CapsMask{StartSessions: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Authenticate(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActorID != "token:testhashid" || got.Label != "fixture" {
		t.Fatalf("%+v", got)
	}
}
