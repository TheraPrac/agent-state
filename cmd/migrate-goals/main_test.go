package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a .md file under a fresh temp tasks/ dir and returns path.
func fixture(t *testing.T, name, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrateGoalsBackfillsAlpha1(t *testing.T) {
	path := fixture(t, "T-001-test.md", `id: T-001
type: task
status: queued
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00
title: Alpha task
priority: 2
tags:
- alpha-1
`)
	r, err := processFile(path, false)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r.Skipped {
		t.Fatal("expected file to be processed (alpha-1 tag maps to G-001)")
	}
	if len(r.GoalsAdded) != 1 || r.GoalsAdded[0] != "G-001" {
		t.Errorf("GoalsAdded = %v, want [G-001]", r.GoalsAdded)
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.Contains(content, "goals:") {
		t.Errorf("goals: block missing from rewritten file:\n%s", content)
	}
	if !strings.Contains(content, "- G-001") {
		t.Errorf("G-001 missing from goals block:\n%s", content)
	}
	// Tags must be preserved.
	if !strings.Contains(content, "alpha-1") {
		t.Errorf("alpha-1 tag should be preserved:\n%s", content)
	}
}

func TestMigrateGoalsBackfillsGoalSlug(t *testing.T) {
	path := fixture(t, "T-002-test.md", `id: T-002
type: task
status: queued
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00
title: Tooling task
priority: 2
tags:
- goal:st-tooling
`)
	r, err := processFile(path, false)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r.Skipped {
		t.Fatal("expected file to be processed (goal:st-tooling maps to G-004)")
	}
	if len(r.GoalsAdded) != 1 || r.GoalsAdded[0] != "G-004" {
		t.Errorf("GoalsAdded = %v, want [G-004]", r.GoalsAdded)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "- G-004") {
		t.Errorf("G-004 missing from goals block:\n%s", string(data))
	}
}

func TestMigrateGoalsFailsOnUnmappedGoalTag(t *testing.T) {
	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	os.MkdirAll(tasksDir, 0755)
	os.WriteFile(filepath.Join(tasksDir, "T-003-unmapped.md"), []byte(`id: T-003
type: task
status: queued
title: Unmapped
tags:
- goal:unknown-goal
`), 0644)

	// Collect unmapped tags the same way main() does.
	dirs := []string{tasksDir}
	var unmapped []string
	for _, d := range dirs {
		_ = filepath.Walk(d, func(path string, info os.FileInfo, _ error) error {
			if info == nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			for _, tag := range readTags(path) {
				if strings.HasPrefix(tag, "goal:") {
					if _, ok := tagToGoal[tag]; !ok {
						unmapped = append(unmapped, tag)
					}
				}
			}
			return nil
		})
	}
	if len(unmapped) == 0 {
		t.Fatal("expected unmapped tag to be detected")
	}
	if unmapped[0] != "goal:unknown-goal" {
		t.Errorf("unmapped[0] = %q, want goal:unknown-goal", unmapped[0])
	}
}

func TestMigrateGoalsPreservesExistingGoals(t *testing.T) {
	path := fixture(t, "T-004-test.md", `id: T-004
type: task
status: queued
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00
title: Task with pre-existing goal
priority: 2
goals:
- G-002
tags:
- alpha-1
`)
	r, err := processFile(path, false)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r.Skipped {
		t.Fatal("expected file processed (alpha-1 adds G-001)")
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	// Both G-002 (pre-existing) and G-001 (new from alpha-1) must be present.
	if !strings.Contains(content, "- G-001") {
		t.Errorf("G-001 missing from goals block:\n%s", content)
	}
	if !strings.Contains(content, "- G-002") {
		t.Errorf("G-002 (pre-existing) missing from goals block:\n%s", content)
	}
}

func TestMigrateGoalsIdempotent(t *testing.T) {
	path := fixture(t, "T-005-test.md", `id: T-005
type: task
status: queued
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00
title: Idempotent test
priority: 2
tags:
- alpha-1
`)
	// Run twice.
	if _, err := processFile(path, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	r2, err := processFile(path, false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	// Second run should be a no-op (G-001 already present).
	if !r2.Skipped && len(r2.GoalsAdded) > 0 {
		t.Errorf("second run should add nothing; GoalsAdded = %v", r2.GoalsAdded)
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	// Exactly one G-001 entry.
	count := strings.Count(content, "- G-001")
	if count != 1 {
		t.Errorf("expected exactly one '- G-001' after two runs, got %d:\n%s", count, content)
	}
}

func TestMigrateGoalsDryRunDoesNotWrite(t *testing.T) {
	path := fixture(t, "T-006-test.md", `id: T-006
type: task
status: queued
title: Dry run test
tags:
- alpha-1
`)
	original, _ := os.ReadFile(path)

	r, err := processFile(path, true /*dryRun*/)
	if err != nil {
		t.Fatalf("processFile dry-run: %v", err)
	}
	if r.Skipped {
		t.Fatal("expected non-skip in dry run for alpha-1 item")
	}
	if len(r.GoalsAdded) == 0 {
		t.Fatal("dry-run should still report what would be added")
	}

	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Error("dry-run must not modify the file")
	}
}
