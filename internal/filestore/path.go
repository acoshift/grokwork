package filestore

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// encodeName makes a single display name safe as one path hop. Only % and /
// are escaped so existing ?path=foo/bar bookmarks (nested folders) keep working.
func encodeName(name string) string {
	name = strings.ReplaceAll(name, "%", "%25")
	return strings.ReplaceAll(name, "/", "%2F")
}

func decodeName(seg string) (string, error) {
	return url.PathUnescape(seg)
}

// SplitPath splits a wire path into display-name segments. A hop of a%2Fb is
// one name containing a slash; a/b is two nested names.
func SplitPath(p string) ([]string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil, nil
	}
	var out []string
	for part := range strings.SplitSeq(p, "/") {
		if part == "" {
			return nil, fmt.Errorf("object path has an empty or '.'/'..' segment")
		}
		name, err := decodeName(part)
		if err != nil {
			return nil, fmt.Errorf("object path is not valid encoding")
		}
		out = append(out, name)
	}
	return out, nil
}

// JoinNames encodes display names and joins them with /.
func JoinNames(names ...string) string {
	var parts []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		parts = append(parts, encodeName(n))
	}
	return strings.Join(parts, "/")
}

// AppendName adds one display name (which may contain '/') onto a wire path.
func AppendName(wire, name string) string {
	name = strings.TrimSpace(name)
	segs, err := SplitPath(wire)
	if err != nil {
		return ""
	}
	if name != "" {
		segs = append(segs, name)
	}
	return JoinNames(segs...)
}

// NativePath decodes a wire path into the backend's slash-joined name sequence
// (GCS object keys). Drive should walk SplitPath hops instead of this string.
func NativePath(wire string) (string, error) {
	segs, err := SplitPath(wire)
	if err != nil {
		return "", err
	}
	return strings.Join(segs, "/"), nil
}

func validDecodedName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("object path has an empty or '.'/'..' segment")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return fmt.Errorf("object path must not contain control characters")
		}
	}
	return nil
}
