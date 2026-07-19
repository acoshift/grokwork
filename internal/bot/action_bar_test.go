package bot

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestParseActionCustomID(t *testing.T) {
	action, tid, ok := parseActionCustomID("gd:cancel:123456789012345678")
	if !ok || action != actionCancel || tid != "123456789012345678" {
		t.Fatalf("got action=%q tid=%q ok=%v", action, tid, ok)
	}
	if _, _, ok := parseActionCustomID("other:cancel:1"); ok {
		t.Fatal("expected reject foreign prefix")
	}
	if _, _, ok := parseActionCustomID("gd:cancel:"); ok {
		t.Fatal("expected reject empty thread")
	}
	if _, _, ok := parseActionCustomID("gd:cancel:a:b"); ok {
		t.Fatal("expected reject extra colon")
	}
}

func TestActionBarRunningHasCancel(t *testing.T) {
	comps := actionBarRunning("t1")
	if len(comps) != 1 {
		t.Fatalf("rows=%d", len(comps))
	}
	row := comps[0].(discordgo.ActionsRow)
	if len(row.Components) != 1 {
		t.Fatalf("buttons=%d", len(row.Components))
	}
	btn := row.Components[0].(discordgo.Button)
	if btn.CustomID != actionCustomID(actionCancel, "t1") {
		t.Fatalf("customID=%q", btn.CustomID)
	}
	if btn.Style != discordgo.DangerButton {
		t.Fatalf("style=%v", btn.Style)
	}
}

func TestActionBarDoneButtons(t *testing.T) {
	comps := actionBarDone("t9")
	row := comps[0].(discordgo.ActionsRow)
	if len(row.Components) != 3 {
		t.Fatalf("buttons=%d want 3", len(row.Components))
	}
	want := []string{
		actionCustomID(actionContinue, "t9"),
		actionCustomID(actionReset, "t9"),
		actionCustomID(actionHistory, "t9"),
	}
	for i, c := range row.Components {
		btn := c.(discordgo.Button)
		if btn.CustomID != want[i] {
			t.Fatalf("btn %d customID=%q want %q", i, btn.CustomID, want[i])
		}
		if len(btn.CustomID) > 100 {
			t.Fatalf("custom_id too long: %d", len(btn.CustomID))
		}
	}
}

func TestContinueModal(t *testing.T) {
	resp := continueModal("thread1")
	if resp.Type != discordgo.InteractionResponseModal {
		t.Fatalf("type=%v", resp.Type)
	}
	if resp.Data.CustomID != actionCustomID(actionContinueMod, "thread1") {
		t.Fatalf("modal id=%q", resp.Data.CustomID)
	}
	if resp.Data.Title == "" {
		t.Fatal("empty title")
	}
}

func TestModalTextValue(t *testing.T) {
	data := discordgo.ModalSubmitInteractionData{
		Components: []discordgo.MessageComponent{
			&discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					&discordgo.TextInput{CustomID: continueModalPromptID, Value: "  add tests  "},
				},
			},
		},
	}
	if got := modalTextValue(data, continueModalPromptID); got != "add tests" {
		t.Fatalf("got %q", got)
	}
	if got := modalTextValue(data, "other"); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestPublicHistoryBase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{":8787", "http://127.0.0.1:8787"},
		{"0.0.0.0:8787", "http://127.0.0.1:8787"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000"},
		{"tailscale-host:8787", "http://tailscale-host:8787"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := publicHistoryBase(tc.in); got != tc.want {
			t.Fatalf("publicHistoryBase(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
	hint := historyHint("abc", ":8787")
	if !strings.Contains(hint, "http://127.0.0.1:8787/history/abc") {
		t.Fatalf("hint=%q", hint)
	}
}

func TestCancelCurrentRunIdle(t *testing.T) {
	b := &Bot{}
	msg, ok := b.cancelCurrentRun("nope", "u")
	if ok || msg == "" {
		t.Fatalf("ok=%v msg=%q", ok, msg)
	}
}

func TestCancelCurrentRunActive(t *testing.T) {
	b := &Bot{}
	const tid = "t-cancel"
	cancelled := false
	job := &runJob{cancel: func() { cancelled = true }, project: "p"}
	if claimed, _, err := b.claimOrEnqueue(tid, job, taskItem{threadID: tid}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	msg, ok := b.cancelCurrentRun(tid, "alice")
	if !ok || !cancelled {
		t.Fatalf("ok=%v cancelled=%v msg=%q", ok, cancelled, msg)
	}
	if !strings.Contains(msg, "Cancelling") {
		t.Fatalf("msg=%q", msg)
	}
}

func TestResetThreadCoreBusy(t *testing.T) {
	b := &Bot{}
	const tid = "t-busy"
	job := &runJob{cancel: func() {}, project: "p"}
	if claimed, _, err := b.claimOrEnqueue(tid, job, taskItem{threadID: tid}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	msg, err := b.resetThreadCore(tid)
	if err == nil || !strings.Contains(msg, "progress") {
		t.Fatalf("msg=%q err=%v", msg, err)
	}
}
