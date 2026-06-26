package command

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/buildinfo"
	"github.com/theraprac/agent-state/internal/config"
)

// driftWarnTTL throttles the in-session drift warning so a tight loop of
// `st` calls doesn't smear stderr. 10 minutes is short enough that an
// idle agent gets re-reminded after a coffee break, long enough that
// `for i in $(...); do st show $i; done` only warns once.
const driftWarnTTL = 10 * time.Minute

// driftWarnMarker stores the warning's most recent (binary, local) pair
// as two lines. The pair-keyed contents auto-clear the throttle when
// either side moves: a fresh `make install` advances buildinfo.Commit,
// a fresh `as/` pull advances local, and either causes the next call
// to find a stale marker and re-emit.
const driftWarnMarkerName = "drift-warned"

// MaybeWarnSingleAgentDrift emits a single stderr line when the running
// `st` binary's stamped commit differs from the local `as/` clone's
// HEAD on `main`/`master`. Closes the gap left by the session-start
// auto-rebuild (I-439, I-477): the hook only fires on startup/resume,
// so a long-running session can run a stale binary for days. Agent-b
// reported 3 days of silent evidence-upload failures from exactly this
// gap (I-624 SBAR).
//
// Silent in five cases:
//   - buildinfo.Commit is "" or "unknown" — unstamped binary, no
//     comparison is possible
//   - sibling `as/` clone is missing — running from the canonical
//     workspace, not a per-agent layout
//   - `as/` is on a feature branch — operator is intentionally
//     diverged; warning would be noisy during in-flight st-CLI work
//   - local SHA equals buildinfo.Commit — no drift
//   - throttle marker matches the current (binary, local) pair and
//     mtime is within driftWarnTTL
//
// All filesystem reads are direct (no `git` subprocess) so the cost is
// two open(2) calls per invocation on the hot path.
func MaybeWarnSingleAgentDrift(cfg *config.Config, w io.Writer) {
	binCommit := buildinfo.Commit
	if binCommit == "" || binCommit == "unknown" {
		return
	}

	// I-778: RepoParent() resolves per-agent repo parent via .as/agent-workspace.yaml
	// (honoring worktree.parent_dir overrides) so the drift warning
	// compares against the running agent's as clone rather than a peer
	// agent's under an ST_ROOT env leak.
	asClone := filepath.Join(cfg.RepoParent(), "as")
	localSHA, branch, ok := readAsCloneHEAD(asClone)
	if !ok {
		return
	}
	if branch != "" && branch != "main" && branch != "master" {
		return
	}
	if localSHA == "" || localSHA == binCommit {
		return
	}

	marker := filepath.Join(cfg.Root(), ".as", driftWarnMarkerName)
	if isFreshDriftMarker(marker, binCommit, localSHA) {
		return
	}

	short := func(s string) string {
		if len(s) >= 8 {
			return s[:8]
		}
		return s
	}
	fmt.Fprintf(w, "warning: st binary at %s but as clone at %s — run `cd %s && git pull && make install`\n",
		short(binCommit), short(localSHA), asClone)

	_ = writeDriftMarker(marker, binCommit, localSHA)
}

// readAsCloneHEAD reads `<asClone>/.git/HEAD` without forking git.
// Falls back to `.git/packed-refs` when the loose ref file is absent —
// `git gc` (and the periodic auto-gc most agent clones hit) moves refs
// into the packed file, so reading loose-only would miss any agent
// clone that has been GC'd at least once.
func readAsCloneHEAD(asClone string) (sha, branch string, ok bool) {
	headPath := filepath.Join(asClone, ".git", "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		return "", "", false
	}
	head := strings.TrimSpace(string(data))
	if !strings.HasPrefix(head, "ref: ") {
		// Detached HEAD — HEAD file already holds the SHA.
		return head, "", true
	}
	ref := strings.TrimPrefix(head, "ref: ")
	branch = strings.TrimPrefix(ref, "refs/heads/")

	refPath := filepath.Join(asClone, ".git", ref)
	if refData, err := os.ReadFile(refPath); err == nil {
		return strings.TrimSpace(string(refData)), branch, true
	}
	if sha := lookupPackedRef(filepath.Join(asClone, ".git", "packed-refs"), ref); sha != "" {
		return sha, branch, true
	}
	return "", "", false
}

// lookupPackedRef scans `.git/packed-refs` for the first non-peeled
// line matching ref. The file format is one entry per line: "<sha>
// <full-ref>", with optional comment lines starting with `#` and
// optional peeled-tag lines starting with `^`. Returns "" when the
// file is missing or the ref is not present.
func lookupPackedRef(packedPath, ref string) string {
	data, err := os.ReadFile(packedPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ref {
			return fields[0]
		}
	}
	return ""
}

// isFreshDriftMarker reports whether the marker file at path records
// the same (binary, local) pair and was written within driftWarnTTL.
// Any read or parse failure is treated as "not fresh" so the warning
// re-fires defensively.
func isFreshDriftMarker(path, binCommit, localSHA string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) >= driftWarnTTL {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(parts) != 2 {
		return false
	}
	return parts[0] == binCommit && parts[1] == localSHA
}

// writeDriftMarker records the pair so subsequent calls within
// driftWarnTTL skip the warning. Best-effort; an error here just means
// the next call re-emits, which is harmless.
func writeDriftMarker(path, binCommit, localSHA string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(binCommit+"\n"+localSHA+"\n"), 0o644)
}
