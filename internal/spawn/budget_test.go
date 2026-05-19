package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realCoordinatorYAML mirrors the live theraprac-workspace/.as/
// coordinator.yaml structure — including a same-named `per_item` under
// a DIFFERENT parent so the test proves the parser matches the full
// nested path, not just the leaf key.
const realCoordinatorYAML = `# Coordinator autonomy boundary
escalation:
  respawn_limit:        3              # B1/C2
  budget_cap_usd:
    per_item:           40             # D1 — the value we want
    per_objective:      150            # D1
  stuck_multiplier:     3
  parallelism_cap:      4
  blast_radius_gate:
    auto_approve_when:
      - files_changed <= 5
    else: escalate
  tripwire_list:
    - prod_infra_apply

decoy:
  budget_cap_usd:
    per_item:           999            # MUST NOT be picked

dedupe:
  window_minutes:       30
`

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestSpawnBudgetParsesPerItem(t *testing.T) {
	got, err := ParsePerItemBudget(writeYAML(t, realCoordinatorYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 40 {
		t.Fatalf("got %v, want 40 (must match escalation.budget_cap_usd.per_item, not the decoy 999)", got)
	}
}

func TestSpawnBudgetFloatValue(t *testing.T) {
	got, err := ParsePerItemBudget(writeYAML(t,
		"escalation:\n  budget_cap_usd:\n    per_item: 12.5\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 12.5 {
		t.Fatalf("got %v, want 12.5", got)
	}
}

func TestSpawnBudgetInlineComment(t *testing.T) {
	got, err := ParsePerItemBudget(writeYAML(t,
		"escalation:\n  budget_cap_usd:\n    per_item:    7   #  ≈ tiny live cap\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 7 {
		t.Fatalf("got %v, want 7 (inline comment must be stripped)", got)
	}
}

func TestSpawnBudgetMissingFile(t *testing.T) {
	_, err := ParsePerItemBudget(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("expected error for missing coordinator.yaml, got nil")
	}
	if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "§11") {
		t.Fatalf("error should cite the missing boundary + §11, got %q", err)
	}
}

func TestSpawnBudgetMissingKey(t *testing.T) {
	_, err := ParsePerItemBudget(writeYAML(t,
		"escalation:\n  respawn_limit: 3\n  budget_cap_usd:\n    per_objective: 150\n"))
	if err == nil {
		t.Fatal("expected error for missing per_item key, got nil")
	}
	if !strings.Contains(err.Error(), "per_item") {
		t.Fatalf("error should name the missing key, got %q", err)
	}
}

func TestSpawnBudgetUnparseable(t *testing.T) {
	_, err := ParsePerItemBudget(writeYAML(t,
		"escalation:\n  budget_cap_usd:\n    per_item: notanumber\n"))
	if err == nil {
		t.Fatal("expected error for non-numeric per_item, got nil")
	}
	if !strings.Contains(err.Error(), "not a number") {
		t.Fatalf("error should explain it is not a number, got %q", err)
	}
}

func TestSpawnBudgetNonPositiveRejected(t *testing.T) {
	for _, v := range []string{"0", "-5"} {
		_, err := ParsePerItemBudget(writeYAML(t,
			"escalation:\n  budget_cap_usd:\n    per_item: "+v+"\n"))
		if err == nil {
			t.Fatalf("per_item=%s must be rejected (no uncapped worker)", v)
		}
		if !strings.Contains(err.Error(), "> 0") {
			t.Fatalf("error should require > 0, got %q", err)
		}
	}
}

func TestSpawnBudgetEmptyValue(t *testing.T) {
	_, err := ParsePerItemBudget(writeYAML(t,
		"escalation:\n  budget_cap_usd:\n    per_item:\n    per_objective: 150\n"))
	if err == nil {
		t.Fatal("expected error for empty per_item value, got nil")
	}
}

func TestCoordinatorYAMLPath(t *testing.T) {
	got := CoordinatorYAMLPath("/ws")
	if want := filepath.Join("/ws", ".as", "coordinator.yaml"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
