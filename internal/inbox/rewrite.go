package inbox

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/acoshift/grokwork/internal/atomicfile"
)

// RewriteActor merges from's feed onto to, ordered by original At (then the
// source seq), then deletes from's files. New seqs are 1..n oldest-first so
// List's reverse-file order stays newest-first.
//
// Canonical items keep their read/unread standing via a remapped cursor; items
// that only existed on from stay unread. Idempotent when from has no file.
func (s *Store) RewriteActor(from, to string) (int, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return 0, fmt.Errorf("inbox: empty actor id")
	}
	if from == to {
		return 0, nil
	}
	fromPath, err := s.path(from)
	if err != nil {
		return 0, err
	}
	toPath, err := s.path(to)
	if err != nil {
		return 0, err
	}
	fromCurPath, err := s.cursorPath(from)
	if err != nil {
		return 0, err
	}
	toCurPath, err := s.cursorPath(to)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fromItems, err := readFile(fromPath)
	if err != nil {
		return 0, err
	}
	if len(fromItems) == 0 {
		_ = os.Remove(fromPath)
		_ = os.Remove(fromCurPath)
		delete(s.seqs, from)
		delete(s.cursors, from)
		return 0, nil
	}
	toItems, err := readFile(toPath)
	if err != nil {
		return 0, err
	}
	toCur := s.cursorLocked(to, toCurPath)

	type tagged struct {
		Item
		fromAlias bool
	}
	type itemKey struct{ at, kind, subject, unit, url, body string }
	keyOf := func(it Item) itemKey {
		return itemKey{it.At, it.Kind, it.Subject, it.UnitID, it.URL, it.Body}
	}
	have := map[itemKey]struct{}{}
	for _, it := range toItems {
		have[keyOf(it)] = struct{}{}
	}
	merged := make([]tagged, 0, len(fromItems)+len(toItems))
	added := 0
	for _, it := range fromItems {
		k := keyOf(it)
		if _, ok := have[k]; ok {
			continue
		}
		have[k] = struct{}{}
		merged = append(merged, tagged{Item: it, fromAlias: true})
		added++
	}
	for _, it := range toItems {
		merged = append(merged, tagged{Item: it, fromAlias: false})
	}
	if added == 0 {
		if err := os.Remove(fromPath); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		if err := os.Remove(fromCurPath); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		delete(s.seqs, from)
		delete(s.cursors, from)
		return 0, nil
	}
	slices.SortFunc(merged, func(a, b tagged) int {
		if n := strings.Compare(a.At, b.At); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Seq, b.Seq); n != 0 {
			return n
		}
		if a.fromAlias == b.fromAlias {
			return 0
		}
		if a.fromAlias {
			return -1
		}
		return 1
	})

	out := make([]Item, len(merged))
	var newRead []int64
	for i, row := range merged {
		oldSeq := row.Seq
		it := row.Item
		it.Seq = int64(i + 1)
		out[i] = it
		if row.fromAlias {
			continue
		}
		if toCur.Unread(oldSeq) {
			continue
		}
		newRead = append(newRead, it.Seq)
	}

	if err := writeItems(toPath, out); err != nil {
		return 0, err
	}
	cur := compactCursor(Cursor{Read: newRead}, int64(len(out)))
	if err := writeCursor(toCurPath, cur); err != nil {
		return 0, err
	}
	if err := os.Remove(fromPath); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if err := os.Remove(fromCurPath); err != nil && !os.IsNotExist(err) {
		return 0, err
	}

	s.seqs[to] = int64(len(out))
	s.cursors[to] = cur
	delete(s.seqs, from)
	delete(s.cursors, from)
	return added, nil
}

func writeItems(path string, items []Item) error {
	if len(items) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	var b strings.Builder
	for _, it := range items {
		line, err := json.Marshal(it)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return atomicfile.Write(path, []byte(b.String()), 0o600)
}
