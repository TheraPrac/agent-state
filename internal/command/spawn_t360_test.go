package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/testutil"
)

// --- Spawn (T-360) — the reasoning-worker launcher ----------------------

// TestSpawnBadItemID — a non-existent id fails BEFORE any side effect
// (binary resolve, budget, start), with a clear "not found" message and
// nothing spawned. This is the induced-failure acceptance criterion.
func TestSpawnBadItemID(t *testing.T) {
	env := testutil.NewEnv(t)
	_, stderr, code := captureSpawnIO(t, func() int {
		return Spawn(env.S, env.Cfg, SpawnOpts{Item: "NOPE-999"})
	})
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "not found") {
		t.Fatalf("stderr should say not found, got %q", stderr)
	}
}

// TestSpawnRefusesUncapped — a real item with no coordinator.yaml and no
// --budget override must NOT spawn: the K1 cap is mandatory (§11). The
// error cites the boundary, and no spawn-logs dir is created.
func TestSpawnRefusesUncapped(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-b")
	env := testutil.NewEnv(t)

	_, stderr, code := captureSpawnIO(t, func() int {
		return Spawn(env.S, env.Cfg, SpawnOpts{Item: "T-001"})
	})
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "§11") {
		t.Fatalf("stderr should cite the autonomy boundary §11, got %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".as", "spawn-logs")); !os.IsNotExist(err) {
		t.Fatalf("uncapped refusal must spawn nothing — spawn-logs dir should not exist (err=%v)", err)
	}
}

// TestSpawnDryRun — the side-effect-free inspection path: resolves the
// binary, budget, cwd, prompt and prints the launch plan WITHOUT
// launching, registering, or starting the item.
func TestSpawnDryRun(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-b")
	env := testutil.NewEnv(t)
	env.Cfg.Worktree = &config.WorktreeConfig{Enabled: true, BaseDir: "worktrees"}

	exe := mkExeT(t, t.TempDir(), "claude-fake")
	t.Setenv("ST_CLAUDE_BIN", exe)

	stdout, stderr, code := captureSpawnIO(t, func() int {
		return Spawn(env.S, env.Cfg, SpawnOpts{Item: "T-001", BudgetOverride: 2.5, DryRun: true})
	})
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"DRY RUN",
		exe,
		"budget(usd): 2.5",
		"session-id:",
		"--max-budget-usd 2.5",
		"--permission-mode bypassPermissions",
		"--output-format stream-json",
		"<prompt ",
		"--- worker prompt ---",
		"You are a spawned reasoning worker for T-001",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output missing %q\n--- got ---\n%s", want, stdout)
		}
	}
	// Strictly side-effect-free.
	if _, err := os.Stat(filepath.Join(env.Root, ".as", "spawn-logs")); !os.IsNotExist(err) {
		t.Errorf("dry-run must not create spawn-logs (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(env.Root), "worktrees")); !os.IsNotExist(err) {
		t.Errorf("dry-run must not start the item / create a worktree (err=%v)", err)
	}
}

// TestSpawnPromptBuild — the worker brief carries the operating frame
// plus the item's own SBAR + acceptance criteria, and omits empty SBAR
// sub-fields rather than emitting blanks.
func TestSpawnPromptBuild(t *testing.T) {
	full := &model.Item{
		ID:    "T-360",
		Type:  "task",
		Title: "st spawn launcher",
		SBAR: model.SBAR{
			Situation:      "the linchpin is missing",
			Background:     "I-554 probe validated it",
			Assessment:     "packaging risk only",
			Recommendation: "single PR, full loop",
		},
		AcceptanceCriteria: []string{"go build green", "live-verify on throwaway item"},
	}
	p := buildWorkerPrompt(full)
	for _, want := range []string{
		"spawned reasoning worker for T-360: st spawn launcher",
		"CLAUDE.md's delivery loop",
		"coordinator.yaml",
		"operating-contract §7",
		"Situation: the linchpin is missing",
		"Background: I-554 probe validated it",
		"Assessment: packaging risk only",
		"Recommendation: single PR, full loop",
		"Acceptance criteria:",
		"- go build green",
		"- live-verify on throwaway item",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, p)
		}
	}

	bare := &model.Item{ID: "T-7", Type: "task", Title: "tiny"}
	pb := buildWorkerPrompt(bare)
	if strings.Contains(pb, "SBAR") {
		t.Errorf("empty SBAR must not emit an SBAR block:\n%s", pb)
	}
	if strings.Contains(pb, "Acceptance criteria:") {
		t.Errorf("no ACs must not emit an acceptance block:\n%s", pb)
	}
	if !strings.Contains(pb, "spawned reasoning worker for T-7: tiny") {
		t.Errorf("frame missing for bare item:\n%s", pb)
	}
}

