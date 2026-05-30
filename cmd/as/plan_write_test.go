package main

// Tests for `st plan write` and `st plan write --self-approve`.
// I-917 (write command) and I-1092 (--self-approve flag).
//
// All tests use the in-process harness (runInProcess /
// runInProcessCapturingStderr) against a temporary workspace so they
// stay fast and hermetic.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validPlanBody returns a minimal plan body that satisfies
// quality.ValidatePlan (Approach non-empty, scope_repos present,
// at least one verifiable AC).
func validPlanBody() string {
	return `---
scope_repos: [as]
---

## Approach
Add the PlanWrite command to internal/command/plan.go and wire it
under planCmd in cmd/as/app.go.

## Acceptance criteria
- cmd: go build ./...
- cmd: go vet ./...
`
}

// itemWithSBAR returns the YAML content of a task that satisfies the
// SBAR substance gate (quality.ValidateSBAR). Used by tests that
// exercise the --self-approve path, which runs PlanApprove and
// therefore triggers the SBAR gate.
func itemWithSBAR(id string) string {
	return `id: ` + id + `
type: task
status: queued
created: 2026-01-01T00:00:00Z
last_touched: 2026-01-01T00:00:00Z
title: Test task with SBAR

priority: 2

sbar:
  situation: Observable symptom for the plan-write test fixture.
  background: In-process test in cmd/as/plan_write_test.go; no production path.
  assessment: Standard test fixture; validates PlanWrite acceptance flow.
  recommendation: Keep fixture stable; supply real SBAR for production items.
`
}

// setupPlanWriteWorkspace creates a workspace with a task that has a
// full SBAR (required for the --self-approve path) and returns the
// workspace root path.
func setupPlanWriteWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"tasks", "issues", "archive", ".as", "templates", ".plans"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	configContent := `project:
  name: test-project
paths:
  root: .
  templates: templates
  changelog: .changelog
  index: index.md
git:
  auto_commit: false
  auto_push: false
`
	if err := os.WriteFile(filepath.Join(dir, ".as", "config.yaml"), []byte(configContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks", "T-001-plan-write-test.md"), []byte(itemWithSBAR("T-001")), 0644); err != nil {
		t.Fatalf("write item: %v", err)
	}
	return dir
}

// TestPlanWriteCreatesFile checks that `st plan write T-001` (with a
// plan body piped via a temp file) creates .plans/T-001.md.
func TestPlanWriteCreatesFile(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	planFile := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(planFile, []byte(validPlanBody()), 0644); err != nil {
		t.Fatalf("write plan file: %v", err)
	}

	stdout, code := runInProcess(t, ws, "plan", "write", "T-001", "--file", planFile)
	if code != 0 {
		t.Fatalf("plan write exit %d; stdout: %q", code, stdout)
	}
	if !strings.Contains(stdout, "Wrote plan for T-001") {
		t.Errorf("expected 'Wrote plan for T-001' in stdout: %q", stdout)
	}

	sidecar := filepath.Join(ws, ".plans", "T-001.md")
	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("sidecar not created at %s: %v", sidecar, err)
	}
	if !strings.Contains(string(data), "Approach") {
		t.Errorf("sidecar missing Approach section: %q", string(data))
	}
}

// TestPlanWriteRejectsUnknownItem checks that `st plan write T-999`
// exits 1 when the item does not exist.
func TestPlanWriteRejectsUnknownItem(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	planFile := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(planFile, []byte(validPlanBody()), 0644)

	_, code := runInProcess(t, ws, "plan", "write", "T-999", "--file", planFile)
	if code == 0 {
		t.Error("expected non-zero exit for unknown item")
	}
}

// TestPlanWriteRejectsEmptyBody checks that `st plan write` with an
// empty plan file exits 1 with a clear error.
func TestPlanWriteRejectsEmptyBody(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	emptyFile := filepath.Join(t.TempDir(), "empty.md")
	os.WriteFile(emptyFile, []byte(""), 0644)

	_, code := runInProcess(t, ws, "plan", "write", "T-001", "--file", emptyFile)
	if code == 0 {
		t.Error("expected non-zero exit for empty body")
	}
}

// TestPlanWriteWithoutSelfApproveDoesNotApprove checks that writing a
// plan without --self-approve leaves PlanApproved=false.
func TestPlanWriteWithoutSelfApproveDoesNotApprove(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	planFile := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(planFile, []byte(validPlanBody()), 0644)

	_, code := runInProcess(t, ws, "plan", "write", "T-001", "--file", planFile)
	if code != 0 {
		t.Fatalf("plan write exit %d", code)
	}

	// plan check should report not approved
	_, checkCode := runInProcess(t, ws, "plan", "check", "T-001")
	if checkCode == 0 {
		t.Error("expected plan check to return non-zero (not approved) without --self-approve")
	}
}

