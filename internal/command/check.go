package command

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/quality"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/sprintinherit"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/validate"
)

func Check(s *store.Store, cfg *config.Config, quiet bool, fix bool) int {
	// syncErr captures an I-807 gate refusal from the fix-path autoSync.
	// Validation always runs to completion; the gate error propagates at
	// the final return alongside any schema/duplicate findings (I-821).
	var syncErr error

	// Auto-fix by default unless quiet mode (read-only for CI/hooks)
	// --fix flag is now redundant but kept for explicitness
	if !quiet {
		fixed := Fix(s, cfg)
		if fixed > 0 {
			fmt.Printf("\n\033[32m%d fix(es) applied\033[0m\n\n", fixed)
		}

		// I-472: clean duplicate-id drift between issues/ and archive/
		// before reporting. Same gate as Fix above — quiet/CI mode
		// stays read-only and surfaces drift as a check failure
		// instead of silently rewriting the working tree. Only same-
		// basename duplicates are auto-fixed; ID-collisions warn but
		// require human triage.
		var dupFixed int
		for _, d := range validate.DuplicateIDs(cfg.ItemDir(), cfg) {
			removed, err := s.RemoveStaleDuplicates(d.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  fix-failed: %s — %v\n", d.ID, err)
				continue
			}
			for _, p := range removed {
				dupFixed++
				fmt.Printf("  fixed: removed duplicate %s\n", p)
			}
		}
		if fixed > 0 || dupFixed > 0 {
			syncErr = autoSync(s, "st check --fix")
		}
	}

	var issues int

	// I-472: report any duplicate-id drift remaining after auto-fix.
	// In quiet/CI mode the fix step above is skipped so this surfaces
	// real duplicates as failures.
	for _, d := range validate.DuplicateIDs(cfg.ItemDir(), cfg) {
		issues++
		if !quiet {
			fmt.Printf("  \033[31m✗\033[0m duplicate id %s in %v\n", d.ID, d.Paths)
		}
	}

	// Validate each item
	for id, item := range s.All() {
		// Schema validation (includes type-specific required fields)
		r := validate.Item(item, cfg)
		for _, e := range r.Errors {
			issues++
			if !quiet {
				fmt.Printf("  \033[31m✗\033[0m %s\n", e)
			}
		}

		// Directory consistency
		path, ok := s.Path(id)
		if ok {
			dir := filepath.Dir(path)
			dr := validate.DirectoryConsistency(item, dir, cfg)
			for _, e := range dr.Errors {
				issues++
				if !quiet {
					fmt.Printf("  \033[31m✗\033[0m %s\n", e)
				}
			}
		}

		// Delivery/UAT gate
		gr := validate.DeliveryGate(item, cfg)
		for _, e := range gr.Errors {
			issues++
			if !quiet {
				fmt.Printf("  \033[31m✗\033[0m %s\n", e)
			}
		}
	}

	// Reciprocal dependency check
	depErrors := validate.ReciprocalDeps(s.All())
	for _, e := range depErrors {
		issues++
		if !quiet {
			fmt.Printf("  \033[31m✗\033[0m %s\n", e)
		}
	}

	// Cycle detection
	g := deps.Build(s.All(), cfg)
	cycles := g.DetectCycles()
	for _, cycle := range cycles {
		issues++
		if !quiet {
			fmt.Printf("  \033[31m✗\033[0m dependency cycle: %v\n", cycle)
		}
	}

	// I-681: sprint-inheritance drift. A non-terminal item that blocks an
	// active-sprint member but carries no sprint is being worked off the
	// in-progress sprint it belongs to. This is surfaced as a non-fatal
	// WARNING (not an `issues++` failure): the offending item is often
	// owned by another agent (the I-676 → T-203 case), and a hard fail
	// here would block every agent's session-start `st check` on a peer's
	// drift the current agent must not touch. The real enforcement is the
	// per-owner gate in `st start` plus auto-inherit in `st push`.
	//
	// Skipped entirely in quiet mode: that is the CI/session-hook
	// read-only fast-path, the warning has no output there, and the
	// registry.Load + Drift walk is non-essential I/O on that path.
	if !quiet {
		if reg, rerr := registry.Load(cfg.EpicsPath()); rerr == nil {
			for _, e := range sprintinherit.Drift(s.All(), g, reg, cfg) {
				fmt.Printf("  \033[33m⚠\033[0m %s\n", e)
			}
		} else {
			fmt.Fprintf(os.Stderr, "  warning: sprint-drift check skipped — registry unreadable: %v\n", rerr)
		}
	}

	// I-736: CLAUDE.md drift sentinel. Soft warnings — never fail.
	// Catches drift via git pull / hand-edits / pre-existing bloat that
	// bypasses hooks/claude-md-bloat-guard.sh. Skipped in quiet mode
	// (parity with sprint-drift warning model).
	if !quiet {
		claudeMdPath := filepath.Join(cfg.Root(), "claude-config", "CLAUDE.md")
		if _, err := os.Stat(claudeMdPath); err == nil {
			for _, f := range quality.ScanCLAUDEMd(claudeMdPath, 150, 200) {
				fmt.Printf("  \033[33m⚠\033[0m claude-md drift [%s] line %d: %s\n", f.Pattern, f.Line, f.Snippet)
			}
		}
	}

	// I-731: active-envs sentinel — surface the operator's declared
	// active envs at session-start + warn on stale declaration.
	// Pairs with claude-config/hooks/active-envs-guard.sh which is the
	// hard gate at PreToolUse time. This sentinel is purely cosmetic
	// (warn-only); the file is at <root>/.as/active-envs.yaml.
	if !quiet {
		activeEnvsPath := filepath.Join(cfg.Root(), ".as", "active-envs.yaml")
		if ae, perr := quality.ParseActiveEnvs(activeEnvsPath); perr == nil {
			activeList := strings.Join(ae.Active, ", ")
			if activeList == "" {
				activeList = "(none)"
			}
			tornList := strings.Join(ae.TornDown, ", ")
			if tornList == "" {
				tornList = "(none)"
			}
			fmt.Printf("  active_envs: \033[32m%s\033[0m\n", activeList)
			fmt.Printf("  torn_down:   \033[90m%s\033[0m\n", tornList)
			for _, v := range quality.ValidateActiveEnvs(ae, quality.ActiveEnvsValidateOpts{}) {
				fmt.Printf("  \033[33m⚠\033[0m active-envs %s: %s\n", v.Field, v.Message)
			}
		} else if !os.IsNotExist(perr) {
			// File exists but couldn't be opened — surface as a warning.
			fmt.Printf("  \033[33m⚠\033[0m active-envs.yaml unreadable: %v\n", perr)
		}
	}

	// Index.md coverage
	indexPath := cfg.IndexPath()
	indexContent, err := os.ReadFile(indexPath)
	if err == nil {
		indexErrors := validate.IndexCoverage(s.All(), string(indexContent), cfg)
		for _, e := range indexErrors {
			issues++
			if !quiet {
				fmt.Printf("  \033[31m✗\033[0m %s\n", e)
			}
		}
	}

	// Git status checks
	issues += checkGitStatus(cfg, quiet)

	// Summary
	if !quiet {
		if issues == 0 {
			fmt.Println("\033[32m✓\033[0m All checks passed")
		} else {
			fmt.Printf("\n\033[31m%d issue(s) found\033[0m\n", issues)
		}
	}

	if issues > 0 || syncErr != nil {
		return 1
	}
	return 0
}

// checkGitStatus verifies the workspace git state (uncommitted changes, behind remote).
func checkGitStatus(cfg *config.Config, quiet bool) int {
	var issues int
	root := cfg.Root()

	// Check for uncommitted agent-state changes
	itemDir := cfg.Paths.Root
	out, err := execGit(root, "status", "--porcelain", "--", itemDir+"/")
	if err == nil && len(out) > 0 {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		count := len(lines)
		issues++
		if !quiet {
			fmt.Printf("  \033[31m✗\033[0m %d uncommitted agent-state change(s)\n", count)
		}
	}

	// Check if behind remote
	_ = execGitNoOutput(root, "fetch", "--quiet")
	behind, err := execGit(root, "rev-list", "--count", "HEAD..@{upstream}")
	if err == nil {
		n := strings.TrimSpace(string(behind))
		if n != "0" && n != "" {
			issues++
			if !quiet {
				fmt.Printf("  \033[31m✗\033[0m local is %s commit(s) behind remote\n", n)
			}
		}
	}

	return issues
}

// execGit runs a git command and returns stdout.
var execGit = func(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	err := cmd.Run()
	return stdout.Bytes(), err
}

// execGitNoOutput runs a git command silently.
var execGitNoOutput = func(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}
