package freshness

import (
	"fmt"
	"strings"

	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
)

// promptDepRecommendationCap caps the per-dep recommendation text
// in the freshness prompt body so cold-path Claude cost stays
// bounded.
const promptDepRecommendationCap = 200

// promptGitLogLineCap caps the recent-git-log section.
const promptGitLogLineCap = 20

// buildFreshnessPrompt assembles the Claude sub-agent prompt that
// adjudicates a Drift verdict. The model's job is to read the
// plan's premises + the heuristic findings + recent context, and
// emit RECOMMENDATION: Fresh | Drift | Stale.
//
// Caps documented above keep the prompt bounded. The output
// format mirrors buildPlanReviewPrompt / buildItemReviewPrompt so
// the existing extractRecommendation parser handles it.
//
// I-717.
func buildFreshnessPrompt(item *model.Item, p *plan.Plan, heuristics []Finding, recentGitLog string, closedDeps []*model.Item) string {
	var b strings.Builder

	b.WriteString("You are adjudicating whether an approved plan is still valid against current code state.\n\n")
	b.WriteString("## Item\n\n")
	if item != nil {
		fmt.Fprintf(&b, "ID: %s\nType: %s\nTitle: %s\n", item.ID, item.Type, item.Title)
	}
	if p != nil {
		b.WriteString("\n## Plan\n\n")
		if p.Approach != "" {
			fmt.Fprintf(&b, "Approach: %s\n\n", p.Approach)
		}
		if len(p.ScopeRepos) > 0 {
			fmt.Fprintf(&b, "Scope repos: %s\n\n", strings.Join(p.ScopeRepos, ", "))
		}
		if len(p.FilesToModify) > 0 {
			b.WriteString("Files to modify:\n")
			for _, f := range p.FilesToModify {
				fmt.Fprintf(&b, "  - %s\n", f)
			}
			b.WriteString("\n")
		}
	}

	if len(heuristics) > 0 {
		b.WriteString("## Heuristic findings (these triggered the Drift verdict)\n\n")
		for _, h := range heuristics {
			fmt.Fprintf(&b, "- %s\n", h)
		}
		b.WriteString("\n")
	}

	if recentGitLog != "" {
		b.WriteString("## Recent commits on touched paths\n\n```\n")
		b.WriteString(capLines(recentGitLog, promptGitLogLineCap))
		b.WriteString("\n```\n\n")
	}

	if len(closedDeps) > 0 {
		b.WriteString("## Closed dependencies since plan approval\n\n")
		for _, d := range closedDeps {
			if d == nil {
				continue
			}
			fmt.Fprintf(&b, "### %s — %s\n", d.ID, d.Title)
			rec := d.SBAR.Recommendation
			if len(rec) > promptDepRecommendationCap {
				rec = rec[:promptDepRecommendationCap] + "…"
			}
			if rec != "" {
				fmt.Fprintf(&b, "Recommendation: %s\n\n", rec)
			}
		}
	}

	b.WriteString("## Instructions\n\n")
	b.WriteString("Adjudicate ONE of:\n\n")
	b.WriteString("  - Fresh — heuristics flagged drift but premises still hold (false-positive demotion).\n")
	b.WriteString("  - Drift — premises shifted; operator should re-prep or acknowledge.\n")
	b.WriteString("  - Stale — premises fundamentally invalidated; operator MUST re-prep.\n\n")
	b.WriteString("Emit your verdict in EXACTLY this form on the LAST line of your response:\n\n")
	b.WriteString("  RECOMMENDATION: <Fresh|Drift|Stale> — <one-line rationale>\n\n")
	b.WriteString("The leading token after the colon must be the verdict word; do not put the rationale first.\n")
	return b.String()
}

// capLines truncates `s` to at most `n` newline-separated lines,
// appending a trailing notice when truncation occurred.
func capLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n…(truncated)"
}
