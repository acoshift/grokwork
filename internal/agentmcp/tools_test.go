package agentmcp

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/agentapi"
	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/projstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

type nopBot struct{}

func (nopBot) SoftAbandonSession(string, string) (string, error) { return "ok", nil }
func (nopBot) SetSessionLabel(string, string) error              { return nil }

func TestCallSessionGetAndStorage(t *testing.T) {
	dir := t.TempDir()
	sessions, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = sessions.Set("t1", sessionstore.Entry{Project: "app", Goal: "g"})
	storage, err := projstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	auth := agentauth.NewStore()
	svc := &agentapi.Service{Auth: auth, Sessions: sessions, Storage: storage, Bot: nopBot{}}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Call(context.Background(), svc, raw, ToolSessionGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := out.(agentapi.SessionInfo)
	if !ok || info.Goal != "g" {
		t.Fatalf("%T %+v", out, out)
	}
	_, err = Call(context.Background(), svc, raw, ToolStoragePut, map[string]any{
		"key": "k", "content": "v", "encoding": "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Call(context.Background(), svc, raw, ToolStorageGet, map[string]any{"key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	b64 := m["contentBase64"].(string)
	dec, _ := base64.StdEncoding.DecodeString(b64)
	if string(dec) != "v" {
		t.Fatalf("%q", dec)
	}
}

func TestToolDefsNonEmpty(t *testing.T) {
	if len(ToolDefs()) < 5 {
		t.Fatal(ToolDefs())
	}
	var hasGet, hasList bool
	for _, d := range ToolDefs() {
		switch d.Name {
		case ToolClickUpGetTask:
			hasGet = true
		case ToolClickUpList:
			hasList = true
		}
	}
	if !hasGet || !hasList {
		t.Fatalf("missing clickup tools: %+v", ToolDefs())
	}
	var hasLinGet, hasLinList, hasReviewers bool
	for _, d := range ToolDefs() {
		switch d.Name {
		case ToolLinearGetIssue:
			hasLinGet = true
		case ToolLinearList:
			hasLinList = true
		case ToolReviewersList:
			hasReviewers = true
		}
	}
	if !hasLinGet || !hasLinList || !hasReviewers {
		t.Fatalf("missing linear/reviewers tools: %+v", ToolDefs())
	}
}

func TestCallReviewersListAndLinearDispatch(t *testing.T) {
	auth := agentauth.NewStore()
	svc := &agentapi.Service{
		Auth: auth,
		ListEligibleReviewers: func(project string) []agentapi.ReviewerRow {
			if project != "app" {
				t.Fatalf("project=%q", project)
			}
			return []agentapi.ReviewerRow{{ID: "eng-1", Name: "eng-1"}}
		},
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Call(t.Context(), svc, raw, ToolReviewersList, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := out.([]agentapi.ReviewerRow)
	if !ok || len(rows) != 1 || rows[0].ID != "eng-1" {
		t.Fatalf("%T %+v", out, out)
	}
	if _, err := Call(t.Context(), svc, raw, ToolLinearGetIssue, map[string]any{"ref": "not-a-ticket"}); err == nil {
		t.Fatal("linear get must reject junk via shipped Call")
	}
}
