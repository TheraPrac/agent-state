package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/buildinfo"
	"github.com/jfinlinson/agent-state/internal/config"
)

// withStampedBinary temporarily overrides buildinfo.Commit so a test
// can exercise the stamped/unstamped branches without rebuilding.
// The test process's package-level Commit is shared, so restore it
// in a defer.
func withStampedBinary(t *testing.T, commit string) {
	t.Helper()
	prev := buildinfo.Commit
	buildinfo.Commit = commit
	t.Cleanup(func() { buildinfo.Commit = prev })
}

// newDriftLayout builds an agent-style directory layout in tmp:
//
//	<root>/
//	  workspace/.as/config.yaml   ← cfg.Root() returns workspace
//	  as/.git/HEAD                ← sibling clone the drift check inspects
//
// localSHA controls what the as/.git ref resolves to; branch controls
// which ref HEAD points at (empty branch = detached HEAD).
func newDriftLayout(t *testing.T, branch, localSHA string) (cfg *config.Config, workspace, asClone string) {
	t.Helper()
	root := t.TempDir()
	workspace = filepath.Join(root, "workspace")
	asClone = filepath.Join(root, "as")

	if err := os.MkdirAll(filepath.Join(workspace, ".as"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".as", "config.yaml"),
		[]byte("paths:\n  root: .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(workspace)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg = c

	if err := os.MkdirAll(filepath.Join(asClone, ".git", "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if branch == "" {
		// Detached HEAD — HEAD file contains the SHA directly.
		if err := os.WriteFile(filepath.Join(asClone, ".git", "HEAD"),
			[]byte(localSHA+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		ref := "refs/heads/" + branch
		if err := os.WriteFile(filepath.Join(asClone, ".git", "HEAD"),
			[]byte("ref: "+ref+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		refPath := filepath.Join(asClone, ".git", ref)
		if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(refPath, []byte(localSHA+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, workspace, asClone
}

func TestSingleAgentDriftSilentUnstamped(t *testing.T) {
	withStampedBinary(t, "unknown")
	cfg, _, _ := newDriftLayout(t, "main", "bbbb2222"+strings.Repeat("0", 32))

	var buf bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent on unstamped binary, got: %s", buf.String())
	}
}

func TestSingleAgentDriftSilentBinaryCommitEmpty(t *testing.T) {
	withStampedBinary(t, "")
	cfg, _, _ := newDriftLayout(t, "main", "bbbb2222"+strings.Repeat("0", 32))

	var buf bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent on empty buildinfo.Commit, got: %s", buf.String())
	}
}

func TestSingleAgentDriftSilentNoAsClone(t *testing.T) {
	withStampedBinary(t, "aaaa1111"+strings.Repeat("0", 32))
	// Build a workspace WITHOUT a sibling as/ clone.
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, ".as"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".as", "config.yaml"),
		[]byte("paths:\n  root: .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(workspace)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	var buf bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent when as/ clone absent, got: %s", buf.String())
	}
}

func TestSingleAgentDriftSilentOnFeatureBranch(t *testing.T) {
	withStampedBinary(t, "aaaa1111"+strings.Repeat("0", 32))
	cfg, _, _ := newDriftLayout(t, "feat/something", "bbbb2222"+strings.Repeat("0", 32))

	var buf bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent on feature branch (in-flight work), got: %s", buf.String())
	}
}

func TestSingleAgentDriftSilentWhenMatching(t *testing.T) {
	sha := "aaaa1111" + strings.Repeat("0", 32)
	withStampedBinary(t, sha)
	cfg, _, _ := newDriftLayout(t, "main", sha)

	var buf bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent when commits match, got: %s", buf.String())
	}
}

func TestSingleAgentDriftWarnsOnDivergence(t *testing.T) {
	binSHA := "aaaa1111" + strings.Repeat("0", 32)
	localSHA := "bbbb2222" + strings.Repeat("0", 32)
	withStampedBinary(t, binSHA)
	cfg, _, asClone := newDriftLayout(t, "main", localSHA)

	var buf bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf)
	out := buf.String()
	if !strings.Contains(out, "warning: st binary at aaaa1111") {
		t.Errorf("expected warning with short binary sha, got: %s", out)
	}
	if !strings.Contains(out, "as clone at bbbb2222") {
		t.Errorf("expected warning with short local sha, got: %s", out)
	}
	if !strings.Contains(out, asClone) {
		t.Errorf("expected fix hint to reference clone path %s, got: %s", asClone, out)
	}
	if !strings.Contains(out, "git pull && make install") {
		t.Errorf("expected fix hint, got: %s", out)
	}
}

func TestSingleAgentDriftHandlesDetachedHEAD(t *testing.T) {
	binSHA := "aaaa1111" + strings.Repeat("0", 32)
	localSHA := "cccc3333" + strings.Repeat("0", 32)
	withStampedBinary(t, binSHA)
	// Empty branch = detached HEAD; the check still fires because the
	// "skip feature branches" rule only triggers when branch != main/master,
	// and detached HEAD reports branch == "".
	cfg, _, _ := newDriftLayout(t, "", localSHA)

	var buf bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf)
	if !strings.Contains(buf.String(), "warning: st binary at aaaa1111") {
		t.Errorf("expected warning even on detached HEAD when SHA differs, got: %s", buf.String())
	}
}

func TestSingleAgentDriftThrottlesWithinTTL(t *testing.T) {
	binSHA := "aaaa1111" + strings.Repeat("0", 32)
	localSHA := "bbbb2222" + strings.Repeat("0", 32)
	withStampedBinary(t, binSHA)
	cfg, workspace, _ := newDriftLayout(t, "main", localSHA)

	// First call emits.
	var buf1 bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf1)
	if buf1.Len() == 0 {
		t.Fatalf("first call should emit warning")
	}

	// Marker file should exist with the (bin, local) pair.
	marker := filepath.Join(workspace, ".as", driftWarnMarkerName)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file should exist after warn: %v", err)
	}

	// Second call within TTL should be silent.
	var buf2 bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf2)
	if buf2.Len() != 0 {
		t.Errorf("second call within TTL should be throttled, got: %s", buf2.String())
	}
}

