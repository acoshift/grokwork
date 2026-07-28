package bot

import (
	"context"
	"testing"
	"time"
)

// drainBotOnCleanup makes a bot finish its in-flight runs before the test's
// t.TempDir() is removed.
//
// A run writes under DataDir well past the point any assertion can observe.
// history.Append is not the last write: finishRun follows it, saving
// sessions.json through writeFileAtomic — which creates a "sessions.json.tmp-*"
// child of DataDir itself — and clearing the run journal under runs/. Measured
// 31-54ms after the final history turn landed.
//
// If TempDir's RemoveAll is walking DataDir when that temp file appears, it
// empties the directory, the temp file recreates a child, and the closing
// unlinkat fails with "directory not empty". The failure surfaces as a cleanup
// error naming the harness rather than the run, and it only shows up under load
// (a full-package run, or -count), because that is what delays finishRun past
// the removal. TestStartFixNoChannelGoesWebNative hit it roughly once in
// fifteen; it is not specific to that test, only to starting a run.
//
// Waiting on a proxy does not close the window. waitHistory returns one write
// too early by construction. WaitIdleForTest clears when st.job is nil and then
// sleeps a fixed 30ms, which sits inside the measured range. drainWG is the real
// signal: drainTaskQueue defers Done at the top, so Wait covers executeTask and
// finishRun both. Stop cancels before waiting, so a test that deliberately
// leaves a long run in flight cannot hang cleanup.
//
// Call this AFTER t.TempDir(); cleanups run LIFO, so it must be registered later
// to run earlier.
func drainBotOnCleanup(t *testing.T, b *Bot) {
	t.Helper()
	t.Cleanup(func() {
		// t.Context is already canceled when Cleanup runs.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.Stop(ctx)
	})
}
