package changelog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".as"), 0755)
	os.MkdirAll(filepath.Join(root, ".changelog"), 0755)
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestAppendAndRead(t *testing.T) {
	cfg := testCfg(t)

	entry := Entry{
		Timestamp: "2026-03-25T10:00:00-06:00",
		Agent:     "agent-a",
		Op:        "create",
		Field:     "status",
		NewValue:  "queued",
	}

	if err := Append(cfg, "T-001", entry); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Append a second entry
	entry2 := Entry{
		Timestamp: "2026-03-25T11:00:00-06:00",
		Agent:     "agent-a",
		Op:        "update",
		Field:     "status",
		OldValue:  "queued",
		NewValue:  "active",
	}
	if err := Append(cfg, "T-001", entry2); err != nil {
		t.Fatalf("Append second: %v", err)
	}

	entries, err := Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Op != "create" {
		t.Errorf("first entry op = %q, want create", entries[0].Op)
	}
	if entries[1].OldValue != "queued" {
		t.Errorf("second entry old = %q, want queued", entries[1].OldValue)
	}
}

func TestReadNonexistent(t *testing.T) {
	cfg := testCfg(t)
	entries, err := Read(cfg, "T-999")
	if err != nil {
		t.Fatalf("Read nonexistent: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

func TestReadAll(t *testing.T) {
	cfg := testCfg(t)

	Append(cfg, "T-001", Entry{Op: "create", Timestamp: "2026-03-25T10:00:00-06:00"})
	Append(cfg, "T-002", Entry{Op: "create", Timestamp: "2026-03-25T10:00:00-06:00"})
	Append(cfg, "T-001", Entry{Op: "start", Timestamp: "2026-03-25T11:00:00-06:00"})

	all, err := ReadAll(cfg)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("got %d items, want 2", len(all))
	}
	if len(all["T-001"]) != 2 {
		t.Errorf("T-001 has %d entries, want 2", len(all["T-001"]))
	}
	if len(all["T-002"]) != 1 {
		t.Errorf("T-002 has %d entries, want 1", len(all["T-002"]))
	}
}

func TestReadAllEmptyDir(t *testing.T) {
	cfg := testCfg(t)
	all, err := ReadAll(cfg)
	if err != nil {
		t.Fatalf("ReadAll empty: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("got %d items, want 0", len(all))
	}
}

func TestReadAllNoDir(t *testing.T) {
	cfg := testCfg(t)
	// Remove the changelog dir
	os.RemoveAll(cfg.ChangelogDir())

	all, err := ReadAll(cfg)
	if err != nil {
		t.Fatalf("ReadAll no dir: %v", err)
	}
	if all != nil {
		t.Errorf("expected nil, got %v", all)
	}
}

func TestAppendAutoTimestamp(t *testing.T) {
	cfg := testCfg(t)
	entry := Entry{Op: "create"} // no timestamp
	if err := Append(cfg, "T-001", entry); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries, _ := Read(cfg, "T-001")
	if len(entries) != 1 {
		t.Fatal("expected 1 entry")
	}
	if entries[0].Timestamp == "" {
		t.Error("expected auto-generated timestamp")
	}
}

func TestAppendCreatesDir(t *testing.T) {
	cfg := testCfg(t)
	// Remove changelog dir to test auto-creation
	os.RemoveAll(cfg.ChangelogDir())

	entry := Entry{Op: "create", Timestamp: "2026-03-25T10:00:00-06:00"}
	if err := Append(cfg, "T-001", entry); err != nil {
		t.Fatalf("Append should create dir: %v", err)
	}

	entries, _ := Read(cfg, "T-001")
	if len(entries) != 1 {
		t.Error("expected 1 entry after dir creation")
	}
}

func TestSnapshotDedup(t *testing.T) {
	cfg := testCfg(t)

	// First snapshot: should be written.
	if _, err := Snapshot(cfg, "T-001", "plan_review", "content-v1"); err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	entries, _ := Read(cfg, "T-001")
	if len(entries) != 1 {
		t.Fatalf("after first snapshot: got %d entries, want 1", len(entries))
	}

	// Identical content: should be deduped (no write).
	if _, err := Snapshot(cfg, "T-001", "plan_review", "content-v1"); err != nil {
		t.Fatalf("duplicate Snapshot: %v", err)
	}
	entries, _ = Read(cfg, "T-001")
	if len(entries) != 1 {
		t.Errorf("after duplicate snapshot: got %d entries, want 1 (dedup should skip)", len(entries))
	}

	// Different content: should be written.
	if _, err := Snapshot(cfg, "T-001", "plan_review", "content-v2"); err != nil {
		t.Fatalf("changed Snapshot: %v", err)
	}
	entries, _ = Read(cfg, "T-001")
	if len(entries) != 2 {
		t.Errorf("after changed snapshot: got %d entries, want 2", len(entries))
	}
}

func TestSnapshotDedupDifferentField(t *testing.T) {
	cfg := testCfg(t)
	// Same content but different field names must both be written.
	Snapshot(cfg, "T-001", "plan_review", "same-content")
	Snapshot(cfg, "T-001", "plan_approve", "same-content")
	entries, _ := Read(cfg, "T-001")
	if len(entries) != 2 {
		t.Errorf("different fields with same content: got %d entries, want 2", len(entries))
	}
}

func TestAppendSizeGuard(t *testing.T) {
	cfg := testCfg(t)

	// Pre-seed the log file with more than sizeGuardBytes of data.
	path := filepath.Join(cfg.ChangelogDir(), "T-001.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Write(make([]byte, sizeGuardBytes+1)); err != nil {
		t.Fatalf("write seed data: %v", err)
	}
	f.Close()

	sizeBefore, _ := os.Stat(path)

	// Append should be a no-op (size guard fires).
	if err := Append(cfg, "T-001", Entry{Op: "update", Field: "title", NewValue: "x"}); err != nil {
		t.Fatalf("Append under size guard returned error: %v", err)
	}

	sizeAfter, _ := os.Stat(path)
	if sizeAfter.Size() != sizeBefore.Size() {
		t.Errorf("size guard did not block write: before=%d after=%d", sizeBefore.Size(), sizeAfter.Size())
	}
}

func TestEntryFormat(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{
			"create",
			Entry{Timestamp: "2026-03-25T10:00:00-06:00", Agent: "agent-a", Op: "create", Field: "status", NewValue: "queued"},
			"2026-03-25T10:00:00  [agent-a]  create  status: queued",
		},
		{
			"update with old+new",
			Entry{Timestamp: "2026-03-25T11:00:00-06:00", Op: "update", Field: "status", OldValue: "queued", NewValue: "active"},
			"2026-03-25T11:00:00  update  status: queued → active",
		},
		{
			"remove",
			Entry{Timestamp: "2026-03-25T12:00:00-06:00", Op: "tag_rm", Field: "tags", OldValue: "alpha"},
			"2026-03-25T12:00:00  tag_rm  tags: (removed alpha)",
		},
		{
			"with reason",
			Entry{Timestamp: "2026-03-25T13:00:00-06:00", Op: "close", Reason: "no longer needed"},
			"2026-03-25T13:00:00  close  — no longer needed",
		},
		{
			"field only",
			Entry{Timestamp: "2026-03-25T14:00:00-06:00", Op: "update", Field: "title"},
			"2026-03-25T14:00:00  update  title",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.entry.Format()
			if got != tt.want {
				t.Errorf("Format():\ngot:  %q\nwant: %q", got, tt.want)
			}
		})
	}
}
