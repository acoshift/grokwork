// Package timeline is the per-unit durable record of what happened during a run.
//
// It exists because Discord messages were doing storage duty. Cards, digests and
// sealed output chunks were posted into a thread and never persisted anywhere
// else, so a unit without a thread — every web-native unit — lost them, and a
// run that was cancelled before producing a final result lost its output
// entirely (history.Turn.Response is only assigned result.Text).
//
// Two boundaries are deliberate and load-bearing:
//
//   - The live tail does NOT come through here. Only *sealed* text blocks are
//     appended. Per-delta events would mean one file write per token and would
//     wreck the streaming cadence; the live tail stays in memory and is read
//     from the run snapshot. This store is the durable record, not the transport.
//
//   - Rendering is downstream. Appending must never block on or fail because of
//     a surface (a Discord 500, a gateway reconnect). Callers log render errors
//     and leave the event committed.
package timeline

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Kind identifies an event's payload shape.
type Kind string

const (
	KindRunStarted Kind = "run.started"
	KindTextBlock  Kind = "text.block" // a SEALED chunk of assistant output
	KindPhase      Kind = "phase"
	KindCompletion Kind = "completion"
	KindBrief      Kind = "brief"
	KindPRStatus   Kind = "pr.status"
	KindCIDigest   Kind = "ci.digest"
	KindDecision   Kind = "decision"
	KindArtifact   Kind = "artifact"
	KindNotice     Kind = "notice"
	KindRunDone    Kind = "run.done"
)

// Event is one durable record. Data is the kind's payload; unknown kinds are
// preserved verbatim on read so a newer writer cannot break an older reader.
type Event struct {
	Seq  int64           `json:"seq"`
	At   string          `json:"at"`
	Kind Kind            `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

// TextBlock is a sealed chunk of assistant output.
type TextBlock struct {
	Text string `json:"text"`
}

// RunStarted opens a run.
type RunStarted struct {
	Prompt  string `json:"prompt,omitempty"`
	Kind    string `json:"kind,omitempty"` // task | fix | investigate | …
	Agent   string `json:"agent,omitempty"`
	Model   string `json:"model,omitempty"`
	Project string `json:"project,omitempty"`
}

// RunDone closes a run.
type RunDone struct {
	Status   string `json:"status"` // done | cancelled | error
	Error    string `json:"error,omitempty"`
	Elapsed  string `json:"elapsed,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
}

// Notice is an operator-facing line (ops warning, resume announcement).
type Notice struct {
	Level string `json:"level,omitempty"` // info | warn
	Text  string `json:"text"`
}

const (
	// maxDataBytes caps one event's payload. A single sealed chunk is bounded by
	// the Discord message cap in practice; this guards a pathological run.
	maxDataBytes = 64 << 10
	// maxEventsPerUnit stops a runaway loop from filling the disk. Reaching it
	// drops further appends and is logged by the caller, never silent.
	maxEventsPerUnit = 5000
)

// ErrFull is returned once a unit has reached maxEventsPerUnit.
var ErrFull = fmt.Errorf("timeline: unit event cap reached")

type Store struct {
	mu   sync.Mutex
	dir  string
	seqs map[string]int64 // last assigned seq per unit; lazily seeded from disk
}

func New(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "timeline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, seqs: map[string]int64{}}, nil
}

// validUnitID mirrors history.validThreadID: unit ids are Discord snowflakes or
// w_<hex>, so anything outside this set is a path-traversal attempt.
func validUnitID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func (s *Store) path(unitID string) string {
	return filepath.Join(s.dir, unitID+".jsonl")
}

