package deploy

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"io"
	"slices"
	"unicode"
)

// redactMask replaces a secret wherever it appears in step output.
var redactMask = []byte("••••")

// minRedactLen guards against a one- or two-character "secret" matching
// everywhere and turning the whole log into mask characters. A credential
// shorter than this is not protectable by string replacement anyway.
const minRedactLen = 4

// Redactor rewrites secret values out of a byte stream before they land
// anywhere durable.
//
// It wraps the log file writer, so every downstream surface — the web tail, the
// raw log, the Discord notice, anything built from the log — inherits redaction
// from one place instead of each remembering to apply it.
//
// This is defence in depth, not a guarantee: a step can base64 or otherwise
// transform a secret before printing it. The controls that actually matter are
// the per-environment capability gate and the ref allowlist.
type Redactor struct {
	w io.Writer
	// secrets is sorted longest-first so an overlapping pair masks the longer
	// match rather than leaving its tail exposed.
	secrets [][]byte
	// maxLen is the longest secret, bounding how much a partial match may hold.
	maxLen int
	// carry holds bytes that could still be the start of a secret spanning two
	// writes.
	carry []byte
}

// NewRedactor builds a Redactor for the given secret values.
//
// For each value it also registers the base64 *decoding* when the value decodes
// cleanly to printable text. The common shape is a credential stored base64 and
// used as `printf '%s' "$KUBECONFIG_B64" | base64 -d`, so the decoded form is
// what a tool echoes. Registering the *encoding* of an already-encoded value
// would match nothing.
func NewRedactor(w io.Writer, values []string) *Redactor {
	seen := map[string]bool{}
	var secrets [][]byte
	add := func(s string) {
		if len(s) < minRedactLen || seen[s] {
			return
		}
		seen[s] = true
		secrets = append(secrets, []byte(s))
	}
	for _, v := range values {
		add(v)
		if dec, ok := decodeBase64Printable(v); ok {
			add(dec)
		}
	}
	// Longest first: masking the longer of two overlapping secrets is strictly
	// safer than masking the shorter and leaving the remainder.
	slices.SortFunc(secrets, func(a, b []byte) int { return cmp.Compare(len(b), len(a)) })
	maxLen := 0
	if len(secrets) > 0 {
		maxLen = len(secrets[0])
	}
	return &Redactor{w: w, secrets: secrets, maxLen: maxLen}
}

// decodeBase64Printable reports the decoded form of s when it is valid base64
// that yields printable text of a usable length.
func decodeBase64Printable(s string) (string, bool) {
	if len(s) < 8 {
		return "", false
	}
	var dec []byte
	var err error
	if dec, err = base64.StdEncoding.DecodeString(s); err != nil {
		if dec, err = base64.URLEncoding.DecodeString(s); err != nil {
			return "", false
		}
	}
	if len(dec) < minRedactLen {
		return "", false
	}
	for _, r := range string(dec) {
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return "", false
		}
	}
	return string(dec), true
}

func (r *Redactor) Write(p []byte) (int, error) {
	n := len(p)
	if len(r.secrets) == 0 {
		return r.w.Write(p)
	}
	buf := p
	if len(r.carry) > 0 {
		buf = append(append([]byte{}, r.carry...), p...)
		r.carry = nil
	}
	buf = r.mask(buf)

	// Hold back only a genuine partial match. Bounding the hold by the longest
	// secret instead would stall the live tail by that many bytes on every
	// write, which for an 8 KiB credential means the page shows nothing for
	// minutes on a step that prints slowly.
	keep := r.partialSuffixLen(buf)
	if keep > 0 {
		r.carry = append([]byte{}, buf[len(buf)-keep:]...)
		buf = buf[:len(buf)-keep]
	}
	if len(buf) > 0 {
		if _, err := r.w.Write(buf); err != nil {
			return 0, err
		}
	}
	// Report the caller's length: the mask changes byte counts, and io.Copy
	// treats a short write as an error.
	return n, nil
}

func (r *Redactor) mask(buf []byte) []byte {
	for _, s := range r.secrets {
		buf = bytes.ReplaceAll(buf, s, redactMask)
	}
	return buf
}

// partialSuffixLen returns how many trailing bytes are a proper prefix of some
// secret, i.e. how much could still become a match once more arrives.
func (r *Redactor) partialSuffixLen(buf []byte) int {
	max := min(r.maxLen-1, len(buf))
	for n := max; n > 0; n-- {
		suffix := buf[len(buf)-n:]
		for _, s := range r.secrets {
			if len(s) > n && bytes.HasPrefix(s, suffix) {
				return n
			}
		}
	}
	return 0
}

// Close flushes any held partial match. A trailing partial is by definition not
// a whole secret, so it is written as-is rather than dropped.
func (r *Redactor) Close() error {
	if len(r.carry) == 0 {
		return nil
	}
	buf := r.mask(r.carry)
	r.carry = nil
	_, err := r.w.Write(buf)
	return err
}

// SecretsCount reports how many distinct values are being scrubbed. For logging
// that redaction is active without naming anything.
func (r *Redactor) SecretsCount() int { return len(r.secrets) }

// capWriter bounds a step's log while keeping both ends of it.
//
// The first headBytes stream straight through, so a live tail is complete for
// any step whose output fits — which is nearly all of them. Past that, output is
// buffered into a rolling tail and written on Close, so the bytes that explain a
// failure survive even when a chatty build pushes the middle out. The trade-off
// is deliberate: a step that overflows shows nothing new mid-run between the
// head and the end, which is better than either unbounded disk use or losing the
// failure itself to a head-only truncation.
type capWriter struct {
	w         io.Writer
	headBytes int
	tailBytes int

	written  int64
	head     int
	tail     []byte
	elided   int64
	overflow bool
}

func newCapWriter(w io.Writer, headBytes, tailBytes int) *capWriter {
	return &capWriter{w: w, headBytes: headBytes, tailBytes: tailBytes}
}

func (c *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if c.head < c.headBytes {
		room := c.headBytes - c.head
		if room > len(p) {
			room = len(p)
		}
		if _, err := c.w.Write(p[:room]); err != nil {
			return 0, err
		}
		c.head += room
		c.written += int64(room)
		p = p[room:]
	}
	if len(p) == 0 {
		return n, nil
	}
	c.overflow = true
	c.tail = append(c.tail, p...)
	if len(c.tail) > c.tailBytes {
		drop := len(c.tail) - c.tailBytes
		c.tail = c.tail[drop:]
		c.elided += int64(drop)
	}
	return n, nil
}

// Close writes the elision marker and the retained tail.
func (c *capWriter) Close() error {
	if !c.overflow {
		return nil
	}
	marker := []byte("\n… " + itoa(c.elided) + " bytes elided …\n")
	if _, err := c.w.Write(marker); err != nil {
		return err
	}
	c.written += int64(len(marker))
	if _, err := c.w.Write(c.tail); err != nil {
		return err
	}
	c.written += int64(len(c.tail))
	return nil
}

// Truncated reports whether output was elided.
func (c *capWriter) Truncated() bool { return c.overflow }

// Written reports how many bytes reached the underlying writer.
func (c *capWriter) Written() int64 { return c.written }

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append(b, byte('0'+n%10))
		n /= 10
	}
	slices.Reverse(b)
	return string(b)
}
