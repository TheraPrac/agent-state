package command

import (
	"bytes"
	"strings"
	"testing"
)

func TestGoalConsistencyDiffCleanExitsZero(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	// Add T-001 to G-001's must_do and to T-001's goals — fully in sync.
	if rc := GoalMustDoAdd(s, cfg, "G-001", "", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}

	var buf bytes.Buffer
	rc := goalConsistencyCheckTo(&buf, s, cfg)
	if rc != 0 {
		t.Errorf("rc=%d, want 0 (clean state)\noutput: %s", rc, buf.String())
	}
	if strings.Contains(buf.String(), "DRIFT") {
		t.Errorf("expected no DRIFT, got:\n%s", buf.String())
	}
}

func TestGoalConsistencyDiffReportsDrift(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	s := reloadStoreGoal(t, cfg)

	// T-001 in must_do but not in T-001.Goals → drift case 1.
	if rc := GoalMustDoAdd(s, cfg, "G-001", "", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	// T-002.Goals references G-001 but T-002 not in must_do → drift case 2.
	if rc := ItemGoalsAdd(s, cfg, "T-002", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}

	var buf bytes.Buffer
	rc := goalConsistencyCheckTo(&buf, s, cfg)
	if rc == 0 {
		t.Errorf("rc=0, want 1 (drift found)")
	}
	out := buf.String()
	if !strings.Contains(out, "DRIFT") {
		t.Errorf("expected DRIFT header, got:\n%s", out)
	}
	if !strings.Contains(out, "T-001") {
		t.Errorf("expected T-001 in drift report, got:\n%s", out)
	}
	if !strings.Contains(out, "T-002") {
		t.Errorf("expected T-002 in drift report, got:\n%s", out)
	}
}

func TestGoalConsistencySkipsInactiveGoals(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	// draft goal — consistency check must ignore it entirely.
	seedGoalFile(t, cfg, "G-002", "draft", 30)
	seedTaskInGoalEnv(t, cfg, "T-003", "queued")
	s := reloadStoreGoal(t, cfg)

	// Add T-003 to must_do of draft goal — would look like drift if we
	// checked draft goals, but we must not.
	if rc := GoalMustDoAdd(s, cfg, "G-002", "", []string{"T-003"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}

	var buf bytes.Buffer
	rc := goalConsistencyCheckTo(&buf, s, cfg)
	if rc != 0 {
		t.Errorf("rc=%d, want 0 — draft goal should be skipped\noutput: %s", rc, buf.String())
	}
}