// Append writes one event and returns it with Seq and At assigned.
func (s *Store) Append(unitID string, kind Kind, data any) (Event, error) {
	if !validUnitID(unitID) {
		return Event{}, fmt.Errorf("timeline: invalid unit id")
	}
	if strings.TrimSpace(string(kind)) == "" {
		return Event{}, fmt.Errorf("timeline: empty kind")
	}

	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return Event{}, err
		}
		if len(b) > maxDataBytes {
			// Truncate rather than drop: a too-large payload still carries signal.
			b, err = json.Marshal(map[string]any{
				"truncated": true,
				"bytes":     len(b),
			})
			if err != nil {
				return Event{}, err
			}
		}
		raw = b
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	last, err := s.lastSeqLocked(unitID)
	if err != nil {
		return Event{}, err
	}
	if last >= maxEventsPerUnit {
		return Event{}, ErrFull
	}

	ev := Event{
		Seq:  last + 1,
		At:   time.Now().UTC().Format(time.RFC3339),
		Kind: kind,
		Data: raw,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, err
	}

	f, err := os.OpenFile(s.path(unitID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Event{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Event{}, err
	}
	s.seqs[unitID] = ev.Seq
	return ev, nil
}

// lastSeqLocked returns the highest seq for a unit, seeding the cache from disk
// on first use so seq survives a restart.
func (s *Store) lastSeqLocked(unitID string) (int64, error) {
	if v, ok := s.seqs[unitID]; ok {
		return v, nil
	}
	events, err := s.readLocked(unitID, 0)
	if err != nil {
		return 0, err
	}
	var max int64
	for _, e := range events {
		if e.Seq > max {
			max = e.Seq
		}
	}
	s.seqs[unitID] = max
	return max, nil
}

// Read returns every event for a unit in append order.
func (s *Store) Read(unitID string) ([]Event, error) { return s.ReadSince(unitID, 0) }

// ReadSince returns events with Seq > afterSeq, for tailing.
func (s *Store) ReadSince(unitID string, afterSeq int64) ([]Event, error) {
	if !validUnitID(unitID) {
		return nil, fmt.Errorf("timeline: invalid unit id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(unitID, afterSeq)
}

func (s *Store) readLocked(unitID string, afterSeq int64) ([]Event, error) {
	f, err := os.Open(s.path(unitID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxDataBytes+4<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// A torn final line (crash mid-write) must not poison the whole
			// timeline — skip it and keep the rest readable.
			continue
		}
		if ev.Seq <= afterSeq {
			continue
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		// Return what parsed; a truncated tail is still useful.
		return out, nil
	}
	return out, nil
}

// Delete removes a unit's timeline (idle cleanup / prune).
func (s *Store) Delete(unitID string) error {
	if !validUnitID(unitID) {
		return fmt.Errorf("timeline: invalid unit id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seqs, unitID)
	err := os.Remove(s.path(unitID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DecodeData unmarshals an event payload into v.
func (e Event) DecodeData(v any) error {
	if len(e.Data) == 0 {
		return fmt.Errorf("timeline: event %d has no data", e.Seq)
	}
	return json.Unmarshal(e.Data, v)
}

// Text returns the text of a KindTextBlock event, or "" for other kinds.
func (e Event) Text() string {
	if e.Kind != KindTextBlock {
		return ""
	}
	var tb TextBlock
	if err := e.DecodeData(&tb); err != nil {
		return ""
	}
	return tb.Text
}

// LastRunTranscript returns the output of the most recent run only.
//
// A unit's timeline spans every run in the thread, so the whole transcript must
// not be attributed to one turn. Runs are delimited by run.done, so the last
// run's blocks are the ones after the second-to-last run.done. With no run.done
// at all (a run still in flight) every block belongs to the current run.
func LastRunTranscript(events []Event) string {
	lastDone, prevDone := -1, -1
	for i, e := range events {
		if e.Kind != KindRunDone {
			continue
		}
		prevDone, lastDone = lastDone, i
	}
	start := 0
	if prevDone >= 0 {
		start = prevDone + 1
	}
	end := len(events)
	if lastDone >= 0 {
		end = lastDone
	}
	if start > end {
		return ""
	}
	return Transcript(events[start:end])
}

// Transcript joins every sealed text block for a unit, in order. This is what
// makes a cancelled run's output readable: the blocks were committed as they
// sealed, before any final result existed.
func Transcript(events []Event) string {
	var b strings.Builder
	for _, e := range events {
		t := e.Text()
		if t == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(t)
	}
	return b.String()
}
