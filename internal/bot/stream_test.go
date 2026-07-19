package bot

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeMessenger struct {
	mu      sync.Mutex
	nextID  int
	sends   []sendCall
	edits   []editCall
	typings int
}

type sendCall struct {
	msgID   string
	content string
}

type editCall struct {
	msgID   string
	content string
}

func (f *fakeMessenger) Send(channelID, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := "m" + itoa(f.nextID)
	f.sends = append(f.sends, sendCall{msgID: id, content: content})
	return id, nil
}

func (f *fakeMessenger) Edit(channelID, msgID, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, editCall{msgID: msgID, content: content})
	return nil
}

func (f *fakeMessenger) Typing(channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typings++
	return nil
}

func (f *fakeMessenger) snapshot() (sends []sendCall, edits []editCall, typings int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sends = append([]sendCall(nil), f.sends...)
	edits = append([]editCall(nil), f.edits...)
	return sends, edits, f.typings
}

// finalBodiesByID applies sends then edits by message id (Discord state).
func finalBodiesByID(sends []sendCall, edits []editCall) []string {
	bodies := map[string]string{}
	order := make([]string, 0, len(sends))
	for _, s := range sends {
		if _, ok := bodies[s.msgID]; !ok {
			order = append(order, s.msgID)
		}
		bodies[s.msgID] = s.content
	}
	for _, e := range edits {
		bodies[e.msgID] = e.content
	}
	out := make([]string, 0, len(order))
	for _, id := range order {
		out = append(out, bodies[id])
	}
	return out
}

var errFake = errString("fake messenger error")

type errString string

func (e errString) Error() string { return string(e) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestStreamTailIsTailNotHead(t *testing.T) {
	head := strings.Repeat("A", 200)
	tail := "UNIQUE_TAIL_MARKER_XYZ"
	s := head + strings.Repeat("b", 2000) + tail
	got := streamTail(s, 80)
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("expected leading ellipsis, got %q", got)
	}
	if !strings.HasSuffix(got, tail) && !strings.Contains(got, "UNIQUE_TAIL") {
		t.Fatalf("expected tail content, got %q", got)
	}
	if strings.Contains(got, "AAAA") && strings.HasPrefix(strings.TrimPrefix(got, "…"), "A") {
		// Head-only freeze would start with A's after ellipsis for a head window.
		// Tail window should not open with the long A run.
		if strings.HasPrefix(got, "…"+strings.Repeat("A", 20)) {
			t.Fatalf("looks like head window, got %q", got)
		}
	}
	if runeLen(got) > 80 {
		t.Fatalf("over budget: %d runes in %q", runeLen(got), got)
	}
}

func TestStreamTailEmojiRuneBudget(t *testing.T) {
	// Each emoji is multiple bytes but one or more runes; must not panic and stay in budget.
	s := strings.Repeat("🙂", 500) + "END"
	got := streamTail(s, 50)
	if got == "" {
		t.Fatal("empty")
	}
	if runeLen(got) > 50 {
		t.Fatalf("runeLen=%d > 50 for %q", runeLen(got), got)
	}
	if !utf8.ValidString(got) {
		t.Fatal("invalid utf8")
	}
	if !strings.Contains(got, "END") && !strings.HasSuffix(got, "🙂") {
		// Tail should include end region.
		t.Fatalf("unexpected tail %q", got)
	}
}

func TestCutPrefixNewline(t *testing.T) {
	s := "line1\nline2\nline3"
	got := cutPrefix(s, 12)
	if runeLen(got) > 12 {
		t.Fatalf("too long %q", got)
	}
}

func TestStreamPosterShortFinishNoResend(t *testing.T) {
	fake := &fakeMessenger{}
	p := newStreamPosterWith(fake, "ch1")
	p.OnDelta("Hello, world!")
	// Force a flush cycle.
	time.Sleep(50 * time.Millisecond)
	p.Flush()
	time.Sleep(100 * time.Millisecond)

	fully := p.Finish()
	if !fully {
		t.Fatal("expected fully streamed short reply")
	}
	if p.Unposted() != "" {
		t.Fatalf("unposted=%q", p.Unposted())
	}
	sends, edits, _ := fake.snapshot()
	if len(sends) == 0 && len(edits) == 0 {
		t.Fatal("expected at least one Discord write")
	}
	bodies := finalBodiesByID(sends, edits)
	if len(bodies) == 0 {
		t.Fatal("no bodies")
	}
	last := bodies[len(bodies)-1]
	if strings.Contains(last, "streaming") {
		t.Fatalf("footer stuck on final message: %q", last)
	}
	if !strings.Contains(last, "Hello, world!") {
		t.Fatalf("final=%q", last)
	}
}

