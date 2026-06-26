package command

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/classify"
	"github.com/theraprac/agent-state/internal/model"
)

func TestFilterByAgent_KeepsSelfAndUnassigned(t *testing.T) {
	items := []*model.Item{
		{ID: "T-001", AssignedTo: "agent-a"},
		{ID: "T-002", AssignedTo: "agent-b"},
		{ID: "T-003", AssignedTo: ""},
		{ID: "T-004", AssignedTo: "agent-a"},
	}
	got := filterByAgent(items, "agent-a")
	ids := []string{}
	for _, it := range got {
		ids = append(ids, it.ID)
	}
	want := "T-001,T-003,T-004"
	if strings.Join(ids, ",") != want {
		t.Errorf("filterByAgent = %v; want %s", ids, want)
	}
}

func TestFilterByAgent_EmptyAgentPassesThrough(t *testing.T) {
	items := []*model.Item{
		{ID: "T-001", AssignedTo: "agent-a"},
		{ID: "T-002", AssignedTo: "agent-b"},
	}
	got := filterByAgent(items, "")
	if len(got) != 2 {
		t.Errorf("empty agent should pass all through; got %d items", len(got))
	}
}

func TestClassifyAgentOwnership(t *testing.T) {
	cases := []struct {
		assigned string
		me       string
		want     string
	}{
		{"agent-a", "agent-a", "self"},
		{"agent-b", "agent-a", "peer"},
		{"", "agent-a", "open"},
		{"agent-a", "", "self"}, // outside agent context, everything is "self"
	}
	for _, c := range cases {
		got := classifyAgentOwnership(&model.Item{AssignedTo: c.assigned}, c.me)
		if got != c.want {
			t.Errorf("classifyAgentOwnership(assigned=%q, me=%q) = %q; want %q",
				c.assigned, c.me, got, c.want)
		}
	}
}

// TestStatus_DefaultScopesToCurrentAgent confirms that from inside an
// agent workspace, statusDashboard's Active Work surface drops items
// owned by peer agents but keeps the current agent's + unassigned.
func TestStatus_DefaultScopesToCurrentAgent(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)

	// Flip T-003 (assigned: agent-a in fixtures) — wait, fixtures show
	// T-003 assigned to agent-a already. To prove the filter, we need
	// at least one active item assigned to a peer. Flip T-001 to
	// active, leave it assigned to agent-a indirectly...
	// Simpler: just start T-001 as agent-b in a second step. But
	// Start() depends on identity. We can mutate the doc directly via
	// the store API.
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		it.Doc.SetField("assigned_to", "agent-b")
		it.AssignedTo = "agent-b"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	out := captureStdout(t, func() {
		if rc := Status(s, cfg, "", StatusOpts{NoRefresh: true}); rc != 0 {
			t.Errorf("Status rc = %d; want 0", rc)
		}
	})

	// T-003 (agent-a, active) must appear; T-001 (agent-b, active)
	// must NOT in scoped view.
	if !strings.Contains(out, "T-003") {
		t.Error("scoped status should include T-003 (agent-a)")
	}
	// Strip the "agent-a" header suffix when scanning for T-001.
	body := stripAfter(out, "━━━ AWAITING")
	activeBody := stripAfter(body, "━━━ YOUR STACK")
	if strings.Contains(activeBody, "T-001") {
		t.Errorf("scoped status leaked peer item T-001:\n%s", activeBody)
	}
}

func TestStatus_GlobalFlagDisablesScoping(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		it.Doc.SetField("assigned_to", "agent-b")
		it.AssignedTo = "agent-b"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	out := captureStdout(t, func() {
		Status(s, cfg, "", StatusOpts{NoRefresh: true, AgentAll: true})
	})
	if !strings.Contains(out, "T-001") {
		t.Error("AgentAll=true should surface peer item T-001")
	}
	if !strings.Contains(out, "T-003") {
		t.Error("AgentAll=true should also keep current-agent item T-003")
	}
}

// TestStatus_RootContextShowsGlobalByDefault — outside any agent
// workspace (AS_AGENT_ID="" and not in an agent dir), the scope is
// not applied and all items render.
func TestStatus_RootContextShowsGlobalByDefault(t *testing.T) {
	os.Unsetenv("AS_AGENT_ID")
	s, cfg := setupTestEnv(t)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		it.Doc.SetField("assigned_to", "agent-b")
		it.AssignedTo = "agent-b"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}
	out := captureStdout(t, func() {
		Status(s, cfg, "", StatusOpts{NoRefresh: true})
	})
	if !strings.Contains(out, "T-001") || !strings.Contains(out, "T-003") {
		t.Errorf("root context should be global; got:\n%s", out)
	}
}

