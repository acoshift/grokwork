package bot

import (
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

const (
	streamEditMinInterval = 800 * time.Millisecond
	streamEditMaxInterval = 3 * time.Second
	streamLiveBudget      = 1700
	streamTypingInterval  = 8 * time.Second
	streamFooter          = "\n\n_(streaming…)_"
	progressInterval      = 4 * time.Second
)

// messageMessenger is the Discord surface used by the stream poster (testable).
type messageMessenger interface {
	Send(channelID, content string) (msgID string, err error)
	Edit(channelID, msgID, content string) error
	Typing(channelID string) error
}

type discordMessenger struct {
	s *discordgo.Session
}

func (m discordMessenger) Send(channelID, content string) (string, error) {
	msg, err := discordSend(m.s, channelID, content)
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

func (m discordMessenger) Edit(channelID, msgID, content string) error {
	return discordEdit(m.s, channelID, msgID, content)
}

func (m discordMessenger) Typing(channelID string) error {
	return m.s.ChannelTyping(channelID)
}

func runeLen(s string) int {
	return utf8.RuneCountInString(s)
}

// cutPrefix returns the longest prefix of s with at most budget runes,
// preferring a newline cut in the latter half of the budget.
func cutPrefix(s string, budget int) string {
	if budget <= 0 || s == "" {
		return ""
	}
	if runeLen(s) <= budget {
		return s
	}
	n := 0
	cut := 0
	for i := range s {
		if n == budget {
			cut = i
			break
		}
		n++
	}
	if cut == 0 {
		cut = len(s)
	}
	chunk := s[:cut]
	if i := strings.LastIndex(chunk, "\n"); i > len(chunk)/2 {
		chunk = chunk[:i]
	}
	for !utf8.ValidString(chunk) && len(chunk) > 0 {
		chunk = chunk[:len(chunk)-1]
	}
	return chunk
}

// streamTail returns a Discord-safe window of s ending at the latest text.
// Over-budget strings start with "…" and keep the tail (not a frozen head).
func streamTail(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if runeLen(s) <= budget {
		return s
	}
	inner := budget - 1
	if inner < 1 {
		inner = 1
	}
	runes := []rune(s)
	if len(runes) <= inner {
		return s
	}
	tail := string(runes[len(runes)-inner:])
	if i := strings.Index(tail, "\n"); i >= 0 && i < len(tail)/2 {
		tail = tail[i+1:]
	}
	for !utf8.ValidString(tail) && len(tail) > 0 {
		tail = tail[1:]
	}
	return "…" + strings.TrimLeft(tail, " \t")
}

// streamPreview is kept as an alias for tests that referenced head preview;
// live streaming uses streamTail.
func streamPreview(s string, budget int) string {
	return streamTail(s, budget)
}

type streamPoster struct {
	msg       messageMessenger
	channelID string

	mu          sync.Mutex
	full        strings.Builder
	sealed      int // byte offset into full already sealed into Discord messages
	liveMsgID   string
	lastPosted  string
	closed      bool
	dirty       bool
	failed      bool
	minInterval time.Duration
	lastEdit    time.Time
	lastTyping  time.Time
	failBackoff time.Duration
	cancelNote  string

	wake chan struct{}
	stop chan struct{}
	wg   sync.WaitGroup
}

// discordOp is work to perform without holding p.mu.
type discordOp struct {
	kind    opKind
	content string
	msgID   string // empty ⇒ Send; else Edit
	// seal advances sealed by sealBytes after a successful post.
	sealBytes int
	// afterSeal clears live message identity (start a new live msg next).
	afterSeal bool
	// live marks this as the live streaming message (commit lastPosted etc.).
	live bool
}

type opKind int

const (
	opNone opKind = iota
	opPost
)

func newStreamPoster(s *discordgo.Session, channelID string) *streamPoster {
	return newStreamPosterWith(discordMessenger{s: s}, channelID)
}

func newStreamPosterWith(msg messageMessenger, channelID string) *streamPoster {
	p := &streamPoster{
		msg:         msg,
		channelID:   channelID,
		minInterval: streamEditMinInterval,
		wake:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
	}
	p.wg.Add(1)
	go p.flushLoop()
	return p
}

func (p *streamPoster) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// OnDelta records text. Never performs Discord HTTP — only wakes the flusher.
// Must not block on messenger latency (mu is never held during Send/Edit).
func (p *streamPoster) OnDelta(delta string) {
	if delta == "" {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.full.WriteString(delta)
	p.dirty = true
	p.mu.Unlock()
	p.signal()
}

// Flush requests a prompt Discord update (still async).
func (p *streamPoster) Flush() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.dirty = true
	p.mu.Unlock()
	p.signal()
}

// Stop ends the flusher goroutine without posting. Safe on early task exits.
func (p *streamPoster) Stop() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	close(p.stop)
	p.wg.Wait()
}

// Abort marks the stream cancelled/failed and finalizes without a streaming footer.
func (p *streamPoster) Abort(note string) {
	p.mu.Lock()
	if !p.closed {
		p.cancelNote = note
	}
	p.mu.Unlock()
	_ = p.Finish()
}

// Finish stops the flusher, seals remaining text, clears streaming footer.
// Returns true when the caller should not sendChunks (all text posted).
// On partial Discord failure, returns false and Unposted() holds the remainder.
func (p *streamPoster) Finish() (streamedFully bool) {
	p.mu.Lock()
	if p.closed {
		unposted := p.unpostedLocked()
		fullEmpty := p.full.Len() == 0
		p.mu.Unlock()
		// Stop()/prior Finish: nothing unposted. Empty buffer still means the
		// normal Finish path should let the caller sendChunks("(empty response)").
		if fullEmpty {
			return false
		}
		return unposted == ""
	}
	p.closed = true
	p.dirty = true
	p.mu.Unlock()

	close(p.stop)
	p.wg.Wait()

	return p.finalize()
}

// Unposted returns text not yet successfully posted to Discord.
func (p *streamPoster) Unposted() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unpostedLocked()
}

