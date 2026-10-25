// Package filename cleans a client-supplied name into one safe path leaf.
package filename

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxFilenameBytes = 120

// Sanitize cleans a client-supplied filename for use as a single path leaf.
// Letters, digits, and combining marks are kept. Marks have to stay: Thai
// vowels and tone marks are category Mn, so a letter-only filter rewrites
// สวัสดี as สว_สด_. The length cap is applied on cluster boundaries so a
// long name is not sliced through a UTF-8 sequence or between a base and
// its vowel.
func Sanitize(name string) string {
	name = strings.TrimSpace(name)
	name = filepath.Base(name)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		name = "file"
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsMark(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == ' ':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name = strings.TrimSpace(b.String())
	name = strings.Trim(name, ".")
	if name == "" {
		name = "file"
	}
	if len(name) <= maxFilenameBytes {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if len(ext) > 20 {
		ext = cutClusters(ext, 20)
	}
	keep := maxFilenameBytes - len(ext)
	if keep < 1 {
		return cutClusters(name, maxFilenameBytes)
	}
	base = cutClusters(base, keep)
	if base == "" {
		base = "file"
	}
	return base + ext
}

// cutClusters returns the longest prefix of s that fits in maxBytes and ends
// on a cluster boundary: a non-mark rune plus the combining marks that follow
// it. A mark that does not fit is dropped together with its base, so the
// prefix never ends on a bare consonant whose vowel was cut off.
func cutClusters(s string, maxBytes int) string {
	if maxBytes <= 0 || s == "" {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	i := 0
	last := 0
	for i < len(s) {
		_, size := utf8.DecodeRuneInString(s[i:])
		if size <= 0 {
			break
		}
		end := i + size
		for end < len(s) {
			m, msize := utf8.DecodeRuneInString(s[end:])
			if msize <= 0 || !unicode.IsMark(m) {
				break
			}
			end += msize
		}
		if end > maxBytes {
			break
		}
		last = end
		i = end
	}
	return s[:last]
}
