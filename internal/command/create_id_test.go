package command

import (
	"strings"
	"testing"
)

// TestCreatePrintsNewItemIDBeforeReview verifies that st create emits "new item: <ID>"
// as the first capturable stdout line, before autoSync and runItemReview produce
// any output (I-1301). The ID must be parseable even when a Claude sub-agent
// floods stdout afterward.
func TestCreatePrintsNewItemIDBeforeReview(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	var code int
	output := captureStdout(t, func() {
		code = Create(s, cfg, "issue", "New item ID test", CreateOpts{
			Priority:       3,
			Situation:      "Test situation for the new item ID output test (I-1301).",
			Background:     "Background for the new item ID output test.",
			Assessment:     "Assessment for the new item ID output test.",
			Recommendation: "Recommendation for the new item ID output test.",
		})
	})

	if code != 0 {
		t.Fatalf("Create returned %d, want 0; output: %s", code, output)
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		t.Fatal("no output from Create")
	}
	if !strings.HasPrefix(lines[0], "new item: ") {
		t.Errorf("first output line = %q; want prefix \"new item: \"", lines[0])
	}
	id := strings.TrimPrefix(lines[0], "new item: ")
	if _, ok := s.Get(id); !ok {
		t.Errorf("item %q not found in store after Create", id)
	}
}
