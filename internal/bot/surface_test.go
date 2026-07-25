package bot

import (
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func newSurfaceStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	st, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestHasDiscordSurface(t *testing.T) {
	st := newSurfaceStore(t)
	if err := st.Set("1234567890123456789", sessionstore.Entry{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("w_deadbeef", sessionstore.Entry{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	b := &Bot{sessions: st}

	cases := map[string]struct {
		unit string
		want bool
	}{
		"discord thread with entry": {"1234567890123456789", true},
		"web unit with entry":       {"w_deadbeef", false},
		"discord thread, no entry":  {"999888777666555444", true}, // shape fallback
		"web unit, no entry":        {"w_notstored", false},       // shape fallback
		"empty":                     {"", false},
	}
	for name, tc := range cases {
		if got := b.hasDiscordSurface(tc.unit); got != tc.want {
			t.Errorf("%s: hasDiscordSurface(%q) = %v, want %v", name, tc.unit, got, tc.want)
		}
	}
}

// TestHasDiscordSurfaceIgnoresOrigin is the misclassification guard. A run started
// from the web UI is Origin=web even when it DID open a Discord thread
// (web_task_start.go sets SourceWeb on both paths), so keying the surface off
// Origin would strip real threads of their status card, cards, and pings.
func TestHasDiscordSurfaceIgnoresOrigin(t *testing.T) {
	st := newSurfaceStore(t)
	if err := st.Set("555444333222111000", sessionstore.Entry{Project: "p", Origin: "web"}); err != nil {
		t.Fatal(err)
	}
	b := &Bot{sessions: st}
	if !b.hasDiscordSurface("555444333222111000") {
		t.Fatal("web-started run that opened a Discord thread must still have a Discord surface")
	}
}

// TestWebUnitHasNoPresentationWithGatewayUp pins the bug P1 fixes: `present` used
// to be `s != nil`, so a web-native run with a healthy gateway tried to post its
// status card to a synthetic unit id, logged a 4xx, and only then degraded.
// Degradation must be a decision, not an API rejection.
func TestWebUnitHasNoPresentationWithGatewayUp(t *testing.T) {
	st := newSurfaceStore(t)
	if err := st.Set("w_abc123", sessionstore.Entry{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	b := &Bot{sessions: st}

	// Stand in for a live gateway; the old expression `s != nil` was true here.
	gatewayUp := true
	present := gatewayUp && b.hasDiscordSurface("w_abc123")
	if present {
		t.Error("web-native unit must have no Discord presentation even with the gateway up")
	}

	// A real thread with the same live gateway still presents.
	if err := st.Set("321321321321321321", sessionstore.Entry{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if !(gatewayUp && b.hasDiscordSurface("321321321321321321")) {
		t.Error("Discord thread must still present with the gateway up")
	}
}

// TestNotifyRoutingSplitsBySurface pins the three-way split the old
// `present || IsWebUnitID(id)` expression encoded, which the rewrite must keep:
// a healthy thread pings in-thread, a web unit DMs, and a *degraded* thread
// (exists but Discord is refusing writes) is skipped rather than redirected.
func TestNotifyRoutingSplitsBySurface(t *testing.T) {
	st := newSurfaceStore(t)
	if err := st.Set("111222333444555666", sessionstore.Entry{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("w_web1", sessionstore.Entry{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	b := &Bot{sessions: st}

	notifies := func(unit string, present bool) bool {
		// mirrors the gate in executeTask with a live session
		return present || !b.hasDiscordSurface(unit)
	}
	if !notifies("111222333444555666", true) {
		t.Error("healthy thread must notify")
	}
	if !notifies("w_web1", false) {
		t.Error("web-native unit must notify (via DM)")
	}
	if notifies("111222333444555666", false) {
		t.Error("degraded thread must be skipped, not redirected to DMs")
	}
}