func TestStreamPosterMultiMessageSealNoDoublePost(t *testing.T) {
	fake := &fakeMessenger{}
	p := newStreamPosterWith(fake, "ch1")
	// Build text well over 2 Discord messages.
	chunk := strings.Repeat("word ", 500) // ~2500 chars per chunk
	var body strings.Builder
	for i := 0; i < 4; i++ {
		body.WriteString(chunk)
		body.WriteString("\nSECTION_" + itoa(i) + "\n")
	}
	text := body.String()
	for i := 0; i < len(text); i += 200 {
		end := i + 200
		if end > len(text) {
			end = len(text)
		}
		p.OnDelta(text[i:end])
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.Flush()
		time.Sleep(50 * time.Millisecond)
		sends, _, _ := fake.snapshot()
		if len(sends) >= 2 {
			break
		}
	}
	fully := p.Finish()
	if !fully {
		t.Fatalf("expected finish to post all, unposted=%d", len(p.Unposted()))
	}
	if p.Unposted() != "" {
		t.Fatalf("unposted remaining %d bytes", len(p.Unposted()))
	}

	sends, edits, _ := fake.snapshot()
	if len(sends) < 2 {
		t.Fatalf("expected multi-message seal, sends=%d edits=%d", len(sends), len(edits))
	}
	if rem := p.Unposted(); rem != "" {
		t.Fatalf("would sendChunks remainder len=%d", len(rem))
	}
	finalBodies := finalBodiesByID(sends, edits)
	joined := strings.Join(finalBodies, "")
	for i := 0; i < 4; i++ {
		mark := "SECTION_" + itoa(i)
		if c := strings.Count(joined, mark); c != 1 {
			t.Fatalf("%s appears %d times in sealed bodies (want 1). bodies=%d joinedLen=%d",
				mark, c, len(finalBodies), len(joined))
		}
	}
	if strings.Contains(joined, "_(streaming") {
		t.Fatal("streaming footer in finalized bodies")
	}
}

func TestStreamPosterNoOpSkipsEdit(t *testing.T) {
	fake := &fakeMessenger{}
	p := newStreamPosterWith(fake, "ch1")
	p.OnDelta("stable")
	time.Sleep(100 * time.Millisecond)
	p.Flush()
	time.Sleep(150 * time.Millisecond)
	sends1, edits1, _ := fake.snapshot()
	n1 := len(edits1) + len(sends1)

	// Wake flusher without new content — should not add writes with same body.
	p.Flush()
	p.Flush()
	time.Sleep(200 * time.Millisecond)
	sends2, edits2, _ := fake.snapshot()
	n2 := len(edits2) + len(sends2)
	if n2 > n1+1 {
		t.Fatalf("noop flushes caused too many writes: before=%d after=%d", n1, n2)
	}
	_ = p.Finish()
}

// TestStreamPosterOnDeltaDoesNotBlockOnSlowMessenger proves OnDelta stays
// responsive while a Discord Send is in flight (mu must not be held during I/O).
func TestStreamPosterOnDeltaDoesNotBlockOnSlowMessenger(t *testing.T) {
	const sendDelay = 400 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})
	slow := &blockingMessenger{
		onSendEnter: func() {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
		},
	}
	p := newStreamPosterWith(slow, "ch1")
	p.OnDelta("first") // schedules flush → blocking Send

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		p.Stop()
		t.Fatal("Send never started")
	}

	// Send is blocked in messenger; OnDelta must still return quickly.
	start := time.Now()
	p.OnDelta("second-while-send-blocked")
	elapsed := time.Since(start)
	if elapsed > sendDelay/4 {
		close(release)
		p.Stop()
		t.Fatalf("OnDelta blocked %s while Send in flight (limit %s)", elapsed, sendDelay/4)
	}
	if !strings.Contains(p.Text(), "second-while-send-blocked") {
		close(release)
		p.Stop()
		t.Fatalf("delta not recorded: %q", p.Text())
	}

	close(release)
	_ = p.Finish()
}

type blockingMessenger struct {
	onSendEnter func()
	n           int
	mu          sync.Mutex
}

func (s *blockingMessenger) Send(channelID, content string) (string, error) {
	if s.onSendEnter != nil {
		s.onSendEnter()
	}
	s.mu.Lock()
	s.n++
	id := "blk-" + itoa(s.n)
	s.mu.Unlock()
	return id, nil
}
func (s *blockingMessenger) Edit(channelID, msgID, content string) error {
	if s.onSendEnter != nil {
		s.onSendEnter()
	}
	return nil
}
func (s *blockingMessenger) Typing(channelID string) error { return nil }

func TestStreamPosterStopEndsFlusher(t *testing.T) {
	fake := &fakeMessenger{}
	p := newStreamPosterWith(fake, "ch1")
	p.Stop()
	// Second Stop must not panic (already closed).
	p.Stop()
}

