package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
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
			name:     "reason contains a } character (balanced parser must not truncate)",
			input:    `{"tier":"opus","reason":"closing brace } in body"}`,
			wantTier: "opus",
		},
		{
			name:     "outer envelope wraps verdict in nested object",
			input:    `{"verdict":{"tier":"haiku","reason":"trivial"}}`,
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
		{
			name:    "missing tier key entirely",
			input:   `{"reason":"forgot to include tier"}`,
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

func TestTruncateForPrompt_UTF8Safe(t *testing.T) {
	// em-dash is 3 bytes (0xe2 0x80 0x94). Byte-slicing at a non-rune
	// boundary produces invalid UTF-8 — the function must walk back.
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{"emdash boundary cut", "fix item — investigate further", 11},
		{"emdash boundary cut 2", "fix item — investigate further", 10},
		{"smart quotes", "‘fix this — now’", 8},
		{"all ASCII unchanged", "this is plain ascii input", 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateForPrompt(tc.in, tc.max)
			if !utf8.ValidString(got) {
				t.Errorf("truncateForPrompt(%q, %d) = %q is NOT valid UTF-8 (bytes=%x)", tc.in, tc.max, got, []byte(got))
			}
		})
	}
}

func TestWriteCache_AtomicRenameNoCorruption(t *testing.T) {
	// Atomic-publish path: even if a hypothetical kill happens between
	// CreateTemp and Rename, the canonical cache file is never observed in a
	// partially-written state. The test asserts: after a write, the file is
	// valid JSON and contains the expected entry.
	dir := t.TempDir()
	path := filepath.Join(dir, "model-rec-cache.json")

	writeCache(path, "I-100", 1234567890, ModelRecResult{Tier: "haiku", Reason: "first"})
	writeCache(path, "I-101", 1234567891, ModelRecResult{Tier: "opus", Reason: "second"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f cacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal: %v (raw: %s)", err, data)
	}
	if e, ok := f.Entries["I-100"]; !ok || e.Result.Tier != "haiku" {
		t.Errorf("I-100 missing or wrong: %+v", e)
	}
	if e, ok := f.Entries["I-101"]; !ok || e.Result.Tier != "opus" {
		t.Errorf("I-101 missing or wrong: %+v", e)
	}

	// Tmp files should NOT linger after success.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".model-rec-cache-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("orphan tmp file: %s", e.Name())
		}
	}
}

func TestModelRecPersist_WritesModelTierRec(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-800
type: issue
title: Backfill target
status: queued
sbar:
  situation: needs model_tier_rec stamped
`
	writeItemFile(t, root, "issues", "I-800", body)
	s, cfg := loadStore(t, root)

	envelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"haiku\",\"reason\":\"simple\"}"}`
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return []byte(envelope), 0, nil
		},
	}

	var out bytes.Buffer
	code := ModelRecPersist(s, cfg, "I-800", engine, false, &out)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "model_tier_rec=haiku") {
		t.Errorf("output missing persisted tier: %q", out.String())
	}

	// Reload to verify the field was actually written to the item file.
	s2, _ := loadStore(t, root)
	item, ok := s2.Get("I-800")
	if !ok {
		t.Fatal("I-800 not found after persist")
	}
	rec, ok := item.Doc.GetField("model_tier_rec")
	if !ok || strings.TrimSpace(rec) != "haiku" {
		t.Errorf("model_tier_rec = %q, want haiku", rec)
	}
}

func TestModelRecPersist_MissingItemReturnsOne(t *testing.T) {
	root := modelRecTestEnv(t)
	s, cfg := loadStore(t, root)

	var out bytes.Buffer
	code := ModelRecPersist(s, cfg, "I-999", RunEngine{}, false, &out)
	if code != 1 {
		t.Errorf("exit = %d, want 1 for missing item", code)
	}
}

