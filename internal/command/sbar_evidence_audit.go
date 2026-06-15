package command

import (
	"fmt"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/quality"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SbarEvidenceAuditOpts holds flags for the sbar-evidence-audit command.
type SbarEvidenceAuditOpts struct {
	Sprint string // filter to items in this sprint slug
	All    bool   // include closed/archived items
}

// SbarEvidenceAudit walks open items (or a sprint slice) and reports any
// sbar.background sentences that contain observation-shaped claims without
// evidence pointers. Read-only — no writes, no enforcement.
func SbarEvidenceAudit(s *store.Store, cfg *config.Config, opts SbarEvidenceAuditOpts) int {
	var filters []store.Filter
	if opts.Sprint != "" {
		filters = append(filters, store.SprintFilter(opts.Sprint))
	}
	if !opts.All {
		filters = append(filters, store.NonTerminalFilter(cfg))
	}

	items := s.List(filters...)

	type finding struct {
		id      string
		message string
	}
	var findings []finding

	for _, item := range items {
		vs := quality.ValidateBackgroundEvidenceClaims(item)
		for _, v := range vs {
			findings = append(findings, finding{id: item.ID, message: v.Message})
		}
	}

	if len(findings) == 0 {
		fmt.Println("sbar-evidence-audit: no unsourced empirical claims found.")
		return 0
	}

	fmt.Printf("sbar-evidence-audit: %d unsourced empirical claim(s) found:\n\n", len(findings))
	for _, f := range findings {
		fmt.Printf("  %s  %s\n", f.id, f.message)
	}
	return 1
}
