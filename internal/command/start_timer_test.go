package command

import (
	"testing"
	"time"
)

// TestStartWritesSessionStartedAt verifies that st start writes
// time_tracking.session_started_at alongside the existing started_at.
func TestStartWritesSessionStartedAt(t *testing.T) {
	s, cfg := setupTestEnv(t)

	before := time.Now()
	if code := Start(s, cfg, "T-001", StartOpts{NoPush: true}); code != 0 {
		t.Fatalf("Start returned %d, want 0", code)
	}
	after := time.Now()

	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after start")
	}

	sessStart, ok := getNestedField(item, "time_tracking", "session_started_at")
	if !ok || sessStart == "" {
		t.Fatal("session_started_at not written by st start")
	}

	t0, err := time.Parse(time.RFC3339, sessStart)
	if err != nil {
		t.Fatalf("session_started_at %q is not valid RFC3339: %v", sessStart, err)
	}
	if t0.Before(before.Add(-time.Second)) || t0.After(after.Add(time.Second)) {
		t.Errorf("session_started_at %v is outside [%v, %v]", t0, before, after)
	}
}
