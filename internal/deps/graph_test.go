package deps

import (
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

func testItems() map[string]*model.Item {
	p1 := 1
	p2 := 2
	return map[string]*model.Item{
		"T-001": {ID: "T-001", Type: "task", Status: "completed", Title: "Done task", Priority: &p1},
		"T-002": {ID: "T-002", Type: "task", Status: "queued", Title: "Ready task", Priority: &p1, DependsOn: []string{"T-001"}},
		"T-003": {ID: "T-003", Type: "task", Status: "queued", Title: "Blocked task", Priority: &p2, DependsOn: []string{"T-002"}},
		"T-004": {ID: "T-004", Type: "task", Status: "queued", Title: "Independent task", Priority: &p2},
		"T-005": {ID: "T-005", Type: "task", Status: "active", Title: "Active task", AssignedTo: "agent-a"},
	}
}

func TestBuild(t *testing.T) {
	g := Build(testItems(), config.Defaults())

	// T-002 depends on T-001
	deps := g.BlockedBy("T-002")
	if len(deps) != 1 || deps[0] != "T-001" {
		t.Errorf("T-002 BlockedBy = %v, want [T-001]", deps)
	}

	// T-001 blocks T-002
	blocks := g.BlocksItems("T-001")
	if len(blocks) != 1 || blocks[0] != "T-002" {
		t.Errorf("T-001 Blocks = %v, want [T-002]", blocks)
	}
}

func TestIsResolved(t *testing.T) {
	g := Build(testItems(), config.Defaults())

	if !g.IsResolved("T-001") {
		t.Error("T-001 (completed) should be resolved")
	}
	if g.IsResolved("T-002") {
		t.Error("T-002 (queued) should not be resolved")
	}
	if g.IsResolved("T-999") {
		t.Error("T-999 (missing) should not be resolved")
	}
}

func TestIsBlocked(t *testing.T) {
	g := Build(testItems(), config.Defaults())

	if g.IsBlocked("T-002") {
		t.Error("T-002 should not be blocked (T-001 is completed)")
	}
	if !g.IsBlocked("T-003") {
		t.Error("T-003 should be blocked (T-002 is queued)")
	}
	if g.IsBlocked("T-004") {
		t.Error("T-004 should not be blocked (no deps)")
	}
}

func TestUnresolvedDeps(t *testing.T) {
	g := Build(testItems(), config.Defaults())

	unresolved := g.UnresolvedDeps("T-003")
	if len(unresolved) != 1 || unresolved[0] != "T-002" {
		t.Errorf("T-003 unresolved = %v, want [T-002]", unresolved)
	}

	unresolved = g.UnresolvedDeps("T-002")
	if len(unresolved) != 0 {
		t.Errorf("T-002 unresolved = %v, want empty", unresolved)
	}
}

func TestReady(t *testing.T) {
	g := Build(testItems(), config.Defaults())

	ready := g.Ready()

	// Should include T-002 (unblocked, queued) and T-004 (no deps, queued)
	// Should NOT include T-003 (blocked), T-001 (completed), T-005 (active/assigned)
	ids := make([]string, len(ready))
	for i, item := range ready {
		ids[i] = item.ID
	}

	if len(ready) != 2 {
		t.Fatalf("Ready = %v, want 2 items", ids)
	}

	// T-002 has priority 1, T-004 has priority 2 — T-002 should be first
	if ready[0].ID != "T-002" {
		t.Errorf("first ready = %s, want T-002 (priority 1)", ready[0].ID)
	}
	if ready[1].ID != "T-004" {
		t.Errorf("second ready = %s, want T-004 (priority 2)", ready[1].ID)
	}
}

func TestDetectCyclesNone(t *testing.T) {
	g := Build(testItems(), config.Defaults())
	cycles := g.DetectCycles()
	if len(cycles) != 0 {
		t.Errorf("expected no cycles, got %v", cycles)
	}
}

func TestDetectCyclesFound(t *testing.T) {
	items := map[string]*model.Item{
		"T-001": {ID: "T-001", Type: "task", Status: "queued", DependsOn: []string{"T-002"}},
		"T-002": {ID: "T-002", Type: "task", Status: "queued", DependsOn: []string{"T-003"}},
		"T-003": {ID: "T-003", Type: "task", Status: "queued", DependsOn: []string{"T-001"}},
	}

	g := Build(items, config.Defaults())
	cycles := g.DetectCycles()
	if len(cycles) == 0 {
		t.Error("expected cycle, got none")
	}
}

func TestTree(t *testing.T) {
	g := Build(testItems(), config.Defaults())
	tree := g.Tree("T-003", 5)

	if tree == "" {
		t.Error("tree should not be empty")
	}
	// Should contain T-003 and its dependency T-002 and T-002's dep T-001
	if !containsStr(tree, "T-003") || !containsStr(tree, "T-002") || !containsStr(tree, "T-001") {
		t.Errorf("tree missing expected IDs:\n%s", tree)
	}
}

func TestIsStageAtOrPast(t *testing.T) {
	tests := []struct {
		stage, target string
		want          bool
	}{
		{"merged", "merged", true},
		{"deployed_dev", "merged", true},
		{"coding", "merged", false},
		{"pr_open", "merged", false},
		{"unknown", "merged", false},
	}
	for _, tt := range tests {
		got := isStageAtOrPast(tt.stage, tt.target)
		if got != tt.want {
			t.Errorf("isStageAtOrPast(%q, %q) = %v, want %v", tt.stage, tt.target, got, tt.want)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
