package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestCaseIntakePromptAttachmentsOnly(t *testing.T) {
	got := caseIntakePrompt("high", "ZD-1", "Checkout 500s", "")
	if !strings.Contains(got, "Checkout 500s") {
		t.Fatalf("missing title: %q", got)
	}
	if !strings.Contains(got, "(no intake notes — see attached files)") {
		t.Fatalf("want attachments-only placeholder: %q", got)
	}
	got = caseIntakePrompt("low", "", "Title", "repro steps")
	if !strings.Contains(got, "repro steps") {
		t.Fatalf("notes body: %q", got)
	}
	if strings.Contains(got, "no intake notes") {
		t.Fatalf("must not placeholder when notes present: %q", got)
	}
}

func TestStartCaseAttachmentsOnlyQueuesInvestigate(t *testing.T) {
	b, _ := testBotWithData(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	b.cfg.GrokBin = writeFakeGrok(t)

	paths, cleanup, err := b.SaveWebAttachments([]WebUpload{webUpload("shot.png", tinyPNG)})
	if err != nil {
		t.Fatal(err)
	}
	// StartCase owns paths on success; cleanup only if we never hand off.
	handedOff := false
	t.Cleanup(func() {
		if !handedOff {
			cleanup()
		}
	})

	res, err := b.StartCase(StartCaseOpts{
		Project: "app", Title: "Screenshot of error", Severity: "high",
		Actor: Actor{ID: "u1", DisplayName: "U"}, AttachmentPaths: paths,
	})
	if err != nil {
		t.Fatal(err)
	}
	handedOff = true
	if res.Status == FixStatusOpened {
		t.Fatal("attachments-only intake must queue an investigate run, not open-only")
	}
	if res.ThreadID == "" {
		t.Fatal("empty thread id")
	}
	WaitIdleForTest(b, 5*time.Second)
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok {
		t.Fatal("session missing")
	}
	if e.Mode != "case" {
		t.Fatalf("mode=%q want case", e.Mode)
	}
	if e.Phase != sessionstore.PhaseInvestigate {
		t.Fatalf("phase=%q want investigate", e.Phase)
	}
}

func TestStartCaseEmptyNotesAndNoAttachmentsIsIntakeOnly(t *testing.T) {
	b, _ := testBotWithData(t)
	res, err := b.StartCase(StartCaseOpts{
		Project: "app", Title: "Just a title", Actor: Actor{ID: "u1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != FixStatusOpened {
		t.Fatalf("status=%q want opened", res.Status)
	}
	WaitIdleForTest(b, 2*time.Second)
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok {
		t.Fatal("session missing")
	}
	if e.Phase != sessionstore.PhaseIntake {
		t.Fatalf("phase=%q want intake", e.Phase)
	}
	if e.SessionID != "" {
		t.Fatalf("intake-only must not run Grok (sessionID=%q)", e.SessionID)
	}
}

func TestStartContinueAttachmentPathsReachTask(t *testing.T) {
	b, _ := testAddressBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })

	// Capture the model prompt file (enriched with attachment paths).
	dir := t.TempDir()
	promptDump := filepath.Join(dir, "prompt.txt")
	bin := filepath.Join(dir, "fake-grok")
	script := `#!/bin/sh
pf=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--prompt-file" ]; then pf="$a"; fi
  prev="$a"
done
if [ -n "$pf" ] && [ -f "$pf" ]; then cat "$pf" > "` + promptDump + `"; fi
printf '%s\n' '{"type":"text","data":"ok"}'
printf '%s\n' '{"type":"end","sessionId":"sess-att","stopReason":"EndTurn","num_turns":1,"usage":{"total_tokens":3}}'
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b.cfg.GrokBin = bin

	if err := b.sessions.Set("cont-att", sessionstore.Entry{Project: "app", Origin: SourceWeb}); err != nil {
		t.Fatal(err)
	}
	paths, cleanup, err := b.SaveWebAttachments([]WebUpload{webUpload("ui.png", tinyPNG)})
	if err != nil {
		t.Fatal(err)
	}
	handedOff := false
	t.Cleanup(func() {
		if !handedOff {
			cleanup()
		}
	})

	res, err := b.StartContinue(ContinueOpts{
		ThreadID: "cont-att", Prompt: "look at the screenshot",
		Actor: Actor{ID: "u", DisplayName: "U"}, AttachmentPaths: paths,
	})
	if err != nil {
		t.Fatal(err)
	}
	handedOff = true
	if res.ThreadID != "cont-att" {
		t.Fatalf("%+v", res)
	}
	WaitIdleForTest(b, 5*time.Second)

	raw, err := os.ReadFile(promptDump)
	if err != nil {
		t.Fatalf("prompt dump missing (run never saw attachments?): %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "look at the screenshot") {
		t.Fatalf("user prompt missing: %q", text)
	}
	if !strings.Contains(text, "Attached files (read these paths with your tools):") {
		t.Fatalf("attachment header missing: %q", text)
	}
	if !strings.Contains(text, "ui.png") {
		t.Fatalf("attachment path missing: %q", text)
	}
}