// TestPlanWriteSelfApproveSucceeds checks that a valid plan body +
// valid SBAR results in PlanApproved=true when --self-approve is set.
func TestPlanWriteSelfApproveSucceeds(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	planFile := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(planFile, []byte(validPlanBody()), 0644)

	stdout, code := runInProcess(t, ws, "plan", "write", "T-001", "--file", planFile, "--self-approve")
	if code != 0 {
		t.Fatalf("plan write --self-approve exit %d; stdout: %q", code, stdout)
	}

	// After --self-approve, plan check must pass (exit 0).
	checkOut, checkCode := runInProcess(t, ws, "plan", "check", "T-001")
	if checkCode != 0 {
		t.Errorf("plan check failed after --self-approve (exit %d): %q", checkCode, checkOut)
	}
	if !strings.Contains(checkOut, "approved") {
		t.Errorf("plan check output missing 'approved': %q", checkOut)
	}
}

// TestPlanWriteSelfApproveFailsBadPlan checks that a plan body with
// no Approach causes --self-approve to exit non-zero and NOT approve.
func TestPlanWriteSelfApproveFailsBadPlan(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	badPlan := `---
scope_repos: [as]
---

## Acceptance criteria
- cmd: go build ./...
`
	planFile := filepath.Join(t.TempDir(), "bad_plan.md")
	os.WriteFile(planFile, []byte(badPlan), 0644)

	_, code := runInProcess(t, ws, "plan", "write", "T-001", "--file", planFile, "--self-approve")
	if code == 0 {
		t.Error("expected non-zero exit when plan is missing Approach")
	}

	// Item must NOT be approved after a failed self-approve.
	_, checkCode := runInProcess(t, ws, "plan", "check", "T-001")
	if checkCode == 0 {
		t.Error("item should not be approved after failed --self-approve")
	}
}

// TestPlanWriteRefusesAlreadyApprovedItem checks that st plan write exits 1
// when the item is already approved, to prevent the approval stamp from
// silently certifying a replacement plan body.
func TestPlanWriteRefusesAlreadyApprovedItem(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	planFile := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(planFile, []byte(validPlanBody()), 0644)

	// First write + self-approve to get the item into approved state.
	_, code := runInProcess(t, ws, "plan", "write", "T-001", "--file", planFile, "--self-approve")
	if code != 0 {
		t.Fatalf("initial plan write --self-approve exit %d", code)
	}

	// Second write on the now-approved item should be refused.
	_, code = runInProcess(t, ws, "plan", "write", "T-001", "--file", planFile)
	if code == 0 {
		t.Error("expected non-zero exit when overwriting an approved item's plan without st plan reset")
	}
}

// TestPlanWriteSelfApproveRefusesAlreadyApprovedItem checks that
// --self-approve also refuses on an already-approved item (preventing the
// idempotent-guard bypass that would skip all static gates on the new body).
func TestPlanWriteSelfApproveRefusesAlreadyApprovedItem(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	planFile := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(planFile, []byte(validPlanBody()), 0644)

	_, code := runInProcess(t, ws, "plan", "write", "T-001", "--file", planFile, "--self-approve")
	if code != 0 {
		t.Fatalf("initial write exit %d", code)
	}

	// Second --self-approve on the same item should fail too.
	_, code = runInProcess(t, ws, "plan", "write", "T-001", "--file", planFile, "--self-approve")
	if code == 0 {
		t.Error("expected non-zero exit: --self-approve on already-approved item must be refused (not silently bypass gates)")
	}
}

// TestPlanWriteStampsLinkedPlans checks that writing a plan without
// --self-approve still stamps linked_plans on the item.
func TestPlanWriteStampsLinkedPlans(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	planFile := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(planFile, []byte(validPlanBody()), 0644)

	_, code := runInProcess(t, ws, "plan", "write", "T-001", "--file", planFile)
	if code != 0 {
		t.Fatalf("plan write exit %d", code)
	}

	// st plan show should reference the sidecar path.
	showOut, showCode := runInProcess(t, ws, "plan", "show", "T-001")
	if showCode != 0 {
		t.Fatalf("plan show exit %d", showCode)
	}
	if !strings.Contains(showOut, "T-001") {
		t.Errorf("plan show missing T-001 reference: %q", showOut)
	}
}

// TestPlanWriteStdinPiped checks that plan body delivered via stdin (not
// --file) is read and written correctly. It swaps os.Stdin with a pipe
// pre-loaded with the plan body, mirroring the os.Stdout swap used by
// runInProcess.
func TestPlanWriteStdinPiped(t *testing.T) {
	ws := setupPlanWriteWorkspace(t)

	// Swap os.Stdin for a pipe containing the plan body.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })

	// Write plan body into the pipe write-end and close it so io.ReadAll
	// sees EOF rather than blocking.
	if _, err := w.WriteString(validPlanBody()); err != nil {
		t.Fatalf("writing to pipe: %v", err)
	}
	w.Close()

	// No --file: command must read from the swapped stdin.
	stdout, code := runInProcess(t, ws, "plan", "write", "T-001")
	if code != 0 {
		t.Fatalf("plan write via stdin exit %d; stdout: %q", code, stdout)
	}
	if !strings.Contains(stdout, "Wrote plan for T-001") {
		t.Errorf("expected 'Wrote plan for T-001' in stdout: %q", stdout)
	}

	sidecar := filepath.Join(ws, ".plans", "T-001.md")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar not created: %v", err)
	}
}
