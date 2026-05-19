package command

import (
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/registry"
)

// I-673: write-side cap, display truncation, and the st-index
// no-longer-silent error path.

func TestNoteAdd_RejectsOversizedMessage(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := NoteAdd(cfg, strings.Repeat("x", registry.MaxNoteBytes+1))
	if code != 2 {
		t.Fatalf("NoteAdd oversized returned %d, want 2", code)
	}
	r, _ := registry.Load(cfg.NotesPath())
	if len(r.Notes) != 0 {
		t.Errorf("oversized note was persisted (%d notes); want 0 — must reject before write", len(r.Notes))
	}
}

func TestNoteEdit_RejectsOversizedMessage(t *testing.T) {
	_, cfg := setupTestEnv(t)
	if code := NoteAdd(cfg, "original short note"); code != 0 {
		t.Fatalf("seed NoteAdd returned %d", code)
	}
	r, _ := registry.Load(cfg.NotesPath())
	id := r.Notes[0].ID

	code := NoteEdit(cfg, id, strings.Repeat("y", registry.MaxNoteBytes+1))
	if code != 2 {
		t.Fatalf("NoteEdit oversized returned %d, want 2", code)
	}
	r2, _ := registry.Load(cfg.NotesPath())
	if r2.Notes[0].Message != "original short note" {
		t.Errorf("message mutated despite rejected oversized edit: %q", r2.Notes[0].Message)
	}
}

func TestTruncateNoteForDisplay(t *testing.T) {
	short := "a concise breadcrumb"
	if got := truncateNoteForDisplay(short); got != short {
		t.Errorf("short message altered: %q", got)
	}

	long := strings.Repeat("z", 5000)
	got := truncateNoteForDisplay(long)
	if got == long {
		t.Fatal("long message was not truncated")
	}
	if len([]rune(got)) >= len([]rune(long)) {
		t.Errorf("truncated form (%d runes) not shorter than original (%d)", len([]rune(got)), len([]rune(long)))
	}
	if !strings.Contains(got, "5000 bytes total") || !strings.Contains(got, "truncated for display") {
		t.Errorf("truncated form lacks byte-total/marker: %q", got[len(got)-60:])
	}

	// Multibyte safety: truncation must not split a rune.
	multi := strings.Repeat("世", 1000) // 1000 runes, 3000 bytes
	mt := truncateNoteForDisplay(multi)
	if !strings.HasPrefix(mt, strings.Repeat("世", 280)) {
		t.Error("multibyte truncation split a rune or wrong boundary")
	}
}

func TestIndex_WarnsOnNotesLoadError(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Force registry.Load to error: make the notes path a directory so
	// os.Open succeeds but io.ReadAll fails. (Post-I-673, an oversized
	// line no longer errors, so this is the realistic remaining error.)
	if err := os.MkdirAll(cfg.NotesPath(), 0755); err != nil {
		t.Fatal(err)
	}

	var code int
	stderr := captureStderr(t, func() int {
		code = Index(s, cfg)
		return code
	})

	if code != 0 {
		t.Errorf("Index returned %d; want 0 (must stay resilient, not hard-fail)", code)
	}
	if !strings.Contains(stderr, "WARNING") || !strings.Contains(stderr, "notes registry failed to load") {
		t.Errorf("st index silently swallowed the notes load error; stderr=%q", stderr)
	}
	if _, err := os.Stat(cfg.IndexPath()); err != nil {
		t.Errorf("index file not written despite resilient degrade: %v", err)
	}
}
