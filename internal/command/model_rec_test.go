package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// modelRecTestEnv creates a temp root with the .as/config.yaml + issues/ dir
// pattern that store.New expects (matches setupTestEnv in command_test.go).
// The config-discovery override is critical: without the .as/config.yaml the
// store walks up the directory tree and finds the REAL workspace items.
func modelRecTestEnv(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	for _, dir := range []string{"issues", "tasks", "archive", ".as"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"),
		[]byte("paths:\n  root: .\n"), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	return root
}

// writeItemFile drops an item .md file directly into <root>/<kind>/<id>.md.
// The store's scan() reads .md files in those dirs and parses ID from the
// `id:` field — filename slug not required for ID detection.
func writeItemFile(t *testing.T, root, kind, id, body string) {
	t.Helper()
	path := filepath.Join(root, kind, id+".md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func loadStore(t *testing.T, root string) (*store.Store, *config.Config) {
	t.Helper()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	return s, cfg
}

func TestModelRec_NoItemFallsBackToSonnet(t *testing.T) {
	root := modelRecTestEnv(t)
	s, cfg := loadStore(t, root)

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "", NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	got := out.String()
	if !strings.HasPrefix(got, "tier:sonnet|") {
		t.Errorf("want tier:sonnet prefix, got %q", got)
	}
	if !strings.Contains(got, "no active item") {
		t.Errorf("want 'no active item' reason, got %q", got)
	}
}

func TestModelRec_MissingItemFallsBackToSonnet(t *testing.T) {
	root := modelRecTestEnv(t)
	s, cfg := loadStore(t, root)

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "I-999", NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.HasPrefix(out.String(), "tier:sonnet|") {
		t.Errorf("want tier:sonnet for missing item, got %q", out.String())
	}
}

func TestModelRec_OperatorOverrideSkipsAPICall(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-500
type: issue
title: An item the operator pinned to haiku
status: queued
model_tier: haiku
sbar:
  situation: anything
`
	writeItemFile(t, root, "issues", "I-500", body)
	s, cfg := loadStore(t, root)

	// Engine left nil — if the override path doesn't short-circuit, the
	// API call would fail and we'd fall back to sonnet. Override must win.
	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "I-500", NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	got := out.String()
	if !strings.HasPrefix(got, "tier:haiku|") {
		t.Errorf("want tier:haiku from override, got %q", got)
	}
	if !strings.Contains(got, "operator override") {
		t.Errorf("want 'operator override' reason, got %q", got)
	}
}

func TestModelRec_InvalidOverrideFallsThrough(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-501
type: issue
title: Bad override
status: queued
model_tier: turbo
`
	writeItemFile(t, root, "issues", "I-501", body)
	s, cfg := loadStore(t, root)

	// No engine wired — invalid override falls through to recommender,
	// which errors with "no RunClaude engine wired", which falls back
	// to sonnet. We want sonnet, NOT "turbo".
	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "I-501", NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.HasPrefix(out.String(), "tier:sonnet|") {
		t.Errorf("invalid override should fall back to sonnet, got %q", out.String())
	}
}

func TestModelRec_HaikuClaudeReturnsValidJSON(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-600
type: issue
title: Workspace config trim
status: queued
scope_class: workspace-config
sbar:
  situation: trim CLAUDE.md
  assessment: low risk
`
	writeItemFile(t, root, "issues", "I-600", body)
	s, cfg := loadStore(t, root)

	// Mock engine returns a stream-json envelope with a haiku verdict.
	envelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"haiku\",\"reason\":\"workspace-config\"}"}`
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			// Sanity: we passed the haiku model in args.
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "claude-haiku-4-5") {
				t.Errorf("expected haiku model in args, got: %s", joined)
			}
			return []byte(envelope), 0, nil
		},
	}

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "I-600", Engine: engine, NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.HasPrefix(out.String(), "tier:haiku|") {
		t.Errorf("want tier:haiku from mocked claude, got %q", out.String())
	}
}

