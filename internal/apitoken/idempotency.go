package apitoken

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// BodyHash is SHA-256 of the raw request body bytes.
func BodyHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// IdempotencyGet looks up a cached creating-POST. Missing → ok=false.
// Same key, different body hash → ErrIdempotencyConflict.
func (s *Store) IdempotencyGet(tokenID, key, bodyHash string) (IdemRecord, bool, error) {
	if s == nil {
		return IdemRecord{}, false, fmt.Errorf("token store is not configured")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return IdemRecord{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.tokens[tokenID]
	if !ok {
		return IdemRecord{}, false, fmt.Errorf("token %q not found", tokenID)
	}
	got, hit := rec.Idempotency[key]
	if !hit {
		return IdemRecord{}, false, nil
	}
	if got.BodyHash != bodyHash {
		return IdemRecord{}, false, ErrIdempotencyConflict
	}
	out := got
	out.Response = slices.Clone(got.Response)
	return out, true, nil
}

// IdempotencyPut stores a response and prunes TTL then cap.
func (s *Store) IdempotencyPut(tokenID, key string, rec IdemRecord) error {
	if s == nil {
		return fmt.Errorf("token store is not configured")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.tokens[tokenID]
	if !ok {
		return fmt.Errorf("token %q not found", tokenID)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = s.now().UTC()
	}
	if tok.Idempotency == nil {
		tok.Idempotency = map[string]IdemRecord{}
	}
	tok.Idempotency[key] = rec
	tok.Idempotency = pruneIdem(tok.Idempotency, s.now())
	s.tokens[tokenID] = tok
	return s.saveLocked()
}

func pruneIdem(m map[string]IdemRecord, now time.Time) map[string]IdemRecord {
	type item struct {
		key string
		rec IdemRecord
	}
	var keep []item
	cutoff := now.Add(-idemTTL)
	for k, rec := range m {
		if rec.CreatedAt.Before(cutoff) {
			continue
		}
		keep = append(keep, item{k, rec})
	}
	slices.SortFunc(keep, func(a, b item) int {
		return a.rec.CreatedAt.Compare(b.rec.CreatedAt)
	})
	if len(keep) > idemCap {
		keep = keep[len(keep)-idemCap:]
	}
	out := make(map[string]IdemRecord, len(keep))
	for _, it := range keep {
		out[it.key] = it.rec
	}
	return out
}
