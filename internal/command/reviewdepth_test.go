package command

import (
	"testing"
)

// --- parseDiffStat ---

func TestParseDiffStat_FilesAndLines(t *testing.T) {
	stat := " internal/command/foo.go | 30 ++++++++++++\n internal/command/bar.go | 10 ++++------\n 2 files changed, 24 insertions(+), 10 deletions(-)"
	files, lines := parseDiffStat(stat)
	if files != 2 {
		t.Errorf("files: got %d, want 2", files)
	}
	if lines != 34 {
		t.Errorf("lines: got %d, want 34 (24+10)", lines)
	}
}

func TestParseDiffStat_InsertionsOnly(t *testing.T) {
	stat := " internal/command/new.go | 45 +++++++++++++++++++++++++++++++++++++++++++++\n 1 file changed, 45 insertions(+)"
	files, lines := parseDiffStat(stat)
	if files != 1 {
		t.Errorf("files: got %d, want 1", files)
	}
	if lines != 45 {
		t.Errorf("lines: got %d, want 45", lines)
	}
}

func TestParseDiffStat_DeletionsOnly(t *testing.T) {
	stat := " foo.go | 5 -----\n 1 file changed, 5 deletions(-)"
	files, lines := parseDiffStat(stat)
	if files != 1 || lines != 5 {
		t.Errorf("got (%d, %d), want (1, 5)", files, lines)
	}
}

func TestParseDiffStat_Empty(t *testing.T) {
	files, lines := parseDiffStat("")
	if files != 0 || lines != 0 {
		t.Errorf("got (%d, %d), want (0, 0)", files, lines)
	}
}

// --- computeDepth ---

func TestComputeDepth_SmallReturnsLow(t *testing.T) {
	got := computeDepth(2, 30, []string{"internal/command/foo.go"})
	if got != "low" {
		t.Errorf("got %q, want %q", got, "low")
	}
}

func TestComputeDepth_ExactSmallBoundaryReturnsLow(t *testing.T) {
	// exactly at the small thresholds (3 files, 50 lines) → still "low"
	got := computeDepth(3, 50, []string{"cmd/as/app.go"})
	if got != "low" {
		t.Errorf("got %q, want %q", got, "low")
	}
}

func TestComputeDepth_LargeLineCountReturnsHigh(t *testing.T) {
	got := computeDepth(4, 250, []string{"internal/command/foo.go"})
	if got != "high" {
		t.Errorf("got %q, want %q", got, "high")
	}
}

func TestComputeDepth_LargeFileCountReturnsHigh(t *testing.T) {
	got := computeDepth(7, 40, []string{"internal/command/foo.go"})
	if got != "high" {
		t.Errorf("got %q, want %q", got, "high")
	}
}

func TestComputeDepth_BlastRadiusDBReturnsHigh(t *testing.T) {
	got := computeDepth(1, 10, []string{"db/changelog/001-init.sql"})
	if got != "high" {
		t.Errorf("got %q, want %q", got, "high")
	}
}

func TestComputeDepth_BlastRadiusAuthReturnsHigh(t *testing.T) {
	got := computeDepth(1, 5, []string{"internal/auth/middleware.go"})
	if got != "high" {
		t.Errorf("got %q, want %q", got, "high")
	}
}

func TestComputeDepth_BlastRadiusHooksReturnsHigh(t *testing.T) {
	got := computeDepth(1, 8, []string{"claude-config/hooks/pre-pr.sh"})
	if got != "high" {
		t.Errorf("got %q, want %q", got, "high")
	}
}

func TestComputeDepth_BlastRadiusWorkflowsReturnsHigh(t *testing.T) {
	got := computeDepth(1, 5, []string{".github/workflows/ci.yml"})
	if got != "high" {
		t.Errorf("got %q, want %q", got, "high")
	}
}

func TestComputeDepth_BlastRadiusInfraReturnsHigh(t *testing.T) {
	got := computeDepth(2, 20, []string{"theraprac-infra/terraform/main.tf"})
	if got != "high" {
		t.Errorf("got %q, want %q", got, "high")
	}
}

func TestComputeDepth_BlastRadiusAnsibleReturnsHigh(t *testing.T) {
	got := computeDepth(1, 5, []string{"ansible/roles/app/tasks/main.yml"})
	if got != "high" {
		t.Errorf("got %q, want %q", got, "high")
	}
}

func TestComputeDepth_MediumReturns(t *testing.T) {
	got := computeDepth(5, 80, []string{"internal/command/foo.go"})
	if got != "medium" {
		t.Errorf("got %q, want %q", got, "medium")
	}
}

func TestComputeDepth_JustOverSmallCeilingReturnsMedium(t *testing.T) {
	// 51 lines, 3 files — one line over the small ceiling
	got := computeDepth(3, 51, []string{"internal/command/foo.go"})
	if got != "medium" {
		t.Errorf("got %q, want %q", got, "medium")
	}
}

func TestComputeDepth_NilPathsNoBlastRadius(t *testing.T) {
	got := computeDepth(2, 30, nil)
	if got != "low" {
		t.Errorf("got %q, want %q", got, "low")
	}
}

// --- ReviewDepth — missing item ---

func TestReviewDepth_MissingItemReturnsNonZero(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := ReviewDepth(s, cfg, "I-MISSING", ReviewDepthOpts{})
	if code == 0 {
		t.Error("expected non-zero exit for missing item")
	}
}
