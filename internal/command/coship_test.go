package command

import (
	"strings"
	"testing"
)

// Flagging an existing item with --api-ref sets Item.CoShipAPIRef and the
// on-disk `coship_api_ref:` field; --off clears both back to empty.
func TestCoShip_SetAndClear(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if code := captureRC(t, func() int {
		return CoShip(s, cfg, []string{"T-001"}, CoShipOpts{APIRef: "fix/api-branch"})
	}); code != 0 {
		t.Fatalf("coship set returned %d, want 0", code)
	}
	it, _ := s.Get("T-001")
	if it.CoShipAPIRef != "fix/api-branch" {
		t.Errorf("CoShipAPIRef = %q, want \"fix/api-branch\"", it.CoShipAPIRef)
	}
	if v, _ := it.Doc.GetField("coship_api_ref"); v != "fix/api-branch" {
		t.Errorf("on-disk coship_api_ref = %q, want \"fix/api-branch\"", v)
	}

	if code := captureRC(t, func() int {
		return CoShip(s, cfg, []string{"T-001"}, CoShipOpts{Off: true})
	}); code != 0 {
		t.Fatalf("coship --off returned %d, want 0", code)
	}
	it, _ = s.Get("T-001")
	if it.CoShipAPIRef != "" {
		t.Errorf("CoShipAPIRef = %q after --off, want empty", it.CoShipAPIRef)
	}
}

// --active-ref prints exactly the ref of the stack-top (active) item.
func TestCoShip_ActiveRefPrintsStackTop(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := captureRC(t, func() int {
		return CoShip(s, cfg, []string{"T-002"}, CoShipOpts{APIRef: "fix/contract"})
	}); code != 0 {
		t.Fatalf("coship set returned %d", code)
	}
	if code := StackPush(s, cfg, "T-001", StackPushOpts{}); code != 0 {
		t.Fatalf("push T-001 returned %d", code)
	}
	if code := StackPush(s, cfg, "T-002", StackPushOpts{Reason: "blocks T-001"}); code != 0 {
		t.Fatalf("push T-002 returned %d", code)
	}

	out := captureStdout(t, func() {
		if code := CoShip(s, cfg, nil, CoShipOpts{ActiveRef: true}); code != 0 {
			t.Fatalf("--active-ref returned %d, want 0", code)
		}
	})
	if strings.TrimSpace(out) != "fix/contract" {
		t.Errorf("--active-ref output = %q, want \"fix/contract\\n\"", out)
	}
}

// --active-ref prints nothing (and exits 0) when the stack top has no co-ship
// ref, and when the stack is empty.
func TestCoShip_ActiveRefEmptyWhenInactive(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Empty stack.
	out := captureStdout(t, func() {
		if code := CoShip(s, cfg, nil, CoShipOpts{ActiveRef: true}); code != 0 {
			t.Fatalf("--active-ref (empty stack) returned %d, want 0", code)
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("--active-ref (empty stack) output = %q, want empty", out)
	}

	// Stack top exists but is not in co-ship mode.
	if code := StackPush(s, cfg, "T-001", StackPushOpts{}); code != 0 {
		t.Fatalf("push T-001 returned %d", code)
	}
	out = captureStdout(t, func() {
		if code := CoShip(s, cfg, nil, CoShipOpts{ActiveRef: true}); code != 0 {
			t.Fatalf("--active-ref (inactive top) returned %d, want 0", code)
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("--active-ref (inactive top) output = %q, want empty", out)
	}
}

// No args lists items currently in co-ship mode.
func TestCoShip_List(t *testing.T) {
	s, cfg := setupTestEnv(t)
	_ = captureRC(t, func() int {
		return CoShip(s, cfg, []string{"T-001"}, CoShipOpts{APIRef: "fix/x"})
	})

	out := captureStdout(t, func() {
		if code := CoShip(s, cfg, nil, CoShipOpts{}); code != 0 {
			t.Fatalf("coship list returned %d, want 0", code)
		}
	})
	if !strings.Contains(out, "T-001") || !strings.Contains(out, "fix/x") {
		t.Errorf("list output %q does not mention flagged T-001 / its ref", out)
	}
}

// Setting --api-ref on an unknown ID is an error, not a silent create.
func TestCoShip_UnknownIDFails(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := captureRC(t, func() int {
		return CoShip(s, cfg, []string{"I-999"}, CoShipOpts{APIRef: "fix/x"})
	}); code != 1 {
		t.Errorf("coship on unknown ID returned %d, want 1", code)
	}
}

// --off requires a single bare item ID.
func TestCoShip_OffRequiresID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := captureRC(t, func() int {
		return CoShip(s, cfg, []string{"not", "an", "id"}, CoShipOpts{Off: true})
	}); code != 2 {
		t.Errorf("coship --off with non-ID returned %d, want 2", code)
	}
}

// --api-ref without an item ID is rejected.
func TestCoShip_APIRefRequiresID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := captureRC(t, func() int {
		return CoShip(s, cfg, nil, CoShipOpts{APIRef: "fix/x"})
	}); code != 2 {
		t.Errorf("coship --api-ref with no ID returned %d, want 2", code)
	}
}
