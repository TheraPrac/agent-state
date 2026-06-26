package command

import (
	"os"
	"testing"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
)

// TestFullLifecycle exercises the complete item lifecycle:
// create → start → update → dep add → tag → close
// This is the session 3 gate test.
func TestFullLifecycle(t *testing.T) {
	// Setup
	root := setupTestEnvRoot(t)
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	// Set agent identity
	os.Setenv("AS_AGENT_ID", "test-agent")
	defer os.Unsetenv("AS_AGENT_ID")

	// === Step 1: Create a task ===
	code := Create(s, cfg, "task", "Lifecycle test task", CreateOpts{Priority: 1})
	if code != 0 {
		t.Fatalf("Create returned %d", code)
	}

	item, ok := s.Get("T-005")
	if !ok {
		t.Fatal("T-005 should exist after create")
	}
	if item.Status != "queued" {
		t.Errorf("after create: status = %q, want queued", item.Status)
	}
	if item.Title != "Lifecycle test task" {
		t.Errorf("after create: title = %q", item.Title)
	}

	// === Step 2: Create a second task for dependency testing ===
	code = Create(s, cfg, "task", "Dependency target", CreateOpts{Priority: 2})
	if code != 0 {
		t.Fatalf("Create second returned %d", code)
	}
	_, ok = s.Get("T-006")
	if !ok {
		t.Fatal("T-006 should exist after create")
	}

	// === Step 3: Create an issue with priority (I-406; severity is dead) ===
	code = Create(s, cfg, "issue", "Bug found", CreateOpts{Priority: 1})
	if code != 0 {
		t.Fatalf("Create issue returned %d", code)
	}
	issue, ok := s.Get("I-002")
	if !ok {
		t.Fatal("I-002 should exist after create")
	}
	if issue.Priority == nil || *issue.Priority != 1 {
		t.Errorf("issue priority = %v, want 1", issue.Priority)
	}

	// === Step 4: Add dependency (T-005 depends on T-006) ===
	code = DepAdd(s, cfg, "T-005", "T-006")
	if code != 0 {
		t.Fatalf("DepAdd returned %d", code)
	}

	item, _ = s.Get("T-005")
	if len(item.DependsOn) == 0 {
		t.Fatal("T-005 should depend on T-006")
	}
	found := false
	for _, d := range item.DependsOn {
		if d == "T-006" {
			found = true
		}
	}
	if !found {
		t.Errorf("T-005 depends_on = %v, want T-006 in list", item.DependsOn)
	}

	// Verify reciprocal
	dep, _ := s.Get("T-006")
	found = false
	for _, b := range dep.Blocks {
		if b == "T-005" {
			found = true
		}
	}
	if !found {
		t.Errorf("T-006 blocks = %v, want T-005 in list", dep.Blocks)
	}

	// === Step 5: Try to start T-005 (should be blocked) ===
	code = Start(s, cfg, "T-005", StartOpts{})
	if code != 1 {
		t.Errorf("Start blocked item returned %d, want 1", code)
	}

	// === Step 6: Start T-006 (dependency target) ===
	code = Start(s, cfg, "T-006", StartOpts{})
	if code != 0 {
		t.Fatalf("Start T-006 returned %d", code)
	}

	item6, _ := s.Get("T-006")
	if item6.Status != "active" {
		t.Errorf("T-006 status = %q, want active", item6.Status)
	}

	// === Step 7: Close T-006 to unblock T-005 ===
	code = Close(s, cfg, "T-006", "done", CloseOpts{})
	if code != 0 {
		t.Fatalf("Close T-006 returned %d", code)
	}

	item6, _ = s.Get("T-006")
	if item6.Status != "done" {
		t.Errorf("T-006 status = %q, want done", item6.Status)
	}

	// === Step 8: Now start T-005 (should succeed, dep resolved) ===
	code = Start(s, cfg, "T-005", StartOpts{})
	if code != 0 {
		t.Fatalf("Start T-005 returned %d", code)
	}

	item, _ = s.Get("T-005")
	if item.Status != "active" {
		t.Errorf("T-005 status = %q, want active", item.Status)
	}

	// === Step 9: Update a field ===
	code = Update(s, cfg, "T-005", "title", "Updated lifecycle task", UpdateModeValue)
	if code != 0 {
		t.Fatalf("Update returned %d", code)
	}

	// Verify via document (Update modifies doc, not model struct)
	item, _ = s.Get("T-005")
	docTitle, _ := item.Doc.GetField("title")
	if docTitle != "Updated lifecycle task" {
		t.Errorf("doc title after update = %q", docTitle)
	}

	// === Step 10: Add tags ===
	code = Tag(s, cfg, "T-005", "add", "lifecycle")
	if code != 0 {
		t.Fatalf("Tag add returned %d", code)
	}
	code = Tag(s, cfg, "T-005", "add", "session-3")
	if code != 0 {
		t.Fatalf("Tag add second returned %d", code)
	}

	item, _ = s.Get("T-005")
	if len(item.Tags) != 2 {
		t.Errorf("tags = %v, want 2 tags", item.Tags)
	}

	// === Step 11: Remove a tag ===
	code = Tag(s, cfg, "T-005", "rm", "session-3")
	if code != 0 {
		t.Fatalf("Tag rm returned %d", code)
	}

	item, _ = s.Get("T-005")
	if len(item.Tags) != 1 || item.Tags[0] != "lifecycle" {
		t.Errorf("tags after rm = %v, want [lifecycle]", item.Tags)
	}

	// === Step 12: Remove dependency ===
	code = DepRm(s, cfg, "T-005", "T-006")
	if code != 0 {
		t.Fatalf("DepRm returned %d", code)
	}

	item, _ = s.Get("T-005")
	for _, d := range item.DependsOn {
		if d == "T-006" {
			t.Error("T-006 should be removed from depends_on")
		}
	}

	// === Step 13: Close T-005 ===
	code = Close(s, cfg, "T-005", "done", CloseOpts{})
	if code != 0 {
		t.Fatalf("Close T-005 returned %d", code)
	}

	item, _ = s.Get("T-005")
	if item.Status != "done" {
		t.Errorf("final status = %q, want done", item.Status)
	}

	// === Step 14: Verify changelog ===
	entries, err := changelog.Read(cfg, "T-005")
	if err != nil {
		t.Fatalf("Read changelog: %v", err)
	}

	// Expected operations: create, dep_add, start, update, tag_add, tag_add, tag_rm, dep_rm, close
	expectedOps := []string{"create", "dep_add", "start", "update", "tag_add", "tag_add", "tag_rm", "dep_rm", "close"}
	if len(entries) != len(expectedOps) {
		t.Errorf("changelog has %d entries, want %d", len(entries), len(expectedOps))
		for i, e := range entries {
			t.Logf("  [%d] %s", i, e.Format())
		}
	} else {
		for i, e := range entries {
			if e.Op != expectedOps[i] {
				t.Errorf("changelog[%d].Op = %q, want %q", i, e.Op, expectedOps[i])
			}
		}
	}

	// === Step 15: View the log ===
	code = Log(s, cfg, "T-005", LogOpts{})
	if code != 0 {
		t.Errorf("Log returned %d, want 0", code)
	}

	// === Step 16: View log for all items ===
	code = Log(s, cfg, "", LogOpts{})
	if code != 0 {
		t.Errorf("Log all returned %d, want 0", code)
	}

	t.Log("Full lifecycle test passed: create → dep add → start (blocked) → close dep → start → update → tag add/rm → dep rm → close → verify changelog")
}

