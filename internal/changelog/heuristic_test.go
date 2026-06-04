package changelog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

func setupHeuristicCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	cfg, err := config.LoadFrom(filepath.Join(asDir, "config.yaml"))
	if err != nil {
		t.Fatalf("config.LoadFrom: %v", err)
	}
	return cfg
}

func TestHeuristicAppendList(t *testing.T) {
	cfg := setupHeuristicCfg(t)
	agentID := cfg.AgentID()

	// Initially empty — no error.
	entries, err := HeuristicList(cfg, agentID, nil)
	if err != nil {
		t.Fatalf("HeuristicList on absent file: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty, got %d entries", len(entries))
	}

	// Append two heuristics.
	if err := HeuristicAppend(cfg, Entry{
		Op: "heuristic_add", Reason: "when X, do Y",
	}); err != nil {
		t.Fatalf("HeuristicAppend #1: %v", err)
	}
	if err := HeuristicAppend(cfg, Entry{
		Op: "heuristic_add", Reason: "when Z, don't do W",
	}); err != nil {
		t.Fatalf("HeuristicAppend #2: %v", err)
	}

	got, err := HeuristicList(cfg, agentID, nil)
	if err != nil {
		t.Fatalf("HeuristicList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	// Kind is forced to KindHeuristic on write.
	for i, e := range got {
		if e.Kind != KindHeuristic {
			t.Errorf("entry %d: Kind = %q, want KindHeuristic", i, e.Kind)
		}
	}
	if got[0].Reason != "when X, do Y" {
		t.Errorf("entry 0 reason: %q", got[0].Reason)
	}
	if got[1].Reason != "when Z, don't do W" {
		t.Errorf("entry 1 reason: %q", got[1].Reason)
	}
}

func TestHeuristicListTagFilter(t *testing.T) {
	cfg := setupHeuristicCfg(t)

	// universal (no tags)
	HeuristicAppend(cfg, Entry{Op: "heuristic_add", Reason: "universal rule"})
	// tagged for "api"
	HeuristicAppend(cfg, Entry{Op: "heuristic_add", Reason: "api rule", RelevanceTags: []string{"api", "auth"}})
	// tagged for "web" only
	HeuristicAppend(cfg, Entry{Op: "heuristic_add", Reason: "web rule", RelevanceTags: []string{"web"}})

	agentID := cfg.AgentID()

	// no filter — all 3
	all, _ := HeuristicList(cfg, agentID, nil)
	if len(all) != 3 {
		t.Fatalf("no filter: expected 3, got %d", len(all))
	}

	// filter by "api" — universal + api rule
	apiMatches, _ := HeuristicList(cfg, agentID, []string{"api"})
	if len(apiMatches) != 2 {
		t.Errorf("api filter: expected 2, got %d", len(apiMatches))
	}

	// filter by "web" — universal + web rule
	webMatches, _ := HeuristicList(cfg, agentID, []string{"web"})
	if len(webMatches) != 2 {
		t.Errorf("web filter: expected 2, got %d", len(webMatches))
	}

	// filter by "infra" — only universal
	infraMatches, _ := HeuristicList(cfg, agentID, []string{"infra"})
	if len(infraMatches) != 1 {
		t.Errorf("infra filter: expected 1 (universal only), got %d", len(infraMatches))
	}
}

func TestHeuristicListAbsentFile(t *testing.T) {
	cfg := setupHeuristicCfg(t)
	entries, err := HeuristicList(cfg, "nonexistent-agent", nil)
	if err != nil {
		t.Errorf("absent file must return nil error, got: %v", err)
	}
	if entries != nil {
		t.Errorf("absent file must return nil slice, got %d entries", len(entries))
	}
}
