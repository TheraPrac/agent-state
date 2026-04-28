//go:build multiagent

// Multi-agent integration test harness — T-328.
//
// Spins up the compiled st binary in subprocesses against tempdir
// workspaces and verifies the cross-process invariants the v1
// multi-agent sprint built in (atomic claims, mail atomicity,
// stale-PID sweep). Unit-test runs skip this file by default; the
// build tag forces opt-in:
//
//	go test -tags multiagent ./internal/command/...
//
// Each scenario:
//  - builds a fresh tempdir workspace via newMultiEnv
//  - launches two subprocess agents with distinct AS_AGENT_ID values
//  - waits for both to finish
//  - asserts on file state + exit codes
package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stBin is the path to the freshly-built st binary, populated by
// TestMain so every test reuses the same build. Test parallelism is
// safe — the binary is read-only.
var stBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "st-multiagent-*")
	if err != nil {
		panic("mktemp: " + err.Error())
	}
	stBin = filepath.Join(tmp, "st")

	cmd := exec.Command("go", "build", "-o", stBin, "./cmd/as")
	cmd.Dir = projectRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmp)
		panic("build st failed: " + string(out))
	}

	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// projectRoot walks up from CWD until it hits go.mod.
func projectRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not find go.mod walking up from " + dir)
		}
		dir = parent
	}
}

// multiEnv is a tempdir workspace prepared for a multi-agent test.
// Worktree integration is intentionally OFF so subprocesses don't
// fork into git operations the test harness can't observe.
type multiEnv struct {
	Root string
}

// newMultiEnv writes a config + a sample task + a sample issue, all
// pre-existing as files (no git) so the subprocess `st` invocations
// see a normal-looking workspace.
func newMultiEnv(t *testing.T) *multiEnv {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"tasks", "issues", "archive", ".as", ".as/agents", ".as/sessions", ".as/mailboxes", "templates"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}

	cfg := `project:
  name: multiagent-test

paths:
  root: .
  templates: templates
  changelog: .changelog
  index: index.md

git:
  auto_commit: false
  auto_push: false
`
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	writeItem(t, filepath.Join(root, "tasks", "T-001-sample.md"), `id: T-001
type: task
status: queued
created: 2026-04-28T10:00:00-06:00
last_touched: 2026-04-28T10:00:00-06:00

completed: null

title: Sample task
priority: 2

depends_on:
- []
`)
	writeItem(t, filepath.Join(root, "tasks", "T-002-second.md"), `id: T-002
type: task
status: queued
created: 2026-04-28T10:00:00-06:00
last_touched: 2026-04-28T10:00:00-06:00

completed: null

title: Second task
priority: 2

depends_on:
- []
`)
	writeItem(t, filepath.Join(root, "tasks", "T-003-third.md"), `id: T-003
type: task
status: queued
created: 2026-04-28T10:00:00-06:00
last_touched: 2026-04-28T10:00:00-06:00

completed: null

title: Third task
priority: 2

depends_on:
- []
`)

	return &multiEnv{Root: root}
}

func writeItem(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stProc is one subprocess agent invocation.
type stProc struct {
	AgentID    string
	SessionID  string
	Args       []string
	Stdout     string
	Stderr     string
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

// runST invokes the binary synchronously and captures everything.
func runST(t *testing.T, env *multiEnv, agentID, sessionID string, args ...string) *stProc {
	t.Helper()
	p := &stProc{AgentID: agentID, SessionID: sessionID, Args: args, StartedAt: time.Now()}
	cmd := exec.Command(stBin, args...)
	cmd.Dir = env.Root
	cmd.Env = append(os.Environ(),
		"AS_AGENT_ID="+agentID,
		"AS_SESSION_ID="+sessionID,
	)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	p.Stdout = out.String()
	p.Stderr = errOut.String()
	p.FinishedAt = time.Now()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			p.ExitCode = ee.ExitCode()
		} else {
			t.Fatalf("exec %s %v: %v", stBin, args, err)
		}
	}
	return p
}

// runConcurrent fires N invocations in parallel and waits for all.
// Returns results in the order of the input procs slice.
func runConcurrent(t *testing.T, env *multiEnv, procs []procSpec) []*stProc {
	t.Helper()
	results := make([]*stProc, len(procs))
	var wg sync.WaitGroup
	// Use a barrier so all subprocesses fire on the same Tick — this
	// maximizes the chance the OS schedules them simultaneously, which
	// is what we want for race coverage.
	start := make(chan struct{})
	for i, ps := range procs {
		i, ps := i, ps
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = runST(t, env, ps.AgentID, ps.SessionID, ps.Args...)
		}()
	}
	close(start)
	wg.Wait()
	return results
}

type procSpec struct {
	AgentID   string
	SessionID string
	Args      []string
}

