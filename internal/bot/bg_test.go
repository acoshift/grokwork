package bot

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestStopCancelsBackgroundContext(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Projects:   config.PathProjects(map[string]string{"p": dir}),
		DataDir:    filepath.Join(dir, "data"),
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(cfg, store, hist)
	ctx := b.bgContext()
	if ctx.Err() != nil {
		t.Fatal("bg context already cancelled")
	}
	b.Stop(context.Background())
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("bg context not cancelled after Stop")
	}
}

func TestSleepCtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("sleepCtx should return false when already cancelled")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel2()
	}()
	start := time.Now()
	if sleepCtx(ctx2, time.Hour) {
		t.Fatal("sleepCtx should return false when cancelled mid-wait")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("sleepCtx did not return promptly on cancel")
	}
}
