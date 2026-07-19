package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestClientIDFromToken(t *testing.T) {
	id := "123456789012345678"
	token := base64.StdEncoding.EncodeToString([]byte(id)) + ".xxx.yyy"
	got, err := ClientIDFromToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("got %q want %q", got, id)
	}

	// No padding
	raw := base64.RawStdEncoding.EncodeToString([]byte(id)) + ".a.b"
	got, err = ClientIDFromToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("raw got %q", got)
	}

	if _, err := ClientIDFromToken("not-a-token"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ClientIDFromToken("Bot " + token); err != nil {
		t.Fatalf("Bot prefix: %v", err)
	}
}

func TestBuildInviteURL(t *testing.T) {
	u := BuildInviteURL("99", BotInvitePermissions, BotInviteScopes)
	if !strings.HasPrefix(u, "https://discord.com/oauth2/authorize?") {
		t.Fatalf("url=%s", u)
	}
	if !strings.Contains(u, "client_id=99") {
		t.Fatalf("missing client_id: %s", u)
	}
	if !strings.Contains(u, "scope=bot") {
		t.Fatalf("missing scope: %s", u)
	}
	if !strings.Contains(u, "permissions=") {
		t.Fatalf("missing permissions: %s", u)
	}
	// Includes PIN_MESSAGES (1<<51); Manage Messages alone no longer allows pin.
	want := int64((1 << 10) | (1 << 11) | (1 << 13) | (1 << 15) | (1 << 16) | (1 << 35) | (1 << 38) | (1 << 51))
	if BotInvitePermissions != want {
		t.Fatalf("permissions=%d want %d", BotInvitePermissions, want)
	}
	if BotInvitePermissions&(1<<51) == 0 {
		t.Fatal("missing PIN_MESSAGES bit")
	}
}

func TestConfigInviteURL(t *testing.T) {
	id := "987654321098765432"
	cfg := &Config{
		DiscordToken: base64.StdEncoding.EncodeToString([]byte(id)) + ".sig.hmac",
	}
	u, err := cfg.InviteURL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "client_id="+id) {
		t.Fatalf("url=%s", u)
	}

	cfg.DiscordClientID = "111"
	u, err = cfg.InviteURL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "client_id=111") {
		t.Fatalf("explicit override failed: %s", u)
	}

	snap := cfg.Snapshot()
	if snap.ClientID != "111" || snap.InviteURL == "" || snap.InviteError != "" {
		t.Fatalf("snapshot=%+v", snap)
	}
}
