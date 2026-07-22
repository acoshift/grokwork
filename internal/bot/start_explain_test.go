package bot

import (
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

// Real path: ParseMessage → snapshotPolicyOntoItem (shipped claim path) must
// produce explain non-ship policy for a builder-capable actor.
func TestStartExplainSnapshotPolicyNonShip(t *testing.T) {
	parsed := ParseMessage("<@bot> /start explain customers see 403 on share", "bot")
	if parsed.Kind != KindStartExplain {
		t.Fatalf("Kind=%d want KindStartExplain; prompt=%q", parsed.Kind, parsed.Prompt)
	}
	if parsed.Prompt == "" {
		t.Fatal("expected explain task body in Prompt")
	}

	yolo := true
	cfg := &config.Config{
		Yolo: &yolo,
		Projects: config.ProjectsMap{
			"app": {
				Path:           filepath.Join(t.TempDir(), "app"),
				AllowedUserIDs: []string{"builder1"},
				// SafeTeamMode off → ResolveCapabilities returns builder
			},
		},
	}
	b := &Bot{cfg: cfg}
	item := taskItem{
		parsed:   parsed,
		threadID: "th-explain",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "builder1", DisplayName: "Builder"},
	}
	b.snapshotPolicyOntoItem(&item, "app", nil)

	if item.snapMode != ModeExplain {
		t.Fatalf("snapMode=%q want %q", item.snapMode, ModeExplain)
	}
	if item.snapRunKind != RunKindExplain {
		t.Fatalf("snapRunKind=%q want %q", item.snapRunKind, RunKindExplain)
	}
	if item.snapAllowPR || item.snapAllowDirect {
		t.Fatalf("explain must not ship: allowPR=%v allowDirect=%v", item.snapAllowPR, item.snapAllowDirect)
	}

	// resolveRunPolicy at execute must match (uses snap + KindStartExplain).
	pol := b.resolveRunPolicy("th-explain", "app", item, "pr", item.actor)
	if pol.Mode != ModeExplain {
		t.Fatalf("resolve Mode=%q", pol.Mode)
	}
	if pol.AllowPR || pol.AllowDirectShip || pol.AllowDirectIntegrate {
		t.Fatalf("resolve must not ship: %+v", pol)
	}
	if pol.Yolo {
		t.Fatal("explain yolo must be false")
	}
	if pol.IncludeGHToken {
		t.Fatal("explain must omit GH token")
	}
	if pol.PrefixKind != "explain" {
		t.Fatalf("PrefixKind=%q want explain", pol.PrefixKind)
	}
	if pol.Tools == nil {
		t.Fatal("explain Tools must be non-nil (tools-off)")
	}
}