func TestModelRecPersist_OperatorOverridePreserved(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-801
type: issue
title: Operator pinned to opus
status: queued
model_tier: opus
sbar:
  situation: operator wants opus
`
	writeItemFile(t, root, "issues", "I-801", body)
	s, cfg := loadStore(t, root)

	// model_tier=opus is an operator override; decideTier short-circuits at the
	// override check before calling the engine. ModelRecPersist will write
	// model_tier_rec=opus (the override-echoed tier). model_tier itself must not
	// change. decideTier still returns opus via the override path.
	var out bytes.Buffer
	code := ModelRecPersist(s, cfg, "I-801", RunEngine{}, true, &out)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}

	s2, _ := loadStore(t, root)
	item, _ := s2.Get("I-801")

	// model_tier (operator field) must be unchanged.
	override, _ := item.Doc.GetField("model_tier")
	if strings.TrimSpace(override) != "opus" {
		t.Errorf("model_tier changed: got %q, want opus", override)
	}

	// model_tier_rec reflects the override-echoed tier.
	rec, _ := item.Doc.GetField("model_tier_rec")
	if strings.TrimSpace(rec) != "opus" {
		t.Errorf("model_tier_rec = %q, want opus", rec)
	}

	// decideTier should still return opus via operator override.
	var recOut bytes.Buffer
	ModelRec(s2, cfg, ModelRecOpts{ItemID: "I-801", NoCache: true}, &recOut)
	if !strings.HasPrefix(recOut.String(), "tier:opus|") {
		t.Errorf("decideTier should return opus via override, got %q", recOut.String())
	}
}

func TestModelRecPersist_NoCacheThreaded(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-802
type: issue
title: Cache bypass test
status: queued
`
	writeItemFile(t, root, "issues", "I-802", body)
	s, cfg := loadStore(t, root)

	// Seed cache with a stale opus entry. With noCache=true, the engine must
	// be called despite the cache hit.
	cacheDir := t.TempDir()
	itemPath, _ := s.Path("I-802")
	mtime := fileMTime(itemPath)
	pre := cacheFile{Entries: map[string]cacheEntry{
		"I-802": {ItemID: "I-802", MTime: mtime, Result: ModelRecResult{Tier: "opus", Reason: "stale"}},
	}}
	data, _ := json.MarshalIndent(pre, "", "  ")
	if err := os.WriteFile(filepath.Join(cacheDir, "model-rec-cache.json"), data, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	engineCalled := false
	envelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"haiku\",\"reason\":\"fresh\"}"}`
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			engineCalled = true
			return []byte(envelope), 0, nil
		},
	}

	// Override cfg CacheDir is not directly injectable via ModelRecPersist;
	// use NoCache=true which bypasses cache read+write entirely.
	var out bytes.Buffer
	code := ModelRecPersist(s, cfg, "I-802", engine, true, &out)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !engineCalled {
		t.Error("engine not called — noCache=true should bypass the cache")
	}
	if !strings.Contains(out.String(), "model_tier_rec=haiku") {
		t.Errorf("expected haiku from fresh call, got: %q", out.String())
	}
}

// --- T-428: Opus second-opinion gate tests ---

func TestIsHighRiskDomain_Tags(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantRisk bool
	}{
		{
			name: "auth tag triggers",
			body: "id: I-test\ntype: issue\ntitle: Auth change\nstatus: queued\ntags: [auth, G-003]\n",
			wantRisk: true,
		},
		{
			name: "billing tag triggers",
			body: "id: I-test\ntype: issue\ntitle: Billing change\nstatus: queued\ntags: [billing]\n",
			wantRisk: true,
		},
		{
			name: "rbac tag triggers",
			body: "id: I-test\ntype: issue\ntitle: RBAC change\nstatus: queued\ntags: [rbac, G-001]\n",
			wantRisk: true,
		},
		{
			name: "unrelated tag not triggered",
			body: "id: I-test\ntype: issue\ntitle: Tooling change\nstatus: queued\ntags: [st-tooling, G-005]\n",
			wantRisk: false,
		},
		{
			name: "no tags not triggered",
			body: "id: I-test\ntype: issue\ntitle: Simple change\nstatus: queued\n",
			wantRisk: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := modelRecTestEnv(t)
			writeItemFile(t, root, "issues", "I-test", tc.body)
			s, _ := loadStore(t, root)
			item, ok := s.Get("I-test")
			if !ok {
				t.Fatal("I-test not found in store")
			}
			got := isHighRiskDomain(item)
			if got != tc.wantRisk {
				t.Errorf("isHighRiskDomain = %v, want %v", got, tc.wantRisk)
			}
		})
	}
}

func TestIsHighRiskDomain_SBARPaths(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantRisk bool
	}{
		{
			name: "internal/auth in situation triggers",
			body: "id: I-test\ntype: issue\ntitle: Auth middleware\nstatus: queued\ntags: []\nsbar:\n  situation: Changes in internal/auth/ to fix the JWT flow.\n",
			wantRisk: true,
		},
		{
			name: "db/changelog in assessment triggers",
			body: "id: I-test\ntype: issue\ntitle: Schema migration\nstatus: queued\nsbar:\n  situation: Adds a column.\n  assessment: Requires a db/changelog migration entry.\n",
			wantRisk: true,
		},
		{
			name: "internal/billing in recommendation triggers",
			body: "id: I-test\ntype: issue\ntitle: Billing fix\nstatus: queued\nsbar:\n  situation: Fix invoicing.\n  recommendation: Edit internal/billing/invoice.go to fix rounding.\n",
			wantRisk: true,
		},
		{
			name: "web component change not triggered",
			body: "id: I-test\ntype: issue\ntitle: UI tweak\nstatus: queued\nsbar:\n  situation: Change the dashboard layout in the web UI.\n",
			wantRisk: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := modelRecTestEnv(t)
			writeItemFile(t, root, "issues", "I-test", tc.body)
			s, _ := loadStore(t, root)
			item, ok := s.Get("I-test")
			if !ok {
				t.Fatal("I-test not found in store")
			}
			got := isHighRiskDomain(item)
			if got != tc.wantRisk {
				t.Errorf("isHighRiskDomain = %v, want %v", got, tc.wantRisk)
			}
		})
	}
}

func TestDecideTier_OpusSecondOpinion_Triggers(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-920
type: issue
title: Auth session change
status: queued
priority: 1
tags: [auth]
sbar:
  situation: Fix session expiry in internal/auth/session.go
`
	writeItemFile(t, root, "issues", "I-920", body)
	s, cfg := loadStore(t, root)

	haikuEnvelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"sonnet\",\"reason\":\"multi-file edit\"}"}`
	opusEnvelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"opus\",\"reason\":\"correctness-critical auth domain\"}"}`

	callCount := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			callCount++
			if callCount == 1 {
				return []byte(haikuEnvelope), 0, nil
			}
			return []byte(opusEnvelope), 0, nil
		},
	}

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "I-920", Engine: engine, NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	got := out.String()
	if !strings.HasPrefix(got, "tier:opus|") {
		t.Errorf("want tier:opus after second-opinion escalation, got %q", got)
	}
	if !strings.Contains(got, "SECOND-OPINION") {
		t.Errorf("want SECOND-OPINION in reason, got %q", got)
	}
	if callCount != 2 {
		t.Errorf("expected 2 engine calls (haiku + opus), got %d", callCount)
	}
}

