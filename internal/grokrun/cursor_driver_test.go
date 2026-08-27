package grokrun

import (
	"slices"
	"strings"
	"testing"
)

func cursorArgs(opt Options) []string {
	return cursorDriver{}.args(argInput{opt: opt, format: "stream-json"})
}

func TestCursorArgsBaseline(t *testing.T) {
	args := cursorArgs(Options{Cwd: "/repo", Model: "composer-2.5"})

	for _, want := range []string{"--print", "--trust", "--stream-partial-output"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}
	if got := argValue(args, "--output-format"); got != "stream-json" {
		t.Errorf("output-format=%q", got)
	}
	if got := argValue(args, "--workspace"); got != "/repo" {
		t.Errorf("workspace=%q", got)
	}
	if got := argValue(args, "--model"); got != "composer-2.5" {
		t.Errorf("model=%q", got)
	}
	for _, never := range []string{"--cwd", "--prompt-file", "--verbatim", "--max-turns", "--mcp-config", "--tools"} {
		if slices.Contains(args, never) {
			t.Errorf("unexpected flag %s in %v", never, args)
		}
	}
	if !(cursorDriver{}).promptOnStdin() {
		t.Error("cursor must take the prompt on stdin")
	}
}

func TestCursorArgsNonStreamOmitsPartialFlag(t *testing.T) {
	args := cursorDriver{}.args(argInput{opt: Options{}, format: "json"})
	if slices.Contains(args, "--stream-partial-output") {
		t.Errorf("stream-partial-output only valid with stream-json: %v", args)
	}
}

func TestCursorArgsYolo(t *testing.T) {
	args := cursorArgs(Options{Yolo: true})
	if !slices.Contains(args, "--yolo") {
		t.Fatalf("missing --yolo: %v", args)
	}
	if slices.Contains(cursorArgs(Options{}), "--yolo") {
		t.Fatal("non-yolo runs must not set --yolo")
	}
}

func TestCursorArgsToolsMapToAskMode(t *testing.T) {
	empty := ""
	args := cursorArgs(Options{Tools: &empty})
	if got := argValue(args, "--mode"); got != "ask" {
		t.Fatalf("tools-off mode=%q args=%v", got, args)
	}
	allow := "Read,Grep,Glob"
	args = cursorArgs(Options{Tools: &allow})
	if got := argValue(args, "--mode"); got != "ask" {
		t.Fatalf("investigate mode=%q args=%v", got, args)
	}
	if slices.Contains(cursorArgs(Options{}), "--mode") {
		t.Fatal("unrestricted runs must not set --mode")
	}
}

func TestCursorArgsSessionResume(t *testing.T) {
	args := cursorArgs(Options{SessionID: "sess-old"})
	if got := argValue(args, "--resume"); got != "sess-old" {
		t.Fatalf("resume=%q args=%v", got, args)
	}

	args = cursorArgs(Options{SessionID: "sess-fresh", ForceNewSession: true})
	if got := argValue(args, "--resume"); got != "sess-fresh" {
		t.Fatalf("force-new must still pass the prebound id (create-or-attach): %v", args)
	}
}

func TestCursorArgsExtraArgs(t *testing.T) {
	args := cursorArgs(Options{ExtraArgs: []string{"--sandbox", "disabled"}})
	if got := argValue(args, "--sandbox"); got != "disabled" {
		t.Fatalf("extra args dropped: %v", args)
	}
}

func TestCursorDecodeStream(t *testing.T) {
	var texts, thoughts []string
	out, err := decodeStream(strings.NewReader(cursorStream), cursorDriver{}, streamCallbacks{
		onText:    func(s string) { texts = append(texts, s) },
		onThought: func(s string) { thoughts = append(thoughts, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.SessionID != "c3b1575d-f8b1-42d0-b964-640d2500448f" {
		t.Errorf("session=%q", out.SessionID)
	}
	if out.Text != "pong" {
		t.Errorf("text=%q (duplicate assistant events must be folded)", out.Text)
	}
	if strings.Join(thoughts, "") != `The response will be exactly "pong".` {
		t.Errorf("thoughts=%q", thoughts)
	}
	if out.Usage == nil || out.Usage.InputTokens != 8623 || out.Usage.CacheReadInputTokens != 6208 {
		t.Errorf("usage=%+v", out.Usage)
	}
	if out.ContextTokensUsed != 8623+32+6208 {
		t.Errorf("context=%d", out.ContextTokensUsed)
	}
}

func TestCursorDecodePartialTokens(t *testing.T) {
	var texts []string
	out, err := decodeStream(strings.NewReader(cursorPartialStream), cursorDriver{}, streamCallbacks{
		onText: func(s string) { texts = append(texts, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Hello world." {
		t.Errorf("text=%q (tokens must concatenate; assembled copy must be skipped)", out.Text)
	}
	if strings.Join(texts, "") != "Hello world." {
		t.Errorf("deltas=%q", texts)
	}
	for _, d := range texts {
		if strings.Contains(d, "\n") {
			t.Errorf("token %q must not start a new paragraph", d)
		}
	}
}

func TestCursorDecodeTwoMessagesAfterTool(t *testing.T) {
	out, err := decodeStream(strings.NewReader(cursorTwoMessageStream), cursorDriver{}, streamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "First paragraph.\n\nSecond paragraph." {
		t.Errorf("text=%q", out.Text)
	}
}

func TestCursorDecodeSecondMessageWithBlankLine(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"First"}]},"timestamp_ms":1}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"First"}]},"timestamp_ms":2}`,
		`{"type":"tool_call","subtype":"started"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]},"timestamp_ms":3}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"\n\n"}]},"timestamp_ms":4}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]},"timestamp_ms":5}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello\n\nworld"}]}}`,
	}, "\n")
	out, err := decodeStream(strings.NewReader(raw), cursorDriver{}, streamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "First\n\nHello\n\nworld" {
		t.Errorf("text=%q", out.Text)
	}
}

