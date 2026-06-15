package command

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/evidence"
)

// ---- parseFailingTests -------------------------------------------------------

func TestParseFailingTests_GoOutput(t *testing.T) {
	output := []byte("--- FAIL: TestFoo (0.00s)\n--- FAIL: TestBar/Sub (0.01s)\nFAIL\n")
	got := parseFailingTests(output)
	if len(got) != 2 || got[0] != "TestBar/Sub" || got[1] != "TestFoo" {
		t.Errorf("parseFailingTests = %v, want [TestBar/Sub TestFoo]", got)
	}
}

func TestParseFailingTests_NoFailures(t *testing.T) {
	output := []byte("=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\nok  github.com/foo 0.5s\n")
	got := parseFailingTests(output)
	if got != nil {
		t.Errorf("parseFailingTests = %v, want nil", got)
	}
}

func TestParseFailingTests_EmptyOutput(t *testing.T) {
	if got := parseFailingTests(nil); got != nil {
		t.Errorf("parseFailingTests(nil) = %v, want nil", got)
	}
}

func TestParseFailingTests_Dedup(t *testing.T) {
	output := []byte("--- FAIL: TestFoo (0.00s)\n--- FAIL: TestFoo (0.00s)\n")
	got := parseFailingTests(output)
	if len(got) != 1 || got[0] != "TestFoo" {
		t.Errorf("parseFailingTests = %v, want [TestFoo]", got)
	}
}

// ---- filterNewFailures -------------------------------------------------------

func TestFilterNewFailures_AllNew(t *testing.T) {
	newFails, preExisting := filterNewFailures([]string{"TestA", "TestB"}, nil)
	if len(newFails) != 2 || len(preExisting) != 0 {
		t.Errorf("newFails=%v preExisting=%v", newFails, preExisting)
	}
}

func TestFilterNewFailures_AllPreExisting(t *testing.T) {
	newFails, preExisting := filterNewFailures([]string{"TestA", "TestB"}, []string{"TestA", "TestB", "TestC"})
	if len(newFails) != 0 || len(preExisting) != 2 {
		t.Errorf("newFails=%v preExisting=%v", newFails, preExisting)
	}
}

func TestFilterNewFailures_Mixed(t *testing.T) {
	newFails, preExisting := filterNewFailures(
		[]string{"TestA", "TestB", "TestC"},
		[]string{"TestB", "TestC", "TestD"},
	)
	if len(newFails) != 1 || newFails[0] != "TestA" {
		t.Errorf("newFails = %v, want [TestA]", newFails)
	}
	if len(preExisting) != 2 {
		t.Errorf("preExisting = %v, want [TestB TestC]", preExisting)
	}
}

// ---- baseline roundtrip -------------------------------------------------------

func TestBaselineRoundtrip(t *testing.T) {
	s, cfg := setupTestEnv(t)
	_ = s

	b := &TestBaseline{
		Suite:        "api_unit",
		RecordedAt:   "2026-06-15T10:00:00Z",
		SHA:          "abc1234",
		FailingTests: []string{"TestFoo", "TestBar"},
	}
	if err := SaveBaseline(cfg, b); err != nil {
		t.Fatalf("SaveBaseline: %v", err)
	}

	got, err := LoadBaseline(cfg, "api_unit")
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if got == nil {
		t.Fatal("LoadBaseline returned nil, want baseline")
	}
	if got.SHA != "abc1234" || len(got.FailingTests) != 2 {
		t.Errorf("loaded = %+v", got)
	}
}

func TestLoadBaseline_Missing(t *testing.T) {
	s, cfg := setupTestEnv(t)
	_ = s

	got, err := LoadBaseline(cfg, "nonexistent_suite")
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing baseline, got %+v", got)
	}
}

// ---- integration: applyBaselineCheck in testRunMode -------------------------

// writeBaselineForTest saves a baseline to cfg's workspace via SaveBaseline.
func writeBaselineForTest(t *testing.T, cfg *config.Config, suite string, failingTests []string) {
	t.Helper()
	b := &TestBaseline{
		Suite:        suite,
		RecordedAt:   "2026-06-15T10:00:00Z",
		SHA:          "main123",
		FailingTests: failingTests,
	}
	if err := SaveBaseline(cfg, b); err != nil {
		t.Fatalf("SaveBaseline: %v", err)
	}
}

// TestTestRunMode_BaselineAllPreExisting: when all failing tests are in the
// baseline (pre-existing on main), st test --run returns 0 and records
// pass evidence with the "baseline:pre-existing/" tag. I-1474.
func TestTestRunMode_BaselineAllPreExisting(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	writeBaselineForTest(t, cfg, "api_unit", []string{"TestFoo"})

	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("--- FAIL: TestFoo (0.01s)\nFAIL\n"), 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("expected exit 0 (all pre-existing), got %d", code)
	}

	item, _ := s.Get("T-003")
	ev, ok := getNestedField(item, "testing_evidence", "api_unit")
	if !ok {
		t.Fatal("testing_evidence.api_unit not set")
	}
	if !strings.HasPrefix(ev, "pass") || !strings.Contains(ev, "baseline:pre-existing/1") {
		t.Errorf("evidence = %q, want pass ... baseline:pre-existing/1 ...", ev)
	}
}

// TestTestRunMode_BaselineNewFailure: when new failures appear beyond the
// baseline, st test --run returns 1 (regression detected). I-1474.
func TestTestRunMode_BaselineNewFailure(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	writeBaselineForTest(t, cfg, "api_unit", []string{"TestFoo"})

	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("--- FAIL: TestFoo (0.01s)\n--- FAIL: TestBar (0.02s)\nFAIL\n"), 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 1 {
		t.Fatalf("expected exit 1 (new failure: TestBar), got %d", code)
	}

	item, _ := s.Get("T-003")
	ev, _ := getNestedField(item, "testing_evidence", "api_unit")
	if !strings.HasPrefix(ev, "fail") {
		t.Errorf("evidence = %q, want fail ...", ev)
	}
}

// TestTestRunMode_NoBaseline: without a stored baseline, behavior is unchanged
// — test failure still returns 1. I-1474.
func TestTestRunMode_NoBaseline(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("--- FAIL: TestFoo (0.01s)\nFAIL\n"), 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 1 {
		t.Fatalf("expected exit 1 (no baseline), got %d", code)
	}

	item, _ := s.Get("T-003")
	ev, _ := getNestedField(item, "testing_evidence", "api_unit")
	if !strings.HasPrefix(ev, "fail") {
		t.Errorf("evidence = %q, want fail ...", ev)
	}
}
