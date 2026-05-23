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
// on-disk location under the standard agent-workspace layout: each
// scope repo lives as a SIBLING of the workspace clone, at
// `<agent-root>/<repo-name>`. The workspace itself is at
// `<agent-root>/theraprac-workspace`, so `theraprac-workspace`
// resolves to cfg.Root() directly and every other recognized repo
// resolves to `filepath.Dir(cfg.Root())/<name>`. Non-standard
// layouts (e.g., an agent with a stripped-down checkout missing
// some siblings) get ("", false) for absent repos, which the
// freshness heuristic treats as fail-open (skip rather than flag
// missing).
//
// statter is injectable so tests can avoid real filesystem probes.
// In production runFreshnessGate passes os.Stat directly.
//
// I-719.
func defaultRepoRoot(cfg *config.Config, statter func(string) error) func(name string) (string, bool) {
	if statter == nil {
		statter = func(path string) error {
			_, err := os.Stat(path)
			return err
		}
	}
	return func(name string) (string, bool) {
		// theraprac-workspace resolves to the workspace itself,
		// preserving today's workspace-relative behavior for any
		// docs/* or .plans/* path that happened to be written
		// with the workspace prefix.
		if name == "theraprac-workspace" {
			return cfg.Root(), true
		}
		// I-778: RepoParent() resolves per-agent repo parent via .as/agent-workspace.yaml
		// so the freshness gate doesn't probe a peer agent's clone under an
		// ST_ROOT env leak.
		candidate := filepath.Join(cfg.RepoParent(), name)
		if err := statter(candidate); err != nil {
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
	// I-717: wire DefaultRunEngine().RunClaude so the freshness
	// gate's Claude sub-agent adjudication phase fires on
	// heuristic-Drift verdicts. Terminal Stale/Fresh short-
	// circuits before the Claude pass; engine errors fail-closed
	// (keep heuristic verdict).
	engine := DefaultRunEngine()
	result, err := freshness.Check(cfg, s, id, freshness.CheckOpts{
		RepoRoot:  defaultRepoRoot(cfg, nil),
		RunClaude: engine.RunClaude,
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
