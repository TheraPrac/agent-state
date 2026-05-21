//go:build !windows

package command

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDefaultRunClaudeProcessGroupKilled pins the I-738 leak: when the
// wall cap fires, the Claude leader AND its forked tool-call children
// must both be reaped. Before I-752, exec.CommandContext only signaled
// the leader (PGID-less); a child `sleep` would orphan and outlive the
// session. The fix sets Setpgid + cmd.Cancel that SIGTERMs -pgid.
//
// UNIX-only: Setpgid lives behind syscall.SysProcAttr which has no
// Windows surface. The `as` repo already targets darwin/linux per
// CLAUDE.md operating principle #32; Windows is out of scope (I-754).
func TestDefaultRunClaudeProcessGroupKilled(t *testing.T) {
	if testing.Short() {
		t.Skip("process-group test skipped in -short mode")
	}

	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "child.pid")

	// Shim mimics `claude` for the test: forks a `sleep 60` child,
	// records its PID, then `wait`s so the leader stays alive until
	// the wall cap fires. The child sleeps independently — if Setpgid
	// is broken, killing the leader leaves the child alive (the bug).
	shimPath := filepath.Join(tmp, "claude")
	shim := "#!/bin/sh\nsleep 60 &\necho \"$!\" > \"" + pidFile + "\"\nwait\n"
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	// Prepend tmp to PATH so exec.LookPath("claude") inside
	// defaultRunClaude resolves to the shim. t.Setenv restores on
	// test exit.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmp+":"+origPath)

	done := make(chan struct{})
	go func() {
		// 1500ms wall cap — fast enough to keep the test snappy,
		// long enough to absorb shell cold-start latency under
		// parallel suite load (300ms races shim startup on a busy
		// CI box).
		_, _, _ = defaultRunClaude(tmp, nil, []string{"AS_CLAUDE_WALL_TIMEOUT=1500ms"})
		close(done)
	}()

	// Poll the PID file until the shim's child is recorded.
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(data) > 0 {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				childPID = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatalf("shim never wrote child PID to %s", pidFile)
	}

	// Wait for defaultRunClaude to return after the wall cap + the
	// 2s WaitDelay grace built into cmd.Cancel.
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("defaultRunClaude did not return within 15s after wall cap")
	}

	// Brief grace for the kernel to reap the child after SIGTERM/SIGKILL
	// propagates through the process group.
	for i := 0; i < 25; i++ {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}

	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(childPID, syscall.SIGKILL) // cleanup so the test doesn't leak
		t.Fatalf("child PID %d still alive after wall cap — process-group kill did not reach it (kill -0 err=%v)", childPID, err)
	}
}
