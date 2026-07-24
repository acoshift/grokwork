package bot

import (
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestCaseInvestigatePolicyNonShip(t *testing.T) {
	pol := BuildRunPolicy(PolicyInput{
		SessionMode:  ModeCase,
		SessionPhase: sessionstore.PhaseInvestigate,
		Caps:         config.BuiltinCapabilityTemplates["investigator"],
		ConfigYolo:   true,
		ShipMode:     sessionstore.ShipModeDirect,
	})
	if pol.Mode != ModeCase {
		t.Fatalf("Mode=%q want case", pol.Mode)
	}
	if pol.AllowPR || pol.AllowDirectShip || pol.AllowDirectIntegrate {
		t.Fatalf("case investigate must not ship: %+v", pol)
	}
	if pol.Yolo || pol.IncludeGHToken {
		t.Fatalf("yolo/token: %+v", pol)
	}
}

func TestCaseFixingPolicyShipsWithBuilder(t *testing.T) {
	pol := BuildRunPolicy(PolicyInput{
		SessionMode:  ModeCase,
		SessionPhase: sessionstore.PhaseFixing,
		Caps:         config.BuiltinCapabilityTemplates["builder"],
		ConfigYolo:   true,
		ShipMode:     sessionstore.ShipModePR,
	})
	if pol.Mode != ModeCase {
		t.Fatalf("Mode must stay case: %q", pol.Mode)
	}
	if !pol.AllowPR {
		t.Fatalf("fixing should allow PR: %+v", pol)
	}
}

func TestCaseFixingWithoutGithubWritesCoercesNonShip(t *testing.T) {
	pol := BuildRunPolicy(PolicyInput{
		SessionMode:  ModeCase,
		SessionPhase: sessionstore.PhaseFixing,
		Caps: config.Capabilities{
			StartSessions:  true,
			Investigate:    true,
			FileEscalation: true,
		},
		ConfigYolo: true,
		ShipMode:   sessionstore.ShipModePR,
	})
	if pol.Mode != ModeCase {
		t.Fatalf("Mode=%q", pol.Mode)
	}
	if pol.AllowPR || pol.AllowDirectIntegrate {
		t.Fatalf("must not ship without GithubWrites: %+v", pol)
	}
	if !pol.Coerced {
		t.Fatal("expected coerced")
	}
}

func TestEscalateKeepsCaseMode(t *testing.T) {
	// Pure: after escalate phase=fixing, mode still case in policy
	pol := BuildRunPolicy(PolicyInput{
		RequestedMode: ModeCase,
		SessionMode:   ModeCase,
		SessionPhase:  sessionstore.PhaseFixing,
		Caps:          config.BuiltinCapabilityTemplates["builder"],
		ShipMode:      sessionstore.ShipModePR,
	})
	if pol.Mode != ModeCase {
		t.Fatalf("escalated fix Mode=%q", pol.Mode)
	}
}

func TestSanitizeCustomerUpdateStripsPaths(t *testing.T) {
	raw := "We fixed it under /Users/acoshift/Projects/app and data/worktrees/x. GH_TOKEN=abc1234567890 is bad."
	clean, hits := SanitizeCustomerUpdate(raw)
	if strings.Contains(clean, "/Users/") || strings.Contains(clean, "data/worktrees") {
		t.Fatalf("path leaked: %q hits=%v", clean, hits)
	}
	if strings.Contains(clean, "GH_TOKEN=abc") {
		t.Fatalf("token leaked: %q", clean)
	}
	if len(hits) == 0 {
		t.Fatal("expected hits")
	}
	// CUSTOMER_UPDATE block
	raw2 := "internal note\nCUSTOMER_UPDATE:\nPlease retry the export."
	clean2, _ := SanitizeCustomerUpdate(raw2)
	if !strings.Contains(clean2, "Please retry") {
		t.Fatalf("block extract: %q", clean2)
	}
}

func TestParseDossierFromReply(t *testing.T) {
	text := "Here is what I found:\n```json\n{\"summary\":\"timeout in checkout\",\"hypotheses\":[\"race\"],\"nextActions\":[\"add lock\"]}\n```\n"
	d := ParseDossierFromReply(text)
	if d == nil || d.Summary != "timeout in checkout" {
		t.Fatalf("dossier=%+v", d)
	}
	merged := MergeDossier(nil, d)
	if merged.Summary != d.Summary {
		t.Fatal("merge nil dst")
	}
}

func TestParseCaseCommands(t *testing.T) {
	p := ParseMessage("<@1> /case high Checkout fails on iOS", "1")
	if p.Kind != KindCase {
		t.Fatalf("kind=%d", p.Kind)
	}
	sev, ref, title := parseCaseArgs(p.Prompt)
	if sev != "high" || title == "" || !strings.Contains(title, "Checkout") {
		t.Fatalf("sev=%q ref=%q title=%q", sev, ref, title)
	}
	p = ParseMessage("<@1> /escalate please own", "1")
	if p.Kind != KindEscalate {
		t.Fatalf("kind=%d", p.Kind)
	}
	p = ParseMessage("<@1> /close fixed shipped in 1.2", "1")
	if p.Kind != KindCloseCase {
		t.Fatalf("kind=%d", p.Kind)
	}
	res, note := parseCloseArgs(p.Prompt)
	if res != "fixed" || note == "" {
		t.Fatalf("res=%q note=%q", res, note)
	}
	p = ParseMessage("<@1> /customer-update Please retry", "1")
	if p.Kind != KindCustomerUpdate {
		t.Fatalf("kind=%d", p.Kind)
	}
	p = ParseMessage("<@1> /answer known limitation", "1")
	if p.Kind != KindAnswer {
		t.Fatalf("kind=%d", p.Kind)
	}
	p = ParseMessage("<@1> /reopen", "1")
	if p.Kind != KindReopenCase {
		t.Fatalf("kind=%d", p.Kind)
	}
	if got := parseReopenPhase(p.Prompt); got != sessionstore.PhaseInvestigate {
		t.Fatalf("default phase=%q", got)
	}
	p = ParseMessage("<@1> /reopen fixing", "1")
	if p.Kind != KindReopenCase {
		t.Fatalf("kind=%d", p.Kind)
	}
	if got := parseReopenPhase(p.Prompt); got != sessionstore.PhaseFixing {
		t.Fatalf("fixing phase=%q", got)
	}
	if got := parseReopenPhase("/reopen shipping"); got != "" {
		t.Fatalf("invalid phase should be empty, got %q", got)
	}
	// freeform close stays task
	p = ParseMessage("<@1> close the ticket in jira", "1")
	if p.Kind != KindTask {
		t.Fatalf("freeform close kind=%d", p.Kind)
	}
	// freeform reopen stays task
	p = ParseMessage("<@1> reopen the ticket in jira", "1")
	if p.Kind != KindTask {
		t.Fatalf("freeform reopen kind=%d", p.Kind)
	}
}

func TestEscalationPackageContainsTitle(t *testing.T) {
	e := sessionstore.Entry{
		Mode:          ModeCase,
		Phase:         sessionstore.PhaseFixing,
		CustomerTitle: "OTP spinner",
		Severity:      "high",
		Dossier:       &sessionstore.Dossier{Summary: "race in auth"},
	}
	pkg := EscalationPackage(e)
	if !strings.Contains(pkg, "OTP spinner") || !strings.Contains(pkg, "SAME branch") {
		t.Fatalf("package=%s", pkg)
	}
}

func TestEnsureCaseShellAndCloseFreeze(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{sessions: store, cfg: &config.Config{
		Projects: config.ProjectsMap{"app": {Path: dir, AllowedUserIDs: []string{"u1"}}},
	}}
	actor := Actor{ID: "u1", DisplayName: "Sup"}
	if err := b.ensureCaseShell("th1", "app", actor, "high", "ZD-1", "Failing export", "discord"); err != nil {
		t.Fatal(err)
	}
	e, ok := store.Get("th1")
	if !ok || e.Mode != ModeCase || e.Phase != sessionstore.PhaseIntake {
		t.Fatalf("entry=%+v ok=%v", e, ok)
	}
	// escalate via patch (same as handler)
	_, _, _ = store.Patch("th1", func(ent *sessionstore.Entry) {
		ent.Phase = sessionstore.PhaseFixing
	})
	e, _ = store.Get("th1")
	if e.Mode != ModeCase {
		t.Fatal("mode lost on escalate")
	}
	// close
	_, _, _ = store.Patch("th1", func(ent *sessionstore.Entry) {
		ent.Phase = sessionstore.PhaseClosed
		ent.Resolution = "fixed"
		ent.Label = sessionstore.LabelDone
	})
	e, _ = store.Get("th1")
	if e.ApplyAutoLabel(sessionstore.LabelNeedsReview) {
		t.Fatal("closed case must not apply auto-label")
	}
	if e.SuggestAutoLabel(true) != sessionstore.LabelDone {
		// returns effective label
		if e.EffectiveLabel() != sessionstore.LabelDone {
			t.Fatalf("label=%q", e.Label)
		}
	}
}

func TestStartInvestigateOnFixingCaseIsNonShip(t *testing.T) {
	// Advisor bug 2: /start investigate during Phase=fixing must not ship.
	pol := BuildRunPolicy(PolicyInput{
		SessionMode:      ModeCase,
		SessionPhase:     sessionstore.PhaseFixing,
		RequestedMode:    ModeCase,
		RequestedRunKind: RunKindInvestigate,
		Caps:             config.BuiltinCapabilityTemplates["builder"],
		ConfigYolo:       true,
		ShipMode:         sessionstore.ShipModePR,
	})
	if pol.AllowPR || pol.AllowDirectIntegrate {
		t.Fatalf("investigate run kind on fixing case must not ship: %+v", pol)
	}
	if pol.PrefixKind != "investigate" {
		t.Fatalf("PrefixKind=%q", pol.PrefixKind)
	}
	// Phase stays fixing in policy metadata for board; mode stays case
	if pol.Mode != ModeCase {
		t.Fatalf("Mode=%q", pol.Mode)
	}
}

func TestClosedCasePolicyIsNoneWithSafeTools(t *testing.T) {
	pol := BuildRunPolicy(PolicyInput{
		SessionMode:  ModeCase,
		SessionPhase: sessionstore.PhaseClosed,
		Caps:         config.BuiltinCapabilityTemplates["builder"],
		ConfigYolo:   true,
		ShipMode:     sessionstore.ShipModeDirect,
	})
	if pol.PrefixKind != "none" {
		t.Fatalf("PrefixKind=%q", pol.PrefixKind)
	}
	if pol.AllowPR || pol.AllowDirectIntegrate || pol.Yolo || pol.IncludeGHToken {
		t.Fatalf("closed must not ship: %+v", pol)
	}
	if pol.Tools == nil {
		t.Fatal("closed case must set Tools non-nil (tools-off)")
	}
}

func TestSanitizeGitHubPAT(t *testing.T) {
	raw := "Use ghp_abcdefghijklmnopqrstuvwxyz012345 and github_pat_11AAAA_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	clean, hits := SanitizeCustomerUpdate(raw)
	if strings.Contains(clean, "ghp_") || strings.Contains(clean, "github_pat_") {
		t.Fatalf("PAT leaked: %q hits=%v", clean, hits)
	}
}

func TestMergeDossierKeepsEscalateNote(t *testing.T) {
	dst := &sessionstore.Dossier{NextActions: []string{"Escalate note: please own", "old"}}
	src := &sessionstore.Dossier{Summary: "new", NextActions: []string{"add tests"}}
	got := MergeDossier(dst, src)
	joined := strings.Join(got.NextActions, "|")
	if !strings.Contains(joined, "Escalate note:") {
		t.Fatalf("lost escalate note: %v", got.NextActions)
	}
	if !strings.Contains(joined, "add tests") {
		t.Fatalf("lost src actions: %v", got.NextActions)
	}
}

// /start fix on Mode=case must not escalate when actor lacks FileEscalation|GithubWrites|StartSessions
// (same gate as /escalate). Drives real snapshotPolicyOntoItem.
func TestStartFixOnCaseDeniedWithoutEscalateCaps(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	// Builtin "operator" = Investigate only (no FileEscalation/StartSessions/GithubWrites).
	// Builtin "investigator" includes FileEscalation (may escalate by design).
	cfg := &config.Config{
		Projects: config.ProjectsMap{
			"app": {
				Path:             dir,
				SafeTeamMode:     &on,
				AllowedUserIDs:   []string{"ops1", "eng1"},
				CapabilityByUser: map[string]string{"ops1": "operator", "eng1": "builder"},
			},
		},
	}
	b := &Bot{sessions: store, cfg: cfg}
	actor := Actor{ID: "ops1", DisplayName: "Ops"}
	if err := b.ensureCaseShell("th-gate", "app", actor, "high", "", "Cannot escalate", "discord"); err != nil {
		t.Fatal(err)
	}
	_, _, _ = store.Patch("th-gate", func(e *sessionstore.Entry) {
		e.Phase = sessionstore.PhaseInvestigate
	})

	caps := cfg.ResolveCapabilities("app", "ops1", nil)
	if canEscalateCase(caps) {
		t.Fatalf("operator should not canEscalateCase: %+v", caps)
	}

	parsed := ParseMessage("<@bot> /start fix please ship it", "bot")
	if parsed.Kind != KindStartFix {
		t.Fatalf("Kind=%d", parsed.Kind)
	}
	item := taskItem{
		parsed:   parsed,
		threadID: "th-gate",
		proj:     projectRef{Name: "app", Cwd: dir},
		actor:    actor,
	}
	b.snapshotPolicyOntoItem(&item, "app", nil)

	after, _ := store.Get("th-gate")
	if after.Phase == sessionstore.PhaseFixing {
		t.Fatal("operator must not promote case to fixing via /start fix")
	}
	if after.Phase != sessionstore.PhaseInvestigate {
		t.Fatalf("phase changed unexpectedly: %q", after.Phase)
	}
	if after.Mode != ModeCase {
		t.Fatalf("Mode=%q", after.Mode)
	}
	if item.snapAllowPR || item.snapAllowDirect {
		t.Fatalf("snap must not allow ship: allowPR=%v allowDirect=%v", item.snapAllowPR, item.snapAllowDirect)
	}

	// Builder can escalate via same path
	item2 := taskItem{
		parsed:   parsed,
		threadID: "th-gate",
		proj:     projectRef{Name: "app", Cwd: dir},
		actor:    Actor{ID: "eng1", DisplayName: "Eng"},
	}
	b.snapshotPolicyOntoItem(&item2, "app", nil)
	after2, _ := store.Get("th-gate")
	if after2.Phase != sessionstore.PhaseFixing {
		t.Fatalf("builder /start fix should escalate: phase=%q", after2.Phase)
	}
	if after2.Mode != ModeCase {
		t.Fatalf("Mode must stay case: %q", after2.Mode)
	}
}
