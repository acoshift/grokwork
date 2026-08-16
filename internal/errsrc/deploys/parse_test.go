package deploys

import (
	"testing"

	"github.com/acoshift/grokwork/internal/errsrc"
)

func TestParseURLConsole(t *testing.T) {
	t.Parallel()
	ref, ok := ParseURL("https://console.deploys.app/deployment/errors?project=acme&location=gke.cluster-rcf2&name=api&id=iss_go_nilmap")
	if !ok {
		t.Fatal("parse")
	}
	if ref.Provider != errsrc.ProviderDeploys || ref.ProjectHint != "acme" || ref.Location != "gke.cluster-rcf2" || ref.Resource != "api" || ref.ID != "iss_go_nilmap" {
		t.Fatalf("%+v", ref)
	}
	// Discord wrap + extra query.
	ref, ok = ParseURL("<https://console.deploys.app/deployment/errors?project=acme&location=loc&name=api&id=x&foo=1>")
	if !ok || ref.ID != "x" {
		t.Fatalf("%v %+v", ok, ref)
	}
}

func TestParseURLRejects(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"",
		"https://console.deploys.app/errors?project=acme",
		"https://console.deploys.app/deployment/errors?project=acme&location=loc&name=api",
		"http://console.deploys.app/deployment/errors?project=acme&location=loc&name=api&id=x",
		"https://api.deploys.app/deployment/errors?project=acme&location=loc&name=api&id=x",
	} {
		if _, ok := ParseURL(raw); ok {
			t.Fatalf("accepted %q", raw)
		}
	}
}
