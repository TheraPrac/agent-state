package command

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/model"
)

// statusMeRender drives statusMeTo against a buffer + a custom agent so
// the test doesn't depend on cfg.Identity() resolving in the test env.
func statusMeRender(t *testing.T, agent string, opts StatusOpts) (string, int) {
	t.Helper()
	s, cfg := setupTestEnv(t)
	opts.Me = true
	opts.Agent = agent
	var buf bytes.Buffer
	rc := statusMeTo(&buf, s, cfg, opts)
	return buf.String(), rc
}

func TestStatusMe_TextHasFourSections(t *testing.T) {
	out, rc := statusMeRender(t, "agent-b", StatusOpts{})
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	for _, label := range []string{"DONE", "IN-FLIGHT", "NEEDS-YOU", "PROPOSED-NEXT"} {
		if !strings.Contains(out, label) {
			t.Errorf("missing section %q\n%s", label, out)
		}
	}
}

func TestStatusMe_InFlightWhenAssignedAndActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-003 in the fixture is already active + assigned to agent-a.
	out, rc := statusMeRender(t, "agent-a", StatusOpts{})
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(out, "T-003") {
		t.Errorf("T-003 (active, assigned to agent-a) must appear in IN-FLIGHT\n%s", out)
	}
	_ = s
	_ = cfg
}

func TestStatusMe_DoneRespectsTouchedByAndWindow(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Mutate T-002 into a terminal state touched by agent-b RIGHT NOW.
	if err := s.Mutate("T-002", func(m *model.Item) error {
		m.Status = "done"
		m.LastTouchedBy = "agent-b"
		m.LastTouched = time.Now()
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	var buf bytes.Buffer
	rc := statusMeTo(&buf, s, cfg, StatusOpts{Me: true, Agent: "agent-b"})
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, buf.String())
	}
	if !strings.Contains(buf.String(), "T-002") {
		t.Errorf("just-closed-by-me item must appear in DONE\n%s", buf.String())
	}

	// Same fixture, narrow window so nothing qualifies.
	buf.Reset()
	statusMeTo(&buf, s, cfg, StatusOpts{Me: true, Agent: "agent-b", Since: "1ms"})
	// Allow a touch of slop — DONE may still contain T-002 if the
	// system clock hasn't ticked past the 1ms cutoff before the call.
	// The real assertion: a 0s window must drop everything.
	buf.Reset()
	statusMeTo(&buf, s, cfg, StatusOpts{Me: true, Agent: "agent-b", Since: "0s"})
	if strings.Contains(buf.String(), "T-002") {
		t.Errorf("0s window must drop T-002 from DONE\n%s", buf.String())
	}
}

// T-461: approval gate removed; all entries are auto-approved. NEEDS-YOU is
// always empty. Agent-proposed entries at pos > 0 appear in PROPOSED-NEXT;
// pos 0 is the current pick (not shown until claimed). User-added entries
// are never shown in another agent's rollup.
func TestStatusMe_ProposedNext(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-b")
	QueueAdd(s, cfg, "T-001", QueueOpts{Reason: "top pick"})  // pos 0 — not shown
	QueueAdd(s, cfg, "T-002", QueueOpts{Reason: "proposed"})   // pos 1 → PROPOSED-NEXT
	t.Setenv("AS_AGENT_ID", "")
	QueueAdd(s, cfg, "I-001", QueueOpts{}) // user-added — must not appear

	var buf bytes.Buffer
	rc := statusMeTo(&buf, s, cfg, StatusOpts{Me: true, Agent: "agent-b"})
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "T-002") {
		t.Errorf("T-002 (agent-proposed, pos=1) must appear in PROPOSED-NEXT\n%s", got)
	}
	if strings.Contains(got, "I-001") {
		t.Errorf("I-001 (user-added) must NOT appear in agent-b's rollup\n%s", got)
	}
}

func TestStatusMe_JSONStableContract(t *testing.T) {
	out, rc := statusMeRender(t, "agent-b", StatusOpts{JSON: true})
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	var r statusMeReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if r.Agent != "agent-b" {
		t.Errorf("agent = %q, want agent-b", r.Agent)
	}
	// Empty sections must be `[]`, not `null` — the contract for downstream
	// T-378/T-379 consumers.
	if r.Done == nil || r.InFlight == nil || r.NeedsYou == nil || r.ProposedNext == nil {
		t.Errorf("empty sections must be `[]` (non-nil slices), got %#v", r)
	}
}

func TestStatusMe_NoIdentityFailsLoudly(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "") // suppress any inherited shell identity
	if rc := statusMeTo(&bytes.Buffer{}, s, cfg, StatusOpts{Me: true}); rc != 1 {
		t.Errorf("no agent identity must rc=1 (loud, never silent), got %d", rc)
	}
}

func TestStatusMe_InvalidSinceFailsLoudly(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := statusMeTo(&bytes.Buffer{}, s, cfg, StatusOpts{Me: true, Agent: "x", Since: "nonsense"}); rc != 2 {
		t.Errorf("bad --since must rc=2, got %d", rc)
	}
}

func TestStatusMe_DaySuffixSince(t *testing.T) {
	d, err := resolveSince("7d")
	if err != nil {
		t.Fatalf("7d: %v", err)
	}
	if d != 7*24*time.Hour {
		t.Errorf("7d → %v, want %v", d, 7*24*time.Hour)
	}
}

func TestStatusMe_Deterministic(t *testing.T) {
	run := func() string {
		out, _ := statusMeRender(t, "agent-b", StatusOpts{})
		return out
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("non-deterministic:\nA:%s\nB:%s", a, b)
	}
}
