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
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/validate"
)

func Check(s *store.Store, cfg *config.Config, quiet bool, fix bool) int {
	// If --fix, apply auto-repairs first
	if fix {
		fixed := Fix(s, cfg)
		if fixed > 0 {
			fmt.Printf("\n\033[32m%d fix(es) applied\033[0m\n\n", fixed)
		} else {
			fmt.Println("\033[32mNo fixable issues found\033[0m")
		}
	}

	var issues int

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

			// If not in fix mode, check for fixable issues and suggest --fix
			if !fix {
				fixableCount, descs := FixableSummary(s, cfg)
				if fixableCount > 0 {
					fmt.Printf("\n\033[33m%d auto-fixable:\033[0m %s\n", fixableCount, strings.Join(descs, ", "))
					fmt.Println("Run \033[1mas check --fix\033[0m to repair")
				}
			}
		}
	}

	if issues > 0 {
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