// TestLifecycleIssueWithPriority tests the issue-specific flow with the
// I-406 unified priority field (severity is dead).
func TestLifecycleIssueWithPriority(t *testing.T) {
	root := setupTestEnvRoot(t)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	// Create issue with priority (no longer takes severity).
	code := Create(s, cfg, "issue", "Security vulnerability", CreateOpts{
		Priority: 0,
	})
	if code != 0 {
		t.Fatalf("Create issue returned %d", code)
	}

	// Start it
	code = Start(s, cfg, "I-002", StartOpts{})
	if code != 0 {
		t.Fatalf("Start issue returned %d", code)
	}

	// Close as resolved
	code = Close(s, cfg, "I-002", "done", CloseOpts{})
	if code != 0 {
		t.Fatalf("Close issue returned %d", code)
	}

	item, _ := s.Get("I-002")
	if item.Status != "done" {
		t.Errorf("issue status = %q, want done", item.Status)
	}
}

// TestLifecycleAbandon tests the abandon flow.
func TestLifecycleAbandon(t *testing.T) {
	root := setupTestEnvRoot(t)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	// Create and start
	Create(s, cfg, "task", "Will abandon", CreateOpts{Priority: 2})
	Start(s, cfg, "T-005", StartOpts{})

	// Abandon with reason
	code := Close(s, cfg, "T-005", "abandoned", CloseOpts{Reason: "out-of-strategy"})
	if code != 0 {
		t.Fatalf("Close abandoned returned %d", code)
	}

	item, _ := s.Get("T-005")
	if item.Status != "abandoned" {
		t.Errorf("status = %q, want abandoned", item.Status)
	}

	// Verify changelog records the reason
	entries, _ := changelog.Read(cfg, "T-005")
	var foundClose bool
	for _, e := range entries {
		if e.Op == "close" {
			if e.Reason != "out-of-strategy" {
				t.Errorf("close changelog reason = %q, want out-of-strategy", e.Reason)
			}
			foundClose = true
		}
	}
	if !foundClose {
		t.Error("no close changelog entry found")
	}
}
