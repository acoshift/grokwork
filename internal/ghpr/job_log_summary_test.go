package ghpr

import (
	"strings"
	"testing"
)

func TestExtractJobLogSummaryDockerBuildx(t *testing.T) {
	log := strings.Join([]string{
		"build\tBuild and push\t#18 exporting to image",
		"build\tBuild and push\t#18 pushing layers",
		"build\tBuild and push\t#18 pushing manifest for ghcr.io/acme/app:main@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"build\tBuild and push\t#18 pushing manifest for ghcr.io/acme/app:sha-deadbee@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"build\tBuild and push\t#18 naming to ghcr.io/acme/app:main done",
		"build\tBuild and push\tdigest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa size: 2143",
		"build\tCheckout\tCloning into '/home/runner/work/app'...",
	}, "\n")
	sum := ExtractJobLogSummary(log)
	if sum.Empty() {
		t.Fatal("expected images")
	}
	if len(sum.Images) != 2 {
		t.Fatalf("images=%d %+v", len(sum.Images), sum.Images)
	}
	if sum.Images[0].Ref != "ghcr.io/acme/app:main" {
		t.Fatalf("first=%+v", sum.Images[0])
	}
	if sum.Images[0].Digest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("digest=%q", sum.Images[0].Digest)
	}
	if sum.Images[1].Ref != "ghcr.io/acme/app:sha-deadbee" {
		t.Fatalf("second=%+v", sum.Images[1])
	}
}

func TestExtractJobLogSummarySuccessfullyTagged(t *testing.T) {
	log := "ci\tBuild\tSuccessfully tagged acme/api:latest\n"
	sum := ExtractJobLogSummary(log)
	if len(sum.Images) != 1 || sum.Images[0].Ref != "acme/api:latest" {
		t.Fatalf("%+v", sum.Images)
	}
}

func TestExtractJobLogSummaryIgnoresNoise(t *testing.T) {
	log := strings.Join([]string{
		"build\tCheckout\thttps://github.com/acme/app.git",
		"build\tTest\tok github.com/acme/app 1.2s",
		"build\tTest\tcoverage: 80.0% of statements",
		"build\tLint\tRunning golangci-lint...",
	}, "\n")
	if !ExtractJobLogSummary(log).Empty() {
		t.Fatalf("noise should not produce summary: %+v", ExtractJobLogSummary(log))
	}
}

func TestExtractJobLogSummaryEmpty(t *testing.T) {
	if !ExtractJobLogSummary("").Empty() || !ExtractJobLogSummary("   \n").Empty() {
		t.Fatal("empty log")
	}
}

func TestExtractJobLogSummaryCap(t *testing.T) {
	var b strings.Builder
	for i := range MaxLogSummaryImages + 5 {
		b.WriteString("j\ts\t#1 pushing manifest for ghcr.io/acme/app:t")
		b.WriteString(strings.Repeat("x", i+1))
		b.WriteString("@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n")
	}
	sum := ExtractJobLogSummary(b.String())
	if len(sum.Images) != MaxLogSummaryImages {
		t.Fatalf("cap: got %d", len(sum.Images))
	}
}

func TestJobLogLineBody(t *testing.T) {
	if got := jobLogLineBody("job\tstep\thello"); got != "hello" {
		t.Fatalf("%q", got)
	}
	if got := jobLogLineBody("plain"); got != "plain" {
		t.Fatalf("%q", got)
	}
}