func (p *streamPoster) unpostedLocked() string {
	s := p.full.String()
	if p.sealed >= len(s) {
		return ""
	}
	return s[p.sealed:]
}

func (p *streamPoster) Text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.full.String()
}

func (p *streamPoster) flushLoop() {
	defer p.wg.Done()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-p.wake:
			p.throttledFlush()
		case <-ticker.C:
			p.mu.Lock()
			dirty := p.dirty && !p.closed
			p.mu.Unlock()
			if dirty {
				p.throttledFlush()
			}
		}
	}
}

func (p *streamPoster) throttledFlush() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	wait := p.minInterval
	if p.failBackoff > 0 {
		wait = p.failBackoff
	}
	elapsed := time.Since(p.lastEdit)
	needWait := elapsed < wait && p.liveMsgID != "" && p.lastEdit != (time.Time{})
	p.mu.Unlock()

	if needWait {
		time.Sleep(wait - elapsed)
	}
	p.flushOnce(false)
}

func (p *streamPoster) flushOnce(final bool) {
	_ = p.apply(final)
}

// apply posts/edits Discord to match buffer state. Never holds mu during Send/Edit.
func (p *streamPoster) apply(final bool) bool {
	for {
		op, ok := p.planOp(final)
		if !ok {
			return false
		}
		if op.kind == opNone {
			return true
		}

		// I/O without lock — OnDelta must not wait on Discord RTT.
		var (
			newID string
			err   error
		)
		if op.msgID == "" {
			newID, err = p.msg.Send(p.channelID, op.content)
		} else {
			err = p.msg.Edit(p.channelID, op.msgID, op.content)
			newID = op.msgID
		}

		p.mu.Lock()
		if err != nil {
			p.onEditErrLocked(err)
			p.mu.Unlock()
			return false
		}
		if op.sealBytes > 0 {
			p.sealed += op.sealBytes
			full := p.full.String()
			for p.sealed < len(full) && (full[p.sealed] == '\n' || full[p.sealed] == '\r') {
				p.sealed++
			}
			if op.afterSeal {
				p.liveMsgID = ""
				p.lastPosted = ""
			} else {
				p.liveMsgID = newID
			}
		} else if op.live {
			p.liveMsgID = newID
			p.lastPosted = op.content
			p.lastEdit = time.Now()
			p.dirty = false
			p.failBackoff = 0
			p.minInterval = streamEditMinInterval
			p.maybeTypingAsyncLocked()
		}
		// More work may remain (multi-seal); loop.
		needMore := false
		if op.sealBytes > 0 || final {
			// Re-check if more sealing needed.
			full := p.full.String()
			remaining := ""
			if p.sealed < len(full) {
				remaining = full[p.sealed:]
			}
			footerBudget := 0
			if !final {
				footerBudget = runeLen(streamFooter)
			}
			if remaining != "" && runeLen(remaining)+footerBudget > maxMsg {
				needMore = true
			} else if remaining != "" && final && runeLen(remaining) > maxMsg {
				needMore = true
			} else if remaining != "" && (op.sealBytes > 0) {
				// After a seal, still need to post live remainder (or more seals).
				needMore = true
			}
		}
		p.mu.Unlock()
		if !needMore && op.live {
			return true
		}
		if !needMore && op.sealBytes > 0 {
			// Fall through to plan live/final remainder.
			continue
		}
		if !needMore {
			return true
		}
	}
}

