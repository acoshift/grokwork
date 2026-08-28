package inbox

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/acoshift/grokwork/internal/atomicfile"
)

// maxReadSeqs caps the per-seq read set. Mark-all clears it; individual marks
// of very old rows while newer ones stay unread is the only way it grows.
const maxReadSeqs = 500

// Cursor is the unread watermark for one actor. JSONL is append-only; this
// sidecar is the only thing GET /inbox must not write.
type Cursor struct {
	// Through marks every seq <= Through as read.
	Through int64 `json:"through,omitzero"`
	// Read is extra seqs > Through that were marked individually. Unread =
	// seq > Through AND seq not in Read.
	Read []int64 `json:"read,omitempty"`
}

// Unread reports whether seq is still unread under this cursor.
func (c Cursor) Unread(seq int64) bool {
	if seq <= 0 || seq <= c.Through {
		return false
	}
	return !slices.Contains(c.Read, seq)
}

// ReadCursor returns the actor's cursor without writing. Invalid / missing
// actor is a zero cursor.
func (s *Store) ReadCursor(actorID string) Cursor {
	p, err := s.cursorPath(actorID)
	if err != nil {
		return Cursor{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursorLocked(actorID, p)
}

// UnreadCount is how many items on the feed are still unread. Invalid actor
// and an empty feed are 0 — callers (nav counts, auth-off) must not error.
func (s *Store) UnreadCount(actorID string) int {
	feedPath, err := s.path(actorID)
	if err != nil {
		return 0
	}
	curPath, err := s.cursorPath(actorID)
	if err != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last, err := s.lastSeqLocked(actorID, feedPath)
	if err != nil {
		return 0
	}
	c := s.cursorLocked(actorID, curPath)
	return unreadCount(last, c)
}

func unreadCount(last int64, c Cursor) int {
	if last <= c.Through {
		return 0
	}
	n := int(last - c.Through)
	for _, seq := range c.Read {
		if seq > c.Through && seq <= last {
			n--
		}
	}
	if n < 0 {
		return 0
	}
	return n
}

// MarkRead records seq as read. Seq <= Through is a no-op. Marking an older
// seq does not hide newer unread items.
func (s *Store) MarkRead(actorID string, seq int64) error {
	if seq <= 0 {
		return fmt.Errorf("inbox: invalid seq")
	}
	return s.mutateCursor(actorID, func(c Cursor, last int64) Cursor {
		if !c.Unread(seq) || seq > last {
			return c
		}
		c.Read = append(slices.Clone(c.Read), seq)
		return compactCursor(c, last)
	})
}

// MarkAllRead sets Through to the current last seq and clears the per-seq set.
func (s *Store) MarkAllRead(actorID string) error {
	return s.mutateCursor(actorID, func(c Cursor, last int64) Cursor {
		return Cursor{Through: last}
	})
}

func (s *Store) mutateCursor(actorID string, fn func(Cursor, int64) Cursor) error {
	feedPath, err := s.path(actorID)
	if err != nil {
		return err
	}
	curPath, err := s.cursorPath(actorID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last, err := s.lastSeqLocked(actorID, feedPath)
	if err != nil {
		return err
	}
	cur := fn(s.cursorLocked(actorID, curPath), last)
	if err := writeCursor(curPath, cur); err != nil {
		return err
	}
	s.cursors[actorID] = cur
	return nil
}

func (s *Store) cursorLocked(actorID, path string) Cursor {
	if c, ok := s.cursors[actorID]; ok {
		return Cursor{Through: c.Through, Read: slices.Clone(c.Read)}
	}
	c, err := readCursorFile(path)
	if err != nil {
		s.cursors[actorID] = Cursor{}
		return Cursor{}
	}
	s.cursors[actorID] = c
	return Cursor{Through: c.Through, Read: slices.Clone(c.Read)}
}

func compactCursor(c Cursor, last int64) Cursor {
	seen := map[int64]struct{}{}
	for _, seq := range c.Read {
		if seq > c.Through && seq <= last {
			seen[seq] = struct{}{}
		}
	}
	for {
		next := c.Through + 1
		if _, ok := seen[next]; !ok {
			break
		}
		delete(seen, next)
		c.Through = next
	}
	if len(seen) == 0 {
		c.Read = nil
		return c
	}
	read := slices.Sorted(maps.Keys(seen))
	if len(read) > maxReadSeqs {
		read = read[len(read)-maxReadSeqs:]
	}
	c.Read = read
	return c
}

func readCursorFile(path string) (Cursor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Cursor{}, nil
		}
		return Cursor{}, err
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		// A torn cursor must not hide the feed; treat as unread-from-zero.
		return Cursor{}, nil
	}
	return c, nil
}

func writeCursor(path string, c Cursor) error {
	if c.Through <= 0 && len(c.Read) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(raw, '\n'), 0o600)
}