func TestDecideTier_OpusSecondOpinion_SkipsP2(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-921
type: issue
title: Auth cleanup (low priority)
status: queued
priority: 2
tags: [auth]
sbar:
  situation: Low-priority refactor of internal/auth/ helpers.
`
	writeItemFile(t, root, "issues", "I-921", body)
	s, cfg := loadStore(t, root)

	haikuEnvelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"sonnet\",\"reason\":\"multi-file edit\"}"}`
	callCount := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			callCount++
			return []byte(haikuEnvelope), 0, nil
		},
	}

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "I-921", Engine: engine, NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	got := out.String()
	if !strings.HasPrefix(got, "tier:sonnet|") {
		t.Errorf("want tier:sonnet (no escalation for p2), got %q", got)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 engine call (no Opus second-opinion for p2), got %d", callCount)
	}
}

func TestDecideTier_OpusSecondOpinion_SkipsNonRisk(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-922
type: issue
title: Tooling update
status: queued
priority: 1
tags: [st-tooling]
sbar:
  situation: Update the CLI help text.
`
	writeItemFile(t, root, "issues", "I-922", body)
	s, cfg := loadStore(t, root)

	haikuEnvelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"sonnet\",\"reason\":\"doc update\"}"}`
	callCount := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			callCount++
			return []byte(haikuEnvelope), 0, nil
		},
	}

	var out bytes.Buffer
	exit := ModelRec(s, cfg, ModelRecOpts{ItemID: "I-922", Engine: engine, NoCache: true}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.HasPrefix(out.String(), "tier:sonnet|") {
		t.Errorf("want tier:sonnet (non-risk domain), got %q", out.String())
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 engine call (no Opus for non-risk domain), got %d", callCount)
	}
}

