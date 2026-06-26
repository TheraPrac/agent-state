package command

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/testutil"
)

// TestCloseDeprecatedResolutionRejectsWithMessage verifies that I-433
// retired resolution names are rejected at the entry point with a
// migration pointer instead of silently producing an "invalid
// resolution" error or — worse — being treated as the legacy value.
//
// I-001 (the seeded issue fixture) is the target so all three
// rejections fire against an item that exists.
func TestCloseDeprecatedResolutionRejectsWithMessage(t *testing.T) {
	cases := []struct {
		resolution string
		wantHint   string // substring required in stderr
	}{
		{"resolved", "deprecated (I-433)"},
		{"completed", "deprecated (I-433)"},
		{"wontfix", "Use \"abandoned\""},
	}

	for _, tc := range cases {
		t.Run(tc.resolution, func(t *testing.T) {
			env := testutil.NewEnv(t)

			stderr := captureStderr(t, func() int {
				return Close(env.S, env.Cfg, "I-001", tc.resolution, CloseOpts{})
			})

			if !strings.Contains(stderr, tc.wantHint) {
				t.Errorf("expected stderr to contain %q for resolution %q, got: %s",
					tc.wantHint, tc.resolution, stderr)
			}

			// The item must NOT have been mutated. status should still be queued.
			env.Reload(t)
			it, ok := env.S.Get("I-001")
			if !ok {
				t.Fatalf("I-001 disappeared after rejected close")
			}
			if it.Status != "queued" {
				t.Errorf("I-001 status mutated to %q despite rejection", it.Status)
			}
		})
	}
}

// captureStderr swaps os.Stderr while fn runs and returns the captured
// output. fn's int return is exit code; tests assert stderr content
// rather than the code so a future return-code change doesn't break
// these regression tests.
func captureStderr(t *testing.T, fn func() int) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	_ = fn()

	w.Close()
	os.Stderr = old
	<-done
	return buf.String()
}
