package commitreview

import (
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/ghpr"
)

// FindingsJSONSchema is the --json-schema passed to grok so the model is
// constrained to emit summary+findings JSON (not free-form prose or tool turns).
const FindingsJSONSchema = `{
  "type": "object",
  "properties": {
    "summary": { "type": "string" },
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "title": { "type": "string" },
          "body": { "type": "string" },
          "severity": { "type": "string", "enum": ["critical", "high", "medium", "low", "info"] },
          "paths": { "type": "array", "items": { "type": "string" } },
          "labels": { "type": "array", "items": { "type": "string" } }
        },
        "required": ["title", "body", "severity"]
      }
    }
  },
  "required": ["summary", "findings"]
}`

// BuildPrompt builds a tools-off review prompt for one commit.
func BuildPrompt(detail ghpr.CommitDetail, maxFindings int) string {
	if maxFindings <= 0 {
		maxFindings = MaxFindings
	}
	var b strings.Builder
	b.WriteString(`You are a senior engineer reviewing a single git commit for a team.

Review ONLY the provided commit metadata and patch. Look for:
- correctness bugs and regressions
- security issues
- missing tests for risky changes
- broken contracts / API misuse
- data loss or concurrency hazards

Do NOT bikeshed style, naming, or formatting unless it causes a real defect.
Do NOT invent files or lines not present in the patch.
Do NOT suggest running shell commands or opening PRs.

Reply with JSON ONLY (no markdown prose outside JSON). Schema:
{
  "summary": "one short paragraph overall assessment",
  "findings": [
    {
      "title": "short issue title (<=80 chars)",
      "body": "markdown: problem, why it matters, suggested fix, file:line when known",
      "severity": "critical|high|medium|low|info",
      "paths": ["relative/path.go"],
      "labels": ["optional-extra-label"]
    }
  ]
}

Rules:
- findings may be an empty array if the commit looks fine
- at most `)
	b.WriteString(fmt.Sprintf("%d", maxFindings))
	b.WriteString(` findings; prefer highest severity
- paths relative to repo root
- severity must be one of the enum values

## Commit

`)
	b.WriteString(fmt.Sprintf("- SHA: %s (%s)\n", detail.SHA, detail.ShortSHA))
	b.WriteString(fmt.Sprintf("- Subject: %s\n", detail.Subject))
	b.WriteString(fmt.Sprintf("- Author: %s <%s>\n", detail.AuthorName, detail.AuthorEmail))
	if !detail.AuthorDate.IsZero() {
		b.WriteString(fmt.Sprintf("- Date: %s\n", detail.AuthorDate.UTC().Format("2006-01-02 15:04 UTC")))
	}
	if body := strings.TrimSpace(detail.Body); body != "" {
		b.WriteString("\n### Message body\n\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	if stat := strings.TrimSpace(detail.Stat); stat != "" {
		b.WriteString("\n### Stat\n\n```\n")
		b.WriteString(stat)
		b.WriteString("\n```\n")
	}
	b.WriteString("\n### Patch\n\n")
	if detail.Diff.Truncated {
		b.WriteString("(Note: patch is truncated for size; review what is present.)\n\n")
	}
	b.WriteString("```diff\n")
	b.WriteString(formatDiffForPrompt(detail.Diff))
	b.WriteString("\n```\n")
	return b.String()
}

func formatDiffForPrompt(d ghpr.Diff) string {
	if len(d.Files) == 0 {
		return "(no patch content)"
	}
	var b strings.Builder
	for _, f := range d.Files {
		path := f.PathNew
		if path == "" {
			path = f.PathOld
		}
		b.WriteString("diff --git a/")
		b.WriteString(path)
		b.WriteString(" b/")
		b.WriteString(path)
		b.WriteByte('\n')
		for _, h := range f.Hunks {
			b.WriteString(h.Header)
			b.WriteByte('\n')
			for _, line := range h.Lines {
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// IssueTitle prefixes the finding title with short SHA context.
func IssueTitle(shortSHA, findingTitle string) string {
	shortSHA = strings.TrimSpace(shortSHA)
	findingTitle = strings.TrimSpace(findingTitle)
	prefix := "[review]"
	if shortSHA != "" {
		prefix = "[review/" + shortSHA + "]"
	}
	t := prefix + " " + findingTitle
	if len(t) > 256 {
		t = t[:256]
	}
	return t
}

// IssueBody builds the GitHub issue body for a finding.
func IssueBody(job *Job, f Finding) string {
	var b strings.Builder
	b.WriteString("## Finding\n\n")
	b.WriteString(strings.TrimSpace(f.Body))
	b.WriteString("\n\n## Context\n\n")
	b.WriteString(fmt.Sprintf("- **Commit:** [`%s`](https://github.com/%s/%s/commit/%s) (`%s`)\n",
		job.SHA, job.Owner, job.Repo, job.SHA, job.ShortSHA))
	if job.Subject != "" {
		b.WriteString(fmt.Sprintf("- **Subject:** %s\n", job.Subject))
	}
	b.WriteString(fmt.Sprintf("- **Repo:** %s/%s\n", job.Owner, job.Repo))
	b.WriteString(fmt.Sprintf("- **Project:** %s\n", job.Project))
	if job.Actor != "" {
		b.WriteString(fmt.Sprintf("- **Reviewed by:** %s via Grok Work\n", job.Actor))
	}
	b.WriteString(fmt.Sprintf("- **Severity:** %s\n", f.Severity))
	if len(f.Paths) > 0 {
		b.WriteString("\n### Paths\n\n")
		for _, p := range f.Paths {
			b.WriteString("- `")
			b.WriteString(p)
			b.WriteString("`\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("<!-- grokwork-commit-review: project=%s owner=%s repo=%s sha=%s fp=%s -->\n",
		job.Project, job.Owner, job.Repo, job.SHA, f.Fingerprint))
	return b.String()
}

// DefaultLabels returns labels for a finding (best-effort on create).
func DefaultLabels(severity string, extra []string) []string {
	labels := []string{"commit-review"}
	if s := normalizeSeverity(severity); s != "" {
		labels = append(labels, "severity:"+s)
	}
	for _, e := range extra {
		e = strings.TrimSpace(e)
		if e == "" || e == "commit-review" || strings.HasPrefix(e, "severity:") {
			continue
		}
		labels = append(labels, e)
	}
	return labels
}
