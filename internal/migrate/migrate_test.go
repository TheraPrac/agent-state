package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/parse"
)

// testConfig returns a config with testing and delivery enabled.
func testConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit":      {Command: "make test-unit"},
			"api_lint":      {Command: "make lint"},
			"web_typecheck": {Command: "make type-check"},
			"web_unit":      {Command: "make test-unit"},
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"api_integration": {Command: "make integration"},
			"web_e2e":         {Command: "make e2e"},
		},
	}
	cfg.Delivery = &config.DeliveryConfig{
		Stages:      []string{"coding", "committed", "pushed", "pr_open", "merged", "deployed_dev", "closed"},
		ArchiveGate: "merged",
	}
	return cfg
}

// writeTestFile writes content to a temp file and returns the path.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// --- Legacy format fixture ---

const legacyFile = `id: T-024
type: task
status: queued
priority: post-mvp
created: 2026-02-08T10:00:00-07:00
last_touched: 2026-02-08T10:00:00-07:00

completed: null

resolution:
- []

work_tracking:
  branch: null
  commits: []

testing_evidence:
  required: true
  runs:
  - command: null
    scope: null
    result: null
    run_at: null
    notes: null

title: Profile-aware template seeding

summary: |
  Update clinical_templates_seed.go to seed note templates based on
  practice profile rather than seeding the same set for all tenants.

parallel_group: null

depends_on:
- []

blocks:
- T-052

related_issues:
- []

linked_plans:
- []

promotion_required:
- []

invariants:
- Template seeding must respect the practice profile.

acceptance_criteria:
- Seeding is profile-aware
- Tests cover profile-aware seeding

next_actions:
- Update clinical_templates_seed.go
`

// --- Modern format fixture ---

const modernFile = `id: T-090
type: task
status: queued
created: 2026-03-13T18:00:00-06:00
last_touched: 2026-03-13T18:00:00-06:00

completed: null

resolution:
- []

work_tracking:
  branch: null
  commits: []
  pr: []

delivery:
  stage: null
  deployed_date: null
  uat_approved_by: null
  uat_approved_date: null

testing_evidence:
  tests_written:
  - []

  required_suites:
    api_unit:      null
    api_lint:      null
    web_typecheck: null
    web_unit:      null

  scope_suites:
    api_integration: null
    web_e2e:         null

  notes: null

title: CSRF protection for web application

summary: |
  The web application needs CSRF protection.

priority: alpha-critical
category: security
repo: web

depends_on:
- []

related_issues:
- []

doc_changes:
- []

acceptance_criteria:
- CSRF token cookie set on initial page load
- All POST/PUT/PATCH/DELETE requests include X-CSRF-Token header

next_actions:
- Implement CSRF token generation middleware
`

// --- File with body ---

const fileWithBody = `id: T-051
type: task
status: queued
created: 2026-02-17T10:00:00-07:00
last_touched: 2026-02-17T10:00:00-07:00

completed: null

resolution:
- []

work_tracking:
  branch: null
  commits: []

testing_evidence:
  required: true
  runs:
  - command: null
    scope: null
    result: null
    run_at: null
    notes: null

title: Note-level diagnosis workflow

depends_on:
- []

blocks:
- T-052

parallel_group: null

linked_plans:
- []

promotion_required:
- []

next_actions:
- DB migration

---

## Design

### Data Model

Three-level diagnosis relationship.
`

// --- File with non-empty linked_plans and parallel_group ---

const fileWithValues = `id: T-006
type: task
status: queued
created: 2026-02-08T10:00:00-07:00
last_touched: 2026-02-08T10:00:00-07:00

completed: null

resolution:
- []

work_tracking:
  branch: null
  commits: []

testing_evidence:
  required: true
  runs:
  - command: null
    scope: null
    result: null
    run_at: null
    notes: null

title: Diagnosis support

parallel_group: B

depends_on:
- []

blocks:
- []

linked_plans:
- theraprac-workspace/.cursor/plans/diagnosis_support.plan.md

promotion_required:
- []

next_actions:
- Start work
`

