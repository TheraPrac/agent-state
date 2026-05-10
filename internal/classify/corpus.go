package classify

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// CorpusEntry is one operator decision captured as feedback for the
// classifier — the classifier consults recent entries as in-context
// examples on subsequent calls. T-346's `st decide` writes these;
// phase 1 of T-345 only reads (corpus is empty on first run).
type CorpusEntry struct {
	ItemID         string    `json:"item_id"`
	DecidedAt      time.Time `json:"decided_at"`
	TouchedFiles   []string  `json:"touched_files"`
	Verdict        Verdict   `json:"verdict"`
	OperatorAction string    `json:"operator_action"` // approved|rejected|edited
	OperatorReason string    `json:"operator_reason"`
}

// ReadCorpus returns up to `limit` recent entries from the corpus log
// at `path`. Returns (nil, nil) if the file does not exist — an empty
// corpus is the expected first-run state, not an error.
//
// `limit` of 0 or negative returns all entries.
func ReadCorpus(path string, limit int) ([]CorpusEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []CorpusEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e CorpusEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// AppendCorpus writes one entry to the corpus log (JSONL). Creates the
// file and any missing parent directories. Used by T-346's `st decide`;
// phase 1 ships the writer so the IO path is unit-testable now.
func AppendCorpus(path string, entry CorpusEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	_, err = f.Write([]byte{'\n'})
	return err
}