func TestSpawnDeriveSlug(t *testing.T) {
	cases := []struct {
		item model.Item
		want string
	}{
		{model.Item{ID: "T-360", Title: "st spawn <item> — launch worker!"}, "st-spawn-item-launch-worker"},
		{model.Item{ID: "T-7", Title: ""}, "t-7"},
		{model.Item{ID: "I-9", Title: "!!!"}, "i-9"},
	}
	for _, c := range cases {
		if got := deriveSlug(&c.item); got != c.want {
			t.Errorf("deriveSlug(%q) = %q, want %q", c.item.Title, got, c.want)
		}
	}
	long := &model.Item{ID: "T-1", Title: strings.Repeat("ab cd ", 20)}
	s := deriveSlug(long)
	if len(s) > 40 {
		t.Errorf("slug too long (%d): %q", len(s), s)
	}
	if strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		t.Errorf("slug has dangling dash: %q", s)
	}
}

func TestSpawnSessionUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u, err := newSessionUUID()
		if err != nil {
			t.Fatalf("newSessionUUID: %v", err)
		}
		if len(u) != 36 {
			t.Fatalf("uuid len = %d, want 36: %q", len(u), u)
		}
		if u[8] != '-' || u[13] != '-' || u[18] != '-' || u[23] != '-' {
			t.Fatalf("uuid dash layout wrong: %q", u)
		}
		if u[14] != '4' {
			t.Fatalf("uuid version nibble = %c, want 4: %q", u[14], u)
		}
		if !strings.ContainsRune("89ab", rune(u[19])) {
			t.Fatalf("uuid variant nibble = %c, want one of 89ab: %q", u[19], u)
		}
		if seen[u] {
			t.Fatalf("duplicate uuid: %q", u)
		}
		seen[u] = true
	}
}

func TestSpawnRedactedArgs(t *testing.T) {
	args := []string{"-p", "this is a long worker prompt", "--max-budget-usd", "40"}
	got := redactedArgs(args)
	if strings.Contains(got, "long worker prompt") {
		t.Errorf("prompt should be redacted, got %q", got)
	}
	if !strings.Contains(got, "<prompt ") || !strings.Contains(got, "--max-budget-usd 40") {
		t.Errorf("redactedArgs lost structure: %q", got)
	}
}

func TestSpawnWorkerRegisterOptions(t *testing.T) {
	o := workerRegisterOptions(
		config.Identity{ID: "agent-b"}, "sid-1234", "spawner-sess", "T-360", 4242)
	if o.Role != "worker" {
		t.Errorf("Role = %q, want worker", o.Role)
	}
	if o.BaseAgentID != "agent-b" || o.ParentAgentID != "agent-b" || o.RootAgentID != "agent-b" {
		t.Errorf("identity mapping wrong: %+v", o)
	}
	if o.SessionID != "sid-1234" || o.SpawnedBySession != "spawner-sess" {
		t.Errorf("session mapping wrong: %+v", o)
	}
	if o.Scope != "item:T-360" || o.PID != 4242 {
		t.Errorf("scope/pid wrong: %+v", o)
	}

	// Explicit RootID is preserved (lineage rollup for cost attribution).
	o2 := workerRegisterOptions(
		config.Identity{ID: "agent-b-3", RootID: "agent-b"}, "s", "p", "I-1", 7)
	if o2.RootAgentID != "agent-b" {
		t.Errorf("RootAgentID = %q, want preserved agent-b", o2.RootAgentID)
	}
}

func TestSpawnWithEnv(t *testing.T) {
	// Replace in place when present.
	got := withEnv([]string{"A=1", "ST_ROOT=/old", "B=2"}, "ST_ROOT", "/new")
	want := []string{"A=1", "ST_ROOT=/new", "B=2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("replace: got %v want %v", got, want)
	}
	// Append when absent.
	got = withEnv([]string{"A=1"}, "ST_ROOT", "/r")
	if len(got) != 2 || got[1] != "ST_ROOT=/r" {
		t.Fatalf("append: got %v", got)
	}
	// Substring keys must not false-match (ST_ROOT vs ST_ROOTX).
	got = withEnv([]string{"ST_ROOTX=keepme"}, "ST_ROOT", "/r")
	if len(got) != 2 || got[0] != "ST_ROOTX=keepme" || got[1] != "ST_ROOT=/r" {
		t.Fatalf("substring guard: got %v", got)
	}
}

// mkExeT writes an executable stub (command-package-local copy of the
// spawn-package helper — test packages don't share helpers).
func mkExeT(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}