// --- File with populated suite values ---

const modernWithResults = `id: T-084
type: task
status: queued
created: 2026-03-11T16:30:00-06:00
last_touched: 2026-03-11T16:30:00-06:00

completed: null

resolution:
- []

work_tracking:
  branch: null
  commits: []

testing_evidence:
  tests_written:
  - []

  required_suites:
    api_unit:      "skip: workspace-only change"
    api_lint:      "skip: workspace-only change"
    web_typecheck: "skip: workspace-only change"
    web_unit:      "skip: workspace-only change"

  scope_suites:
    api_integration: "skip: workspace-only change"
    web_e2e:         "skip: workspace-only change"

  notes: null

title: Generate index.md from item files

depends_on:
- T-136

blocks:
- []

next_actions:
- Defer to T-136
`

func TestExtractSections(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-024.md", legacyFile)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	sections := extractSections(item.Doc)

	// Should have all top-level keys
	expected := []string{
		"id", "type", "status", "priority", "created", "last_touched",
		"completed", "resolution", "work_tracking", "testing_evidence",
		"title", "summary", "parallel_group", "depends_on", "blocks",
		"related_issues", "linked_plans", "promotion_required",
		"invariants", "acceptance_criteria", "next_actions",
	}
	for _, key := range expected {
		if _, ok := sections[key]; !ok {
			t.Errorf("missing section: %s", key)
		}
	}

	// work_tracking should include nested lines
	wt := sections["work_tracking"]
	if len(wt.lines) != 3 {
		t.Errorf("work_tracking lines: got %d, want 3", len(wt.lines))
	}

	// testing_evidence should include all nested content
	te := sections["testing_evidence"]
	if len(te.lines) < 5 {
		t.Errorf("testing_evidence lines: got %d, want >= 5", len(te.lines))
	}
}

func TestExtractBody(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-051.md", fileWithBody)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	body := extractBody(item.Doc)
	if len(body) == 0 {
		t.Fatal("expected body lines, got none")
	}
	if body[0] != "---" {
		t.Errorf("body[0]: got %q, want %q", body[0], "---")
	}
	// Should contain design content
	found := false
	for _, line := range body {
		if strings.Contains(line, "Data Model") {
			found = true
			break
		}
	}
	if !found {
		t.Error("body should contain 'Data Model'")
	}
}

func TestExtractBodyNone(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-024.md", legacyFile)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	body := extractBody(item.Doc)
	if len(body) != 0 {
		t.Errorf("expected no body, got %d lines", len(body))
	}
}

func TestCanonical_LegacyFormat(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-024.md", legacyFile)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	canonical := Canonical(item, cfg)

	// Should have modern testing_evidence
	if !strings.Contains(canonical, "required_suites:") {
		t.Error("expected required_suites in canonical output")
	}
	if !strings.Contains(canonical, "scope_suites:") {
		t.Error("expected scope_suites in canonical output")
	}
	if strings.Contains(canonical, "runs:") {
		t.Error("legacy runs: should be removed")
	}
	if strings.Contains(canonical, "required: true") {
		t.Error("legacy required: field should be removed")
	}

	// Should have delivery section
	if !strings.Contains(canonical, "delivery:") {
		t.Error("expected delivery section")
	}

	// Should NOT have blocks
	if strings.Contains(canonical, "\nblocks:") {
		t.Error("blocks should be removed")
	}

	// Should NOT have promotion_required
	if strings.Contains(canonical, "promotion_required") {
		t.Error("promotion_required should be removed")
	}

	// Should NOT have parallel_group: null
	if strings.Contains(canonical, "parallel_group") {
		t.Error("parallel_group: null should be removed")
	}

	// Should NOT have linked_plans (was empty)
	if strings.Contains(canonical, "linked_plans") {
		t.Error("linked_plans: (empty) should be removed")
	}

	// Should have doc_changes
	if !strings.Contains(canonical, "doc_changes:") {
		t.Error("expected doc_changes: field")
	}

	// Should have work_tracking.pr
	if !strings.Contains(canonical, "  pr: []") {
		t.Error("expected pr: [] in work_tracking")
	}

	// Should convert string priority to numeric
	if !strings.Contains(canonical, "priority: 3") {
		t.Error("expected priority: 3 (converted from post-mvp)")
	}

	// Should preserve invariants
	if !strings.Contains(canonical, "invariants:") {
		t.Error("expected invariants to be preserved")
	}
}

