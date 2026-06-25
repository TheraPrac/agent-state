package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
)

// fleetRepoState is one agent's clone of one repo, classified against the
// authoritative origin/main tip. I-1590.
type fleetRepoState struct {
	repo      string
	branch    string
	headShort string
	behind    int // commits behind origin/main; -1 = unknown
	onMain    bool
	dirty     bool
	missing   bool
}

// ok reports a clone that needs no action: present, on main, clean, current.
func (s fleetRepoState) ok() bool {
	return !s.missing && s.onMain && !s.dirty && s.behind == 0
}

// label renders the clone's state for the status table.
func (s fleetRepoState) label() string {
	switch {
	case s.missing:
		return "missing"
	case !s.onMain:
		return "not-on-main (" + s.branch + ")"
	case s.dirty && s.behind > 0:
		return fmt.Sprintf("behind %d + dirty", s.behind)
	case s.dirty:
		return "dirty"
	case s.behind < 0:
		return "drifted (?)"
	case s.behind == 0:
		return "current"
	default:
		return fmt.Sprintf("behind %d", s.behind)
	}
}

// discoverFleetAgents returns the sorted theraprac-agent-* roots under the fleet
// parent (the directory containing the running agent's own root).
func discoverFleetAgents(cfg *config.Config) ([]string, error) {
	return agentDirsUnder(filepath.Dir(cfg.AgentRoot()))
}

// agentDirsUnder returns the sorted theraprac-agent-* subdirectories of parent.
func agentDirsUnder(parent string) ([]string, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "theraprac-agent-") {
			roots = append(roots, filepath.Join(parent, e.Name()))
		}
	}
	sort.Strings(roots)
	return roots, nil
}

// fleetRepos returns the repo dir names to audit per agent: the per-agent code
// clones (theraprac-api/web/infra/as), deduped in order. theraprac-workspace is
// deliberately excluded — it perpetually carries uncommitted runtime state
// (.as/queue.yaml, stacks, drift markers) so it always reads "dirty", and it has
// its own continuous-sync path (session-start I-1321 + `st sync`). Auditing it
// here only adds always-dirty noise that masks real code/as drift.
func fleetRepos(cfg *config.Config) []string {
	seen := map[string]bool{}
	var repos []string
	if cfg.Worktree != nil {
		for _, r := range cfg.Worktree.Repos {
			if r != "" && !seen[r] {
				seen[r] = true
				repos = append(repos, r)
			}
		}
	}
	return repos
}

