package bot

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// launchd StandardOutPath / StandardErrorPath files under DataDir.
var processLogNames = []string{"stdout.log", "stderr.log"}

const logTrimInterval = 24 * time.Hour

// Cap the bytes we keep (and read) so a file with few newlines cannot pull
// hundreds of MB into memory just to honour logTailLines.
var logTailMaxBytes int64 = 32 << 20

func (b *Bot) startLogTrim() {
	if b == nil {
		return
	}
	b.logTrimOnce.Do(func() {
		keep := 0
		if b.cfg != nil {
			keep = b.cfg.LogTailLinesValue()
		}
		log.Printf("bg: starting process-log trimmer interval=%s keepLines=%d initial_delay=30s",
			logTrimInterval, keep)
		go b.runLogTrim()
	})
}

func (b *Bot) runLogTrim() {
	ctx := b.bgContext()
	log.Printf("bg: process-log trimmer running (waiting 30s before first cycle)")
	if !sleepCtx(ctx, 30*time.Second) {
		log.Printf("bg: process-log trimmer stopped before first cycle")
		return
	}
	b.runLogTrimCycle("initial")

	ticker := time.NewTicker(logTrimInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("bg: process-log trimmer stopped")
			return
		case <-ticker.C:
			b.runLogTrimCycle("tick")
		}
	}
}

func (b *Bot) runLogTrimCycle(reason string) {
	if b == nil || b.cfg == nil {
		return
	}
	keep := b.cfg.LogTailLinesValue()
	if keep <= 0 {
		log.Printf("bg: log trim skipped reason=%s (disabled)", reason)
		return
	}
	dir := b.cfg.DataDir
	if dir == "" {
		return
	}
	start := time.Now()
	var trimmed int
	for _, name := range processLogNames {
		res, err := trimLogFile(filepath.Join(dir, name), keep)
		if err != nil {
			log.Printf("warn: log trim file=%s: %v", name, err)
			continue
		}
		if res.Skipped {
			continue
		}
		log.Printf("bg: log trim file=%s before=%d after=%d lines=%d",
			name, res.Before, res.After, res.KeptLines)
		trimmed++
	}
	log.Printf("bg: log trim done reason=%s files=%d elapsed=%s",
		reason, trimmed, time.Since(start).Round(time.Millisecond))
}

type trimResult struct {
	Path      string
	Before    int64
	After     int64
	KeptLines int
	Skipped   bool
}

// trimLogFile rewrites path in place so it holds at most keepLines trailing
// lines. Rename/rotate is wrong here: launchd keeps stdout/stderr open for the
// process lifetime, so a new file would stay empty while the old inode grows.
func trimLogFile(path string, keepLines int) (trimResult, error) {
	res := trimResult{Path: path}
	if keepLines <= 0 {
		res.Skipped = true
		return res, nil
	}
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			res.Skipped = true
			return res, nil
		}
		return res, err
	}
	if !st.Mode().IsRegular() {
		res.Skipped = true
		return res, nil
	}
	res.Before = st.Size()
	if res.Before == 0 {
		res.Skipped = true
		return res, nil
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return res, err
	}
	defer f.Close()

	tail, whole, err := readTail(f, res.Before, keepLines)
	if err != nil {
		return res, err
	}
	res.KeptLines = countLines(tail)
	if whole {
		res.After = res.Before
		res.Skipped = true
		return res, nil
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return res, err
	}
	n, err := f.Write(tail)
	if err != nil {
		return res, err
	}
	if err := f.Truncate(int64(n)); err != nil {
		return res, err
	}
	if err := f.Sync(); err != nil {
		return res, err
	}
	res.After = int64(n)
	resyncStdio(f, res.After)
	return res, nil
}

func readTail(f *os.File, size int64, n int) (tail []byte, whole bool, err error) {
	if size == 0 || n <= 0 {
		return nil, true, nil
	}
	window := min(size, logTailMaxBytes)
	start := size - window
	buf := make([]byte, window)
	if _, err := f.ReadAt(buf, start); err != nil {
		return nil, false, err
	}
	if start > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	tail = lastNLines(buf, n)
	return tail, start == 0 && len(tail) == len(buf), nil
}

func lastNLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return nil
	}
	i := len(data)
	if data[i-1] == '\n' {
		i--
	}
	seen := 0
	for i > 0 {
		i--
		if data[i] == '\n' {
			seen++
			if seen == n {
				return data[i+1:]
			}
		}
	}
	return data
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

func resyncStdio(src *os.File, size int64) {
	st, err := src.Stat()
	if err != nil {
		return
	}
	for _, out := range []*os.File{os.Stdout, os.Stderr} {
		if out == nil {
			continue
		}
		ost, err := out.Stat()
		if err != nil {
			continue
		}
		if os.SameFile(st, ost) {
			_, _ = out.Seek(size, io.SeekStart)
		}
	}
}