func TestRed_DefaultScopesToCurrentAgent(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-c")
	s, cfg := setupTestEnv(t)

	// Promote two items to awaiting_decision: one owned by agent-c,
	// one by agent-a. Only the agent-c one should appear by default.
	card := classify.BuildDecisionCard(
		classify.Result{Verdict: classify.VerdictRed, Reason: "RBAC", ClassifiedBy: "model:claude", InputHash: "h", ClassifiedAt: time.Now().UTC()},
		[]string{"foo.go"}, "approve to merge", time.Now().UTC(),
	)
	// Move T-003 (agent-a) into awaiting first.
	if err := FlipToAwaitingDecision(s, cfg, "T-003", card); err != nil {
		t.Fatalf("flip T-003: %v", err)
	}
	// Create an active-then-paused item for agent-c. Reuse T-001:
	// flip to active assigned to agent-c, then to awaiting_decision.
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		it.Doc.SetField("assigned_to", "agent-c")
		it.AssignedTo = "agent-c"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}
	if err := FlipToAwaitingDecision(s, cfg, "T-001", card); err != nil {
		t.Fatalf("flip T-001: %v", err)
	}

	out := captureStdout(t, func() {
		Red(s, cfg, RedOpts{})
	})
	if !strings.Contains(out, "T-001") {
		t.Error("Red should show agent-c's T-001")
	}
	if strings.Contains(out, "T-003") {
		t.Errorf("Red default should hide peer agent-a's T-003:\n%s", out)
	}

	allOut := captureStdout(t, func() {
		Red(s, cfg, RedOpts{AgentAll: true})
	})
	if !strings.Contains(allOut, "T-003") {
		t.Errorf("Red --all should include peer T-003:\n%s", allOut)
	}
}

func TestRed_EmptyListExits0(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		if rc := Red(s, cfg, RedOpts{}); rc != 0 {
			t.Errorf("empty Red rc = %d; want 0", rc)
		}
	})
	if !strings.Contains(out, "No items awaiting") {
		t.Errorf("empty banner missing; got %q", out)
	}
}

func TestQueueShow_VisualClassesApplyInsideAgent(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-c")
	s, cfg := setupTestEnv(t)

	// T-003 is assigned to agent-a (per fixture). T-001 has no
	// assignment. Add both to the queue.
	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd T-001: %d", rc)
	}
	if rc := QueueAdd(s, cfg, "T-003", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd T-003: %d", rc)
	}

	// Raw=true: legacy queue view where agent-scoped visual treatment renders.
	out := captureStdout(t, func() {
		QueueShow(s, cfg, QueueShowOpts{Raw: true})
	})
	// Scoping headline should be present.
	if !strings.Contains(out, "agent-c") {
		t.Errorf("scoped queue should mention agent-c; got:\n%s", out)
	}
	// Peer assignment annotation on T-003 row.
	if !strings.Contains(out, "[agent-a]") {
		t.Errorf("peer item should render with [agent-a] tag; got:\n%s", out)
	}

	// --all suppresses the visual treatment.
	allOut := captureStdout(t, func() {
		QueueShow(s, cfg, QueueShowOpts{Raw: true, AgentAll: true})
	})
	if strings.Contains(allOut, "[agent-a]") {
		t.Errorf("--all should suppress peer annotation; got:\n%s", allOut)
	}
}

func TestResolveAgentContext_ScopedFromEnv(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-c")
	_, cfg := setupTestEnv(t)
	ctx := cfg.ResolveAgentContext()
	if !ctx.Scoped || ctx.CurrentAgent != "agent-c" {
		t.Errorf("AS_AGENT_ID set → scoped=true agent=agent-c; got %+v", ctx)
	}
}

func TestResolveAgentContext_UnscopedAtRoot(t *testing.T) {
	os.Unsetenv("AS_AGENT_ID")
	_, cfg := setupTestEnv(t)
	ctx := cfg.ResolveAgentContext()
	if ctx.Scoped || ctx.CurrentAgent != "" {
		t.Errorf("no AS_AGENT_ID, root dir → scoped=false; got %+v", ctx)
	}
}

// stripAfter returns the portion of s before the first occurrence of
// marker. Used to constrain scoped-output assertions to a specific
// section (e.g. "Active Work" only, before AWAITING DECISION).
func stripAfter(s, marker string) string {
	if i := strings.Index(s, marker); i >= 0 {
		return s[:i]
	}
	return s
}
