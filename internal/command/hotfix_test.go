package command

import (
	"strings"
	"testing"
)

// Flagging an existing item sets Item.Hotfix and the on-disk `hotfix:` field,
// and clearing it flips both back.
func TestHotfixFlagAndClearExistingItem(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if code := captureRC(t, func() int { return Hotfix(s, cfg, []string{"T-001"}, HotfixOpts{}) }); code != 0 {
		t.Fatalf("hotfix T-001 returned %d, want 0", code)
	}
	it, _ := s.Get("T-001")
	if !it.Hotfix {
		t.Error("Item.Hotfix = false after flag, want true")
	}
	if v, _ := it.Doc.GetField("hotfix"); v != "true" {
		t.Errorf("on-disk hotfix field = %q, want \"true\"", v)
	}

	if code := captureRC(t, func() int { return Hotfix(s, cfg, []string{"T-001"}, HotfixOpts{Off: true}) }); code != 0 {
		t.Fatalf("hotfix --off T-001 returned %d, want 0", code)
	}
	it, _ = s.Get("T-001")
	if it.Hotfix {
		t.Error("Item.Hotfix = true after --off, want false")
	}
	if v, _ := it.Doc.GetField("hotfix"); v != "false" {
		t.Errorf("on-disk hotfix field = %q after --off, want \"false\"", v)
	}
}

// Flagging an already-flagged item is a no-op success (idempotent).
func TestHotfixIdempotent(t *testing.T) {
	s, cfg := setupTestEnv(t)
	for i := 0; i < 2; i++ {
		if code := captureRC(t, func() int { return Hotfix(s, cfg, []string{"T-001"}, HotfixOpts{}) }); code != 0 {
			t.Fatalf("flag #%d returned %d, want 0", i+1, code)
		}
	}
	it, _ := s.Get("T-001")
	if !it.Hotfix {
		t.Error("Item.Hotfix = false, want true")
	}
}

// `--off` requires a single bare item ID; a title-shaped arg is rejected.
func TestHotfixOffRequiresID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := captureRC(t, func() int { return Hotfix(s, cfg, []string{"not", "an", "id"}, HotfixOpts{Off: true}) }); code != 2 {
		t.Errorf("hotfix --off with non-ID returned %d, want 2", code)
	}
}

// A single bare ID that doesn't resolve is an error, not a silent create.
func TestHotfixUnknownIDFails(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := captureRC(t, func() int { return Hotfix(s, cfg, []string{"I-999"}, HotfixOpts{}) }); code != 1 {
		t.Errorf("hotfix on unknown ID returned %d, want 1", code)
	}
}

// The create path makes a flagged p0 issue from a free-text title.
func TestHotfixCreatePath(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	code := captureRC(t, func() int {
		return Hotfix(s, cfg, []string{"Prod", "login", "broken"}, HotfixOpts{})
	})
	if code != 0 {
		t.Fatalf("hotfix create returned %d, want 0", code)
	}

	var flagged []string
	for id, it := range s.All() {
		if it.Hotfix {
			flagged = append(flagged, id)
			if it.Type != "issue" {
				t.Errorf("%s type = %q, want issue", id, it.Type)
			}
			if it.Priority == nil || *it.Priority != 0 {
				t.Errorf("%s priority = %v, want 0", id, it.Priority)
			}
			if it.Title != "Prod login broken" {
				t.Errorf("%s title = %q, want \"Prod login broken\"", id, it.Title)
			}
		}
	}
	if len(flagged) != 1 {
		t.Fatalf("flagged items = %v, want exactly 1 new flagged issue", flagged)
	}
}

// No args lists items currently in hotfix mode.
func TestHotfixList(t *testing.T) {
	s, cfg := setupTestEnv(t)
	_ = captureRC(t, func() int { return Hotfix(s, cfg, []string{"T-001"}, HotfixOpts{}) })

	out := captureStdout(t, func() {
		if code := Hotfix(s, cfg, nil, HotfixOpts{}); code != 0 {
			t.Fatalf("hotfix list returned %d, want 0", code)
		}
	})
	if !strings.Contains(out, "T-001") {
		t.Errorf("list output %q does not mention flagged T-001", out)
	}
}

// captureRC runs fn while swallowing its stdout (Hotfix prints banners) and
// returns the exit code, keeping test output clean.
func captureRC(t *testing.T, fn func() int) int {
	t.Helper()
	var rc int
	_ = captureStdout(t, func() { rc = fn() })
	return rc
}
