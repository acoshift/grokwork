package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
)

func TestSessionTokenLoginStampSurvivesRenewAndReload(t *testing.T) {
	dir := t.TempDir()
	st, err := newSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateTokenLogin("member-1", "Member", "", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if !sess.TokenLogin {
		t.Fatal("CreateTokenLogin did not stamp the session")
	}
	plain, err := st.Create("member-1", "Member", "", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if plain.TokenLogin {
		t.Fatal("OAuth session was stamped as a token login")
	}

	raw, err := os.ReadFile(filepath.Join(dir, sessionsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"tokenLogin": true`) != 1 {
		t.Fatalf("persisted stamp:\n%s", raw)
	}

	st.mu.Lock()
	held := st.sessions[sess.ID]
	held.ExpiresAt = time.Now().Add(time.Hour)
	st.sessions[sess.ID] = held
	st.mu.Unlock()

	got, renewed, ok := st.Get(sess.ID)
	if !ok || !renewed || !got.TokenLogin {
		t.Fatalf("after renew: ok=%v renewed=%v token=%v", ok, renewed, got != nil && got.TokenLogin)
	}

	st2, err := newSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got2, _, ok := st2.Get(sess.ID)
	if !ok || !got2.TokenLogin {
		t.Fatal("reload dropped the token-login stamp")
	}
	gotPlain, _, ok := st2.Get(plain.ID)
	if !ok || gotPlain.TokenLogin {
		t.Fatal("reload stamped an OAuth session")
	}
}