func TestStreamPosterAbortClearsFooter(t *testing.T) {
	fake := &fakeMessenger{}
	p := newStreamPosterWith(fake, "ch1")
	p.OnDelta("partial answer")
	time.Sleep(80 * time.Millisecond)
	p.Flush()
	time.Sleep(100 * time.Millisecond)
	p.Abort("cancelled")
	if p.Unposted() != "" {
		t.Fatalf("unposted=%q", p.Unposted())
	}
	sends, edits, _ := fake.snapshot()
	bodies := finalBodiesByID(sends, edits)
	if len(bodies) == 0 {
		t.Fatal("no posts")
	}
	last := bodies[len(bodies)-1]
	if strings.Contains(last, "_(streaming") {
		t.Fatalf("streaming footer remains: %q", last)
	}
	if !strings.Contains(last, "cancelled") {
		t.Fatalf("expected cancelled note in %q", last)
	}
}

func TestStreamPosterLiveShowsTail(t *testing.T) {
	fake := &fakeMessenger{}
	p := newStreamPosterWith(fake, "ch1")
	// Under one message limit but over live budget so live uses tail window.
	prefix := strings.Repeat("HEADHEAD ", 300)
	suffix := "TAIL_VISIBLE_MARKER"
	p.OnDelta(prefix + suffix)
	deadline := time.Now().Add(2 * time.Second)
	var saw string
	for time.Now().Before(deadline) {
		p.Flush()
		time.Sleep(40 * time.Millisecond)
		sends, edits, _ := fake.snapshot()
		if len(sends) > 0 {
			saw = sends[len(sends)-1].content
		}
		if len(edits) > 0 {
			saw = edits[len(edits)-1].content
		}
		if strings.Contains(saw, "TAIL_VISIBLE") && strings.Contains(saw, "streaming") {
			break
		}
	}
	if !strings.Contains(saw, "TAIL_VISIBLE") {
		t.Fatalf("live window missing tail: %q", truncateForTest(saw, 200))
	}
	if strings.HasPrefix(strings.TrimPrefix(saw, "…"), "HEADHEAD HEADHEAD") && !strings.HasPrefix(saw, "…") {
		t.Fatalf("expected tail window with ellipsis, got %q", truncateForTest(saw, 120))
	}
	_ = p.Finish()
}

func TestThoughtTrackerActivity(t *testing.T) {
	var tr thoughtTracker
	tr.OnDelta("Analyzing the codebase")
	tr.OnActivity("tool bash")
	got := tr.Latest()
	if !strings.Contains(got, "bash") {
		t.Fatalf("got %q", got)
	}
}

func TestThoughtTrackerPhases(t *testing.T) {
	var tr thoughtTracker
	_, phases := tr.Progress()
	if phases != "read → edit → test → PR" {
		t.Fatalf("idle chips: %q", phases)
	}

	tr.OnActivity("read_file: internal/bot/stream.go")
	act, phases := tr.Progress()
	if !strings.Contains(act, "read_file") {
		t.Fatalf("activity: %q", act)
	}
	if phases != "**read** → edit → test → PR" {
		t.Fatalf("after read: %q", phases)
	}

	tr.OnActivity("search_replace: stream.go")
	_, phases = tr.Progress()
	if phases != "✓read → **edit** → test → PR" {
		t.Fatalf("after edit: %q", phases)
	}

	tr.OnActivity("run_terminal_command: go test ./internal/bot")
	_, phases = tr.Progress()
	if phases != "✓read → ✓edit → **test** → PR" {
		t.Fatalf("after test: %q", phases)
	}

	tr.OnActivity("run_terminal_command: gh pr create --title fix")
	_, phases = tr.Progress()
	if phases != "✓read → ✓edit → ✓test → **PR**" {
		t.Fatalf("after pr: %q", phases)
	}
}

func TestClassifyPhase(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		{"read_file: bot.go", phaseRead},
		{"grep: TODO", phaseRead},
		{"list_dir: .", phaseRead},
		{"search_replace: a.go", phaseEdit},
		{"write: out.txt", phaseEdit},
		{"run_terminal_command: go test ./...", phaseTest},
		{"run_terminal_command: npm test", phaseTest},
		{"run_terminal_command: gh pr create", phasePR},
		{"run_terminal_command: git push -u origin HEAD", phasePR},
		{"run_terminal_command: ls -la", phaseRead},
		{"run_terminal_command: echo hi", -1},
		{"", -1},
	}
	for _, tc := range cases {
		if got := classifyPhase(tc.line); got != tc.want {
			t.Errorf("classifyPhase(%q)=%d want %d", tc.line, got, tc.want)
		}
	}
}

func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
