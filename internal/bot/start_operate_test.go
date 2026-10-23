package bot

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestStartOperateSnapshotPolicyNonShip(t *testing.T) {
	parsed := ParseMessage("<@bot> /start operate deploy api to prod", "bot")
	if parsed.Kind != KindStartOperate {
		t.Fatalf("Kind=%d want KindStartOperate; prompt=%q", parsed.Kind, parsed.Prompt)
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
		threadID: "th-op",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "builder1", DisplayName: "Builder"},
	}
	b.snapshotPolicyOntoItem(&item, "app")

	if item.snapMode != ModeOperate || item.snapRunKind != RunKindOperate {
		t.Fatalf("snap mode=%q kind=%q", item.snapMode, item.snapRunKind)
	}
	if item.snapAllowPR || item.snapAllowDirect {
		t.Fatalf("operate must not ship: allowPR=%v allowDirect=%v", item.snapAllowPR, item.snapAllowDirect)
	}

	pol := b.resolveRunPolicy("th-op", "app", item, "pr", item.actor, grokrun.AgentGrok)
	if pol.Mode != ModeOperate || pol.RunKind != RunKindOperate || pol.PrefixKind != "operate" {
		t.Fatalf("resolve: %+v", pol)
	}
	if pol.AllowPR || pol.AllowDirectShip || pol.Yolo || pol.IncludeGHToken || pol.Tools == nil {
		t.Fatalf("resolve must be file-only: %+v", pol)
	}
	if *pol.Tools != grokrun.AgentGrok.ExplainTools() {
		t.Fatalf("tools=%s", *pol.Tools)
	}
}

func TestSnapshotFixOnOperateStaysOperate(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.sessions.Set("op-th", sessionstore.Entry{Project: "app", Mode: ModeOperate}); err != nil {
		t.Fatal(err)
	}
	item := taskItem{
		parsed:   ParseMessage("<@bot> /start fix ship it", "bot"),
		threadID: "op-th",
		proj:     projectRef{Name: "app"},
		actor:    Actor{ID: "u", DisplayName: "U"},
	}
	b.snapshotPolicyOntoItem(&item, "app")
	if item.snapMode != ModeOperate || item.snapRunKind != RunKindOperate {
		t.Fatalf("snap mode=%q kind=%q", item.snapMode, item.snapRunKind)
	}
	if item.snapAllowPR || item.snapAllowDirect {
		t.Fatal("fix on operate must not ship")
	}
	e, ok := b.sessions.Get("op-th")
	if !ok || e.Mode != ModeOperate {
		t.Fatalf("session mode rewritten: ok=%v mode=%q", ok, e.Mode)
	}
	pol := b.resolveRunPolicy("op-th", "app", item, "", item.actor, grokrun.AgentGrok)
	if pol.Mode != ModeOperate || pol.RunKind != RunKindOperate || pol.AllowDirectShip || pol.Tools == nil {
		t.Fatalf("resolve: %+v", pol)
	}
}

func TestRefuseOperateOnExistingMode(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.sessions.Set("fix-th", sessionstore.Entry{Project: "app", Mode: ModeFix}); err != nil {
		t.Fatal(err)
	}
	if err := b.refuseOperateOnExistingMode("fix-th"); !errors.Is(err, ErrOperateModeConflict) {
		t.Fatalf("fix session: %v", err)
	}
	if err := b.sessions.Set("op-th", sessionstore.Entry{Project: "app", Mode: ModeOperate}); err != nil {
		t.Fatal(err)
	}
	if err := b.refuseOperateOnExistingMode("op-th"); err != nil {
		t.Fatalf("operate session: %v", err)
	}
	if err := b.refuseOperateOnExistingMode("missing"); err != nil {
		t.Fatalf("missing: %v", err)
	}
}

func TestRefuseShipOnOperateSession(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.sessions.Set("op-th", sessionstore.Entry{Project: "app", Mode: ModeOperate}); err != nil {
		t.Fatal(err)
	}
	if err := b.refuseShipOnOperateSession("op-th"); !errors.Is(err, ErrOperateRejectsShip) {
		t.Fatalf("operate: %v", err)
	}
	if err := b.sessions.Set("fix-th", sessionstore.Entry{Project: "app", Mode: ModeFix}); err != nil {
		t.Fatal(err)
	}
	if err := b.refuseShipOnOperateSession("fix-th"); err != nil {
		t.Fatalf("fix: %v", err)
	}
}

func TestWantsOperateStartMode(t *testing.T) {
	if !WantsOperateStartMode("operate") || !WantsOperateStartMode(" OPERATE ") {
		t.Fatal("want operate")
	}
	if WantsOperateStartMode("") || WantsOperateStartMode("fix") || WantsOperateStartMode("plan") {
		t.Fatal("empty/fix/plan are not operate")
	}
}
