package bot

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduleRepoFetchDoesNotBlock(t *testing.T) {
	orig := runRepoFetch
	t.Cleanup(func() { runRepoFetch = orig })

	started := make(chan struct{})
	release := make(chan struct{})
	var got string
	runRepoFetch = func(ctx context.Context, repo string) (bool, error) {
		got = repo
		close(started)
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := &Bot{bgCtx: ctx}
	cwd := t.TempDir()

	returned := make(chan struct{})
	go func() {
		b.ScheduleRepoFetch(cwd, "project=proj acme/app")
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("ScheduleRepoFetch blocked on the fetch")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fetch did not start")
	}
	if got != cwd {
		t.Fatalf("fetch path=%q want %q", got, cwd)
	}
	close(release)

	entered := make(chan struct{})
	finished := make(chan error, 1)
	runRepoFetch = func(ctx context.Context, repo string) (bool, error) {
		close(entered)
		<-ctx.Done()
		finished <- ctx.Err()
		return false, ctx.Err()
	}
	b.ScheduleRepoFetch(cwd, "project=proj acme/app")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second fetch did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight fetch was not cancelled with the bot")
	}
}

func TestScheduleRepoFetchRetries(t *testing.T) {
	orig := runRepoFetch
	t.Cleanup(func() { runRepoFetch = orig })

	done := make(chan struct{})
	var n atomic.Int32
	runRepoFetch = func(ctx context.Context, repo string) (bool, error) {
		if n.Add(1) < int32(postMergeFetchAttempts) {
			return false, errors.New("unable to create lock")
		}
		close(done)
		return true, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	(&Bot{bgCtx: ctx}).ScheduleRepoFetch(t.TempDir(), "project=proj acme/app")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("attempts=%d", n.Load())
	}
	if n.Load() != int32(postMergeFetchAttempts) {
		t.Fatalf("attempts=%d", n.Load())
	}
}

func TestScheduleRepoFetchNilAndEmpty(t *testing.T) {
	var b *Bot
	b.ScheduleRepoFetch(t.TempDir(), "x") // nil receiver

	orig := runRepoFetch
	t.Cleanup(func() { runRepoFetch = orig })
	called := make(chan struct{}, 1)
	runRepoFetch = func(ctx context.Context, repo string) (bool, error) {
		called <- struct{}{}
		return false, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	(&Bot{bgCtx: ctx}).ScheduleRepoFetch("  ", "project=proj acme/app")
	select {
	case <-called:
		t.Fatal("empty path must not fetch")
	case <-time.After(50 * time.Millisecond):
	}
}
