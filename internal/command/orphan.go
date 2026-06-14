package command

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OrphanStash scans the workspace for dirty agent-state files not owned by
// agentID, stashes each one with a recoverable label, and prints a banner
// block describing what was stashed. Files owned by agentID or with no
// assigned_to field are left untouched.
//
// T-403: belt-and-braces companion to T-401 (ownership gate) and T-402
// (atomic writes). Handles crash leftovers that bypass both.
func OrphanStash(workspaceRoot, itemDir, agentID string) []string {
	var stashed []string

	out, err := execGitOrphan(workspaceRoot, "status", "--porcelain", "--", itemDir+"/")
	if err != nil || len(out) == 0 {
		return nil
	}

	today := time.Now().UTC().Format("2006-01-02")
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if len(line) < 4 {
			continue
		}
		relPath := strings.TrimSpace(line[3:])
		if relPath == "" {
			continue
		}

		owner := readAssignedTo(filepath.Join(workspaceRoot, relPath))

		// Leave files with no owner or owned by the current agent.
		if owner == "" || owner == agentID {
			continue
		}

		label := fmt.Sprintf("st-orphan: %s owned-by:%s dropped-by:%s date:%s",
			relPath, owner, agentID, today)
		stashOut, stashErr := execGitOrphanCapture(workspaceRoot, "stash", "push",
			"-m", label, "--", relPath)
		if stashErr != nil {
			fmt.Fprintf(os.Stderr, "orphan: failed to stash %s: %v\n", relPath, stashErr)
			continue
		}
		ref := strings.TrimSpace(string(stashOut))
		stashed = append(stashed, fmt.Sprintf("  %s → %s (owned by %s)", relPath, ref, owner))
	}

	if len(stashed) > 0 {
		fmt.Printf("orphan: stashed %d file(s) not owned by %s:\n", len(stashed), agentID)
		for _, s := range stashed {
			fmt.Println(s)
		}
		fmt.Printf("  recover: git -C %q stash show <ref> | git -C %q stash apply <ref>\n",
			workspaceRoot, workspaceRoot)
		fmt.Printf("  list: st orphan list --workspace %q\n", workspaceRoot)
	}
	return stashed
}

// OrphanList prints git stashes in workspaceRoot whose messages begin with
// "st-orphan:" and the recovery command for each.
func OrphanList(workspaceRoot string) {
	out, err := execGitOrphan(workspaceRoot, "stash", "list")
	if err != nil {
		fmt.Fprintf(os.Stderr, "orphan list: %v\n", err)
		return
	}
	if len(out) == 0 {
		fmt.Println("orphan: no stashes found")
		return
	}

	found := 0
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		// Format: "stash@{N}: On branch: st-orphan: ..."
		if !strings.Contains(line, "st-orphan:") {
			continue
		}
		found++
		// Extract stash ref (first field before first colon-space).
		parts := strings.SplitN(line, ": ", 2)
		ref := parts[0]
		fmt.Printf("%s\n", line)
		fmt.Printf("  recover: git -C %q stash show %s\n", workspaceRoot, ref)
		fmt.Printf("           git -C %q stash apply %s\n", workspaceRoot, ref)
	}
	if found == 0 {
		fmt.Println("orphan: no st-orphan stashes found")
	}
}

// readAssignedTo reads the `assigned_to:` field from a YAML item file.
// Returns "" if the file cannot be read or the field is absent/empty.
func readAssignedTo(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "assigned_to:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "assigned_to:"))
			// Strip YAML quotes if present.
			val = strings.Trim(val, `"'`)
			return val
		}
	}
	return ""
}

// execGitOrphan runs a git command in dir and returns stdout.
var execGitOrphan = func(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = nil
	err := cmd.Run()
	return buf.Bytes(), err
}

// execGitOrphanCapture runs a git command and returns combined stdout+stderr.
var execGitOrphanCapture = func(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return out, err
}
