package command

import (
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/evidence"
	"github.com/jfinlinson/agent-state/internal/store"
)

func setupUATTestEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupPRTestEnv(t) // active T-003, testing config

	// Give T-003 test evidence and manifest
	item, _ := s.Get("T-003")
	setNestedField(item, "testing_evidence", "api_unit", "pass abc123 2026-03-27T10:00:00-06:00 evidence:s3://bucket/log.txt")
	setNestedField(item, "testing_evidence", "api_lint", "pass abc123 2026-03-27T10:00:00-06:00")
	setNestedField(item, "manifest", "prs", "api#42")
	setNestedField(item, "delivery", "stage", "pr_open")
	item.AcceptanceCriteria = []string{
		"API unit tests pass",
		"PR manifest recorded",
		"cmd:echo hello",
	}
	s.Write(item)

	return s, cfg
}

func TestUATBasicReport(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	opts := UATOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return []byte("hello\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := UAT(s, cfg, "T-003", opts)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 32768)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if code != 0 {
		t.Fatalf("UAT returned %d, want 0\noutput: %s", code, output)
	}

	// Should contain report sections
	if !strings.Contains(output, "UAT Report") {
		t.Error("missing UAT Report header")
	}
	if !strings.Contains(output, "AUTOMATED CHECKS") {
		t.Error("missing AUTOMATED CHECKS section")
	}
	if !strings.Contains(output, "ACCEPTANCE CRITERIA") {
		t.Error("missing ACCEPTANCE CRITERIA section")
	}
	if !strings.Contains(output, "SUMMARY") {
		t.Error("missing SUMMARY section")
	}
}

func TestUATAutoChecksPass(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return []byte("ok"), 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := UAT(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("UAT returned %d, want 0 (all auto checks should pass)", code)
	}
}

func TestUATMissingEvidence(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	// Remove test evidence
	item, _ := s.Get("T-003")
	setNestedField(item, "testing_evidence", "api_unit", "null")
	s.Write(item)

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return []byte("ok"), 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := UAT(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("UAT returned %d, want 1 (missing evidence should fail)", code)
	}
}

func TestUATCmdCriterionPass(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	opts := UATOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			if strings.Contains(cmd, "echo hello") {
				return []byte("hello\n"), 0, nil
			}
			return nil, 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := UAT(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("UAT returned %d", code)
	}
}

func TestUATCmdCriterionFail(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	// Change cmd criterion to something that fails
	item, _ := s.Get("T-003")
	item.AcceptanceCriteria = []string{"cmd:exit 1"}
	s.Write(item)

	opts := UATOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return []byte("failed\n"), 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := UAT(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("UAT returned %d, want 1 (cmd failed)", code)
	}
}

func TestUATManualCriteria(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	item, _ := s.Get("T-003")
	item.AcceptanceCriteria = []string{"User can see the modal"}
	s.Write(item)

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return nil, 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	// Prose ACs without cmd: prefix are pending (manual review), not auto-fail.
	// They don't block the pipeline — they show as warnings.
	code := UAT(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("UAT returned %d, want 0 (manual ACs are pending, not blocking)", code)
	}
}

func TestUATItemNotFound(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	opts := UATOpts{Backend: &evidence.LocalBackend{Dir: t.TempDir()}}
	code := UAT(s, cfg, "T-999", opts)
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

func TestUATNoAcceptanceCriteria(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	item, _ := s.Get("T-003")
	item.AcceptanceCriteria = nil
	s.Write(item)

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return nil, 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	// No ACs — cross-cutting checks still run
	code := UAT(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("UAT returned %d", code)
	}
}

// Ensure imports used
var _ config.Config
var _ evidence.LocalBackend