func TestCanonical_ModernFormat(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-090.md", modernFile)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	canonical := Canonical(item, cfg)

	// Should still have all modern sections
	for _, expected := range []string{
		"required_suites:", "scope_suites:", "delivery:", "doc_changes:",
	} {
		if !strings.Contains(canonical, expected) {
			t.Errorf("expected %q in canonical output", expected)
		}
	}

	// Should preserve category, repo
	if !strings.Contains(canonical, "category: security") {
		t.Error("expected category: security")
	}
	if !strings.Contains(canonical, "repo: web") {
		t.Error("expected repo: web")
	}
}

func TestCanonical_PreservesNonEmptyLinkedPlans(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-006.md", fileWithValues)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	canonical := Canonical(item, cfg)

	// linked_plans should be preserved (non-empty)
	if !strings.Contains(canonical, "linked_plans:") {
		t.Error("linked_plans with content should be preserved")
	}
	if !strings.Contains(canonical, "diagnosis_support.plan.md") {
		t.Error("linked_plans content should be preserved")
	}

	// parallel_group B should be preserved (non-null)
	if !strings.Contains(canonical, "parallel_group: B") {
		t.Error("parallel_group: B should be preserved")
	}

	// blocks should still be removed
	if strings.Contains(canonical, "\nblocks:") {
		t.Error("blocks should be removed even when empty")
	}

	// promotion_required should be removed
	if strings.Contains(canonical, "promotion_required") {
		t.Error("promotion_required should be removed")
	}
}

func TestCanonical_BodyPreserved(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-051.md", fileWithBody)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	canonical := Canonical(item, cfg)

	// Body should be preserved
	if !strings.Contains(canonical, "---") {
		t.Error("body separator --- should be preserved")
	}
	if !strings.Contains(canonical, "## Design") {
		t.Error("body content should be preserved")
	}
	if !strings.Contains(canonical, "Three-level diagnosis relationship.") {
		t.Error("body text should be preserved")
	}
}

func TestCanonical_PreservesSuiteValues(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-084.md", modernWithResults)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	canonical := Canonical(item, cfg)

	// Suite values should be preserved (skip: workspace-only change)
	if !strings.Contains(canonical, `"skip: workspace-only change"`) {
		t.Error("suite values should be preserved with quoting")
	}
}

func TestCanonical_FieldOrder(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-024.md", legacyFile)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	canonical := Canonical(item, cfg)
	lines := strings.Split(canonical, "\n")

	// Find positions of key fields to verify order
	positions := make(map[string]int)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, key := range []string{"id:", "completed:", "resolution:", "work_tracking:", "delivery:", "testing_evidence:", "title:", "depends_on:", "next_actions:"} {
			if strings.HasPrefix(trimmed, key) {
				positions[key] = i
				break
			}
		}
	}

	// Verify canonical ordering
	order := []string{"id:", "completed:", "resolution:", "work_tracking:", "delivery:", "testing_evidence:", "title:", "depends_on:", "next_actions:"}
	for i := 1; i < len(order); i++ {
		prev := order[i-1]
		curr := order[i]
		if positions[prev] >= positions[curr] {
			t.Errorf("field order: %s (line %d) should come before %s (line %d)",
				prev, positions[prev], curr, positions[curr])
		}
	}
}

