package ghpr

import (
	"regexp"
	"strings"
)

// MaxLogSummaryImages caps how many docker image refs we surface on the
// Actions job log page (keeps multi-tag matrix builds readable).
const MaxLogSummaryImages = 24

// LogImage is one container image reference extracted from a job log.
type LogImage struct {
	// Ref is the image name, optionally with a tag (no digest suffix).
	Ref string
	// Digest is sha256:… when known (from push/manifest lines).
	Digest string
}

// ShortDigest returns a compact digest for chips (sha256: + 12 hex digits).
func (i LogImage) ShortDigest() string {
	d := strings.TrimSpace(i.Digest)
	if d == "" {
		return ""
	}
	const prefix = "sha256:"
	if strings.HasPrefix(strings.ToLower(d), prefix) && len(d) > len(prefix)+12 {
		return d[:len(prefix)+12]
	}
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

// JobLogSummary is a structured skim of a GitHub Actions job log — facts an
// operator wants without scrolling the raw stream (docker images today).
type JobLogSummary struct {
	Images []LogImage
}

// Empty reports whether the summary has nothing to render.
func (s JobLogSummary) Empty() bool {
	return len(s.Images) == 0
}

// High-confidence docker image patterns from buildx / docker / build-push-action.
var (
	// #18 pushing manifest for ghcr.io/acme/app:main@sha256:…
	rePushManifest = regexp.MustCompile(`(?i)(?:pushing manifest(?: list)? for|exporting manifest list to)\s+(\S+)`)
	// #18 naming to ghcr.io/acme/app:main done
	reNamingTo = regexp.MustCompile(`(?i)#\d+\s+naming to\s+(\S+)`)
	// Successfully tagged acme/app:latest
	reTagged = regexp.MustCompile(`(?i)Successfully tagged\s+(\S+)`)
	// digest: sha256:… size: …
	reDigestLine = regexp.MustCompile(`(?i)^\s*digest:\s*(sha256:[0-9a-f]{64})\b`)
	// standalone image@sha256:… or image:tag@sha256:…
	reImageAtDigest = regexp.MustCompile(`(?i)\b((?:[a-z0-9]+(?:[._-][a-z0-9]+)*\.)+[a-z]{2,}/[a-z0-9][a-z0-9._/-]*:[a-z0-9_][a-z0-9._-]*)@(sha256:[0-9a-f]{64})\b`)
	// plain registry/image:tag (must look like a registry host, not a URL path)
	reRegistryTag = regexp.MustCompile(`(?i)\b((?:[a-z0-9]+(?:[._-][a-z0-9]+)*\.)+[a-z]{2,}/[a-z0-9][a-z0-9._/-]*:[a-z0-9_][a-z0-9._-]*)\b`)
	// docker.io-style without host: library/name:tag or org/name:tag after "tagged"/"pushing"
	reShortTagged = regexp.MustCompile(`(?i)\b([a-z0-9]+(?:[._-][a-z0-9]+)*/[a-z0-9][a-z0-9._/-]*:[a-z0-9_][a-z0-9._-]*)\b`)
)

// ExtractJobLogSummary skims a gh-formatted job log (job\tstep\tline) for
// docker image refs and digests. Conservative: only high-confidence lines so
// random paths and URLs do not appear as images.
func ExtractJobLogSummary(log string) JobLogSummary {
	if strings.TrimSpace(log) == "" {
		return JobLogSummary{}
	}
	// key → digest (last non-empty wins); order preserves first-seen refs.
	digests := map[string]string{}
	var order []string
	add := func(ref, digest string) {
		ref, digest = normalizeImageRef(ref, digest)
		if ref == "" {
			return
		}
		if _, ok := digests[ref]; !ok {
			order = append(order, ref)
		}
		if digest != "" {
			digests[ref] = digest
		} else if _, ok := digests[ref]; !ok {
			digests[ref] = ""
		}
	}

	var lastRef string
	for line := range strings.SplitSeq(log, "\n") {
		line = jobLogLineBody(line)
		if line == "" {
			continue
		}

		if m := reDigestLine.FindStringSubmatch(line); len(m) == 2 {
			if lastRef != "" {
				add(lastRef, m[1])
			}
			continue
		}
		if m := rePushManifest.FindStringSubmatch(line); len(m) == 2 {
			ref, dig := splitRefDigest(m[1])
			add(ref, dig)
			lastRef = ref
			continue
		}
		if m := reNamingTo.FindStringSubmatch(line); len(m) == 2 {
			ref, dig := splitRefDigest(strings.TrimSuffix(m[1], " done"))
			// naming lines often end with "done" already stripped by the capture
			ref = strings.TrimSpace(strings.TrimSuffix(ref, "done"))
			add(ref, dig)
			lastRef = ref
			continue
		}
		if m := reTagged.FindStringSubmatch(line); len(m) == 2 {
			ref, dig := splitRefDigest(m[1])
			add(ref, dig)
			lastRef = ref
			continue
		}
		// Fall through: only accept registry-qualified tags on docker-ish lines.
		if !looksLikeDockerLine(line) {
			continue
		}
		if m := reImageAtDigest.FindStringSubmatch(line); len(m) == 3 {
			add(m[1], m[2])
			lastRef = m[1]
			continue
		}
		if m := reRegistryTag.FindStringSubmatch(line); len(m) == 2 {
			ref, dig := splitRefDigest(m[1])
			add(ref, dig)
			lastRef = ref
			continue
		}
		if m := reShortTagged.FindStringSubmatch(line); len(m) == 2 {
			ref, dig := splitRefDigest(m[1])
			// Reject bare single-segment names like "done:1" from step noise.
			if strings.Count(ref, "/") < 1 {
				continue
			}
			add(ref, dig)
			lastRef = ref
		}
	}

	if len(order) == 0 {
		return JobLogSummary{}
	}
	if len(order) > MaxLogSummaryImages {
		order = order[:MaxLogSummaryImages]
	}
	images := make([]LogImage, 0, len(order))
	for _, ref := range order {
		images = append(images, LogImage{Ref: ref, Digest: digests[ref]})
	}
	return JobLogSummary{Images: images}
}

// jobLogLineBody strips the gh "job\tstep\t" prefix when present.
func jobLogLineBody(line string) string {
	line = strings.TrimRight(line, "\r")
	if i := strings.IndexByte(line, '\t'); i >= 0 {
		rest := line[i+1:]
		if j := strings.IndexByte(rest, '\t'); j >= 0 {
			return strings.TrimSpace(rest[j+1:])
		}
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(line)
}

func looksLikeDockerLine(line string) bool {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "docker"):
		return true
	case strings.Contains(l, "buildx"):
		return true
	case strings.Contains(l, "pushing"):
		return true
	case strings.Contains(l, "naming to"):
		return true
	case strings.Contains(l, "manifest"):
		return true
	case strings.Contains(l, "ghcr.io/"), strings.Contains(l, "gcr.io/"),
		strings.Contains(l, "azurecr.io/"), strings.Contains(l, ".ecr."),
		strings.Contains(l, "docker.io/"):
		return true
	default:
		return false
	}
}

