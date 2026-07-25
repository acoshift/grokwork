// Package inbox is a per-actor notification feed.
//
// It exists so a notification has somewhere to go when no push channel can reach
// the recipient. Run-finished pings were Discord-shaped: a thread got one message
// mentioning everyone, and a unit without a thread DMed each person — but a DM
// needs a Discord snowflake, so any recipient who logged in without Discord was
// silently dropped from the list. Unreachable must mean "queued here", not "never
// told".
//
// Delivery is per actor, unlike a thread message that names several people at
// once: an inbox entry addressed to one person must not read "@you @them".
package inbox

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Item is one delivered notification.
type Item struct {
	Seq     int64  `json:"seq"`
	At      string `json:"at"`
	Kind    string `json:"kind"`    // run.done | review.requested | ci.failed | …
	Subject string `json:"subject"` // one-line summary
	Body    string `json:"body,omitempty"`
	URL     string `json:"url,omitempty"` // link to the work
	UnitID  string `json:"unitId,omitempty"`
	Project string `json:"project,omitempty"`
}

const (
	// maxItemsPerActor bounds one actor's feed. Oldest entries are dropped on
	// read rather than rewriting the file on every append.
	maxItemsPerActor = 500
	maxBodyBytes     = 8 << 10
)

type Store struct {
	mu   sync.Mutex
	dir  string
	seqs map[string]int64
}

func New(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "inbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, seqs: map[string]int64{}}, nil
}

// actorFileName maps an actor id to a safe filename. Actor ids are namespaced
// ("discord:123", "oidc:alice"), so ":" and any other separator must not reach
// the filesystem.
func actorFileName(actorID string) (string, error) {
	id := strings.TrimSpace(actorID)
	if id == "" || len(id) > 128 {
		return "", fmt.Errorf("inbox: invalid actor id")
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '-':
			b.WriteRune(r)
		case r == ':' || r == '.':
			b.WriteByte('_')
		default:
			// Anything else would be a path or encoding surprise; refuse rather
			// than silently writing to a neighbouring actor's feed.
			return "", fmt.Errorf("inbox: unsupported character in actor id")
		}
	}
	name := b.String()
	if name == "" || name == "_" {
		return "", fmt.Errorf("inbox: empty actor id after sanitize")
	}
	// Sanitizing alone is NOT injective: "oidc:a" and the literal id "oidc_a"
	// both fold to "oidc_a", which would put two different people on one feed —
	// each reading the other's notifications. A digest of the untouched id keeps
	// the readable prefix while making the mapping one-to-one.
	sum := sha256.Sum256([]byte(id))
	return name + "-" + hex.EncodeToString(sum[:4]) + ".jsonl", nil
}

func (s *Store) path(actorID string) (string, error) {
	name, err := actorFileName(actorID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, name), nil
}

// Append delivers one item to an actor's feed.
func (s *Store) Append(actorID string, it Item) (Item, error) {
	p, err := s.path(actorID)
	if err != nil {
		return Item{}, err
	}
	if strings.TrimSpace(it.Subject) == "" {
		return Item{}, fmt.Errorf("inbox: empty subject")
	}
	if len(it.Body) > maxBodyBytes {
		it.Body = it.Body[:maxBodyBytes] + "\n…(truncated)"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	last, err := s.lastSeqLocked(actorID, p)
	if err != nil {
		return Item{}, err
	}
	it.Seq = last + 1
	if it.At == "" {
		it.At = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(it)
	if err != nil {
		return Item{}, err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Item{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Item{}, err
	}
	s.seqs[actorID] = it.Seq
	return it, nil
}

func (s *Store) lastSeqLocked(actorID, path string) (int64, error) {
	if v, ok := s.seqs[actorID]; ok {
		return v, nil
	}
	items, err := readFile(path)
	if err != nil {
		return 0, err
	}
	var max int64
	for _, it := range items {
		if it.Seq > max {
			max = it.Seq
		}
	}
	s.seqs[actorID] = max
	return max, nil
}

// List returns an actor's items, newest first, capped at maxItemsPerActor.
func (s *Store) List(actorID string) ([]Item, error) {
	p, err := s.path(actorID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := readFile(p)
	if err != nil {
		return nil, err
	}
	// Newest first: an inbox is read from the top.
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	if len(items) > maxItemsPerActor {
		items = items[:maxItemsPerActor]
	}
	return items, nil
}

// Count returns how many items an actor has.
func (s *Store) Count(actorID string) int {
	items, err := s.List(actorID)
	if err != nil {
		return 0
	}
	return len(items)
}

func readFile(path string) ([]Item, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Item
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8<<10), maxBodyBytes+4<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var it Item
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			// A torn line from a crash mid-write must not hide the rest.
			continue
		}
		out = append(out, it)
	}
	return out, nil
}
