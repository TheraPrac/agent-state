package command

import (
	"os"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/store"
)

// assignPeerOnDisk writes assigned_to: <owner> directly to the item file so the
// fresh re-read in Start() (I-1435) sees the peer assignment rather than the
// in-memory value — mirrors the established TestStartAssignedToOther setup.
func assignPeerOnDisk(t *testing.T, s *store.Store, id, owner string) {
	t.Helper()
	p, ok := s.Path(id)
	if !ok {
		t.Fatalf("Path(%s): not found", id)
	}
	content, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	updated := strings.ReplaceAll(string(content),
		"last_touched:", "assigned_to: "+owner+"\nlast_touched:")
	if err := os.WriteFile(p, []byte(updated), 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// I-1633 (failure-mode-first): an item assigned to a peer is refused, and the
// message must name the owner (so the caller knows whom to coordinate with).
func TestStart_PeerAssignedDeniedNamesOwner(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-b")
	assignPeerOnDisk(t, s, "T-001", "agent-a")

	if code := Start(s, cfg, "T-001", StartOpts{}); code != 1 {
		t.Fatalf("Start of peer-assigned item returned %d, want 1 (denied)", code)
	}
}

// I-1633: --takeover "<reason>" bypasses the peer-assignment guard, reassigns
// the item to the caller, and writes an audited start_takeover entry recording
// the prior owner and the reason.
func TestStart_TakeoverReassignsAndAudits(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-b")
	assignPeerOnDisk(t, s, "T-001", "agent-a")

	reason := "agent-a handed off in mail"
	if code := Start(s, cfg, "T-001", StartOpts{Takeover: reason}); code != 0 {
		t.Fatalf("Start --takeover returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.AssignedTo != "agent-b" {
		t.Errorf("assigned_to = %q after takeover, want agent-b", item.AssignedTo)
	}

	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("changelog.Read: %v", err)
	}
	var found *changelog.Entry
	for i := range entries {
		if entries[i].Op == "start_takeover" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no start_takeover changelog entry written")
	}
	if found.OldValue != "agent-a" {
		t.Errorf("start_takeover OldValue = %q, want agent-a (prior owner)", found.OldValue)
	}
	if found.NewValue != "agent-b" {
		t.Errorf("start_takeover NewValue = %q, want agent-b (caller)", found.NewValue)
	}
	if !strings.Contains(found.Reason, reason) {
		t.Errorf("start_takeover Reason = %q, want it to contain %q", found.Reason, reason)
	}
}

// I-1633 review (finding [1]): --takeover without a resolved agent identity
// must refuse — otherwise the Mutate (gated on agentID != "") skips the
// reassignment yet a start_takeover audit would still be written, a false
// handoff record with the item still held by the peer.
func TestStart_TakeoverWithoutIdentityDenied(t *testing.T) {
	for _, k := range []string{"AS_AGENT_ID", "AS_AGENT_PARENT_ID", "AS_AGENT_ROOT_ID", "AS_AGENT_SPAWNED_BY_SESSION", "AS_AGENT_DELEGATED_ITEM", "AS_AGENT_ROLE"} {
		t.Setenv(k, "")
	}
	s, cfg := setupTestEnv(t)
	if cfg.Identity().ID != "" {
		t.Skipf("test env resolved a non-empty agent id (%q); cannot exercise the empty-identity path", cfg.Identity().ID)
	}
	assignPeerOnDisk(t, s, "T-001", "agent-a")

	if code := Start(s, cfg, "T-001", StartOpts{Takeover: "handed off"}); code != 1 {
		t.Fatalf("Start --takeover with no identity returned %d, want 1 (denied)", code)
	}
	entries, _ := changelog.Read(cfg, "T-001")
	for _, e := range entries {
		if e.Op == "start_takeover" {
			t.Fatalf("start_takeover audit written despite denied takeover (false handoff record)")
		}
	}
}

// I-1633: a whitespace-only --takeover reason is NOT a takeover — the guard
// still refuses (guards against an accidental empty flag stealing an item).
func TestStart_TakeoverEmptyReasonStillDenied(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-b")
	assignPeerOnDisk(t, s, "T-001", "agent-a")

	if code := Start(s, cfg, "T-001", StartOpts{Takeover: "  "}); code != 1 {
		t.Fatalf("Start --takeover with whitespace reason returned %d, want 1 (denied)", code)
	}
}
