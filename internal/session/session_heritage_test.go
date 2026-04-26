package session

import (
	"testing"
	"time"
)

func TestSessionHeritage_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir, time.Hour)

	spec := IdentitySpec{
		AgentID:          "agent-child",
		ParentAgentID:    "agent-a",
		RootAgentID:      "agent-root",
		Role:             "reviewer",
		SpawnedBySession: "sess-parent-99",
		DelegatedItemID:  "I-300",
	}

	created, err := mgr.EnsureSessionWithIdentity("sess-child-1", spec)
	if err != nil {
		t.Fatalf("EnsureSessionWithIdentity: %v", err)
	}

	if created.ParentAgentID != "agent-a" {
		t.Errorf("ParentAgentID = %q, want agent-a", created.ParentAgentID)
	}
	if created.RootAgentID != "agent-root" {
		t.Errorf("RootAgentID = %q, want agent-root", created.RootAgentID)
	}
	if created.Role != "reviewer" {
		t.Errorf("Role = %q, want reviewer", created.Role)
	}

	loaded, err := mgr.Load("sess-child-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load returned nil")
	}
	if loaded.ParentAgentID != spec.ParentAgentID ||
		loaded.RootAgentID != spec.RootAgentID ||
		loaded.Role != spec.Role ||
		loaded.SpawnedBySession != spec.SpawnedBySession ||
		loaded.DelegatedItemID != spec.DelegatedItemID {
		t.Errorf("heritage round-trip lost data: %+v", loaded)
	}
}

func TestSessionHeritage_OmittedWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir, time.Hour)

	if _, err := mgr.EnsureSession("sess-no-heritage", "agent-solo"); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	loaded, err := mgr.Load("sess-no-heritage")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load returned nil")
	}
	if loaded.ParentAgentID != "" || loaded.RootAgentID != "" || loaded.Role != "" {
		t.Errorf("heritage should be empty: %+v", loaded)
	}
	if loaded.AgentID != "agent-solo" {
		t.Errorf("AgentID = %q, want agent-solo", loaded.AgentID)
	}
}
