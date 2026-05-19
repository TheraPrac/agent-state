package coordinator

import (
	"os"
	"path/filepath"
	"testing"
)

// validYAML mirrors the real .as/coordinator.yaml schema (contract §11)
// closely enough that a parser regression here would also break the live
// boundary read.
const validYAML = `# Coordinator autonomy boundary
escalation:
  respawn_limit:        3              # B1/C2
  budget_cap_usd:
    per_item:           40             # D1
    per_objective:      150            # D1
  stuck_multiplier:     3              # D2
  parallelism_cap:      4              # D3
  blast_radius_gate:                   # K5 — NOT modelled by the loop
    auto_approve_when:
      - files_changed <= 5
    else: escalate
  tripwire_list:                       # E2
    - prod_infra_apply
    - aws_destructive
    - secret_or_iam_key_rotation

dedupe:
  window_minutes:       30

escalation_channel:                    # K6
  default:    alerts_band
  active_ping:
    - category_E
    - budget_cap
`

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "coordinator.yaml")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func TestLoadBoundary_Valid(t *testing.T) {
	b, err := LoadBoundary(writeTmp(t, validYAML))
	if err != nil {
		t.Fatalf("LoadBoundary: unexpected error: %v", err)
	}
	if b.RespawnLimit != 3 {
		t.Errorf("RespawnLimit = %d, want 3", b.RespawnLimit)
	}
	if b.PerItemUSD != 40 {
		t.Errorf("PerItemUSD = %v, want 40", b.PerItemUSD)
	}
	if b.PerObjectiveUSD != 150 {
		t.Errorf("PerObjectiveUSD = %v, want 150", b.PerObjectiveUSD)
	}
	if b.StuckMultiplier != 3 {
		t.Errorf("StuckMultiplier = %v, want 3", b.StuckMultiplier)
	}
	if b.ParallelismCap != 4 {
		t.Errorf("ParallelismCap = %d, want 4", b.ParallelismCap)
	}
	if b.DedupeWindowMin != 30 {
		t.Errorf("DedupeWindowMin = %d, want 30", b.DedupeWindowMin)
	}
	wantTrip := []string{"prod_infra_apply", "aws_destructive", "secret_or_iam_key_rotation"}
	if len(b.TripwireList) != len(wantTrip) {
		t.Fatalf("TripwireList = %v, want %v", b.TripwireList, wantTrip)
	}
	for i, w := range wantTrip {
		if b.TripwireList[i] != w {
			t.Errorf("TripwireList[%d] = %q, want %q", i, b.TripwireList[i], w)
		}
	}
	if !b.IsTripwire("aws_destructive") || b.IsTripwire("not_a_tripwire") {
		t.Errorf("IsTripwire wrong: %v", b.TripwireList)
	}
	wantPing := []string{"category_E", "budget_cap"}
	if len(b.ActivePingClasses) != len(wantPing) {
		t.Fatalf("ActivePingClasses = %v, want %v", b.ActivePingClasses, wantPing)
	}
	if !b.ActivePings("category_E") || b.ActivePings("category_A") {
		t.Errorf("ActivePings wrong: %v", b.ActivePingClasses)
	}
}

func TestLoadBoundary_MissingFile(t *testing.T) {
	_, err := LoadBoundary(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected hard error for missing file, got nil (would run unbounded)")
	}
}

func TestLoadBoundary_MissingRequiredKey(t *testing.T) {
	// per_objective removed → must be a hard error, not a 0 default.
	body := `escalation:
  respawn_limit: 3
  budget_cap_usd:
    per_item: 40
  stuck_multiplier: 3
  parallelism_cap: 4
dedupe:
  window_minutes: 30
`
	_, err := LoadBoundary(writeTmp(t, body))
	if err == nil {
		t.Fatal("expected hard error for missing per_objective, got nil")
	}
}

func TestLoadBoundary_NonPositive(t *testing.T) {
	for _, bad := range []string{"0", "-1"} {
		body := `escalation:
  respawn_limit: ` + bad + `
  budget_cap_usd:
    per_item: 40
    per_objective: 150
  stuck_multiplier: 3
  parallelism_cap: 4
dedupe:
  window_minutes: 30
`
		if _, err := LoadBoundary(writeTmp(t, body)); err == nil {
			t.Fatalf("respawn_limit=%s: expected hard error, got nil", bad)
		}
	}
}

func TestLoadBoundary_Unparseable(t *testing.T) {
	body := `escalation:
  respawn_limit: not-a-number
  budget_cap_usd:
    per_item: 40
    per_objective: 150
  stuck_multiplier: 3
  parallelism_cap: 4
dedupe:
  window_minutes: 30
`
	if _, err := LoadBoundary(writeTmp(t, body)); err == nil {
		t.Fatal("expected hard error for non-numeric respawn_limit, got nil")
	}
}

func TestLoadBoundary_EmptyListsAllowed(t *testing.T) {
	// No tripwires / no active_ping is a valid deliberate operator choice
	// (unlike a missing scalar). It must parse, not error.
	body := `escalation:
  respawn_limit: 3
  budget_cap_usd:
    per_item: 40
    per_objective: 150
  stuck_multiplier: 3
  parallelism_cap: 4
  tripwire_list:
dedupe:
  window_minutes: 30
escalation_channel:
  default: alerts_band
  active_ping:
`
	b, err := LoadBoundary(writeTmp(t, body))
	if err != nil {
		t.Fatalf("empty lists should be valid: %v", err)
	}
	if len(b.TripwireList) != 0 || len(b.ActivePingClasses) != 0 {
		t.Errorf("expected empty lists, got trip=%v ping=%v", b.TripwireList, b.ActivePingClasses)
	}
	if b.IsTripwire("anything") {
		t.Error("empty tripwire list must match nothing")
	}
}

// TestLoadBoundary_RealFile parses the live boundary if it is resolvable
// from the test's working dir, so a schema drift between the real file and
// this parser is caught. Skipped (not failed) when not resolvable — unit
// tests run from the agent-root checkout, not the workspace.
func TestLoadBoundary_RealFile(t *testing.T) {
	candidates := []string{
		"../../../../theraprac-workspace/.as/coordinator.yaml",
		"../../../theraprac-workspace/.as/coordinator.yaml",
	}
	var path string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			path = c
			break
		}
	}
	if path == "" {
		t.Skip("live coordinator.yaml not resolvable from test cwd")
	}
	b, err := LoadBoundary(path)
	if err != nil {
		t.Fatalf("live coordinator.yaml failed to parse — schema drift: %v", err)
	}
	if b.RespawnLimit <= 0 || b.PerItemUSD <= 0 || b.ParallelismCap <= 0 {
		t.Errorf("live boundary parsed but values implausible: %+v", b)
	}
}
