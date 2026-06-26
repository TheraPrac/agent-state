package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// setupAgentTestCfg builds a Config rooted at a temp dir with a minimal
// .as/config.yaml so cfg.AgentsDir() resolves correctly.
func setupAgentTestCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".as"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestRegisterAndCleanup(t *testing.T) {
	cfg := setupAgentTestCfg(t)
	reg, cleanup, err := Register(cfg, Options{
		BaseAgentID: "agent-a",
		Role:        "worker",
		Scope:       "sprint:test",
		SessionID:   "sess-123",
		PID:         os.Getpid(),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.AgentID != "agent-a-1" {
		t.Errorf("AgentID = %q, want agent-a-1", reg.AgentID)
	}
	if reg.Root != "agent-a" {
		t.Errorf("Root = %q, want agent-a (defaults to BaseAgentID)", reg.Root)
	}
	if reg.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", reg.PID, os.Getpid())
	}

	// File should exist on disk.
	path := filepath.Join(cfg.AgentsDir(), reg.AgentID+".yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registration file not written: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent_id: agent-a-1", "root: agent-a", "role: worker", "scope: sprint:test", "session_id: sess-123"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("file missing %q:\n%s", want, body)
		}
	}

	// Cleanup removes the file.
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove file: %v", err)
	}
	// Idempotent: calling again is safe.
	cleanup()
}

