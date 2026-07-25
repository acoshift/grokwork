package deploy

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

type fakeNotifier struct {
	mu       sync.Mutex
	channel  []string
	inbox    []string
	chanErr  error
	inboxErr error
}

func (f *fakeNotifier) SendChannel(_, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chanErr != nil {
		return f.chanErr
	}
	f.channel = append(f.channel, content)
	return nil
}

func (f *fakeNotifier) SendInbox(_, subject, body, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inboxErr != nil {
		return f.inboxErr
	}
	f.inbox = append(f.inbox, subject+"\n"+body)
	return nil
}

func (f *fakeNotifier) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.channel...), append([]string(nil), f.inbox...)
}

func noticeEngine(t *testing.T) (*Engine, *fakeNotifier) {
	t.Helper()
	eng, _, _ := testEngine(t, twoStepManifest)
	n := &fakeNotifier{}
	eng.SetNotifier(n)
	eng.SetPublicBaseURL("https://grokwork.internal/")
	return eng, n
}

func succeededRun() Run {
	return Run{
		Project: "app", Service: "api", Env: "prod", ShortSHA: "abc1234",
		Status: StatusSucceeded, ActorName: "Alice", ActorID: "u1",
		StartedAt: "2026-07-20T10:00:00Z", EndedAt: "2026-07-20T10:02:14Z",
		Steps: []StepRecord{{Name: "build", Status: StatusSucceeded}, {Name: "rollout", Status: StatusSucceeded}},
	}
}

func TestFormatNoticeSuccess(t *testing.T) {
	eng, _ := noticeEngine(t)
	got := eng.formatNotice(succeededRun())
	for _, want := range []string{"✅", "api", "prod", "deployed", "abc1234", "2m14s", "Alice", "2 steps"} {
		if !strings.Contains(got, want) {
			t.Fatalf("notice missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "https://grokwork.internal/projects/app/deploys/") {
		t.Fatalf("no run link:\n%s", got)
	}
}

func TestFormatNoticeNamesFailedStep(t *testing.T) {
	eng, _ := noticeEngine(t)
	r := succeededRun()
	r.Status = StatusFailed
	r.Steps[1].Status = StatusFailed
	got := eng.formatNotice(r)
	if !strings.Contains(got, "❌") || !strings.Contains(got, "rollout") {
		t.Fatalf("notice does not name the failed step:\n%s", got)
	}
}

// TestDeployNoticeHasNoLocalPath: local paths may appear in the web UI but must
// never reach Discord.
func TestDeployNoticeHasNoLocalPath(t *testing.T) {
	eng, _ := noticeEngine(t)
	r := succeededRun()
	r.CheckoutPath = "/Users/someone/Projects/secret-client/data/deploys/checkouts/app/d_x"
	got := eng.formatNotice(r)
	for _, leak := range []string{"/Users/", "/data/deploys/", "checkouts"} {
		if strings.Contains(got, leak) {
			t.Fatalf("notice leaked a local path (%q):\n%s", leak, got)
		}
	}
}

func TestDeployNoticeClampedToDiscordLimit(t *testing.T) {
	eng, _ := noticeEngine(t)
	r := succeededRun()
	r.Service = strings.Repeat("s", 4000)
	got := eng.formatNotice(r)
	if len(got) > discordMaxMsg {
		t.Fatalf("notice is %d bytes, over the %d Discord cap", len(got), discordMaxMsg)
	}
}

func TestNotifyFallsBackToInboxWithoutChannel(t *testing.T) {
	eng, n := noticeEngine(t)
	n.chanErr = errors.New("no Discord channel mapped")
	r := succeededRun()
	r.Status = StatusFailed
	r.Steps[1].Status = StatusFailed

	eng.notifyFinished(r)
	ch, ib := n.snapshot()
	if len(ch) != 0 {
		t.Fatalf("channel got %d messages despite the error", len(ch))
	}
	// A web-only project's failed prod deploy must still reach someone.
	if len(ib) != 1 || !strings.Contains(ib[0], "failed") {
		t.Fatalf("inbox fallback did not fire: %v", ib)
	}
}

// TestNotifySuccessDoesNotChaseTheInbox: a success with no channel is not worth
// a personal notification; only non-success is.
func TestNotifySuccessDoesNotChaseTheInbox(t *testing.T) {
	eng, n := noticeEngine(t)
	n.chanErr = errors.New("no Discord channel mapped")
	eng.notifyFinished(succeededRun())
	_, ib := n.snapshot()
	if len(ib) != 0 {
		t.Fatalf("inbox got a success notice: %v", ib)
	}
}

func TestNotifyUsesChannelWhenAvailable(t *testing.T) {
	eng, n := noticeEngine(t)
	eng.notifyFinished(succeededRun())
	ch, ib := n.snapshot()
	if len(ch) != 1 {
		t.Fatalf("channel got %d messages, want 1", len(ch))
	}
	if len(ib) != 0 {
		t.Fatalf("inbox also notified: %v", ib)
	}
}

func TestNotifyNoBaseURLOmitsLink(t *testing.T) {
	eng, _ := noticeEngine(t)
	eng.SetPublicBaseURL("")
	got := eng.formatNotice(succeededRun())
	if strings.Contains(got, "http") {
		t.Fatalf("notice invented a link with no public base URL:\n%s", got)
	}
}
