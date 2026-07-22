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
	comps := actionBarRunning("t1", "")
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

func TestActionBarRunningWithWebLink(t *testing.T) {
	comps := actionBarRunning("t1", "http://ui.example/sessions/t1")
	row := comps[0].(discordgo.ActionsRow)
	if len(row.Components) != 2 {
		t.Fatalf("buttons=%d want 2", len(row.Components))
	}
	link := row.Components[1].(discordgo.Button)
	if link.Style != discordgo.LinkButton || link.URL != "http://ui.example/sessions/t1" {
		t.Fatalf("link=%+v", link)
	}
	if link.Label != "Open on Web" {
		t.Fatalf("label=%q", link.Label)
	}
	if link.CustomID != "" {
		t.Fatalf("link must not set custom_id: %q", link.CustomID)
	}
}

func TestActionBarDoneButtons(t *testing.T) {
	comps := actionBarDone("t9", "")
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

func TestActionBarDoneWithWebLink(t *testing.T) {
	comps := actionBarDone("t9", "http://ui.example/sessions/t9?project=api")
	row := comps[0].(discordgo.ActionsRow)
	if len(row.Components) != 4 {
		t.Fatalf("buttons=%d want 4", len(row.Components))
	}
	link := row.Components[3].(discordgo.Button)
	if link.Style != discordgo.LinkButton {
		t.Fatalf("style=%v", link.Style)
	}
	if link.URL != "http://ui.example/sessions/t9?project=api" {
		t.Fatalf("url=%q", link.URL)
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

func TestHistoryHint(t *testing.T) {
	hint := historyHint("abc", "http://100.x.y.z:8787/")
	if !strings.Contains(hint, "http://100.x.y.z:8787/history/abc") {
		t.Fatalf("hint=%q", hint)
	}
	if strings.Contains(hint, "//history") {
		t.Fatalf("double slash: %q", hint)
	}
	pathOnly := historyHint("abc", "")
	if !strings.Contains(pathOnly, "`/history/abc`") {
		t.Fatalf("path-only hint=%q", pathOnly)
	}
	if strings.Contains(pathOnly, "http") {
		t.Fatalf("path-only should not invent host: %q", pathOnly)
	}
}

func TestWebSessionURL(t *testing.T) {
	got := webSessionURL("abc", "http://100.x.y.z:8787/", "api")
	if got != "http://100.x.y.z:8787/sessions/abc?project=api" {
		t.Fatalf("got %q", got)
	}
	if webSessionURL("abc", "", "api") != "" {
		t.Fatal("empty base should yield empty URL")
	}
	if webSessionURL("", "http://x", "api") != "" {
		t.Fatal("empty thread should yield empty URL")
	}
	noProj := webSessionURL("tid", "http://ui/", "")
	if noProj != "http://ui/sessions/tid" {
		t.Fatalf("no project: %q", noProj)
	}
	// Path-escape thread IDs that need it (web-native w_ ids are safe; odd chars covered).
	if got := webSessionURL("a/b", "http://ui", ""); got != "http://ui/sessions/a%2Fb" {
		t.Fatalf("escape: %q", got)
	}
}

func TestWithWebSessionLine(t *testing.T) {
	if got := withWebSessionLine("Done · **api**", "http://ui/sessions/1"); got != "Done · **api**\nContinue on web: http://ui/sessions/1" {
		t.Fatalf("got %q", got)
	}
	if got := withWebSessionLine("Done", ""); got != "Done" {
		t.Fatalf("empty url: %q", got)
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