func TestSuffixAssignment(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	r1, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if r1.AgentID != "agent-a-1" || r2.AgentID != "agent-a-2" {
		t.Errorf("expected agent-a-1/agent-a-2, got %q/%q", r1.AgentID, r2.AgentID)
	}

	// Lowest free integer fills holes: remove agent-a-1, next reg should
	// claim that slot rather than agent-a-3.
	if err := os.Remove(filepath.Join(cfg.AgentsDir(), r1.AgentID+".yaml")); err != nil {
		t.Fatal(err)
	}
	r3, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if r3.AgentID != "agent-a-1" {
		t.Errorf("expected agent-a-1 (filling hole), got %q", r3.AgentID)
	}

	// Different prefixes are independent counters.
	r4, _, err := Register(cfg, Options{BaseAgentID: "agent-b", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if r4.AgentID != "agent-b-1" {
		t.Errorf("expected agent-b-1, got %q", r4.AgentID)
	}
}

func TestStaleSweep(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	// Live registration (this process).
	live, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}

	// Manually write a "stale" registration with a guaranteed-dead PID.
	// Linux/macOS reserve PID 0; PID 999999 is typically unused. Pick a
	// large value AND verify it isn't actually live before relying on it.
	deadPID := 999999
	for IsPIDLive(deadPID) {
		deadPID++ // unlikely loop, but safe
	}
	stalePath := filepath.Join(cfg.AgentsDir(), "agent-z-7.yaml")
	if err := os.WriteFile(stalePath, []byte("agent_id: agent-z-7\nroot: agent-z\npid: 999999\nstarted: 2026-01-01T00:00:00Z\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Patch the file to use the actually-dead PID we found above.
	if deadPID != 999999 {
		body, _ := os.ReadFile(stalePath)
		patched := strings.Replace(string(body), "pid: 999999", "pid: 999998", 1)
		_ = os.WriteFile(stalePath, []byte(patched), 0644)
	}

	cleaned, err := Sweep(cfg)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(cleaned) != 1 || cleaned[0] != "agent-z-7" {
		t.Errorf("expected sweep to remove [agent-z-7], got %v", cleaned)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale file not removed: %v", err)
	}
	livePath := filepath.Join(cfg.AgentsDir(), live.AgentID+".yaml")
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("sweep removed the live registration!: %v", err)
	}
}

func TestChildAgentLineage(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	root, _, err := Register(cfg, Options{
		BaseAgentID: "agent-a",
		Role:        "worker",
		PID:         os.Getpid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if root.Parent != "" {
		t.Errorf("root agent should have no parent, got %q", root.Parent)
	}
	if root.Root != root.Parent && root.Root != "agent-a" {
		t.Errorf("root agent's Root should equal BaseAgentID for root agents, got %q", root.Root)
	}

	// Child: parent + root carry through, suffix assigned under parent prefix.
	child, _, err := Register(cfg, Options{
		BaseAgentID:      "agent-a",
		ParentAgentID:    root.AgentID, // "agent-a-1"
		RootAgentID:      root.Root,
		Role:             "child",
		SpawnedBySession: "parent-sess",
		PID:              os.Getpid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Parent != root.AgentID {
		t.Errorf("child Parent = %q, want %q", child.Parent, root.AgentID)
	}
	if child.Root != "agent-a" {
		t.Errorf("child Root = %q, want agent-a (inherited)", child.Root)
	}
	if !strings.HasPrefix(child.AgentID, root.AgentID+"-") {
		t.Errorf("child AgentID should start with %q-, got %q", root.AgentID, child.AgentID)
	}
	if child.SpawnedBySession != "parent-sess" {
		t.Errorf("SpawnedBySession lost in registration: %q", child.SpawnedBySession)
	}

	// Round-trip: load and verify lineage survives parse.
	loaded, err := LoadRegistration(cfg, child.AgentID)
	if err != nil {
		t.Fatalf("LoadRegistration: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded == nil")
	}
	if loaded.Parent != root.AgentID || loaded.Root != "agent-a" || loaded.Role != "child" {
		t.Errorf("round-trip lost fields: parent=%q root=%q role=%q", loaded.Parent, loaded.Root, loaded.Role)
	}
}

func TestLoadRegistrationMissing(t *testing.T) {
	cfg := setupAgentTestCfg(t)
	reg, err := LoadRegistration(cfg, "nope-1")
	if err != nil {
		t.Fatalf("err on missing: %v", err)
	}
	if reg != nil {
		t.Errorf("expected nil for missing file, got %+v", reg)
	}
}

func TestListRegistrationsSkipsBadFiles(t *testing.T) {
	cfg := setupAgentTestCfg(t)
	if err := os.MkdirAll(cfg.AgentsDir(), 0755); err != nil {
		t.Fatal(err)
	}
	// One good registration.
	if _, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	// One garbage file — listing must skip it without erroring.
	if err := os.WriteFile(filepath.Join(cfg.AgentsDir(), "garbage.yaml"), []byte("not yaml\nat all"), 0644); err != nil {
		t.Fatal(err)
	}
	regs, err := ListRegistrations(cfg)
	if err != nil {
		t.Fatalf("ListRegistrations: %v", err)
	}
	if len(regs) != 1 || regs[0].AgentID != "agent-a-1" {
		t.Errorf("expected 1 good reg, got %d: %+v", len(regs), regs)
	}
}

// I-404 follow-up: registration round-trips the build commit so
// `st status` can detect drift between parallel agents.
func TestRegistrationRoundTripsCommit(t *testing.T) {
	cfg := setupAgentTestCfg(t)
	reg := &Registration{
		AgentID: "agent-x-1",
		Root:    "agent-x",
		PID:     12345,
		Started: "2026-04-27T10:00:00Z",
		Commit:  "abcdef1234567890abcdef1234567890abcdef12",
	}
	path := filepath.Join(cfg.AgentsDir(), "agent-x-1.yaml")
	if err := os.MkdirAll(cfg.AgentsDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeRegistration(path, reg); err != nil {
		t.Fatalf("writeRegistration: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "commit: abcdef1234567890abcdef1234567890abcdef12") {
		t.Errorf("yaml missing commit line: %s", string(body))
	}
	got, err := parseRegistration(body)
	if err != nil {
		t.Fatalf("parseRegistration: %v", err)
	}
	if got.Commit != reg.Commit {
		t.Errorf("Commit roundtrip: got %q, want %q", got.Commit, reg.Commit)
	}
}

// Older registration files (pre-instrumentation) had no commit field.
// They must still parse cleanly with an empty Commit string so drift
// detection treats them as "<unstamped>" rather than crashing.
func TestRegistrationParsesWithoutCommit(t *testing.T) {
	body := []byte(`agent_id: agent-old-1
root: agent-old
pid: 9999
started: 2026-04-25T08:00:00Z
`)
	got, err := parseRegistration(body)
	if err != nil {
		t.Fatalf("parseRegistration: %v", err)
	}
	if got.Commit != "" {
		t.Errorf("expected empty Commit for legacy reg, got %q", got.Commit)
	}
	if got.AgentID != "agent-old-1" {
		t.Errorf("AgentID parse failed: %q", got.AgentID)
	}
}

func TestRegisterSelfAndDeregister(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	// Base-id keyed (no nextSuffix): the file is <id>.yaml exactly.
	reg, err := RegisterSelf(cfg, SelfOptions{AgentID: "agent-b", PID: 4242, SessionID: "sess-xyz"})
	if err != nil {
		t.Fatalf("RegisterSelf: %v", err)
	}
	if reg.AgentID != "agent-b" {
		t.Fatalf("AgentID = %q, want agent-b (no suffix)", reg.AgentID)
	}
	path := filepath.Join(cfg.AgentsDir(), "agent-b.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registration file not written: %v", err)
	}
	got, err := LoadRegistration(cfg, "agent-b")
	if err != nil || got == nil {
		t.Fatalf("LoadRegistration: %v / %v", got, err)
	}
	if got.PID != 4242 || got.SessionID != "sess-xyz" || got.Started == "" {
		t.Errorf("loaded reg = %+v, want pid 4242 / sess-xyz / non-empty Started", got)
	}

	// Idempotent overwrite — same file, refreshed fields, no suffix file.
	if _, err := RegisterSelf(cfg, SelfOptions{AgentID: "agent-b", PID: 9999, SessionID: "sess-new"}); err != nil {
		t.Fatalf("re-RegisterSelf: %v", err)
	}
	got, _ = LoadRegistration(cfg, "agent-b")
	if got.PID != 9999 || got.SessionID != "sess-new" {
		t.Errorf("overwrite not reflected: %+v", got)
	}
	if matches, _ := filepath.Glob(filepath.Join(cfg.AgentsDir(), "agent-b*.yaml")); len(matches) != 1 {
		t.Errorf("expected exactly one agent-b registration file, got %v", matches)
	}

	// Missing AgentID is an error (caller must resolve identity).
	if _, err := RegisterSelf(cfg, SelfOptions{}); err == nil {
		t.Error("RegisterSelf with empty AgentID should error")
	}

	// Deregister removes it; second call is a no-op (idempotent).
	if err := DeregisterSelf(cfg, "agent-b"); err != nil {
		t.Fatalf("DeregisterSelf: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("registration still present after deregister: %v", err)
	}
	if err := DeregisterSelf(cfg, "agent-b"); err != nil {
		t.Errorf("DeregisterSelf (absent) should be a no-op, got %v", err)
	}
}

func TestRegisterSelf_StartedReset(t *testing.T) {
	cfg := setupAgentTestCfg(t)
	dir := cfg.AgentsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// Seed a prior registration with a distinctly OLD Started + session
	// S1 (writeRegistration is package-private — accessible here).
	old := "2020-01-01T00:00:00Z"
	if err := writeRegistration(filepath.Join(dir, "agent-b.yaml"), &Registration{
		AgentID: "agent-b", Root: "agent-b", PID: 1, Started: old, SessionID: "S1",
	}); err != nil {
		t.Fatal(err)
	}

	// Same session → Started preserved (deterministic string equality).
	if _, err := RegisterSelf(cfg, SelfOptions{AgentID: "agent-b", PID: 2, SessionID: "S1"}); err != nil {
		t.Fatal(err)
	}
	g, _ := LoadRegistration(cfg, "agent-b")
	if g.Started != old {
		t.Errorf("same-session resume: Started = %q, want preserved %q", g.Started, old)
	}
	if g.PID != 2 {
		t.Errorf("same-session resume: PID not refreshed (%d)", g.PID)
	}

	// Different session → genuinely new run → Started recomputed. It is
	// time.Now() (2026+) which can NEVER equal the 2020 sentinel, so
	// this is fully deterministic (no same-second collision risk).
	if _, err := RegisterSelf(cfg, SelfOptions{AgentID: "agent-b", PID: 3, SessionID: "S2"}); err != nil {
		t.Fatal(err)
	}
	g, _ = LoadRegistration(cfg, "agent-b")
	if g.Started == old {
		t.Errorf("new session must reset Started, but kept the %q sentinel", old)
	}
	if g.SessionID != "S2" {
		t.Errorf("new session not recorded: %q", g.SessionID)
	}
}

func TestRegistration_GoalFocusRoundTrip(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	// Write a registration with GoalFocus set, then read it back.
	path := filepath.Join(cfg.AgentsDir(), "agent-a.yaml")
	if err := os.MkdirAll(cfg.AgentsDir(), 0755); err != nil {
		t.Fatal(err)
	}
	reg := &Registration{AgentID: "agent-a", Root: "agent-a", PID: 1, Started: "2026-01-01T00:00:00Z", GoalFocus: "G-001"}
	if err := writeRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRegistration(cfg, "agent-a")
	if err != nil {
		t.Fatalf("LoadRegistration: %v", err)
	}
	if got.GoalFocus != "G-001" {
		t.Errorf("GoalFocus = %q, want G-001", got.GoalFocus)
	}

	// Empty GoalFocus must not write the key at all.
	reg.GoalFocus = ""
	if err := writeRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "goal_focus") {
		t.Errorf("empty GoalFocus must not be written: %s", body)
	}
}

func TestRegisterSelf_PreservesGoalFocus(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	// Prime a registration with a goal focus.
	if err := SetGoalFocus(cfg, "agent-a", "G-002"); err != nil {
		t.Fatal(err)
	}

	// RegisterSelf for a NEW session must still carry the focus forward.
	if _, err := RegisterSelf(cfg, SelfOptions{AgentID: "agent-a", PID: 99, SessionID: "new-session"}); err != nil {
		t.Fatal(err)
	}
	got, _ := LoadRegistration(cfg, "agent-a")
	if got.GoalFocus != "G-002" {
		t.Errorf("RegisterSelf must preserve GoalFocus across sessions; got %q", got.GoalFocus)
	}
}

func TestSetClearGetGoalFocus(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	if got := GetGoalFocus(cfg, "agent-a"); got != "" {
		t.Errorf("GetGoalFocus before set = %q, want empty", got)
	}

	if err := SetGoalFocus(cfg, "agent-a", "G-003"); err != nil {
		t.Fatal(err)
	}
	if got := GetGoalFocus(cfg, "agent-a"); got != "G-003" {
		t.Errorf("GetGoalFocus after set = %q, want G-003", got)
	}

	if err := ClearGoalFocus(cfg, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if got := GetGoalFocus(cfg, "agent-a"); got != "" {
		t.Errorf("GetGoalFocus after clear = %q, want empty", got)
	}

	// Clear is idempotent when already empty.
	if err := ClearGoalFocus(cfg, "agent-a"); err != nil {
		t.Errorf("double clear returned error: %v", err)
	}
}

func TestSetGoalFocus_AutoCreatesRegistration(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	// No registration exists yet.
	if _, err := LoadRegistration(cfg, "agent-x"); err != nil || func() bool { r, _ := LoadRegistration(cfg, "agent-x"); return r != nil }() {
		t.Skip("registration already exists, skip stub-create test")
	}

	if err := SetGoalFocus(cfg, "agent-x", "G-001"); err != nil {
		t.Fatalf("SetGoalFocus with no prior file: %v", err)
	}
	got, err := LoadRegistration(cfg, "agent-x")
	if err != nil || got == nil {
		t.Fatalf("registration not created: %v / %v", got, err)
	}
	if got.GoalFocus != "G-001" {
		t.Errorf("GoalFocus = %q, want G-001", got.GoalFocus)
	}
}

func TestClearGoalFocusForAllAgents(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	for _, id := range []string{"agent-a", "agent-b", "agent-c"} {
		if err := SetGoalFocus(cfg, id, "G-001"); err != nil {
			t.Fatal(err)
		}
	}
	// agent-d focused on a different goal — must not be touched.
	if err := SetGoalFocus(cfg, "agent-d", "G-002"); err != nil {
		t.Fatal(err)
	}

	cleared, err := ClearGoalFocusForAllAgents(cfg, "G-001")
	if err != nil {
		t.Fatalf("ClearGoalFocusForAllAgents: %v", err)
	}
	if len(cleared) != 3 {
		t.Errorf("cleared %d agents, want 3: %v", len(cleared), cleared)
	}
	for _, id := range []string{"agent-a", "agent-b", "agent-c"} {
		if got := GetGoalFocus(cfg, id); got != "" {
			t.Errorf("%s still has focus %q after sweep", id, got)
		}
	}
	if got := GetGoalFocus(cfg, "agent-d"); got != "G-002" {
		t.Errorf("agent-d focus changed from G-002 to %q", got)
	}
}