// readItemFile returns the contents of an item file matching the id
// prefix in any of the standard subdirs. Helper for assertions.
func readItemFile(t *testing.T, env *multiEnv, id string) string {
	t.Helper()
	for _, sub := range []string{"tasks", "issues", "archive"} {
		entries, err := os.ReadDir(filepath.Join(env.Root, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), id+"-") && e.Name() != id+".md" {
				continue
			}
			body, err := os.ReadFile(filepath.Join(env.Root, sub, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			return string(body)
		}
	}
	return ""
}

// extractFieldValue parses a top-level YAML-ish "key: value" line out
// of an item body. Returns "" when absent.
func extractFieldValue(body, key string) string {
	prefix := key + ":"
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.HasPrefix(ln, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(ln, prefix))
		}
	}
	return ""
}

// summarize is for failure messages — no output is more confusing
// than `subprocess failed: exit 1` with no context.
func (p *stProc) summarize() string {
	return fmt.Sprintf("agent=%s session=%s args=%v exit=%d\n  stdout: %s\n  stderr: %s",
		p.AgentID, p.SessionID, p.Args, p.ExitCode,
		strings.TrimSpace(p.Stdout), strings.TrimSpace(p.Stderr))
}

// TestMultiAgent_ConcurrentStartClaimsExactlyOne — Scenario 1.
//
// Two subprocesses race `st start T-001`. The Mutate-based
// compare-and-claim (store.Mutate + ErrAlreadyClaimed) guarantees
// exactly one wins. The loser's stderr names the conflict; the item
// file's claimed_by reflects exactly one of the two session ids.
func TestMultiAgent_ConcurrentStartClaimsExactlyOne(t *testing.T) {
	env := newMultiEnv(t)

	results := runConcurrent(t, env, []procSpec{
		{AgentID: "agent-test-a", SessionID: "session-a-1", Args: []string{"start", "T-001"}},
		{AgentID: "agent-test-b", SessionID: "session-b-1", Args: []string{"start", "T-001"}},
	})

	winners := 0
	losers := 0
	for _, r := range results {
		switch r.ExitCode {
		case 0:
			winners++
		default:
			losers++
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("expected exactly 1 winner and 1 loser, got winners=%d losers=%d\n  proc[0]: %s\n  proc[1]: %s",
			winners, losers, results[0].summarize(), results[1].summarize())
	}

	// The losing process should advise the operator about the conflict.
	for _, r := range results {
		if r.ExitCode == 0 {
			continue
		}
		if !strings.Contains(r.Stderr, "claimed") {
			t.Errorf("loser stderr missing 'claimed' marker: %s", r.summarize())
		}
	}

	body := readItemFile(t, env, "T-001")
	if body == "" {
		t.Fatalf("T-001 file disappeared after concurrent start")
	}
	claimedBy := extractFieldValue(body, "claimed_by")
	if claimedBy != "session-a-1" && claimedBy != "session-b-1" {
		t.Errorf("claimed_by=%q is neither session-a-1 nor session-b-1", claimedBy)
	}
	status := extractFieldValue(body, "status")
	if status != "active" {
		t.Errorf("status=%q, want active", status)
	}
}

// TestMultiAgent_ConcurrentMailSendBothLand — Scenario 2.
//
// Two subprocesses call `st mail send` to the same recipient at the
// same time. The mail file naming uses a UTC nanosecond stamp +
// from-id, so the two writes don't collide. Both messages must land
// (no torn writes, no clobbered file).
func TestMultiAgent_ConcurrentMailSendBothLand(t *testing.T) {
	env := newMultiEnv(t)

	results := runConcurrent(t, env, []procSpec{
		{
			AgentID: "agent-test-a", SessionID: "session-a-1",
			Args: []string{"mail", "send", "agent-test-c", "--kind", "warning", "--body", "hello from a"},
		},
		{
			AgentID: "agent-test-b", SessionID: "session-b-1",
			Args: []string{"mail", "send", "agent-test-c", "--kind", "warning", "--body", "hello from b"},
		},
	})

	for _, r := range results {
		if r.ExitCode != 0 {
			t.Fatalf("mail send unexpectedly failed: %s", r.summarize())
		}
	}

	mailbox := filepath.Join(env.Root, ".as", "mailbox", "agent-test-c")
	entries, err := os.ReadDir(mailbox)
	if err != nil {
		t.Fatalf("read mailbox %s: %v", mailbox, err)
	}
	yamls := 0
	for _, e := range entries {
		if e.IsDir() {
			continue // archive subdir
		}
		if strings.HasSuffix(e.Name(), ".yaml") {
			yamls++
		}
	}
	if yamls != 2 {
		t.Errorf("expected exactly 2 .yaml messages in mailbox, got %d (entries: %v)", yamls, entries)
	}

	// Each message body must be intact (no torn write).
	bodies := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(mailbox, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		bodies = append(bodies, string(body))
	}
	combined := strings.Join(bodies, "\n")
	if !strings.Contains(combined, "hello from a") || !strings.Contains(combined, "hello from b") {
		t.Errorf("expected both message bodies to land. got combined:\n%s", combined)
	}
}

// TestMultiAgent_ConcurrentMailArchiveExactlyOnce — Scenario 3.
//
// Pre-seed a single message via one `st mail send`. Two subprocesses
// then race `st mail archive <id>`; the OS-level os.Rename is atomic
// on the same filesystem, so exactly one wins. The message must land
// in archive/ exactly once and the original location is empty.
func TestMultiAgent_ConcurrentMailArchiveExactlyOnce(t *testing.T) {
	env := newMultiEnv(t)

	// Send one message and capture its id from stdout.
	send := runST(t, env, "agent-test-a", "session-a-1",
		"mail", "send", "agent-test-c", "--kind", "warning", "--body", "to-be-archived")
	if send.ExitCode != 0 {
		t.Fatalf("seed mail send failed: %s", send.summarize())
	}
	// Stdout: "Sent agent-test-a → agent-test-c (kind=info, id=<id>)"
	id := extractAfter(send.Stdout, "id=")
	id = strings.TrimRight(id, ")\n ")
	if id == "" {
		t.Fatalf("could not parse mail id from send stdout: %q", send.Stdout)
	}

	results := runConcurrent(t, env, []procSpec{
		{
			AgentID: "agent-test-c", SessionID: "session-c-1",
			Args: []string{"mail", "archive", id},
		},
		{
			AgentID: "agent-test-c", SessionID: "session-c-2",
			Args: []string{"mail", "archive", id},
		},
	})

	winners := 0
	losers := 0
	for _, r := range results {
		if r.ExitCode == 0 {
			winners++
		} else {
			losers++
		}
	}
	if winners != 1 || losers != 1 {
		t.Errorf("expected exactly one winner archiving, got winners=%d losers=%d\n  proc[0]: %s\n  proc[1]: %s",
			winners, losers, results[0].summarize(), results[1].summarize())
	}

	mailbox := filepath.Join(env.Root, ".as", "mailbox", "agent-test-c")
	archive := filepath.Join(mailbox, "archive")

	pendingCount := 0
	if entries, err := os.ReadDir(mailbox); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			pendingCount++
		}
	}
	if pendingCount != 0 {
		t.Errorf("expected 0 pending messages after archive, got %d", pendingCount)
	}

	archiveCount := 0
	if entries, err := os.ReadDir(archive); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			archiveCount++
		}
	}
	if archiveCount != 1 {
		t.Errorf("expected exactly 1 message in archive, got %d", archiveCount)
	}
}

