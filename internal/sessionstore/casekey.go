package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// Case keys — the short, quotable identity of a support case.
//
// A case's storage key is its Discord thread snowflake, which is unusable as a
// reference: nobody types 1390000000000000022 into a commit message, and it
// says nothing about which project it belongs to. CaseKey is the human handle
// — "WEBAPP-14" — assigned once at intake and never changed, so that a case
// filed months later can point at it and the pointer still resolves.
//
// Numbers come from a per-prefix high-water mark persisted next to
// sessions.json, deliberately NOT from "count the cases that exist": a case
// can be abandoned (/reset deletes the entry), and a recycled number would
// silently re-aim every reference already written down.

const caseSeqFileName = "case-seq.json"

// DefaultCaseKeyPrefix is used when a project name yields no usable letters.
const DefaultCaseKeyPrefix = "CASE"

// maxCaseKeyPrefixRunes keeps "MYVERYLONGPROJECT-4" from becoming the widest
// column on the board.
const maxCaseKeyPrefixRunes = 10

// CaseKeyPrefix derives a key prefix from a project name: ASCII letters and
// digits only, uppercased, leading digits dropped so the result can never be
// read as a number. Callers pass a configured override through here too, so a
// hand-set prefix obeys the same shape as a derived one.
//
// ASCII specifically, not "any letter": a case key exists to be typed into a
// commit message, a URL and another team's chat. A project named in Thai or
// Cyrillic gets CASE-1 rather than a key nobody can retype from memory.
// Derivation must also be idempotent — ParseCaseKey validates a key by
// re-deriving its prefix and comparing.
func CaseKeyPrefix(project string) string {
	var b strings.Builder
	for _, r := range project {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(unicode.ToUpper(r))
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9' && b.Len() > 0:
			b.WriteRune(r)
		}
		if b.Len() >= maxCaseKeyPrefixRunes {
			break
		}
	}
	if b.Len() == 0 {
		return DefaultCaseKeyPrefix
	}
	return b.String()
}

// ParseCaseKey splits "WEBAPP-14" into its prefix and number. ok is false for
// anything that is not exactly one prefix, one hyphen and one positive number,
// so a user-typed reference can be validated before it is stored or resolved.
func ParseCaseKey(key string) (prefix string, num int, ok bool) {
	key = strings.ToUpper(strings.TrimSpace(key))
	i := strings.LastIndexByte(key, '-')
	if i <= 0 || i == len(key)-1 {
		return "", 0, false
	}
	prefix, rest := key[:i], key[i+1:]
	if CaseKeyPrefix(prefix) != prefix {
		return "", 0, false
	}
	num, err := strconv.Atoi(rest)
	if err != nil || num <= 0 || strings.HasPrefix(rest, "0") {
		return "", 0, false
	}
	return prefix, num, true
}

// NormalizeCaseKey returns the canonical spelling of a reference, or "" when it
// is not a case key at all. Used on every user-supplied reference so "webapp-14"
// and " WEBAPP-14 " land on the same string the board renders.
func NormalizeCaseKey(key string) string {
	prefix, num, ok := ParseCaseKey(key)
	if !ok {
		return ""
	}
	return prefix + "-" + strconv.Itoa(num)
}

func (s *Store) seqFilePath() string {
	return filepath.Join(filepath.Dir(s.filePath), caseSeqFileName)
}

func (s *Store) loadSeq() map[string]int {
	out := map[string]int{}
	raw, err := os.ReadFile(s.seqFilePath())
	if err != nil {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]int{}
	}
	return out
}

func (s *Store) saveSeq(seq map[string]int) error {
	raw, err := json.MarshalIndent(seq, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.seqFilePath(), raw, 0o600)
}

// AllocateCaseKey reserves and returns the next key for prefix ("WEBAPP-14").
//
// The mark is raised past any key already in the store as well as past the
// persisted counter, so a lost or hand-edited case-seq.json can only ever
// under-count — it can never hand out a key some case is already using.
func (s *Store) AllocateCaseKey(prefix string) (string, error) {
	prefix = CaseKeyPrefix(prefix)
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.loadSeq()
	next := seq[prefix]
	for _, e := range s.entries {
		p, n, ok := ParseCaseKey(e.CaseKey)
		if ok && p == prefix && n > next {
			next = n
		}
	}
	next++
	seq[prefix] = next
	if err := s.saveSeq(seq); err != nil {
		return "", err
	}
	return prefix + "-" + strconv.Itoa(next), nil
}

// FindByCaseKey resolves a case key to its thread id. Lookup is on the
// canonical spelling, so callers may pass whatever the user typed.
func (s *Store) FindByCaseKey(key string) (string, Entry, bool) {
	key = NormalizeCaseKey(key)
	if key == "" {
		return "", Entry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.entries {
		if e.CaseKey == key {
			// Hand back state the caller owns outright — see Entry.clone.
			return id, e.clone(), true
		}
	}
	return "", Entry{}, false
}

// RelatedCaseKeys returns this case's references, canonicalised and free of
// duplicates and of any self-reference.
func (e Entry) RelatedCaseKeys() []string {
	if len(e.RelatedCases) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(e.RelatedCases))
	for _, raw := range e.RelatedCases {
		key := NormalizeCaseKey(raw)
		if key == "" || key == e.CaseKey {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}