func splitRefDigest(s string) (ref, digest string) {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	s = strings.TrimSuffix(s, ",")
	if s == "" {
		return "", ""
	}
	if i := strings.LastIndex(s, "@sha256:"); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

func normalizeImageRef(ref, digest string) (string, string) {
	ref = strings.TrimSpace(ref)
	ref = strings.Trim(ref, `"'`)
	ref = strings.TrimSuffix(ref, ",")
	// Drop trailing "done" if the regex left it glued on.
	ref = strings.TrimSpace(strings.TrimSuffix(ref, "done"))
	if ref == "" || ref == "done" {
		return "", ""
	}
	// Reject obvious non-images.
	if strings.Contains(ref, "://") || strings.HasPrefix(ref, "http") {
		return "", ""
	}
	if strings.ContainsAny(ref, " \t<>()[]{}") {
		return "", ""
	}
	digest = strings.TrimSpace(digest)
	if digest != "" && !strings.HasPrefix(strings.ToLower(digest), "sha256:") {
		digest = ""
	}
	// Re-split if ref still carries @sha256.
	if r, d := splitRefDigest(ref); d != "" {
		ref = r
		if digest == "" {
			digest = d
		}
	}
	// Need either a registry host or org/name:tag shape.
	if !strings.Contains(ref, "/") {
		return "", ""
	}
	if !strings.Contains(ref, ":") && digest == "" {
		// untagged registry/path without digest is noisy; keep only with digest
		return "", ""
	}
	return ref, digest
}