// planOp decides the next Discord write under lock, then returns without I/O.
// ok=false means a hard planning failure (should not happen in practice).
func (p *streamPoster) planOp(final bool) (discordOp, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !final && p.closed {
		return discordOp{kind: opNone}, true
	}

	full := p.full.String()
	if full == "" && !final {
		p.dirty = false
		return discordOp{kind: opNone}, true
	}

	remaining := ""
	if p.sealed < len(full) {
		remaining = full[p.sealed:]
	}
	if remaining == "" {
		p.dirty = false
		return discordOp{kind: opNone}, true
	}

	footerBudget := 0
	if !final {
		footerBudget = runeLen(streamFooter)
	}

	// Seal complete messages until remainder fits.
	if runeLen(remaining)+footerBudget > maxMsg && (!final || runeLen(remaining) > maxMsg) {
		chunk := cutPrefix(remaining, maxMsg)
		if chunk == "" {
			p.failed = true
			return discordOp{}, false
		}
		// When final, seal if remaining alone exceeds maxMsg.
		if final && runeLen(remaining) <= maxMsg {
			// fall through to final live post
		} else if !final || runeLen(remaining) > maxMsg {
			return discordOp{
				kind:      opPost,
				content:   chunk,
				msgID:     p.liveMsgID,
				sealBytes: len(chunk),
				afterSeal: true,
			}, true
		}
	}

	var content string
	if final {
		content = remaining
		if p.cancelNote != "" {
			content = content + "\n\n_(" + p.cancelNote + ")_"
		}
		if runeLen(content) > maxMsg && p.cancelNote != "" {
			// Prefer fitting body; note may push over — seal body without note first.
			if runeLen(remaining) > maxMsg {
				chunk := cutPrefix(remaining, maxMsg)
				if chunk == "" {
					p.failed = true
					return discordOp{}, false
				}
				return discordOp{
					kind:      opPost,
					content:   chunk,
					msgID:     p.liveMsgID,
					sealBytes: len(chunk),
					afterSeal: true,
				}, true
			}
		}
	} else {
		body := streamTail(remaining, streamLiveBudget)
		content = body + streamFooter
		if runeLen(content) > maxMsg {
			body = streamTail(remaining, maxMsg-runeLen(streamFooter)-1)
			content = body + streamFooter
		}
	}

	if content == p.lastPosted {
		p.dirty = false
		return discordOp{kind: opNone}, true
	}

	return discordOp{
		kind:    opPost,
		content: content,
		msgID:   p.liveMsgID,
		live:    true,
	}, true
}

func (p *streamPoster) onEditErrLocked(err error) {
	log.Printf("warn: stream post/edit channel=%s: %v", p.channelID, err)
	p.failed = true
	if p.failBackoff == 0 {
		p.failBackoff = streamEditMinInterval * 2
	} else {
		p.failBackoff *= 2
		if p.failBackoff > streamEditMaxInterval {
			p.failBackoff = streamEditMaxInterval
		}
	}
}

func (p *streamPoster) maybeTypingAsyncLocked() {
	if time.Since(p.lastTyping) < streamTypingInterval {
		return
	}
	p.lastTyping = time.Now()
	ch := p.channelID
	go func() {
		if err := p.msg.Typing(ch); err != nil {
			log.Printf("warn: stream typing channel=%s: %v", ch, err)
		}
	}()
}

func (p *streamPoster) finalize() bool {
	p.mu.Lock()
	full := p.full.String()
	p.mu.Unlock()
	if full == "" {
		return false
	}
	if !p.apply(true) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.liveMsgID != "" {
		p.sealed = len(p.full.String())
		p.liveMsgID = ""
		p.lastPosted = ""
	}
	return p.unpostedLocked() == ""
}

// Live progress phases (read → edit → test → PR).
const (
	phaseRead = iota
	phaseEdit
	phaseTest
	phasePR
	phaseCount
)

var phaseLabels = [phaseCount]string{"read", "edit", "test", "PR"}

type thoughtTracker struct {
	mu       sync.Mutex
	buf      strings.Builder
	last     string
	activity string
	seen     [phaseCount]bool
	current  int // only meaningful when seen[current]
}

func (t *thoughtTracker) OnDelta(delta string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.WriteString(delta)
	s := t.buf.String()
	if len(s) > 400 {
		s = s[len(s)-400:]
		t.buf.Reset()
		t.buf.WriteString(s)
	}
	s = strings.Join(strings.Fields(s), " ")
	if runeLen(s) > 80 {
		r := []rune(s)
		s = "…" + string(r[len(r)-79:])
	}
	t.last = s
}

