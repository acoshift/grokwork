package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNotifyOnDoneDefaultsAndSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+filepath.ToSlash(dir)+`", "allowedUserIds": ["u1"]}},
  "channels": {},
  "grokBin": "grok"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path

	if got := cfg.NotifyOnDoneValue(); got != NotifyOnDoneErrors {
		t.Fatalf("default mode=%q", got)
	}
	if got := cfg.NotifyOnDoneLongMsValue(); got != DefaultNotifyOnDoneLongMs {
		t.Fatalf("default long=%d", got)
	}
	if err := cfg.SetNotifyOnDone("always", 0); err != nil {
		t.Fatal(err)
	}
	if cfg.NotifyOnDoneValue() != NotifyOnDoneAlways {
		t.Fatal(cfg.NotifyOnDoneValue())
	}
	if err := cfg.SetNotifyOnDone("long_only", 120_000); err != nil {
		t.Fatal(err)
	}
	if cfg.NotifyOnDoneValue() != NotifyOnDoneLongOnly || cfg.NotifyOnDoneLongMsValue() != 120_000 {
		t.Fatalf("%s %d", cfg.NotifyOnDoneValue(), cfg.NotifyOnDoneLongMsValue())
	}
	if err := cfg.SetNotifyOnDone("nope", 0); err == nil {
		t.Fatal("expected reject")
	}
	// Round-trip
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw2, &again); err != nil {
		t.Fatal(err)
	}
	if again.NotifyOnDone != NotifyOnDoneLongOnly || again.NotifyOnDoneLongMs != 120_000 {
		t.Fatalf("disk: %s", raw2)
	}
	snap := cfg.Snapshot()
	if snap.NotifyOnDone != NotifyOnDoneLongOnly || snap.NotifyOnDoneLongMs != 120_000 {
		t.Fatalf("snap %+v", snap)
	}
}

func TestNormalizeNotifyOnDone(t *testing.T) {
	if NormalizeNotifyOnDone("") != NotifyOnDoneErrors {
		t.Fatal("empty")
	}
	if NormalizeNotifyOnDone("ALWAYS") != NotifyOnDoneAlways {
		t.Fatal("case")
	}
	if NormalizeNotifyOnDone("weird") != NotifyOnDoneErrors {
		t.Fatal("invalid")
	}
}
