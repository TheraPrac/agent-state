package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
)

// closeACCheck enforces I-1486: a non-force `done` close must have all of the
// item's `cmd:` acceptance_criteria actually pass. Returns "" when the gate is
// satisfied (or legitimately bypassed), else a printable block message.
//
// It reuses the UAT acceptance-criteria evaluation path (evaluateAcceptance
// criteria → evaluateCriterion → rewriteACPaths + runCmdInDirWithTimeout) so a
// `cmd:` AC is judged identically at close and at `st uat`, including I-144's
// filtered-test override. Only `cmd:` criteria gate; prose/auto/pending criteria
// warn elsewhere but never block here.
//
// Escapes (mirroring the --evidence-skip pattern):
//   - SkipACRequested + non-empty SkipAC reason → audit-logged bypass.
//   - NoAC → permits closing an item that has ZERO `cmd:` AC (otherwise blocked,
//     so "done with nothing verifiable" is a deliberate, explicit choice).
//   - --force bypasses this gate entirely (handled by the caller).
func closeACCheck(item *model.Item, cfg *config.Config, opts CloseOpts) string {
	if opts.SkipACRequested {
		if strings.TrimSpace(opts.SkipAC) == "" {
			return "close: --skip-ac requires a non-empty reason (why the acceptance criteria can't be verified at close)"
		}
		logACSkip(cfg, item.ID, opts.SkipAC)
		return ""
	}

	// Build the AC runner. Tests inject ACRunCmd; production uses the same
	// worktree-scoped runner UAT builds.
	runCmd := opts.ACRunCmd
	if runCmd == nil {
		runDir := cfg.Root()
		if wtBase := cfg.WorktreeForItem(item.ID); wtBase != "" {
			if _, err := os.Stat(wtBase); err == nil {
				runDir = wtBase
			}
		}
		runCmd = func(cmd string) ([]byte, int, error) {
			cmd = rewriteACPaths(cfg, item.ID, runDir, cmd)
			return runCmdInDirWithTimeout(runDir, cmd, 10*time.Minute)
		}
	}

	results := evaluateAcceptanceCriteria(item, cfg, runCmd)

	cmdCount := 0
	var failures []string
	for _, r := range results {
		if r.Mode != "cmd" || r.Skipped {
			continue
		}
		cmdCount++
		if !r.Passed {
			failures = append(failures, fmt.Sprintf("  ✗ %s — %s", r.Label, r.Detail))
		}
	}

	if cmdCount == 0 {
		if opts.NoAC {
			return ""
		}
		return fmt.Sprintf("close: %s has no `cmd:` acceptance_criteria to verify.\n"+
			"  A verified `done` requires at least one runnable AC. Add one, or close with\n"+
			"  --no-ac to record that this item has nothing automatically verifiable,\n"+
			"  or --skip-ac \"<reason>\" / --force to bypass.", item.ID)
	}

	if len(failures) > 0 {
		return fmt.Sprintf("close: %s acceptance_criteria did not pass (%d of %d cmd AC failing):\n%s\n"+
			"  fix and re-close, or bypass with --skip-ac \"<reason>\" (audited) / --force.",
			item.ID, len(failures), cmdCount, strings.Join(failures, "\n"))
	}
	return ""
}

// logACSkip appends a one-line audit record of a --skip-ac bypass to
// .as/close-ac-skip.log, modeled on logEvidenceSkip. Non-fatal on write error.
func logACSkip(cfg *config.Config, id, reason string) {
	logPath := filepath.Join(cfg.Root(), ".as", "close-ac-skip.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "close-ac-skip: could not write audit log: %v\n", err)
		return
	}
	defer f.Close()
	itemRef := id
	if itemRef == "" {
		itemRef = "(unknown)"
	}
	fmt.Fprintf(f, "%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), itemRef, reason)
}
