package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
)

func setupFixEnv(t *testing.T) (*store.Store, *config.Config, string) {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}

	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Task missing blocks field
	writeFile(t, filepath.Join(root, "tasks", "T-001-needs-blocks.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Task missing blocks
depends_on:
- []
`)

	// Task with slug-format dependency
	writeFile(t, filepath.Join(root, "tasks", "T-002-slug-deps.md"), `id: T-002
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Task with slug deps
depends_on:
- T-001-needs-blocks
blocks:
- []
`)

	// Issue missing severity, depends_on, blocks
	writeFile(t, filepath.Join(root, "issues", "I-001-bare.md"), `id: I-001
type: issue
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Bare issue
`)

	// Write an index that's missing items
	writeFile(t, filepath.Join(root, "index.md"), "# Agent State Index\ngenerated: auto\n\n## Active Work\n(none)\n")

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s, cfg, root
}

func TestFixRequiredFields(t *testing.T) {
	s, cfg, root := setupFixEnv(t)

	fixed := fixRequiredFields(s, cfg)
	if fixed == 0 {
		t.Error("expected fixes to be applied")
	}

	// Re-read T-001 and check blocks was inserted
	content, err := os.ReadFile(filepath.Join(root, "tasks", "T-001-needs-blocks.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "blocks:") {
		t.Error("expected blocks field to be inserted in T-001")
	}

	// I-406: severity is no longer required on issues. The fix sweep
	// now only inserts depends_on + blocks for issues.
	content, err = os.ReadFile(filepath.Join(root, "issues", "I-001-bare.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"depends_on:", "blocks:"} {
		if !strings.Contains(string(content), field) {
			t.Errorf("expected %s to be inserted in I-001", field)
		}
	}
}

func TestFixStaleDeps(t *testing.T) {
	s, cfg, root := setupFixEnv(t)

	fixed := fixStaleDeps(s, cfg)
	if fixed == 0 {
		t.Error("expected slug deps to be fixed")
	}

	// Re-read T-002 and check the slug was normalized
	content, err := os.ReadFile(filepath.Join(root, "tasks", "T-002-slug-deps.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "T-001-needs-blocks") {
		t.Error("slug dep should have been normalized to T-001")
	}
	if !strings.Contains(string(content), "T-001") {
		t.Error("expected bare T-001 dep in T-002")
	}
}

func TestFixIndex(t *testing.T) {
	s, cfg, root := setupFixEnv(t)

	fixed := fixIndex(s, cfg)
	if fixed == 0 {
		t.Error("expected index to be regenerated")
	}

	// Re-read index.md and check items are listed
	content, err := os.ReadFile(filepath.Join(root, "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "T-001") {
		t.Error("expected T-001 in regenerated index")
	}
}

func TestFixFull(t *testing.T) {
	s, cfg, _ := setupFixEnv(t)

	fixed := Fix(s, cfg)
	if fixed == 0 {
		t.Error("expected some fixes")
	}
}

// TestFixLegacyAliases covers Bug A from I-562: items whose status is a
// known pre-I-433 alias must be auto-rewritten to the current vocabulary
// so st check converges without requiring a manual guard bypass. The
// alias map lives in internal/validate/vocab_suggest.go — this test
// covers every entry there to catch coverage drift if a new alias is
// added but the rewrite path forgets to inherit it (regression of the
// PR-79 review finding).
func TestFixLegacyAliases(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// One fixture per known legacy alias. Issues use open/resolved/wontfix;
	// tasks use completed (the pre-I-433 task-side alias for `done`).
	cases := []struct {
		path       string
		typeKind   string
		oldStatus  string
		wantStatus string
	}{
		{"issues/I-001-legacy-open.md", "issue", "open", "queued"},
		{"issues/I-002-legacy-resolved.md", "issue", "resolved", "done"},
		{"issues/I-003-legacy-wontfix.md", "issue", "wontfix", "abandoned"},
		{"tasks/T-001-legacy-completed.md", "task", "completed", "done"},
	}
	for _, c := range cases {
		body := "id: " + strings.TrimSuffix(strings.Split(c.path, "/")[1], ".md")[:5] + `
type: ` + c.typeKind + `
status: ` + c.oldStatus + `
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Legacy ` + c.oldStatus + ` fixture
depends_on:
- []
blocks:
- []
`
		writeFile(t, filepath.Join(root, c.path), body)
	}

	// Negative case: already on current vocabulary, must not be rewritten.
	writeFile(t, filepath.Join(root, "issues", "I-099-already-queued.md"), `id: I-099
type: issue
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Issue already on current vocabulary
depends_on:
- []
blocks:
- []
`)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	fixed := fixLegacyAliases(s, cfg)
	if fixed != len(cases) {
		t.Errorf("expected %d alias rewrites, got %d", len(cases), fixed)
	}

	for _, c := range cases {
		content, err := os.ReadFile(filepath.Join(root, c.path))
		if err != nil {
			t.Fatal(err)
		}
		want := "status: " + c.wantStatus
		if !strings.Contains(string(content), want) {
			t.Errorf("%s should have %q, got:\n%s", c.path, want, string(content))
		}
		stale := "status: " + c.oldStatus
		if strings.Contains(string(content), stale) {
			t.Errorf("%s should not still contain %q, got:\n%s", c.path, stale, string(content))
		}
	}

	// Already-current item must be untouched.
	content, err := os.ReadFile(filepath.Join(root, "issues", "I-099-already-queued.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "status: queued") {
		t.Errorf("I-099 should still have status: queued, got:\n%s", string(content))
	}

	// Idempotence: a second pass produces zero rewrites.
	fixed2 := fixLegacyAliases(s, cfg)
	if fixed2 != 0 {
		t.Errorf("second pass should produce 0 rewrites, got %d", fixed2)
	}
}

func TestCheckWithFix(t *testing.T) {
	s, cfg, _ := setupFixEnv(t)

	// Run check with fix=true — should apply fixes and then report remaining
	code := Check(s, cfg, true, true)
	_ = code // may still have issues (reciprocal deps, etc.) but shouldn't crash
}

func TestFixableSummary(t *testing.T) {
	s, cfg, _ := setupFixEnv(t)

	count, descs := FixableSummary(s, cfg)
	if count == 0 {
		t.Error("expected fixable issues")
	}
	if len(descs) == 0 {
		t.Error("expected descriptions of fixable issues")
	}
}

func TestNormalizeDeps(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []string
		changed bool
	}{
		{
			"slug format normalized",
			[]string{"T-013-subscription-billing", "T-014-client-design"},
			[]string{"T-013", "T-014"},
			true,
		},
		{
			"bare IDs unchanged",
			[]string{"T-013", "T-014"},
			[]string{"T-013", "T-014"},
			false,
		},
		{
			"mixed",
			[]string{"T-013", "T-014-some-slug"},
			[]string{"T-013", "T-014"},
			true,
		},
		{
			"empty",
			nil,
			nil,
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, changed := normalizeDeps(tt.input)
			if changed != tt.changed {
				t.Errorf("changed = %v, want %v", changed, tt.changed)
			}
			if tt.want != nil {
				if len(result) != len(tt.want) {
					t.Fatalf("len = %d, want %d", len(result), len(tt.want))
				}
				for i := range result {
					if result[i] != tt.want[i] {
						t.Errorf("result[%d] = %q, want %q", i, result[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestIsSlugID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"T-013-subscription-billing", true},
		{"I-038-staff-role", true},
		{"T-013", false},
		{"I-001", false},
		{"hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := isSlugID(tt.id); got != tt.want {
				t.Errorf("isSlugID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
