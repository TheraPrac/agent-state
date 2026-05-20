package command

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestArc_AddSetsField(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := ArcAdd(s, cfg, "trust-surface", []string{"T-001"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	it, _ := s.Get("T-001")
	if it.Arc != "trust-surface" {
		t.Errorf("T-001.Arc = %q, want trust-surface", it.Arc)
	}
}

func TestArc_RmClearsField(t *testing.T) {
	s, cfg := setupTestEnv(t)
	ArcAdd(s, cfg, "trust-surface", []string{"T-001"})
	if rc := ArcRm(s, cfg, []string{"T-001"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	it, _ := s.Get("T-001")
	if it.Arc != "" {
		t.Errorf("T-001.Arc after rm = %q, want empty", it.Arc)
	}
}

func TestArc_ShowListsMembers(t *testing.T) {
	s, cfg := setupTestEnv(t)
	ArcAdd(s, cfg, "trust-surface", []string{"T-001", "T-002"})
	ArcAdd(s, cfg, "autonomy", []string{"T-003"}) // different arc

	var buf bytes.Buffer
	arcShowTo(&buf, s, cfg, "trust-surface", false)
	out := buf.String()
	if !strings.Contains(out, "T-001") || !strings.Contains(out, "T-002") {
		t.Errorf("show trust-surface missing members:\n%s", out)
	}
	if strings.Contains(out, "T-003") {
		t.Errorf("show trust-surface must NOT include T-003 (different arc):\n%s", out)
	}
}

func TestArc_ShowJSONStable(t *testing.T) {
	s, cfg := setupTestEnv(t)
	ArcAdd(s, cfg, "trust-surface", []string{"T-001"})
	var buf bytes.Buffer
	arcShowTo(&buf, s, cfg, "trust-surface", true)
	var got struct {
		Arc   string `json:"arc"`
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if got.Arc != "trust-surface" || len(got.Items) != 1 || got.Items[0].ID != "T-001" {
		t.Errorf("bad JSON: %+v", got)
	}
}

func TestArc_ListEnumeratesWithCounts(t *testing.T) {
	s, cfg := setupTestEnv(t)
	ArcAdd(s, cfg, "trust-surface", []string{"T-001", "T-002"})
	ArcAdd(s, cfg, "autonomy", []string{"T-003"})

	var buf bytes.Buffer
	arcListTo(&buf, s, cfg, false)
	out := buf.String()
	if !strings.Contains(out, "trust-surface") || !strings.Contains(out, "autonomy") {
		t.Errorf("list missing arcs:\n%s", out)
	}
	if !strings.Contains(out, "  2") || !strings.Contains(out, "  1") {
		t.Errorf("list missing counts:\n%s", out)
	}
}

func TestArc_ListEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	arcListTo(&buf, s, cfg, false)
	if !strings.Contains(buf.String(), "no arcs in use") {
		t.Errorf("empty arcs must say so loudly, got:\n%s", buf.String())
	}
}

func TestArc_AddArgsValidation(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := ArcAdd(s, cfg, "", []string{"T-001"}); rc != 2 {
		t.Errorf("empty name must rc=2, got %d", rc)
	}
	if rc := ArcAdd(s, cfg, "trust-surface", nil); rc != 2 {
		t.Errorf("empty ids must rc=2, got %d", rc)
	}
}

// Status --me --arc filters all four sections.
func TestStatusMe_ArcFilter(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-b")
	QueueAdd(s, cfg, "T-001", QueueOpts{})             // agent-proposed (NEEDS-YOU)
	QueueAdd(s, cfg, "T-002", QueueOpts{})             // agent-proposed (NEEDS-YOU)
	ArcAdd(s, cfg, "trust-surface", []string{"T-001"}) // only T-001 in arc

	var buf bytes.Buffer
	statusMeTo(&buf, s, cfg, StatusOpts{Me: true, Agent: "agent-b", Arc: "trust-surface"})
	out := buf.String()
	if !strings.Contains(out, "T-001") {
		t.Errorf("arc-filtered status must INCLUDE T-001:\n%s", out)
	}
	if strings.Contains(out, "T-002") {
		t.Errorf("arc-filtered status must EXCLUDE T-002 (not in arc):\n%s", out)
	}
}