func (t *thoughtTracker) OnActivity(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := classifyPhase(line); p >= 0 {
		t.seen[p] = true
		t.current = p
	}
	if runeLen(line) > 80 {
		r := []rune(line)
		line = "…" + string(r[len(r)-79:])
	}
	t.activity = line
}

func (t *thoughtTracker) Latest() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.activity != "" {
		if t.last != "" {
			return t.activity + " · " + t.last
		}
		return t.activity
	}
	return t.last
}

// Progress returns the latest activity snippet and phase chip line for the
// Discord status message.
func (t *thoughtTracker) Progress() (activity, phases string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.activity != "" {
		if t.last != "" {
			activity = t.activity + " · " + t.last
		} else {
			activity = t.activity
		}
	} else {
		activity = t.last
	}
	return activity, formatPhaseChips(t.seen, t.current)
}

func formatPhaseChips(seen [phaseCount]bool, current int) string {
	parts := make([]string, 0, phaseCount)
	for i, lab := range phaseLabels {
		switch {
		case seen[i] && i == current:
			parts = append(parts, "**"+lab+"**")
		case seen[i]:
			parts = append(parts, "✓"+lab)
		default:
			parts = append(parts, lab)
		}
	}
	return strings.Join(parts, " → ")
}

// classifyPhase maps a tool activity line to a progress phase.
// Returns -1 when the tool does not clearly match a chip.
func classifyPhase(line string) int {
	line = strings.TrimSpace(line)
	if line == "" {
		return -1
	}
	name, detail := line, ""
	if i := strings.Index(line, ":"); i >= 0 {
		name = strings.TrimSpace(line[:i])
		detail = strings.TrimSpace(line[i+1:])
	}
	name = strings.TrimPrefix(strings.ToLower(name), "tool ")
	detailL := strings.ToLower(detail)
	combined := name + " " + detailL

	if isPRActivity(combined, detailL) {
		return phasePR
	}
	if isTestActivity(name, detailL) {
		return phaseTest
	}
	if isEditActivity(name, detailL) {
		return phaseEdit
	}
	if isReadActivity(name, detailL) {
		return phaseRead
	}
	return -1
}

func isPRActivity(combined, detail string) bool {
	needles := []string{"gh pr", "pull request", "git push", "create pull request"}
	for _, n := range needles {
		if strings.Contains(combined, n) || strings.Contains(detail, n) {
			return true
		}
	}
	return false
}

func isTestActivity(name, detail string) bool {
	if strings.Contains(name, "test") {
		return true
	}
	needles := []string{
		"go test", "npm test", "npm run test", "pnpm test", "yarn test",
		"bun test", "pytest", "cargo test", "make test", "mvn test",
		"gradle test", "vitest", "jest",
	}
	for _, n := range needles {
		if strings.Contains(detail, n) {
			return true
		}
	}
	// Generic: bare `test` as a command token.
	if strings.Contains(detail, " test ") || strings.HasPrefix(detail, "test ") ||
		strings.HasSuffix(detail, " test") {
		return true
	}
	return false
}

func isEditActivity(name, detail string) bool {
	switch name {
	case "search_replace", "write", "edit", "str_replace", "apply_patch",
		"create_file", "delete_file", "rename", "write_file", "edit_file":
		return true
	}
	if strings.Contains(name, "write") || strings.Contains(name, "edit") ||
		strings.Contains(name, "replace") || strings.Contains(name, "patch") {
		return true
	}
	// Shell edits (rare but useful).
	if strings.Contains(detail, "sed -i") || strings.Contains(detail, "tee ") {
		return true
	}
	return false
}

func isReadActivity(name, detail string) bool {
	switch name {
	case "read_file", "read", "grep", "list_dir", "glob", "web_search",
		"web_fetch", "open_page", "open_page_with_find", "search_tool",
		"list_directory", "find_files", "cat", "head", "tail":
		return true
	}
	if strings.HasPrefix(name, "read") || strings.Contains(name, "search") ||
		strings.Contains(name, "grep") || strings.Contains(name, "list") {
		return true
	}
	// Shell inspection without mutation.
	if name == "run_terminal_command" || name == "run_terminal_cmd" || name == "bash" || name == "shell" {
		readCmds := []string{"cat ", "head ", "tail ", "less ", "rg ", "grep ", "find ", "ls ", "git log", "git show", "git diff", "git status"}
		for _, c := range readCmds {
			if strings.Contains(detail, c) || strings.HasPrefix(detail, strings.TrimSpace(c)) {
				// Prefer not classifying pure git push/status mixups as read when PR wins already.
				return true
			}
		}
	}
	return false
}