func TestModelRec_EngineFailureFallsBackToSonnet(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-601
type: issue
title: An item
status: queued
`
	writeItemFile(t, root, "issues", "I-601", body)
	s, cfg := loadStore(t, root)

	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return nil, 1, errors.New("network down")
		},
	}

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "I-601", Engine: engine, NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.HasPrefix(out.String(), "tier:sonnet|") {
		t.Errorf("engine failure should fall back to sonnet, got %q", out.String())
	}
}

func TestModelRec_InvalidTierFromClaudeFallsBack(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-602
type: issue
title: An item
status: queued
`
	writeItemFile(t, root, "issues", "I-602", body)
	s, cfg := loadStore(t, root)

	envelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"super-opus\",\"reason\":\"whatever\"}"}`
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return []byte(envelope), 0, nil
		},
	}

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "I-602", Engine: engine, NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.HasPrefix(out.String(), "tier:sonnet|") {
		t.Errorf("invalid tier from claude should fall back to sonnet, got %q", out.String())
	}
}

func TestModelRec_CacheHitSkipsAPICall(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-700
type: issue
title: Cached item
status: queued
`
	writeItemFile(t, root, "issues", "I-700", body)
	s, cfg := loadStore(t, root)

	cacheDir := t.TempDir()

	// Pre-seed the cache with a known result keyed by (item-id, mtime).
	// Use s.Path to get the canonical path so file layout changes don't
	// silently break the mtime computation.
	itemPath, _ := s.Path("I-700")
	mtime := fileMTime(itemPath)
	pre := cacheFile{Entries: map[string]cacheEntry{
		"I-700": {ItemID: "I-700", MTime: mtime, Result: ModelRecResult{Tier: "opus", Reason: "cached"}},
	}}
	data, _ := json.MarshalIndent(pre, "", "  ")
	cachePath := filepath.Join(cacheDir, "model-rec-cache.json")
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Engine that panics if called — proves we hit the cache, not the API.
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			t.Errorf("RunClaude called despite cache hit")
			return nil, 1, errors.New("should not happen")
		},
	}

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{
		ItemID:   "I-700",
		Engine:   engine,
		CacheDir: cacheDir,
	}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.HasPrefix(out.String(), "tier:opus|") {
		t.Errorf("want tier:opus from cache, got %q", out.String())
	}
}

func TestModelRec_CacheMissOnItemModification(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-701
type: issue
title: Modified item
status: queued
`
	writeItemFile(t, root, "issues", "I-701", body)
	s, cfg := loadStore(t, root)

	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "model-rec-cache.json")

	// Pre-seed cache with an OLD mtime (epoch 1) — item's actual mtime is now.
	pre := cacheFile{Entries: map[string]cacheEntry{
		"I-701": {ItemID: "I-701", MTime: 1, Result: ModelRecResult{Tier: "opus", Reason: "stale-cache"}},
	}}
	data, _ := json.MarshalIndent(pre, "", "  ")
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	envelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"haiku\",\"reason\":\"fresh-call\"}"}`
	engineCalled := false
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			engineCalled = true
			return []byte(envelope), 0, nil
		},
	}

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{
		ItemID:   "I-701",
		Engine:   engine,
		CacheDir: cacheDir,
	}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !engineCalled {
		t.Errorf("expected engine to be called (stale cache mtime)")
	}
	if !strings.HasPrefix(out.String(), "tier:haiku|") {
		t.Errorf("want tier:haiku from fresh call, got %q", out.String())
	}
}

func TestParseRecVerdict(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantTier string
		wantErr  bool
	}{
		{
			name:     "clean JSON",
			input:    `{"tier":"haiku","reason":"trivial"}`,
			wantTier: "haiku",
		},
		{
			name:     "JSON wrapped in prose",
			input:    "Here's my verdict:\n{\"tier\":\"opus\",\"reason\":\"hard\"}\nThanks!",
			wantTier: "opus",
		},
		{
			name:     "code fence",
			input:    "```json\n{\"tier\":\"sonnet\",\"reason\":\"normal\"}\n```",
			wantTier: "sonnet",
		},
		{
			name:     "uppercase tier normalizes",
			input:    `{"tier":"HAIKU","reason":"shouty"}`,
			wantTier: "haiku",
		},
		{
			name:    "no JSON",
			input:   "I think this should be haiku.",
			wantErr: true,
		},
		{
			name:    "invalid tier",
			input:   `{"tier":"giga","reason":"???"}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := parseRecVerdict(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got tier=%q", res.Tier)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if res.Tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", res.Tier, tc.wantTier)
			}
		})
	}
}
