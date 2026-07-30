package bot

import (
	"strings"
)

// TasklistItem is one "- [ ]"/"- [x]" line of a GitHub tasklist.
type TasklistItem struct {
	Text       string // item text, checkbox marker and any session annotation stripped
	Checked    bool
	RawLine    string // the exact original line (the edit key)
	SessionURL string // from a trailing " — [session](url)" annotation, else ""
	ThreadID   string // parsed from SessionURL's "/sessions/{id}" path segment, else ""
}

const sessionAnnotOpen = " — [session]("

// ParseTasklist extracts GitHub-style tasklist items from an issue body.
// Lines inside fenced code blocks (``` / ~~~) are ignored. An unparseable body
// yields an empty slice, never an error.
func ParseTasklist(body string) []TasklistItem {
	var items []TasklistItem
	inFence := false
	var fenceChar byte
	for _, line := range splitBodyLines(body) {
		content := lineContent(line)
		if c, ok := fenceMarker(content); ok {
			if !inFence {
				inFence = true
				fenceChar = c
			} else if c == fenceChar {
				inFence = false
				fenceChar = 0
			}
			continue
		}
		if inFence {
			continue
		}
		if item, ok := parseTasklistLine(content); ok {
			items = append(items, item)
		}
	}
	return items
}

// AnnotateTasklistLine appends " — [session](url)" to the FIRST line equal to
// rawLine that has no annotation yet. Returns the new body and whether it changed.
// Line endings in the body are preserved.
func AnnotateTasklistLine(body, rawLine, sessionURL string) (string, bool) {
	rawLine = lineContent(rawLine)
	sessionURL = strings.TrimSpace(sessionURL)
	if rawLine == "" || sessionURL == "" {
		return body, false
	}
	lines := splitBodyLines(body)
	changed := false
	for i, line := range lines {
		content := lineContent(line)
		if content != rawLine {
			continue
		}
		if _, url := splitSessionAnnotation(content); url != "" {
			continue
		}
		lines[i] = content + sessionAnnotOpen + sessionURL + ")" + lineEnding(line)
		changed = true
		break
	}
	if !changed {
		return body, false
	}
	return strings.Join(lines, ""), true
}

// CheckTasklistLine flips "[ ]" to "[x]" on the first unchecked line whose
// annotation contains "/sessions/"+threadID. Returns new body and changed.
func CheckTasklistLine(body, threadID string) (string, bool) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return body, false
	}
	needle := "/sessions/" + threadID
	lines := splitBodyLines(body)
	changed := false
	for i, line := range lines {
		content := lineContent(line)
		item, ok := parseTasklistLine(content)
		if !ok || item.Checked {
			continue
		}
		if !strings.Contains(item.SessionURL, needle) {
			continue
		}
		flipped, ok := flipTasklistUnchecked(content)
		if !ok {
			continue
		}
		lines[i] = flipped + lineEnding(line)
		changed = true
		break
	}
	if !changed {
		return body, false
	}
	return strings.Join(lines, ""), true
}

func parseTasklistLine(content string) (TasklistItem, bool) {
	rest := content
	// Leading whitespace allowed.
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if len(rest) < 5 {
		return TasklistItem{}, false
	}
	// Bullet: - or *
	if rest[0] != '-' && rest[0] != '*' {
		return TasklistItem{}, false
	}
	rest = rest[1:]
	// At least one space after bullet.
	if len(rest) == 0 || (rest[0] != ' ' && rest[0] != '\t') {
		return TasklistItem{}, false
	}
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	// Checkbox: [ ] / [x] / [X]
	if len(rest) < 3 || rest[0] != '[' || rest[2] != ']' {
		return TasklistItem{}, false
	}
	mark := rest[1]
	var checked bool
	switch mark {
	case ' ':
		checked = false
	case 'x', 'X':
		checked = true
	default:
		return TasklistItem{}, false
	}
	rest = rest[3:]
	// Space after checkbox before text (GitHub allows zero? require at least separation when text present).
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	text, sessionURL := splitSessionAnnotation(rest)
	return TasklistItem{
		Text:       text,
		Checked:    checked,
		RawLine:    content,
		SessionURL: sessionURL,
		ThreadID:   threadIDFromSessionURL(sessionURL),
	}, true
}

// splitSessionAnnotation peels a trailing " — [session](<url>)" suffix.
func splitSessionAnnotation(text string) (itemText, sessionURL string) {
	i := strings.LastIndex(text, sessionAnnotOpen)
	if i < 0 {
		return text, ""
	}
	// Exact suffix: open mark through a closing ')' at end of line.
	after := text[i+len(sessionAnnotOpen):]
	if after == "" || after[len(after)-1] != ')' {
		return text, ""
	}
	// URL must not itself contain ')' — GitHub markdown links rarely do; refuse
	// anything ambiguous rather than mis-parse a hand-edited line.
	url := after[:len(after)-1]
	if strings.Contains(url, ")") {
		return text, ""
	}
	return text[:i], url
}

// threadIDFromSessionURL takes the path segment after "/sessions/" up to end or '?'.
func threadIDFromSessionURL(u string) string {
	u = strings.TrimSpace(u)
	const marker = "/sessions/"
	i := strings.Index(u, marker)
	if i < 0 {
		return ""
	}
	rest := u[i+len(marker):]
	if j := strings.IndexByte(rest, '?'); j >= 0 {
		rest = rest[:j]
	}
	if j := strings.IndexByte(rest, '#'); j >= 0 {
		rest = rest[:j]
	}
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// fenceMarker reports whether line opens/closes a fenced code block and which
// fence character (backtick or tilde) it uses. Closing must match the opener.
func fenceMarker(line string) (char byte, ok bool) {
	// Allow leading whitespace (CommonMark: up to 3 spaces; we accept tabs too).
	rest := line
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if strings.HasPrefix(rest, "```") {
		return '`', true
	}
	if strings.HasPrefix(rest, "~~~") {
		return '~', true
	}
	return 0, false
}

// flipTasklistUnchecked rewrites the first "[ ]" checkbox on a tasklist line to "[x]".
func flipTasklistUnchecked(content string) (string, bool) {
	// Walk like parseTasklistLine to the checkbox, then rewrite only "[ ]".
	rest := content
	idx := 0
	for idx < len(rest) && (rest[idx] == ' ' || rest[idx] == '\t') {
		idx++
	}
	if idx >= len(rest) || (rest[idx] != '-' && rest[idx] != '*') {
		return content, false
	}
	idx++
	for idx < len(rest) && (rest[idx] == ' ' || rest[idx] == '\t') {
		idx++
	}
	if idx+2 >= len(rest) || rest[idx] != '[' || rest[idx+1] != ' ' || rest[idx+2] != ']' {
		return content, false
	}
	out := content[:idx+1] + "x" + content[idx+2:]
	return out, true
}

// splitBodyLines keeps each line's trailing \n (and \r when present as \r\n)
// attached so join reconstructs the original endings.
func splitBodyLines(body string) []string {
	if body == "" {
		return nil
	}
	var lines []string
	for len(body) > 0 {
		i := strings.IndexByte(body, '\n')
		if i < 0 {
			lines = append(lines, body)
			break
		}
		lines = append(lines, body[:i+1])
		body = body[i+1:]
	}
	return lines
}

func lineContent(line string) string {
	return strings.TrimRight(line, "\r\n")
}

func lineEnding(line string) string {
	if strings.HasSuffix(line, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return "\n"
	}
	return ""
}
