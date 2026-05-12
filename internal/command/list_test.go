package command

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// TestList_PreservesStageAndAssignedSuffixes is the I-444 contract test:
// migrating st list to the shared FormatItemRow helper must not drop the
// list-specific "(stage)" or "[assigned]" suffixes. The shared formatter
// doesn't carry those columns; list.go appends them after the base row.
func TestList_PreservesStageAndAssignedSuffixes(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Item with both a delivery.stage and an assignment so we exercise
	// both suffixes in a single render pass.
	os.WriteFile(filepath.Join(root, "tasks", "T-100-suffix.md"), []byte(`id: T-100
type: task
status: active
created: 2026-04-01T10:00:00-06:00
last_touched: 2026-04-01T10:00:00-06:00

completed: null

title: Suffix demo task
assigned_to: agent-b

delivery:
  stage: pushed

depends_on:
- []

next_actions:
- []
`), 0644)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := List(s, cfg, ListOpts{})
	w.Close()
	os.Stdout = old
	if code != 0 {
		t.Fatalf("List exit=%d, want 0", code)
	}
	data, _ := io.ReadAll(r)
	out := string(data)

	// (stage) and [assigned] must both survive the migration to the
	// shared formatter — they're list-specific decorations appended
	// after the FormatItemRow output.
	if !strings.Contains(out, "(pushed)") {
		t.Errorf("List output missing (pushed) stage suffix:\n%s", out)
	}
	if !strings.Contains(out, "[agent-b]") {
		t.Errorf("List output missing [agent-b] assigned suffix:\n%s", out)
	}
	// And T-100 itself must still be in the output (the row rendered at all).
	if !strings.Contains(out, "T-100") {
		t.Errorf("List output missing T-100 row:\n%s", out)
	}
}
