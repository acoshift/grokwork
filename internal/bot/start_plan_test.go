package bot

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestStartPlanSnapshotPolicyNonShip(t *testing.T) {
	parsed := ParseMessage("<@bot> /start plan add a plan mode", "bot")
	if parsed.Kind != KindStartPlan {
		t.Fatalf("Kind=%d want KindStartPlan; prompt=%q", parsed.Kind, parsed.Prompt)
	}

	cfg := &config.Config{
		Yolo: new(true),
		Projects: config.ProjectsMap{
			"app": {
				Path:           filepath.Join(t.TempDir(), "app"),
				AllowedUserIDs: []string{"builder1"},
			},
		},
	}
	b := &Bot{cfg: cfg}
	item := taskItem{
		parsed:   parsed,
		threadID: "th-plan",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "builder1", DisplayName: "Builder"},
	}
	b.snapshotPolicyOntoItem(&item, "app")

	if item.snapMode != ModePlan {
		t.Fatalf("snapMode=%q want %q", item.snapMode, ModePlan)
	}
	if item.snapRunKind != RunKindPlan {
		t.Fatalf("snapRunKind=%q want %q", item.snapRunKind, RunKindPlan)
	}
	if item.snapAllowPR || item.snapAllowDirect {
		t.Fatalf("plan must not ship: allowPR=%v allowDirect=%v", item.snapAllowPR, item.snapAllowDirect)
	}

	pol := b.resolveRunPolicy("th-plan", "app", item, "pr", item.actor, grokrun.AgentGrok)
	if pol.Mode != ModePlan {
		t.Fatalf("resolve Mode=%q", pol.Mode)
	}
	if pol.AllowPR || pol.AllowDirectShip || pol.AllowDirectIntegrate {
		t.Fatalf("resolve must not ship: %+v", pol)
	}
	if pol.Yolo || pol.IncludeGHToken {
		t.Fatal("plan must omit yolo and GH token")
	}
	if pol.PrefixKind != "plan" {
		t.Fatalf("PrefixKind=%q want plan", pol.PrefixKind)
	}
	if pol.Tools == nil || *pol.Tools == "" {
		t.Fatal("plan Tools must be file-only")
	}
}

func TestRefusePlanOnExistingMode(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.sessions.Set("fix-th", sessionstore.Entry{Project: "app", Mode: ModeFix}); err != nil {
		t.Fatal(err)
	}
	if err := b.refusePlanOnExistingMode("fix-th"); !errors.Is(err, ErrPlanModeConflict) {
		t.Fatalf("fix session: %v", err)
	}
	if err := b.sessions.Set("case-th", sessionstore.Entry{Project: "app", Mode: ModeCase}); err != nil {
		t.Fatal(err)
	}
	if err := b.refusePlanOnExistingMode("case-th"); !errors.Is(err, ErrPlanModeConflict) {
		t.Fatalf("case session: %v", err)
	}
	if err := b.sessions.Set("plan-th", sessionstore.Entry{Project: "app", Mode: ModePlan}); err != nil {
		t.Fatal(err)
	}
	if err := b.refusePlanOnExistingMode("plan-th"); err != nil {
		t.Fatalf("plan session: %v", err)
	}
	if err := b.refusePlanOnExistingMode("missing"); err != nil {
		t.Fatalf("missing: %v", err)
	}
}

func TestWantsPlanStartMode(t *testing.T) {
	if !WantsPlanStartMode("plan") || !WantsPlanStartMode(" PLAN ") {
		t.Fatal("want plan")
	}
	if WantsPlanStartMode("") || WantsPlanStartMode("fix") || WantsPlanStartMode("investigate") {
		t.Fatal("empty/fix/investigate are not plan")
	}
}
