package grokrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// watchSessionTools tails ~/.grok/sessions/<cwd>/<sessionID>/updates.jsonl for
// tool_call events. streaming-json only emits thought/text/end today, so this
// is how live progress learns which tools are running.
//
// Starts at the current EOF (resume-safe: only tools from this run). Polls until
// ctx is cancelled. onActivity must be non-nil.
func watchSessionTools(ctx context.Context, cwd, sessionID string, onActivity func(string)) {
	if onActivity == nil || sessionID == "" {
		return
	}
	path := sessionUpdatesPath(cwd, sessionID)
	if path == "" {
		return
	}

	var offset int64
	if st, err := os.Stat(path); err == nil {
		offset = st.Size()
	}

	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

	var pending []byte
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, chunk, err := readFromOffset(path, offset)
			if err != nil || n == 0 {
				continue
			}
			offset += n
			pending = append(pending, chunk...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := pending[:i]
				pending = pending[i+1:]
				if act := parseToolCallActivity(line); act != "" {
					onActivity(act)
				}
			}
			// Cap incomplete-line buffer (corrupt/huge lines).
			if len(pending) > 256*1024 {
				pending = pending[len(pending)-64*1024:]
			}
		}
	}
}

func sessionUpdatesPath(cwd, sessionID string) string {
	home := grokHome()
	if home == "" || sessionID == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil || abs == "" {
		return ""
	}
	return filepath.Join(home, "sessions", encodeSessionDir(abs), sessionID, "updates.jsonl")
}

func readFromOffset(path string, offset int64) (n int64, data []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, nil, err
	}
	size := st.Size()
	if size <= offset {
		return 0, nil, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, nil, err
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return 0, nil, err
	}
	return int64(len(data)), data, nil
}

func parseToolCallActivity(line []byte) string {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return ""
	}
	var root struct {
		Params struct {
			Update json.RawMessage `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &root); err != nil || len(root.Params.Update) == 0 {
		return ""
	}
	var u struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Title         string          `json:"title"`
		RawInput      json.RawMessage `json:"rawInput"`
		Meta          json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(root.Params.Update, &u); err != nil {
		return ""
	}
	if !strings.EqualFold(u.SessionUpdate, "tool_call") {
		return ""
	}
	name := strings.TrimSpace(u.Title)
	if metaName := toolNameFromMeta(u.Meta); metaName != "" {
		name = metaName
	}
	detail := toolDetailFromRawInput(u.RawInput)
	switch {
	case name != "" && detail != "":
		return fmt.Sprintf("%s: %s", name, truncate(detail, 60))
	case name != "":
		return "tool " + name
	case detail != "":
		return truncate(detail, 80)
	default:
		return ""
	}
}

func toolNameFromMeta(meta json.RawMessage) string {
	if len(meta) == 0 {
		return ""
	}
	var m struct {
		Tool struct {
			Name string `json:"name"`
		} `json:"x.ai/tool"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.Tool.Name)
}

func toolDetailFromRawInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	keys := []string{
		"command", "description", "target_file", "file_path", "path",
		"pattern", "target_directory", "query", "url", "prompt",
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}