func TestModelRecConfirmOpus_ForcesCheck(t *testing.T) {
	root := modelRecTestEnv(t)
	body := `id: I-930
type: issue
title: Auth token validation
status: queued
priority: 1
tags: [auth]
sbar:
  situation: Strengthen token validation in internal/auth/jwt.go
`
	writeItemFile(t, root, "issues", "I-930", body)
	s, cfg := loadStore(t, root)

	haikuEnvelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"sonnet\",\"reason\":\"multi-file\"}"}`
	opusEnvelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"opus\",\"reason\":\"auth correctness critical\"}"}`

	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			// decideTier calls haiku first, then fires the Opus gate (p1+auth predicate).
			// ModelRecConfirmOpus detects "SECOND-OPINION" in the base output and skips
			// the redundant second Opus call.
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "claude-haiku") {
				return []byte(haikuEnvelope), 0, nil
			}
			return []byte(opusEnvelope), 0, nil
		},
	}

	var out bytes.Buffer
	code := ModelRecConfirmOpus(s, cfg, "I-930", engine, true, &out)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	outStr := out.String()
	// The automatic gate already escalated to opus; confirm-opus detects this and
	// skips the redundant Opus call. Output should show the gate result.
	if !strings.Contains(outStr, "opus") {
		t.Errorf("expected opus in output, got:\n%s", outStr)
	}

	// The early-return path ("automatic gate already escalated") skips the Mutate,
	// so model_tier_rec may be set by the internal stampModelRec path but not here.
	// The important assertion is that opus was returned and code==0 (above).
}

// TestModelRecConfirmOpus_PersistsWhenNotAutoEscalated tests the path where
// ModelRec returns sonnet (no automatic escalation) and --confirm-opus forces
// the check and persists the result.
func TestModelRecConfirmOpus_PersistsWhenNotAutoEscalated(t *testing.T) {
	root := modelRecTestEnv(t)
	// p2 item with auth tag: itemPriorityIsHighRisk returns false (priority=2),
	// so the automatic gate in decideTier does NOT fire. ModelRec returns sonnet.
	// ModelRecConfirmOpus then forces the Opus check and persists opus.
	body := `id: I-931
type: issue
title: Auth cleanup (low priority confirm-opus test)
status: queued
priority: 2
tags: [auth]
sbar:
  situation: Refactor session handling in internal/auth/.
`
	writeItemFile(t, root, "issues", "I-931", body)
	s, cfg := loadStore(t, root)

	haikuEnvelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"sonnet\",\"reason\":\"multi-file\"}"}`
	opusEnvelope := `{"type":"result","subtype":"success","is_error":false,"result":"{\"tier\":\"opus\",\"reason\":\"auth correctness critical\"}"}`

	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "claude-haiku") {
				return []byte(haikuEnvelope), 0, nil
			}
			return []byte(opusEnvelope), 0, nil
		},
	}

	var out bytes.Buffer
	code := ModelRecConfirmOpus(s, cfg, "I-931", engine, true, &out)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	outStr := out.String()
	if !strings.Contains(outStr, "escalated to opus") {
		t.Errorf("expected escalation message, got:\n%s", outStr)
	}

	// Reload and confirm model_tier_rec was persisted.
	s2, _ := loadStore(t, root)
	item, _ := s2.Get("I-931")
	rec, ok := item.Doc.GetField("model_tier_rec")
	if !ok || strings.TrimSpace(rec) != "opus" {
		t.Errorf("model_tier_rec = %q, want opus", rec)
	}
}

func TestModelRecConfirmOpus_MissingItemReturnsOne(t *testing.T) {
	root := modelRecTestEnv(t)
	s, cfg := loadStore(t, root)

	var out bytes.Buffer
	code := ModelRecConfirmOpus(s, cfg, "I-999", RunEngine{}, true, &out)
	if code != 1 {
		t.Errorf("exit = %d, want 1 for missing item", code)
	}
}
