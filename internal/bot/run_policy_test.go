package bot

import (
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestBuildRunPolicyInvestigateNonShip(t *testing.T) {
	pol := BuildRunPolicy(PolicyInput{
		RequestedMode: ModeInvestigate,
		Caps:          config.BuiltinCapabilityTemplates["investigator"],
		ConfigYolo:    true, // global yolo must not apply
		ShipMode:      sessionstore.ShipModeDirect,
	})
	if pol.AllowPR || pol.AllowDirectShip || pol.AllowDirectIntegrate {
		t.Fatalf("investigate must not ship: %+v", pol)
	}
	if pol.Yolo {
		t.Fatal("investigate yolo must be false")
	}
	if pol.Tools == nil {
		t.Fatal("investigate Tools must be non-nil (restricted)")
	}
	if pol.IncludeGHToken {
		t.Fatal("investigate must omit GH token")
	}
	if pol.PrefixKind != "investigate" {
		t.Fatalf("PrefixKind=%q", pol.PrefixKind)
	}
	if pol.RefreshPR {
		t.Fatal("must not refresh PR cards")
	}
}

func TestBuildRunPolicyEmptyModeBuilderShipsPR(t *testing.T) {
	pol := BuildRunPolicy(PolicyInput{
		SessionMode: "",
		Caps:        config.BuiltinCapabilityTemplates["builder"],
		ConfigYolo:  true,
		ShipMode:    sessionstore.ShipModePR,
	})
	if !pol.AllowPR || pol.AllowDirectShip {
		t.Fatalf("PR ship: %+v", pol)
	}
	if !pol.Yolo {
		t.Fatal("yolo should pass through for fix")
	}
	if pol.Tools != nil {
		t.Fatal("fix tools unrestricted")
	}
	if !pol.IncludeGHToken {
		t.Fatal("fix needs GH token")
	}
}

func TestBuildRunPolicyEmptyModeBuilderDirect(t *testing.T) {
	pol := BuildRunPolicy(PolicyInput{
		Caps:       config.BuiltinCapabilityTemplates["builder"],
		ConfigYolo: true,
		ShipMode:   sessionstore.ShipModeDirect,
	})
	if pol.AllowPR || !pol.AllowDirectShip || !pol.AllowDirectIntegrate {
		t.Fatalf("direct ship: %+v", pol)
	}
}

func TestBuildRunPolicyCoerceStartSessionsWithoutGithubWrites(t *testing.T) {
	pol := BuildRunPolicy(PolicyInput{
		SessionMode: "",
		Caps: config.Capabilities{
			StartSessions: true,
			Investigate:   true,
			// no GithubWrites
		},
		ConfigYolo: true,
		ShipMode:   sessionstore.ShipModePR,
	})
	if !pol.Coerced {
		t.Fatal("expected coerce")
	}
	if pol.AllowPR || pol.AllowDirectShip {
		t.Fatalf("coerced must not ship: %+v", pol)
	}
	if pol.Mode != ModeInvestigate {
		t.Fatalf("mode=%q", pol.Mode)
	}
	if pol.Tools == nil || pol.Yolo {
		t.Fatalf("investigate gates: tools=%v yolo=%v", pol.Tools, pol.Yolo)
	}
}

func TestBuildRunPolicySafeTeamUnmappedInvestigator(t *testing.T) {
	// Caps as ResolveCapabilities would return for unmapped under SafeTeamMode.
	pol := BuildRunPolicy(PolicyInput{
		Caps:       config.BuiltinCapabilityTemplates["investigator"],
		ConfigYolo: true,
		ShipMode:   sessionstore.ShipModeDirect,
	})
	// Empty mode + investigator (no StartSessions) → coerce investigate
	if pol.AllowPR || pol.AllowDirectIntegrate {
		t.Fatalf("investigator freeform must not ship: %+v", pol)
	}
}

func TestInvestigatePromptNoPR(t *testing.T) {
	p := investigatePromptPrefix("grok/discord/1")
	for _, bad := range []string{"gh pr create` (or", "Open a pull request with"} {
		if strings.Contains(p, bad) {
			t.Fatalf("investigate must not instruct PR: %q in\n%s", bad, p)
		}
	}
	if !strings.Contains(p, "INVESTIGATE") {
		t.Fatal("missing INVESTIGATE")
	}
}

func TestAttributionFooter(t *testing.T) {
	s := attributionFooter("alice", "99", "https://discord/x")
	if !strings.Contains(s, "alice") {
		t.Fatalf("footer=%s", s)
	}
	if strings.Contains(s, "99") || strings.Contains(s, "https://discord/x") {
		t.Fatalf("must not include Discord id or thread URL: footer=%s", s)
	}
}

func TestIntentPreview(t *testing.T) {
	long := strings.Repeat("a", 100)
	got := intentPreview(long, 80)
	if len([]rune(got)) != 80 {
		t.Fatalf("len=%d got=%q", len([]rune(got)), got)
	}
}
