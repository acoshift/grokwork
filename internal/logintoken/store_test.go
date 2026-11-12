package logintoken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMintStoresHashOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	wire, rec, err := s.Mint(MintOpts{OwnerID: "member-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.TokenHash != "" {
		t.Fatal("Mint result included the hash")
	}
	if rec.OwnerID != "member-1" || rec.ID == "" {
		t.Fatalf("record: %+v", rec)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), wire) {
		t.Fatal("plaintext token was written to the store")
	}
	if !strings.Contains(string(raw), `"tokenHash"`) {
		t.Fatalf("store missing hash: %s", raw)
	}
	got, err := s.Authenticate(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenHash != "" || got.ID != rec.ID || got.OwnerID != "member-1" {
		t.Fatalf("authenticate: %+v", got)
	}
}

func TestAuthenticateWrongSecret(t *testing.T) {
	s := newTestStore(t)
	wire, rec, err := s.Mint(MintOpts{OwnerID: "member-1"})
	if err != nil {
		t.Fatal(err)
	}
	bad := wire[:len(wire)-1] + flip(wire[len(wire)-1])
	_, err = s.Authenticate(bad)
	if err == nil || AuthTokenID(err) != "" {
		t.Fatalf("wrong secret: err=%v id=%q", err, AuthTokenID(err))
	}
	if _, err := s.Authenticate("not-a-token"); err == nil || AuthTokenID(err) != "" {
		t.Fatalf("garbage: err=%v id=%q", err, AuthTokenID(err))
	}
	if _, err := s.Authenticate("gl_" + rec.ID + "_wrongsecret"); err == nil || AuthTokenID(err) != "" {
		t.Fatalf("same id wrong secret: err=%v id=%q", err, AuthTokenID(err))
	}
	if _, err := s.Authenticate(wire); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeBlocksLoginAndIsOwnerScoped(t *testing.T) {
	s := newTestStore(t)
	wire, rec, err := s.Mint(MintOpts{OwnerID: "member-1"})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.Mint(MintOpts{OwnerID: "admin-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("admin-1", rec.ID); err != ErrNotOwner {
		t.Fatalf("cross-account revoke: %v", err)
	}
	if _, err := s.Authenticate(wire); err != nil {
		t.Fatalf("token revoked by the wrong account: %v", err)
	}
	if err := s.Revoke("member-1", rec.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(wire)
	if err == nil || AuthTokenID(err) != rec.ID {
		t.Fatalf("revoked: err=%v id=%q", err, AuthTokenID(err))
	}
	if _, err := s.Authenticate(other); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("member-1", rec.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if err := s.Revoke("member-1", "0123456789abcdef"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestExpiryEnforcedAtAuthenticate(t *testing.T) {
	loc := time.FixedZone("ICT", 7*3600)
	start := time.Date(2026, 6, 15, 15, 0, 0, 0, loc)
	s := newTestStore(t)
	s.SetNow(func() time.Time { return start })

	end, err := ParseExpiry("2026-06-15", start, loc)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd := time.Date(2026, 6, 16, 0, 0, 0, 0, loc)
	if !end.Equal(wantEnd) {
		t.Fatalf("date-only expiry %v want %v", end, wantEnd)
	}
	wire, _, err := s.Mint(MintOpts{OwnerID: "member-1", ExpiresAt: end})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseExpiry("2026-06-14", start, loc); err != ErrPastExpiry {
		t.Fatalf("past date: %v", err)
	}
	if _, _, err := s.Mint(MintOpts{OwnerID: "member-1", ExpiresAt: start.Add(-time.Minute)}); err != ErrPastExpiry {
		t.Fatalf("past mint: %v", err)
	}
	if _, err := s.Authenticate(wire); err != nil {
		t.Fatal(err)
	}

	s.SetNow(func() time.Time { return time.Date(2026, 6, 15, 23, 59, 0, 0, loc) })
	if _, err := s.Authenticate(wire); err != nil {
		t.Fatalf("still the chosen date: %v", err)
	}
	s.SetNow(func() time.Time { return wantEnd })
	_, err = s.Authenticate(wire)
	if err == nil {
		t.Fatal("token accepted at the exclusive end")
	}
	if AuthTokenID(err) == "" {
		t.Fatal("expired match should name the public id")
	}

	instant, err := ParseExpiry("2026-06-15T18:30", start, loc)
	if err != nil {
		t.Fatal(err)
	}
	if !instant.Equal(time.Date(2026, 6, 15, 18, 30, 0, 0, loc)) {
		t.Fatalf("datetime expiry %v", instant)
	}
	s.SetNow(func() time.Time { return start })
	timed, _, err := s.Mint(MintOpts{OwnerID: "member-1", ExpiresAt: instant})
	if err != nil {
		t.Fatal(err)
	}
	s.SetNow(func() time.Time { return instant.Add(-time.Second) })
	if _, err := s.Authenticate(timed); err != nil {
		t.Fatal(err)
	}
	s.SetNow(func() time.Time { return instant })
	if _, err := s.Authenticate(timed); err == nil {
		t.Fatal("datetime token accepted at the instant")
	}

	open, _, err := s.Mint(MintOpts{OwnerID: "member-1"})
	if err != nil {
		t.Fatal(err)
	}
	s.SetNow(func() time.Time { return start.AddDate(10, 0, 0) })
	if _, err := s.Authenticate(open); err != nil {
		t.Fatalf("no-expiry token: %v", err)
	}
}

func TestListByOwnerHidesOtherAccounts(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Mint(MintOpts{OwnerID: "member-1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Mint(MintOpts{OwnerID: "admin-1"}); err != nil {
		t.Fatal(err)
	}
	mine := s.ListByOwner("member-1")
	if len(mine) != 1 || mine[0].OwnerID != "member-1" || mine[0].TokenHash != "" {
		t.Fatalf("list: %+v", mine)
	}
	if len(s.ListByOwner("stranger")) != 0 {
		t.Fatal("stranger saw tokens")
	}
}

func TestParseExpiryRejectsGarbage(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if _, err := ParseExpiry("tomorrow", now, time.UTC); err != ErrBadExpiry {
		t.Fatalf("garbage: %v", err)
	}
	got, err := ParseExpiry("  ", now, time.UTC)
	if err != nil || !got.IsZero() {
		t.Fatalf("blank: %v %v", got, err)
	}
	abs, err := ParseExpiry("2026-06-15T18:30:00Z", now, time.FixedZone("ICT", 7*3600))
	if err != nil {
		t.Fatal(err)
	}
	if !abs.Equal(time.Date(2026, 6, 15, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("rfc3339: %v", abs)
	}
}

func TestUnparseableFileRefusesToOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("truncated store opened")
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}
