package agentapi

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/projstore"
	"github.com/acoshift/grokwork/internal/reviewstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

type fakeBot struct {
	labels map[string]string
	soft   []string
}

func (f *fakeBot) SoftAbandonSession(threadID, who string) (string, error) {
	f.soft = append(f.soft, threadID+":"+who)
	return "abandoned", nil
}
func (f *fakeBot) SetSessionLabel(threadID, label string) error {
	if f.labels == nil {
		f.labels = map[string]string{}
	}
	f.labels[threadID] = label
	return nil
}

func testService(t *testing.T) (*Service, *agentauth.Store, *sessionstore.Store, *fakeBot) {
	t.Helper()
	dir := t.TempDir()
	sessions, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reviews, err := reviewstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := projstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	auth := agentauth.NewStore()
	fb := &fakeBot{}
	eligible := map[string]bool{"builder-1": true}
	svc := &Service{
		Auth:     auth,
		Sessions: sessions,
		Reviews:  reviews,
		Storage:  storage,
		Bot:      fb,
		Audit:    al,
		EligibleReviewer: func(project, reviewerID string) bool {
			return project == "app" && eligible[reviewerID]
		},
		DisplayName: func(id string) string { return id },
	}
	return svc, auth, sessions, fb
}

func TestSessionGetBoundToCred(t *testing.T) {
	svc, auth, sessions, _ := testService(t)
	if err := sessions.Set("t1", sessionstore.Entry{
		Project: "app", Goal: "fix", WorktreeBranch: "grokwork/t1",
		PRs: []sessionstore.TrackedPR{{Owner: "o", Repo: "r", Number: 1, URL: "https://github.com/o/r/pull/1"}},
	}); err != nil {
		t.Fatal(err)
	}
	raw, _, err := auth.Mint("t1", "app", "actor", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	info, err := svc.SessionGet(raw)
	if err != nil {
		t.Fatal(err)
	}
	if info.ThreadID != "t1" || info.Project != "app" || info.Goal != "fix" || len(info.PRs) != 1 {
		t.Fatalf("%+v", info)
	}
	// Token for t1 cannot read as if it were another thread — Get always uses cred.ThreadID.
	if info.ThreadID == "t-other" {
		t.Fatal("wrong thread")
	}
	// Different project token cannot list t1's storage.
	raw2, _, err := auth.Mint("t2", "other", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StoragePut(raw2, "k", "x", "", "text"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.StorageGet(raw, "k"); err == nil {
		t.Fatal("token for app must not read other project's key under same name without put")
	}
	// app project put/get — text encoding must not base64-decode "test".
	if _, err := svc.StoragePut(raw, "shared/x", "test", "text/plain", "text"); err != nil {
		t.Fatal(err)
	}
	b64probe, _, err := svc.StorageGet(raw, "shared/x")
	if err != nil {
		t.Fatal(err)
	}
	if dec, _ := base64.StdEncoding.DecodeString(b64probe); string(dec) != "test" {
		t.Fatalf("text put corrupted: %q", dec)
	}
	// base64 encoding path
	if _, err := svc.StoragePut(raw, "shared/x", base64.StdEncoding.EncodeToString([]byte("data")), "text/plain", "base64"); err != nil {
		t.Fatal(err)
	}
	b64, meta, err := svc.StorageGet(raw, "shared/x")
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := base64.StdEncoding.DecodeString(b64)
	if string(dec) != "data" || meta.Key != "shared/x" {
		t.Fatalf("%q %+v", dec, meta)
	}
	// Path escape
	if _, err := svc.StoragePut(raw, "../etc/passwd", "eHgg", "", "text"); err == nil {
		t.Fatal("expected path escape reject")
	}
}

func TestCredCannotActOnOtherThread(t *testing.T) {
	svc, auth, sessions, fb := testService(t)
	_ = sessions.Set("t1", sessionstore.Entry{Project: "app"})
	_ = sessions.Set("t2", sessionstore.Entry{Project: "app"})
	raw, _, err := auth.Mint("t1", "app", "actor", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SessionDone(raw); err != nil {
		t.Fatal(err)
	}
	if fb.labels["t1"] != "done" {
		t.Fatalf("labels=%v", fb.labels)
	}
	if fb.labels["t2"] != "" {
		t.Fatal("must not label other thread")
	}
	if err := svc.SessionAbandon(raw, "bye"); err != nil {
		t.Fatal(err)
	}
	if len(fb.soft) != 1 || !strings.HasPrefix(fb.soft[0], "t1:") {
		t.Fatalf("soft=%v", fb.soft)
	}
}

func TestListPRsSessionAndProject(t *testing.T) {
	svc, auth, sessions, _ := testService(t)
	_ = sessions.Set("t1", sessionstore.Entry{
		Project: "app",
		PRs:     []sessionstore.TrackedPR{{Owner: "o", Repo: "r", Number: 1}},
	})
	_ = sessions.Set("t2", sessionstore.Entry{
		Project: "app",
		PRs:     []sessionstore.TrackedPR{{Owner: "o", Repo: "r", Number: 2}},
	})
	_ = sessions.Set("t3", sessionstore.Entry{
		Project: "other",
		PRs:     []sessionstore.TrackedPR{{Owner: "o", Repo: "r", Number: 9}},
	})
	raw, _, _ := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	sess, err := svc.ListPRs(raw, "session")
	if err != nil || len(sess) != 1 || sess[0].Number != 1 {
		t.Fatalf("session: %v %+v", err, sess)
	}
	proj, err := svc.ListPRs(raw, "project")
	if err != nil || len(proj) != 2 {
		t.Fatalf("project: %v %+v", err, proj)
	}
}

func TestReviewRequestEligibility(t *testing.T) {
	svc, auth, _, _ := testService(t)
	raw, _, _ := auth.Mint("t1", "app", "actor", "", agentauth.DefaultShipCaps(), time.Hour)
	_, err := svc.RequestTeamReview(raw, "o", "r", 3, "investigator-1", "", "")
	if err == nil || !strings.Contains(err.Error(), "eligible") {
		t.Fatalf("expected ineligible, got %v", err)
	}
	req, err := svc.RequestTeamReview(raw, "o", "r", 3, "builder-1", "please", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if req.ReviewerID != "builder-1" || req.Project != "app" || req.ThreadID != "t1" {
		t.Fatalf("%+v", req)
	}
	if !strings.Contains(req.RequesterName, "agent") {
		t.Fatalf("requester name should note agent: %q", req.RequesterName)
	}
}