// extractAfter returns the substring after the first occurrence of
// `marker`, up to but not including the next whitespace or end. Test
// helper for parsing CLI stdout.
func extractAfter(s, marker string) string {
	idx := strings.Index(s, marker)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(marker):]
	end := strings.IndexAny(rest, " \t\n)")
	if end < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}

// TestMultiAgent_StaleClaimSweepReleasesGhostsPreservesLive —
// Scenario 4.
//
// The agent registry's Sweep removes registrations whose PID is no
// longer alive. This test plants two registrations: one with the
// current process's PID (live) and one with a known-dead PID. Then
// it triggers a `st status` (which loads + sweeps via `agent.ListRegistrations`/
// `agent.Sweep` indirectly through `primeClaimState`-style code paths).
// Asserts: live registration survives, dead one is removed.
func TestMultiAgent_StaleClaimSweepReleasesGhostsPreservesLive(t *testing.T) {
	env := newMultiEnv(t)

	// Find a known-dead PID by spawning a true-process and reaping it.
	dead := exec.Command("/usr/bin/true")
	if err := dead.Run(); err != nil {
		t.Fatalf("spawn /usr/bin/true: %v", err)
	}
	deadPID := dead.Process.Pid

	livePID := os.Getpid()

	agentsDir := filepath.Join(env.Root, ".as", "agents")
	writeItem(t, filepath.Join(agentsDir, "agent-ghost.yaml"), fmt.Sprintf(`agent_id: agent-ghost
session_id: ghost-session
pid: %d
hostname: test-host
started_at: 2026-04-28T10:00:00Z
scope: ""
`, deadPID))
	writeItem(t, filepath.Join(agentsDir, "agent-live.yaml"), fmt.Sprintf(`agent_id: agent-live
session_id: live-session
pid: %d
hostname: test-host
started_at: 2026-04-28T10:00:00Z
scope: ""
`, livePID))

	// `st agent sweep` (or any command that calls agent.Sweep). The
	// public surface is `st agent prune` — verify by trying a
	// no-op-ish command first. agent.Sweep is also called from
	// primeClaimState in start.go, so `st start T-002` triggers it.
	r := runST(t, env, "agent-test-a", "session-a-1", "start", "T-002")
	if r.ExitCode != 0 {
		t.Fatalf("st start T-002 (sweep trigger) failed: %s", r.summarize())
	}

	if _, err := os.Stat(filepath.Join(agentsDir, "agent-ghost.yaml")); err == nil {
		t.Errorf("ghost registration should have been swept, still exists")
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error on ghost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "agent-live.yaml")); err != nil {
		t.Errorf("live registration was incorrectly swept: %v", err)
	}
}
