package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/timeline"
)

// TestSessionPageShowsRecoveredOutput is the user-visible half of the data-loss
// fix. A cancelled run records an empty history response, so the session page
// used to show "(no response text yet)" and the work was simply gone for a
// web-native unit. The per-unit timeline is the only surviving copy.
func TestSessionPageShowsRecoveredOutput(t *testing.T) {
	srv, _, _ := testServer(t)

	// A turn that ended with no final reply — exactly what cancel produces.
	if err := srv.history.Append("thread-99", history.Turn{
		Prompt: "do the thing",
		Status: "cancelled",
		Error:  "Cancelled",
	}); err != nil {
		t.Fatal(err)
	}
	events := srv.bot.Events()
	if events == nil {
		t.Fatal("timeline store not initialized on the test bot")
	}
	if _, err := events.Append("thread-99", timeline.KindTextBlock,
		timeline.TextBlock{Text: "I inspected the flaky test and found a race"}); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Append("thread-99", timeline.KindRunDone,
		timeline.RunDone{Status: "cancelled"}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99?project=proj", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, "I inspected the flaky test and found a race") {
		t.Error("recovered run output missing from the session page")
	}
	if !strings.Contains(body, `class="bubble-note`) {
		t.Error("recovered output must be labelled as such, not passed off as a normal reply")
	}
	if strings.Contains(body, "(no response text yet)") {
		t.Error("still showing the dead-end placeholder despite having a durable record")
	}
}

// TestSessionPagePlaceholderWithoutTimeline keeps the honest empty state for a
// run that genuinely produced nothing.
func TestSessionPagePlaceholderWithoutTimeline(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.history.Append("thread-99", history.Turn{
		Prompt: "do the thing",
		Status: "error",
		Error:  "boom",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99?project=proj", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "(no response text yet)") {
		t.Error("with no timeline record the honest placeholder must remain")
	}
	if strings.Contains(body, `class="bubble-note`) {
		t.Error("provenance note rendered with nothing to attribute")
	}
}

// TestRecoveredOutputOnlyOnNewestTurn guards the attribution rule: the timeline
// spans every run in a unit, so an older turn must not be back-filled with a
// later run's output.
func TestRecoveredOutputOnlyOnNewestTurn(t *testing.T) {
	srv, _, _ := testServer(t)
	for _, turn := range []history.Turn{
		{Prompt: "first ask", Response: "first answer", Status: "done"},
		{Prompt: "second ask", Status: "cancelled", Error: "Cancelled"},
	} {
		if err := srv.history.Append("thread-99", turn); err != nil {
			t.Fatal(err)
		}
	}
	events := srv.bot.Events()
	if _, err := events.Append("thread-99", timeline.KindTextBlock,
		timeline.TextBlock{Text: "SECOND RUN PARTIAL"}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99?project=proj", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()

	if strings.Count(body, "SECOND RUN PARTIAL") != 1 {
		t.Errorf("recovered text appears %d times, want exactly once (newest turn only)", strings.Count(body, "SECOND RUN PARTIAL"))
	}
	if !strings.Contains(body, "first answer") {
		t.Error("earlier turn's real response was lost")
	}
}

// TestWebNativeUnitGetsCompletionSummary closes C3 row 1. The completion summary
// used to be computed only as a side effect of posting a Discord card, and its
// collector returned early on a nil session — so a web-native unit (every commit
// review, every PR dispatch) never got one anywhere.
func TestWebNativeUnitGetsCompletionSummary(t *testing.T) {
	srv, _, _ := testServer(t)
	events := srv.bot.Events()
	if events == nil {
		t.Fatal("timeline store missing")
	}
	if _, err := events.Append("thread-99", timeline.KindCompletion, bot.CompletionCardInput{
		Status:  "Done",
		Project: "proj",
		Branch:  "grokwork/thread-99",
		Diff: bot.DiffSummary{
			FileCount:  3,
			Insertions: 42,
			Deletions:  7,
			Risky:      []string{"internal/config/config.go"},
			HasCommits: true,
		},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99?project=proj", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="session-completion-panel"`) {
		t.Fatal("completion panel missing")
	}
	for _, want := range []string{"grokwork/thread-99", "3 files", "+42", "-7", "internal/config/config.go"} {
		if !strings.Contains(body, want) {
			t.Errorf("completion panel missing %q", want)
		}
	}
}

func TestNoCompletionPanelWithoutRecord(t *testing.T) {
	srv, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99?project=proj", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `id="session-completion-panel"`) {
		t.Error("panel rendered with no completion record")
	}
}
