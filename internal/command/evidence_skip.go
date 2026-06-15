// I-756: evidence_skip.go — audit log for --evidence-skip bypasses.
package command

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
)

// logEvidenceSkip appends a one-line audit record to .as/sbar-evidence-skip.log
// when an agent bypasses the empirical-claim gate via --evidence-skip.
// id is the item ID (empty string at create time, before ID is allocated).
func logEvidenceSkip(cfg *config.Config, id, reason string) {
	logPath := filepath.Join(cfg.Root(), ".as", "sbar-evidence-skip.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		// Non-fatal: audit log failure should not block the operation.
		fmt.Fprintf(os.Stderr, "evidence-skip: could not write audit log: %v\n", err)
		return
	}
	defer f.Close()
	itemRef := id
	if itemRef == "" {
		itemRef = "(pre-creation)"
	}
	fmt.Fprintf(f, "%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), itemRef, reason)
}
