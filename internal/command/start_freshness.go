package command

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/freshness"
	"github.com/jfinlinson/agent-state/internal/store"
)

// defaultRepoRoot resolves a scope-repo name to its conventional
// on-disk location: a sibling of the workspace clone. The standard
// agent-workspace layout puts each repo at
// `<agent-root>/<repo-name>`, and the workspace itself lives at
// `<agent-root>/theraprac-workspace`. So
// `filepath.Dir(cfg.Root())/<name>` is the canonical resolution.
//
// Returns ("", false) when the candidate path does not exist on
// disk so the heuristic's fail-open branch fires (don't falsely
// flag a file missing when the repo itself isn't present in this
// layout, e.g. an agent that doesn't check out every sibling).
//
// I-719.
func defaultRepoRoot(cfg *config.Config) func(name string) (string, bool) {
	return func(name string) (string, bool) {
		if cfg == nil {
			return "", false
		}
		// theraprac-workspace resolves to the workspace itself,
		// preserving today's workspace-relative behavior for any
		// docs/* or .plans/* path that happened to be written
		// with the workspace prefix.
		if name == "theraprac-workspace" {
			return cfg.Root(), true
		}
		candidate := filepath.Join(filepath.Dir(cfg.Root()), name)
		if _, err := os.Stat(candidate); err != nil {
			return "", false
		}
		return candidate, true
	}
}

// runFreshnessGate is the command-side bridge between Start and the
// freshness package. Returns:
//
//	0 → proceed (Fresh, or Drift+ack)
//	2 → refuse (Drift without ack, or Stale)
//
// Behavior on engine errors / unexpected returns: surface a stderr
// warning and proceed (don't wedge starts on a freshness-package
// regression). The gate is advisory in the "Claude exec error"
// sense — the heuristics path is fast and side-effect-free; only
// catastrophic bugs would prevent it returning a verdict.
//
// I-711 — the public entry point is freshness.Check; this helper
// glues that into command.StartOpts (specifically --ack-drift).
func runFreshnessGate(cfg *config.Config, s *store.Store, id string, opts StartOpts) int {
	result, err := freshness.Check(cfg, s, id, freshness.CheckOpts{
		RepoRoot: defaultRepoRoot(cfg),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: freshness gate errored on %s (%v) — proceeding without re-validation\n", id, err)
		return 0
	}
	if result == nil {
		return 0
	}

	switch result.Verdict {
	case freshness.VerdictFresh:
		return 0
	case freshness.VerdictDrift:
		if opts.AckDrift != "" {
			fmt.Fprintf(os.Stderr, "freshness gate: %s — DRIFT findings:\n", id)
			for _, f := range result.Findings {
				fmt.Fprintf(os.Stderr, "  %s\n", f)
			}
			fmt.Fprintf(os.Stderr, "  proceeding with operator ack: %q\n", opts.AckDrift)
			return 0
		}
		fmt.Fprintf(os.Stderr, "freshness gate: %s — DRIFT (refusing activation):\n", id)
		for _, f := range result.Findings {
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
		fmt.Fprintf(os.Stderr,
			"Plan premises may have shifted since approval. Either:\n"+
				"  - Re-prep: `st plan reset %s && st plan prep %s`, or\n"+
				"  - Acknowledge and proceed: `st start %s --ack-drift \"<one-line reason>\"`\n",
			id, id, id)
		return 2
	case freshness.VerdictStale:
		fmt.Fprintf(os.Stderr, "freshness gate: %s — STALE (refusing activation):\n", id)
		for _, f := range result.Findings {
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
		fmt.Fprintf(os.Stderr,
			"Plan premises are invalidated. Run `st plan reset %s` then `st plan prep %s` before re-trying. (No --ack-stale opt-out — re-prep is required.)\n",
			id, id)
		return 2
	}
	return 0
}
