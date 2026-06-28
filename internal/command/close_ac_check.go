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

// closeACCheck enforces I-1486: a non-force `done` close requires that the
// item's acceptance_criteria were actually verified by `st uat`. It does NOT
// re-run the AC at close — close runs post-merge, when the item's worktree is
// usually pruned, so re-running would execute against the wrong tree (or fail
// flaky/slow) and is redundant with the `st uat` that the delivery loop runs
// before merge. Instead it requires the `testing_evidence.uat` marker that
// `st uat` writes (keyed on the AC results) to read `pass`.
//
// Returns "" when satisfied (or legitimately bypassed via --skip-ac), else a
// printable block message. --force bypasses this gate entirely (handled by the
// caller). The --skip-ac audit record is written by the caller AFTER the close
// actually commits, so a close that later fails another gate leaves no phantom
// bypass entry.
func closeACCheck(item *model.Item, opts CloseOpts) string {
	// Marker check FIRST: a genuinely-verified item never needs (and must not be
	// recorded as) a skip, so passing --skip-ac on an already-passing item is a
	// no-op pass, not an audited bypass (review).
	if uatMarkerPasses(item) {
		return ""
	}
	if opts.SkipACRequested {
		if strings.TrimSpace(opts.SkipAC) == "" {
			return "close: --skip-ac requires a non-empty reason (why acceptance_criteria can't be verified via st uat)"
		}
		return ""
	}

	val, ok := getNestedField(item, "testing_evidence", "uat")
	detail := "no `st uat` run recorded"
	if ok {
		detail = "last st uat: " + strings.TrimSpace(val)
	}
	return fmt.Sprintf("close: %s has no verified acceptance_criteria (%s).\n"+
		"  Run `st uat %s` first — it evaluates every acceptance criterion and records the pass — then re-close.\n"+
		"  bypass: --skip-ac \"<reason>\" (audited) or --force.", item.ID, detail, item.ID)
}

// uatMarkerPasses reports whether the item carries a passing st-uat marker.
func uatMarkerPasses(item *model.Item) bool {
	val, ok := getNestedField(item, "testing_evidence", "uat")
	return ok && strings.HasPrefix(strings.TrimSpace(val), "pass")
}

// logACSkip appends a one-line audit record of a --skip-ac bypass to
// .as/close-ac-skip.log, modeled on logEvidenceSkip. Called by the caller only
// after the close has committed. Non-fatal on write error.
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