func TestSingleAgentDriftReWarnsAfterTTL(t *testing.T) {
	binSHA := "aaaa1111" + strings.Repeat("0", 32)
	localSHA := "bbbb2222" + strings.Repeat("0", 32)
	withStampedBinary(t, binSHA)
	cfg, workspace, _ := newDriftLayout(t, "main", localSHA)

	// First emit.
	var buf1 bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf1)
	if buf1.Len() == 0 {
		t.Fatalf("first call should emit warning")
	}

	// Backdate marker past TTL.
	marker := filepath.Join(workspace, ".as", driftWarnMarkerName)
	past := time.Now().Add(-2 * driftWarnTTL)
	if err := os.Chtimes(marker, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Next call should re-emit.
	var buf2 bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf2)
	if buf2.Len() == 0 {
		t.Errorf("call after TTL expiry should re-emit warning")
	}
}

func TestSingleAgentDriftReWarnsWhenPairChanges(t *testing.T) {
	binSHA := "aaaa1111" + strings.Repeat("0", 32)
	localSHA1 := "bbbb2222" + strings.Repeat("0", 32)
	withStampedBinary(t, binSHA)
	cfg, workspace, asClone := newDriftLayout(t, "main", localSHA1)

	// First emit on (binSHA, localSHA1).
	var buf1 bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf1)
	if buf1.Len() == 0 {
		t.Fatalf("first call should emit warning")
	}

	// Advance local SHA — the marker now records a stale pair, even
	// though mtime is still fresh.
	localSHA2 := "cccc3333" + strings.Repeat("0", 32)
	refPath := filepath.Join(asClone, ".git", "refs", "heads", "main")
	if err := os.WriteFile(refPath, []byte(localSHA2+"\n"), 0o644); err != nil {
		t.Fatalf("update local ref: %v", err)
	}

	// Marker is still on disk with the old pair; new pair triggers re-emit.
	var buf2 bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf2)
	if !strings.Contains(buf2.String(), "as clone at cccc3333") {
		t.Errorf("expected re-warn with new local sha after pair changed, got: %s", buf2.String())
	}
	// Sanity: marker must still exist (now bumped to the new pair).
	marker := filepath.Join(workspace, ".as", driftWarnMarkerName)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker should be updated, not removed: %v", err)
	}
}

func TestSingleAgentDriftMissingAsCloneHEAD(t *testing.T) {
	if _, _, ok := readAsCloneHEAD("/path/that/does/not/exist"); ok {
		t.Errorf("expected ok=false for missing as clone")
	}
}

// Packed refs case: `git gc` moves loose refs into .git/packed-refs.
// readAsCloneHEAD must fall back to that file when the loose ref is
// absent, otherwise the drift warning silently no-ops on any clone
// that's been auto-gc'd — which is most active clones.
func TestSingleAgentDriftWarnsWhenRefIsPacked(t *testing.T) {
	binSHA := "aaaa1111" + strings.Repeat("0", 32)
	localSHA := "bbbb2222" + strings.Repeat("0", 32)
	withStampedBinary(t, binSHA)
	cfg, _, asClone := newDriftLayout(t, "main", localSHA)

	// Simulate `git gc`: remove the loose ref and write packed-refs.
	if err := os.Remove(filepath.Join(asClone, ".git", "refs", "heads", "main")); err != nil {
		t.Fatalf("removing loose ref: %v", err)
	}
	packed := "# pack-refs with: peeled fully-peeled sorted\n" +
		localSHA + " refs/heads/main\n" +
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef refs/tags/v0.1\n" +
		"^cafebabecafebabecafebabecafebabecafebabe\n"
	if err := os.WriteFile(filepath.Join(asClone, ".git", "packed-refs"),
		[]byte(packed), 0o644); err != nil {
		t.Fatalf("writing packed-refs: %v", err)
	}

	var buf bytes.Buffer
	MaybeWarnSingleAgentDrift(cfg, &buf)
	if !strings.Contains(buf.String(), "as clone at bbbb2222") {
		t.Errorf("expected warning resolved from packed-refs, got: %s", buf.String())
	}
}

func TestSingleAgentDriftPackedRefsParsingSkipsCommentsAndPeels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "packed-refs")
	contents := "# pack-refs with: peeled\n" +
		"^aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"\n" +
		"1111111111111111111111111111111111111111 refs/tags/v0\n" +
		"2222222222222222222222222222222222222222 refs/heads/main\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lookupPackedRef(path, "refs/heads/main"); got != "2222222222222222222222222222222222222222" {
		t.Errorf("lookupPackedRef = %q, want the main sha", got)
	}
	if got := lookupPackedRef(path, "refs/heads/missing"); got != "" {
		t.Errorf("lookupPackedRef for missing ref = %q, want empty", got)
	}
}