// authoritativeMainSHA fetches origin/main in refClone (best-effort) and returns
// the resolved origin/main tip SHA. ok=false when the ref cannot be resolved.
func authoritativeMainSHA(refClone string) (string, bool) {
	_, _ = runGit(refClone, "fetch", "-q", "origin", "main") // best-effort refresh
	out, err := runGit(refClone, "rev-parse", "origin/main")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// classifyClone inspects an agent's repo clone against authSHA. The behind count
// is computed in refClone's object store — all clones share the same remote
// history, so an agent sitting on an older main commit is an ancestor present in
// the (freshly fetched) running agent's clone. Any git error degrades the count
// to -1 ("drifted (?)") rather than failing the audit.
func classifyClone(cloneDir, repo, authSHA, refClone string) fleetRepoState {
	st := fleetRepoState{repo: repo, behind: -1}
	if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
		st.missing = true
		return st
	}
	sha, branch, ok := readAsCloneHEAD(cloneDir)
	if !ok {
		st.missing = true
		return st
	}
	st.branch = branch
	st.headShort = shortSHA(sha)
	st.onMain = branch == "main" || branch == "master"
	if out, err := runGit(cloneDir, "status", "--porcelain"); err == nil && strings.TrimSpace(out) != "" {
		st.dirty = true
	}
	if authSHA != "" {
		if sha == authSHA {
			st.behind = 0
		} else if out, err := runGit(refClone, "rev-list", "--count", sha+".."+authSHA); err == nil {
			if n, e := strconv.Atoi(strings.TrimSpace(out)); e == nil {
				st.behind = n
			}
		}
	}
	return st
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// fleetContext resolves the agents, repos, and per-repo authoritative tip shared
// by status and sync. The authoritative tip is fetched once per repo in the
// running agent's own clone (one RTT per repo, not per agent).
func fleetContext(cfg *config.Config) (roots, repos []string, auth map[string]string, err error) {
	roots, err = discoverFleetAgents(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	repos = fleetRepos(cfg)
	auth = map[string]string{}
	self := cfg.AgentRoot()
	for _, repo := range repos {
		if sha, ok := authoritativeMainSHA(filepath.Join(self, repo)); ok {
			auth[repo] = sha
		}
	}
	return roots, repos, auth, nil
}

// FleetStatus audits every theraprac-agent-* clone: branch, HEAD, and how far it
// is behind origin/main. Read-only apart from the per-repo fetch. I-1590.
func FleetStatus(cfg *config.Config) int {
	roots, repos, auth, err := fleetContext(cfg)
	if err != nil || len(roots) == 0 {
		fmt.Fprintf(os.Stderr, "fleet: no agents found under %s\n", filepath.Dir(cfg.AgentRoot()))
		return 1
	}

	fmt.Printf("%s━━━ FLEET STATUS ━━━%s\n", cBoldW, cReset)
	drifted := 0
	for _, root := range roots {
		clean := true
		var states []fleetRepoState
		for _, repo := range repos {
			s := classifyClone(filepath.Join(root, repo), repo, auth[repo], filepath.Join(cfg.AgentRoot(), repo))
			states = append(states, s)
			if !s.missing && !s.ok() {
				clean = false
			}
		}
		marker := fmt.Sprintf("%s✓%s", cGreen, cReset)
		if !clean {
			marker = fmt.Sprintf("%s⚠%s", cYellow, cReset)
			drifted++
		}
		fmt.Printf("\n%s %s%s%s\n", marker, cBold, filepath.Base(root), cReset)
		for _, s := range states {
			if s.missing {
				continue
			}
			color := cGreen
			if !s.ok() {
				color = cYellow
			}
			fmt.Printf("    %-20s %s%-24s%s %s\n", s.repo, color, s.label(), cReset, s.headShort)
		}
	}
	fmt.Printf("\n%d/%d agents fully current with origin/main.\n", len(roots)-drifted, len(roots))
	return 0
}

// FleetSync fast-forwards every clean, on-main clone to origin/main and rebuilds
// the `as` binary when that clone advanced. Never forces: dirty, diverged, or
// off-main clones are reported and left untouched. I-1590.
func FleetSync(cfg *config.Config) int {
	roots, repos, auth, err := fleetContext(cfg)
	if err != nil || len(roots) == 0 {
		fmt.Fprintf(os.Stderr, "fleet: no agents found under %s\n", filepath.Dir(cfg.AgentRoot()))
		return 1
	}

	fmt.Printf("%s━━━ FLEET SYNC ━━━%s\n", cBoldW, cReset)
	rc := 0
	for _, root := range roots {
		fmt.Printf("\n%s%s%s\n", cBold, filepath.Base(root), cReset)
		for _, repo := range repos {
			cloneDir := filepath.Join(root, repo)
			s := classifyClone(cloneDir, repo, auth[repo], filepath.Join(cfg.AgentRoot(), repo))
			action, detail, advanced := syncClone(cloneDir, s)
			if action == "skip" && detail == "" {
				continue // missing clone — nothing to report
			}
			color := cGreen
			if action == "skip" {
				color = cYellow
			} else if action == "fail" {
				color = cRed
				rc = 1
			}
			fmt.Printf("    %-20s %s%s%s %s\n", repo, color, action, cReset, detail)
			if advanced && repo == "as" {
				if out, err := rebuildAsBinary(root); err != nil {
					fmt.Printf("      %srebuild failed%s: %v\n%s", cRed, cReset, err, fleetIndent(out))
					rc = 1
				} else {
					fmt.Printf("      rebuilt bin/st\n")
				}
			}
		}
	}
	return rc
}

// syncClone fast-forwards a single clone to origin/main when it is safe (on main,
// clean, behind), and classifies the outcome. It never forces: dirty, diverged,
// and off-main clones are skipped with a reason. Returns the action verb
// ("current"/"ff'd"/"skip"/"fail"), a human detail string, and whether the clone
// advanced (used to trigger an as-binary rebuild). A missing clone returns
// ("skip", "", false) so the caller can drop it silently.
func syncClone(cloneDir string, s fleetRepoState) (action, detail string, advanced bool) {
	switch {
	case s.missing:
		return "skip", "", false
	case s.ok():
		return "current", "", false
	case !s.onMain:
		return "skip", "— not on main (" + s.branch + ")", false
	case s.dirty:
		return "skip", "— dirty working tree", false
	}
	if _, err := runGit(cloneDir, "fetch", "-q", "origin", "main"); err != nil {
		return "fail", fmt.Sprintf("— fetch: %v", err), false
	}
	if _, err := runGit(cloneDir, "merge", "--ff-only", "origin/main"); err != nil {
		return "skip", "— not fast-forwardable (diverged); sync manually", false
	}
	head, _ := runGit(cloneDir, "rev-parse", "HEAD")
	return "ff'd", "→ " + shortSHA(head), true
}

// rebuildAsBinary runs `make -C <agentRoot>/as install` to rebuild that agent's
// per-agent st binary after its as clone advanced.
func rebuildAsBinary(agentRoot string) (string, error) {
	cmd := exec.Command("make", "-C", filepath.Join(agentRoot, "as"), "install")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// fleetIndent prefixes each line of s with six spaces for nested output.
func fleetIndent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("      " + line + "\n")
	}
	return b.String()
}