func TestPlanFile_DetectsChanges(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-024.md", legacyFile)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	result := PlanFile(item, path, cfg)

	if !result.HasChanges() {
		t.Error("legacy file should have changes")
	}

	// Should detect testing format change
	found := false
	for _, c := range result.Changes {
		if c.Type == "testing_format" {
			found = true
			break
		}
	}
	if !found {
		t.Error("should detect testing_format change")
	}

	// Should detect dropped fields
	dropTypes := map[string]bool{}
	for _, c := range result.Changes {
		if c.Type == "drop_field" {
			dropTypes[c.Detail] = true
		}
	}
	if !dropTypes["remove blocks: (computed)"] {
		t.Error("should detect blocks drop")
	}
	if !dropTypes["remove promotion_required: (unused)"] {
		t.Error("should detect promotion_required drop")
	}
}

func TestPlanFile_NoChangesForCanonical(t *testing.T) {
	dir := t.TempDir()

	// Write a file that's already canonical
	path := writeTestFile(t, dir, "T-001.md", `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

resolution:
- []

work_tracking:
  branch: null
  commits: []
  pr: []

delivery:
  stage: null
  deployed_date: null
  uat_approved_by: null
  uat_approved_date: null

testing_evidence:
  tests_written:
  - []

  required_suites:
    api_lint:      null
    api_unit:      null
    web_typecheck: null
    web_unit:      null

  scope_suites:
    api_integration: null
    web_e2e:         null

  notes: null

title: First task

depends_on:
- []

doc_changes:
- []

next_actions:
- []
`)

	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	result := PlanFile(item, path, cfg)

	if result.HasChanges() {
		// Show diff for debugging
		beforeLines := strings.Split(result.Before, "\n")
		afterLines := strings.Split(result.After, "\n")
		for i := 0; i < len(beforeLines) || i < len(afterLines); i++ {
			var bl, al string
			if i < len(beforeLines) {
				bl = beforeLines[i]
			}
			if i < len(afterLines) {
				al = afterLines[i]
			}
			if bl != al {
				t.Errorf("diff at line %d:\n  before: %q\n  after:  %q", i+1, bl, al)
			}
		}
		t.Error("already-canonical file should have no changes")
	}
}