func TestCursorDecodePartialTokensWithBlankLine(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]},"timestamp_ms":1}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"\n\n"}]},"timestamp_ms":2}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]},"timestamp_ms":3}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello\n\nworld"}]}}`,
	}, "\n")
	out, err := decodeStream(strings.NewReader(raw), cursorDriver{}, streamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Hello\n\nworld" {
		t.Errorf("text=%q (blank lines inside a message must not be treated as a new message)", out.Text)
	}
}

func TestCursorDecodeWholeMessagesStillSeparate(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"one"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}`,
	}, "\n")
	out, err := decodeStream(strings.NewReader(raw), cursorDriver{}, streamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "one\n\ntwo" {
		t.Errorf("text=%q", out.Text)
	}
}

func TestCursorDecodeFinal(t *testing.T) {
	out, ok := cursorDriver{}.decodeFinal([]byte(cursorFinalJSON))
	if !ok {
		t.Fatal("decodeFinal rejected json")
	}
	if out.Text != "json-ok" || out.SessionID != "fc9e92cc-7174-4411-84de-f4404bd949f4" {
		t.Errorf("out=%+v", out)
	}
	if out.Usage == nil || out.Usage.OutputTokens != 36 {
		t.Errorf("usage=%+v", out.Usage)
	}
}

func TestCursorSessionSignalsNeverRetry(t *testing.T) {
	d := cursorDriver{}
	if d.sessionAlreadyExists(Result{Code: 1, Stderr: "already in use"}) {
		t.Fatal("resume is create-or-attach")
	}
	if d.sessionMissing(Result{Code: 1, Stderr: "not found"}) {
		t.Fatal("resume is create-or-attach")
	}
}

// cursorStream is a trimmed capture of a real
// `cursor-agent --print --output-format stream-json --stream-partial-output`
// run. The assistant message is repeated; the second copy has no timestamp_ms.
const cursorStream = `{"type":"system","subtype":"init","session_id":"c3b1575d-f8b1-42d0-b964-640d2500448f","model":"Composer 2.5 Fast"}
{"type":"thinking","subtype":"delta","text":"The response will be","session_id":"c3b1575d-f8b1-42d0-b964-640d2500448f","timestamp_ms":1}
{"type":"thinking","subtype":"delta","text":" exactly \"pong\".","session_id":"c3b1575d-f8b1-42d0-b964-640d2500448f","timestamp_ms":2}
{"type":"thinking","subtype":"completed","session_id":"c3b1575d-f8b1-42d0-b964-640d2500448f"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"pong"}]},"session_id":"c3b1575d-f8b1-42d0-b964-640d2500448f","timestamp_ms":3}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"pong"}]},"session_id":"c3b1575d-f8b1-42d0-b964-640d2500448f"}
{"type":"result","subtype":"success","is_error":false,"result":"pong","session_id":"c3b1575d-f8b1-42d0-b964-640d2500448f","usage":{"inputTokens":8623,"outputTokens":32,"cacheReadTokens":6208,"cacheWriteTokens":0}}
`

const cursorFinalJSON = `{"type":"result","subtype":"success","is_error":false,"result":"json-ok","session_id":"fc9e92cc-7174-4411-84de-f4404bd949f4","usage":{"inputTokens":8657,"outputTokens":36,"cacheReadTokens":6176,"cacheWriteTokens":0}}
`

// cursorPartialStream is the --stream-partial-output shape: one timestamped
// assistant event per token, then the assembled message with no timestamp_ms.
const cursorPartialStream = `{"type":"system","subtype":"init","session_id":"d6445ce5-1119-4b30-a2c3-8aba02462ed6"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}]},"session_id":"d6445ce5-1119-4b30-a2c3-8aba02462ed6","timestamp_ms":1}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":" world"}]},"session_id":"d6445ce5-1119-4b30-a2c3-8aba02462ed6","timestamp_ms":2}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"."}]},"session_id":"d6445ce5-1119-4b30-a2c3-8aba02462ed6","timestamp_ms":3}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello world."}]},"session_id":"d6445ce5-1119-4b30-a2c3-8aba02462ed6"}
{"type":"result","subtype":"success","is_error":false,"result":"Hello world.","session_id":"d6445ce5-1119-4b30-a2c3-8aba02462ed6","usage":{"inputTokens":1,"outputTokens":3,"cacheReadTokens":0,"cacheWriteTokens":0}}
`

// cursorTwoMessageStream is a tool-using run: tokens, a timestamped flush of
// the first message (CLI does this on toolCallStarted), then a second message.
const cursorTwoMessageStream = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"First"}]},"timestamp_ms":1}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":" paragraph."}]},"timestamp_ms":2}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"First paragraph."}]},"timestamp_ms":3}
{"type":"tool_call","subtype":"started","call_id":"c1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Second"}]},"timestamp_ms":4}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":" paragraph."}]},"timestamp_ms":5}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Second paragraph."}]}}
`
