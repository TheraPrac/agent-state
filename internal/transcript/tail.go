package transcript

import (
	"bytes"
	"io"
	"os"
)

// TailReader incrementally reads new rows appended to a session JSONL.
// Phase 4 of T-353: the live-stream substrate for `st watch`.
//
// It is deliberately offset-bookkeeping only (no fsnotify): the caller
// polls Read on a backoff. Robustness rules (operator silent-failure
// principle — a watcher must not crash or lie when files move under it):
//
//   - missing/unreadable file → nil rows, nil-safe (an agent may not
//     have written its JSONL yet; that is normal, not an error);
//   - size < off → truncation or rotation: restart from 0 so nothing
//     after the rotation is missed;
//   - only newline-terminated lines are consumed; a partial final line
//     (a write in flight) is left for the next Read, never parsed half.
type TailReader struct {
	path string
	off  int64
}

// NewTailReader starts a reader at the END of the file, so a watcher
// shows what happens FROM NOW, not the whole backlog (use ResolveSession
// + ReadFile for history — that is `st transcript`'s job).
func NewTailReader(path string) *TailReader {
	tr := &TailReader{path: path}
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		tr.off = fi.Size()
	}
	return tr
}

// NewTailReaderFromStart starts at byte 0 (used by tests / a future
// "replay then follow" mode).
func NewTailReaderFromStart(path string) *TailReader {
	return &TailReader{path: path}
}

// Path is the file this reader follows.
func (tr *TailReader) Path() string { return tr.path }

// Read returns rows appended since the last call. It never returns an
// error: a vanished/unreadable file yields nil (the absence is the
// caller's to surface, not a crash), truncation transparently restarts.
func (tr *TailReader) Read() []Row {
	fi, err := os.Stat(tr.path)
	if err != nil || fi.IsDir() {
		return nil
	}
	size := fi.Size()
	if size < tr.off {
		tr.off = 0 // truncated or rotated → re-read from the top
	}
	if size == tr.off {
		return nil
	}
	f, err := os.Open(tr.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(tr.off, io.SeekStart); err != nil {
		return nil
	}
	buf, err := io.ReadAll(f)
	if err != nil || len(buf) == 0 {
		return nil
	}
	// Consume only up to and including the last newline; keep any
	// trailing partial line for the next Read.
	nl := bytes.LastIndexByte(buf, '\n')
	if nl < 0 {
		return nil // no complete line yet
	}
	complete := buf[:nl+1]
	tr.off += int64(len(complete))

	var rows []Row
	for _, line := range bytes.Split(complete, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rows = append(rows, ParseLine(line)...)
	}
	return rows
}