func TestIsNullSection(t *testing.T) {
	tests := []struct {
		name string
		s    *rawSection
		want bool
	}{
		{"null value", &rawSection{key: "field", lines: []string{"field: null"}}, true},
		{"empty value", &rawSection{key: "field", lines: []string{"field:"}}, true},
		{"tilde", &rawSection{key: "field", lines: []string{"field: ~"}}, true},
		{"non-null", &rawSection{key: "field", lines: []string{"field: B"}}, false},
		{"multi-line", &rawSection{key: "field", lines: []string{"field:", "  sub: val"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNullSection(tt.s)
			if got != tt.want {
				t.Errorf("isNullSection: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsEmptyListSection(t *testing.T) {
	tests := []struct {
		name string
		s    *rawSection
		want bool
	}{
		{"empty list", &rawSection{key: "f", lines: []string{"f:", "- []"}}, true},
		{"empty list brackets", &rawSection{key: "f", lines: []string{"f:", "- [[]]"}}, true},
		{"populated", &rawSection{key: "f", lines: []string{"f:", "- item1"}}, false},
		{"key only", &rawSection{key: "f", lines: []string{"f:"}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isEmptyListSection(tt.s)
			if got != tt.want {
				t.Errorf("isEmptyListSection: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNeedsQuoting(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"simple text", false},
		{"text with: colon", true},
		{"text with # hash", true},
		{"`backtick` text", true},
		{"normal-title", false},
	}

	for _, tt := range tests {
		got := needsQuoting(tt.input)
		if got != tt.want {
			t.Errorf("needsQuoting(%q): got %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestCanonical_NoTestingConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-024.md", legacyFile)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Config without testing/delivery
	cfg := config.Defaults()
	canonical := Canonical(item, cfg)

	// Should still drop blocks, promotion_required
	if strings.Contains(canonical, "\nblocks:") {
		t.Error("blocks should be removed even without testing config")
	}
	if strings.Contains(canonical, "promotion_required") {
		t.Error("promotion_required should be removed")
	}

	// Should NOT have delivery section (no config)
	if strings.Contains(canonical, "delivery:") {
		t.Error("should not add delivery without config")
	}

	// Testing evidence should be preserved as raw (since no testing config)
	if !strings.Contains(canonical, "testing_evidence:") {
		t.Error("testing_evidence should be preserved as raw without testing config")
	}
}

func TestCanonical_RoundtripStability(t *testing.T) {
	// Parse canonical output, then canonicalize again — should be identical
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-024.md", legacyFile)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	first := Canonical(item, cfg)

	// Write canonical output, re-parse, re-canonicalize
	path2 := writeTestFile(t, dir, "T-024-canonical.md", first)
	item2, err := parse.File(path2)
	if err != nil {
		t.Fatalf("parse canonical: %v", err)
	}

	second := Canonical(item2, cfg)

	if first != second {
		firstLines := strings.Split(first, "\n")
		secondLines := strings.Split(second, "\n")
		for i := 0; i < len(firstLines) || i < len(secondLines); i++ {
			var fl, sl string
			if i < len(firstLines) {
				fl = firstLines[i]
			}
			if i < len(secondLines) {
				sl = secondLines[i]
			}
			if fl != sl {
				t.Errorf("roundtrip diff at line %d:\n  first:  %q\n  second: %q", i+1, fl, sl)
			}
		}
		t.Error("canonical output should be stable across roundtrips")
	}
}

func TestPriorityConversion(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"alpha-critical", "priority: alpha-critical", "priority: 0"},
		{"blocking", "priority: blocking", "priority: 0"},
		{"production-critical", "priority: production-critical", "priority: 1"},
		{"high", "priority: high", "priority: 1"},
		{"normal", "priority: normal", "priority: 2"},
		{"medium", "priority: medium", "priority: 2"},
		{"med", "priority: med", "priority: 2"},
		{"post-alpha", "priority: post-alpha", "priority: 3"},
		{"post-mvp", "priority: post-mvp", "priority: 3"},
		{"low", "priority: low", "priority: 4"},
		{"numeric-passthrough", "priority: 2", "priority: 2"},
		{"null-passthrough", "priority: null", "priority: null"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := fmt.Sprintf("id: T-999\ntype: task\nstatus: queued\ncreated: 2026-03-25T10:00:00-06:00\nlast_touched: 2026-03-25T10:00:00-06:00\ntitle: test\n%s\n", tt.raw)
			dir := t.TempDir()
			path := writeTestFile(t, dir, "T-999.md", content)
			item, err := parse.File(path)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			cfg := testConfigNoTesting()
			canonical := Canonical(item, cfg)
			if !strings.Contains(canonical, tt.want) {
				t.Errorf("canonical does not contain %q:\n%s", tt.want, canonical)
			}
		})
	}
}

func TestCategoryToTags(t *testing.T) {
	content := `id: T-999
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
category: security
`
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-999.md", content)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := testConfigNoTesting()
	canonical := Canonical(item, cfg)

	// Should have tags with category value
	if !strings.Contains(canonical, "tags:\n- security") {
		t.Errorf("expected tags with security, got:\n%s", canonical)
	}
	// Should still have category field
	if !strings.Contains(canonical, "category: security") {
		t.Errorf("expected category: security preserved, got:\n%s", canonical)
	}
}

func TestCategoryToTagsNoDuplicate(t *testing.T) {
	content := `id: T-999
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
category: security
tags:
- security
- hardening
`
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-999.md", content)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := testConfigNoTesting()
	canonical := Canonical(item, cfg)

	// Should not duplicate security in tags
	count := strings.Count(canonical, "- security")
	if count != 1 {
		t.Errorf("expected 1 occurrence of '- security', got %d:\n%s", count, canonical)
	}
}

func TestCategoryToTagsRoundtripStable(t *testing.T) {
	content := `id: T-999
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
category: agent-tooling
`
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-999.md", content)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := testConfigNoTesting()

	first := Canonical(item, cfg)

	// Re-parse and re-canonicalize
	path2 := writeTestFile(t, dir, "T-999-c.md", first)
	item2, err := parse.File(path2)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	second := Canonical(item2, cfg)

	if first != second {
		t.Errorf("category-to-tags not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestCanonicalOrderIncludesTaxonomy(t *testing.T) {
	content := `id: T-999
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
category: api
epic: brightly-dancing-fox
sprint: calmly-running-bear
tags:
- api
- billing
sessions:
- abc-123
`
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-999.md", content)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := testConfigNoTesting()
	canonical := Canonical(item, cfg)

	// Verify field ordering: tags, epic, sprint should appear after category
	catIdx := strings.Index(canonical, "category:")
	tagsIdx := strings.Index(canonical, "tags:")
	epicIdx := strings.Index(canonical, "epic:")
	sprintIdx := strings.Index(canonical, "sprint:")
	sessionsIdx := strings.Index(canonical, "sessions:")
	depsIdx := strings.Index(canonical, "depends_on:")

	if tagsIdx < catIdx {
		t.Error("tags should appear after category")
	}
	if epicIdx < tagsIdx {
		t.Error("epic should appear after tags")
	}
	if sprintIdx < epicIdx {
		t.Error("sprint should appear after epic")
	}
	if sessionsIdx < sprintIdx {
		t.Error("sessions should appear after sprint")
	}
	if depsIdx > 0 && sessionsIdx > depsIdx {
		t.Error("sessions should appear before depends_on")
	}
}

// testConfigNoTesting returns a config without testing/delivery for simpler tests.
func testConfigNoTesting() *config.Config {
	cfg := config.Defaults()
	return cfg
}

func TestCanonical_TitleNeedsQuoting(t *testing.T) {
	dir := t.TempDir()
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: "Title with: colon"
priority: 1
`
	path := writeTestFile(t, dir, "T-001.md", content)
	cfg := testConfig()
	item, err := parse.File(path)
	if err != nil {
		t.Fatal(err)
	}

	got := Canonical(item, cfg)
	if !strings.Contains(got, `title: "Title with: colon"`) {
		t.Errorf("title not quoted:\n%s", got)
	}
}

func TestCanonical_EmptyTitleFromSections(t *testing.T) {
	dir := t.TempDir()
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Extracted Title
priority: 1
`
	path := writeTestFile(t, dir, "T-001.md", content)
	cfg := testConfig()
	item, err := parse.File(path)
	if err != nil {
		t.Fatal(err)
	}

	got := Canonical(item, cfg)
	if !strings.Contains(got, "title: Extracted Title") {
		t.Errorf("title not found:\n%s", got)
	}
}

func TestCanonical_DocChangesNonEmpty(t *testing.T) {
	dir := t.TempDir()
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
priority: 1

doc_changes:
- "Updated README.md"
- "Added CHANGELOG.md"
`
	path := writeTestFile(t, dir, "T-001.md", content)
	cfg := testConfig()
	item, err := parse.File(path)
	if err != nil {
		t.Fatal(err)
	}

	got := Canonical(item, cfg)
	if !strings.Contains(got, "- Updated README.md") {
		t.Errorf("doc_changes not preserved:\n%s", got)
	}
}

func TestHelperFunctions(t *testing.T) {
	// formatTime with non-zero time
	ts := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	if got := formatTime(ts); got != "2026-03-25T10:00:00Z" {
		t.Errorf("formatTime = %q", got)
	}
	// formatTime with zero
	if got := formatTime(time.Time{}); got != "null" {
		t.Errorf("formatTime(zero) = %q, want null", got)
	}

	// nullOr with value
	if got := nullOr("hello"); got != "hello" {
		t.Errorf("nullOr(hello) = %q", got)
	}
	if got := nullOr(""); got != "null" {
		t.Errorf("nullOr('') = %q, want null", got)
	}
	if got := nullOr("~"); got != "null" {
		t.Errorf("nullOr('~') = %q, want null", got)
	}

	// extractScalarValue
	if got := extractScalarValue("key: value"); got != "value" {
		t.Errorf("extractScalarValue = %q", got)
	}
	if got := extractScalarValue("key: val # comment"); got != "val" {
		t.Errorf("extractScalarValue(comment) = %q", got)
	}
	if got := extractScalarValue("no-colon"); got != "" {
		t.Errorf("extractScalarValue(no colon) = %q", got)
	}

	// mapStr
	if got := mapStr(nil, "key"); got != "" {
		t.Errorf("mapStr(nil) = %q", got)
	}
	if got := mapStr(map[string]interface{}{"k": "v"}, "missing"); got != "" {
		t.Errorf("mapStr(missing) = %q", got)
	}
	if got := mapStr(map[string]interface{}{"k": 42}, "k"); got != "" {
		t.Errorf("mapStr(non-string) = %q", got)
	}
	if got := mapStr(map[string]interface{}{"k": "v"}, "k"); got != "v" {
		t.Errorf("mapStr(hit) = %q", got)
	}

	// suiteValue — no colon in value, so no quoting
	te := map[string]interface{}{"api_unit": "pass | 2026-03-25"}
	if got := suiteValue(te, "api_unit"); got != "pass | 2026-03-25" {
		t.Errorf("suiteValue = %q", got)
	}
	if got := suiteValue(te, "missing"); got != "null" {
		t.Errorf("suiteValue(missing) = %q", got)
	}
	// suiteValue — value with colon gets quoted
	te2 := map[string]interface{}{"api_unit": "pass: yes"}
	if got := suiteValue(te2, "api_unit"); got != `"pass: yes"` {
		t.Errorf("suiteValue(colon) = %q", got)
	}
}

// I-776: scope_class is a new top-level scalar emitted after priority/severity/
// category/repo/source. Round-trip: parse → canonical → re-parse → field preserved.
func TestCanonical_ScopeClassRoundTrip(t *testing.T) {
	dir := t.TempDir()
	content := `id: I-776
type: issue
status: active
created: 2026-05-23T07:00:00-06:00
last_touched: 2026-05-23T07:00:00-06:00

title: Hard gate carve-out for workspace-config items

priority: 3
scope_class: workspace-config

depends_on:
- []
`
	path := writeTestFile(t, dir, "I-776.md", content)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if item.ScopeClass != "workspace-config" {
		t.Fatalf("input parse: ScopeClass = %q, want workspace-config", item.ScopeClass)
	}

	canonical := Canonical(item, testConfig())
	if !strings.Contains(canonical, "scope_class: workspace-config") {
		t.Errorf("canonical missing scope_class line:\n%s", canonical)
	}

	// scope_class must appear AFTER priority (canonical order — see migrate.go).
	priorityIdx := strings.Index(canonical, "priority:")
	scopeIdx := strings.Index(canonical, "scope_class:")
	if priorityIdx < 0 || scopeIdx < 0 || scopeIdx < priorityIdx {
		t.Errorf("scope_class must appear after priority; priorityIdx=%d scopeIdx=%d", priorityIdx, scopeIdx)
	}

	// Re-parse to confirm round-trip fidelity.
	roundPath := writeTestFile(t, dir, "I-776-canonical.md", canonical)
	round, err := parse.File(roundPath)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if round.ScopeClass != "workspace-config" {
		t.Errorf("round-trip ScopeClass = %q", round.ScopeClass)
	}
}

// I-776: clearing a previously-set scope_class must NOT leave a noisy
// `scope_class: null` line — the field is opt-in only.
func TestCanonical_ScopeClassClearedDoesNotEmitNull(t *testing.T) {
	dir := t.TempDir()
	content := `id: I-776
type: issue
status: active
created: 2026-05-23T07:00:00-06:00
last_touched: 2026-05-23T07:00:00-06:00

title: Clear-after-set scope_class

priority: 3
scope_class: workspace-config

depends_on:
- []
`
	path := writeTestFile(t, dir, "I-776.md", content)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	item.ScopeClass = "" // clear the typed field

	canonical := Canonical(item, testConfig())
	if strings.Contains(canonical, "scope_class: null") {
		t.Errorf("canonical should NOT emit `scope_class: null` after clear:\n%s", canonical)
	}
	if strings.Contains(canonical, "scope_class:") {
		t.Errorf("canonical should NOT emit any scope_class line after clear:\n%s", canonical)
	}
}

// I-776: items with a scope_class get the class's required_suites set in
// the canonical required_suites: block (not the default api/web).
func TestCanonical_ScopeClassRequiredSuitesBlock(t *testing.T) {
	dir := t.TempDir()
	content := `id: I-776
type: issue
status: active
created: 2026-05-23T07:00:00-06:00
last_touched: 2026-05-23T07:00:00-06:00

title: t

priority: 3
scope_class: workspace-config

depends_on:
- []

testing_evidence:
  tests_written:
  - []

  workspace_test: pass abc1234 2026-05-23T07:00:00-06:00

  notes: null
`
	path := writeTestFile(t, dir, "I-776.md", content)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := testConfig()
	cfg.Testing.ScopeClasses = map[string]config.ScopeClassConfig{
		"workspace-config": {
			RequiredSuites: map[string]config.SuiteConfig{
				"workspace_test": {Command: "bash hooks/run-changed-hook-tests.sh"},
			},
		},
	}

	canonical := Canonical(item, cfg)
	rsIdx := strings.Index(canonical, "required_suites:")
	if rsIdx < 0 {
		t.Fatalf("canonical missing required_suites block:\n%s", canonical)
	}
	rsEnd := strings.Index(canonical[rsIdx:], "\n\n")
	if rsEnd < 0 {
		rsEnd = len(canonical) - rsIdx
	}
	block := canonical[rsIdx : rsIdx+rsEnd]
	if !strings.Contains(block, "workspace_test:") {
		t.Errorf("required_suites block should list workspace_test, got:\n%s", block)
	}
	for _, defaultSuite := range []string{"api_unit", "api_lint", "web_typecheck", "web_unit"} {
		if strings.Contains(block, defaultSuite+":") {
			t.Errorf("required_suites block should NOT list default-class suite %s, got:\n%s", defaultSuite, block)
		}
	}
}

// I-776: items without a scope_class must not emit a stray "scope_class:"
// line — keeps the diff clean for the 99% of items that have never declared
// a class.
func TestCanonical_ScopeClassEmptyOmitted(t *testing.T) {
	dir := t.TempDir()
	content := `id: T-099
type: task
status: queued
created: 2026-05-23T07:00:00-06:00
last_touched: 2026-05-23T07:00:00-06:00

title: An ordinary task without scope_class

priority: 2

depends_on:
- []
`
	path := writeTestFile(t, dir, "T-099.md", content)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if item.ScopeClass != "" {
		t.Fatalf("ScopeClass should be empty, got %q", item.ScopeClass)
	}

	canonical := Canonical(item, testConfig())
	if strings.Contains(canonical, "scope_class:") {
		t.Errorf("canonical should NOT contain a scope_class line:\n%s", canonical)
	}
}
