package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ModelRecOpts configures the model-rec command.
type ModelRecOpts struct {
	// ItemID is the item to recommend a tier for. Empty → default fallback.
	ItemID string
	// Engine is the Claude subprocess engine; tests inject a mock.
	Engine RunEngine
	// CacheDir overrides the cache location (testing). Empty → cfg.Root()/.as/runs.
	CacheDir string
	// NoCache disables both read and write of the cache (testing / fresh decisions).
	NoCache bool
}

// ModelRecResult is the recommender's verdict.
type ModelRecResult struct {
	Tier   string `json:"tier"`
	Reason string `json:"reason"`
}

// validTiers is the canonical set; Claude is constrained to these three.
var validTiers = map[string]struct{}{
	"haiku":  {},
	"sonnet": {},
	"opus":   {},
}

const (
	defaultTier        = "sonnet"
	defaultReason      = "default policy for standard work"
	fallbackReason     = "rec service unavailable — defaulting to sonnet"
	overrideReason     = "operator override via model_tier field"
	noItemReason       = "no active item — defaulting to sonnet"
	recommenderModel   = "claude-haiku-4-5"
	recommenderTimeout = 30 // seconds; haiku one-shot is fast
)

// ModelRec runs the recommender for the given item and writes a one-line
// `tier:<x>|reason:<y>` result to out. Exit 0 always — on any failure we fall
// back to sonnet and report the fallback. Callers (the stop-metrics hook in
// PR C) parse the tier field and compare against the session's current model.
//
// Decision sequence:
//   1. If ItemID empty → return default (no-item) sonnet.
//   2. Load item; if missing → return default (no-item) sonnet.
//   3. If item has explicit model_tier field → return verbatim (no API call).
//   4. Read cache keyed by (item-id, file-mtime); on hit → return cached.
//   5. Call haiku via `claude -p --model claude-haiku-4-5`; parse JSON.
//   6. On any error (engine missing, exec failure, parse failure, invalid tier)
//      → fall back to sonnet, log to stderr, do NOT cache the failure.
func ModelRec(s *store.Store, cfg *config.Config, opts ModelRecOpts, out io.Writer) int {
	res := decideTier(s, cfg, opts)
	fmt.Fprintf(out, "tier:%s|reason:%s\n", res.Tier, res.Reason)
	return 0
}

func decideTier(s *store.Store, cfg *config.Config, opts ModelRecOpts) ModelRecResult {
	if opts.ItemID == "" {
		return ModelRecResult{Tier: defaultTier, Reason: noItemReason}
	}

	item, ok := s.Get(opts.ItemID)
	if !ok {
		return ModelRecResult{Tier: defaultTier, Reason: noItemReason}
	}

	// Operator-explicit override skips the API call entirely.
	if override := readItemTierOverride(item); override != "" {
		if _, valid := validTiers[override]; valid {
			return ModelRecResult{Tier: override, Reason: overrideReason}
		}
		// Invalid override falls through to recommender — invalid values
		// shouldn't silently lock you to an arbitrary tier.
	}

	// Cache: (item-id, item-file-mtime) → result. mtime invalidates the cache
	// whenever the item changes (sbar update, tag add, scope_class set, etc.).
	cachePath := cachePathFor(cfg, opts)
	itemPath, _ := s.Path(opts.ItemID)
	mtime := fileMTime(itemPath)
	if !opts.NoCache {
		if hit, ok := readCache(cachePath, opts.ItemID, mtime); ok {
			return hit
		}
	}

	// Fall through to the Claude call.
	res, err := callRecommender(item, cfg, opts.Engine)
	if err != nil {
		fmt.Fprintf(os.Stderr, "model-rec: %v — falling back to %s\n", err, defaultTier)
		return ModelRecResult{Tier: defaultTier, Reason: fallbackReason}
	}

	if !opts.NoCache {
		writeCache(cachePath, opts.ItemID, mtime, res)
	}
	return res
}

// readItemTierOverride returns the value of the top-level `model_tier` field
// on the item, or "" if absent.
func readItemTierOverride(item *model.Item) string {
	if item.Doc == nil {
		return ""
	}
	val, ok := item.Doc.GetField("model_tier")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(val))
}

// callRecommender builds the Haiku prompt, runs claude, parses the JSON
// verdict. Single failure path: any error returned here triggers the
// sonnet fallback in the caller.
func callRecommender(item *model.Item, cfg *config.Config, engine RunEngine) (ModelRecResult, error) {
	if engine.RunClaude == nil {
		// Test code paths that don't inject an engine fall through here;
		// production wiring sets Engine = DefaultRunEngine() at the cmd layer.
		return ModelRecResult{}, fmt.Errorf("no RunClaude engine wired")
	}

	prompt := buildRecPrompt(item)
	args := buildClaudeArgs(cfg, prompt, RunOpts{
		PermissionMode: "plan", // pure JSON output; no tools needed
		Model:          recommenderModel,
	}, cfg.Root())

	sessionID := generateSessionID()
	env := []string{"AS_SESSION_ID=" + sessionID}

	output, exitCode, err := engine.RunClaude(cfg.Root(), args, env)
	if err != nil {
		return ModelRecResult{}, fmt.Errorf("claude exec: %w", err)
	}
	if exitCode != 0 {
		return ModelRecResult{}, fmt.Errorf("claude exit %d: %s", exitCode, truncateOutput(string(output)))
	}

	parsed, err := parseClaudeOutput(output)
	if err != nil {
		return ModelRecResult{}, fmt.Errorf("parse envelope: %w", err)
	}
	if parsed.IsError {
		return ModelRecResult{}, fmt.Errorf("claude reported error: %v", parsed.Errors)
	}

	return parseRecVerdict(parsed.Result)
}

// buildRecPrompt assembles the Haiku input from item metadata.
//
// Design intent: Haiku has no incentive to over-recommend (it doesn't get
// paid more for opus). Sketch the actual decision criteria in-prompt rather
// than letting it free-associate.
func buildRecPrompt(item *model.Item) string {
	var sb strings.Builder
	sb.WriteString("You are a model-tier recommender for an agent runtime. ")
	sb.WriteString("Reply ONLY with one line of JSON: {\"tier\":\"haiku|sonnet|opus\",\"reason\":\"<≤20 words>\"}\n\n")
	sb.WriteString("Tiers (cheapest to most capable):\n")
	sb.WriteString("- haiku: trivial reads, syncs, drain triage, single-file edits, status grep, workspace-config items\n")
	sb.WriteString("- sonnet: standard coding, SBAR review, docs, multi-file edits without architectural decisions\n")
	sb.WriteString("- opus: architecture decisions, hard debugging, multi-system coordination, referendums, cross-repo refactors\n\n")
	sb.WriteString("You have NO incentive to recommend higher than needed. Be honest. ")
	sb.WriteString("If you cannot tell, prefer sonnet.\n\n")
	sb.WriteString("Item:\n")
	sb.WriteString(fmt.Sprintf("  id: %s\n", item.ID))
	sb.WriteString(fmt.Sprintf("  type: %s\n", item.Type))
	sb.WriteString(fmt.Sprintf("  title: %s\n", item.Title))

	if scopeClass := readItemString(item, "scope_class"); scopeClass != "" {
		sb.WriteString(fmt.Sprintf("  scope_class: %s\n", scopeClass))
	}
	if tags := readItemTags(item); len(tags) > 0 {
		sb.WriteString(fmt.Sprintf("  tags: %s\n", strings.Join(tags, ", ")))
	}
	if dependsOn, blocks := readItemDepCounts(item); dependsOn+blocks > 0 {
		sb.WriteString(fmt.Sprintf("  depends_on: %d, blocks: %d\n", dependsOn, blocks))
	}

	// SBAR — situation + assessment are usually enough; background can be long.
	if sit := readItemString(item, "sbar.situation"); sit != "" {
		sb.WriteString(fmt.Sprintf("\nSituation:\n%s\n", truncateForPrompt(sit, 400)))
	}
	if ass := readItemString(item, "sbar.assessment"); ass != "" {
		sb.WriteString(fmt.Sprintf("\nAssessment:\n%s\n", truncateForPrompt(ass, 400)))
	}

	return sb.String()
}

// parseRecVerdict extracts the {tier, reason} JSON from Claude's stdout.
// Claude may wrap it in code fences or add prose — try to find the first
// JSON object containing "tier".
var jsonObjRe = regexp.MustCompile(`(?s)\{[^{}]*"tier"[^{}]*\}`)

func parseRecVerdict(s string) (ModelRecResult, error) {
	match := jsonObjRe.FindString(s)
	if match == "" {
		return ModelRecResult{}, fmt.Errorf("no JSON {tier:...} found in output: %s", truncateOutput(s))
	}
	var res ModelRecResult
	if err := json.Unmarshal([]byte(match), &res); err != nil {
		return ModelRecResult{}, fmt.Errorf("parse JSON: %w (got %q)", err, match)
	}
	res.Tier = strings.ToLower(strings.TrimSpace(res.Tier))
	if _, valid := validTiers[res.Tier]; !valid {
		return ModelRecResult{}, fmt.Errorf("invalid tier %q (want haiku|sonnet|opus)", res.Tier)
	}
	res.Reason = strings.TrimSpace(res.Reason)
	if res.Reason == "" {
		res.Reason = defaultReason
	}
	return res, nil
}

// --- cache helpers ---

type cacheEntry struct {
	ItemID string         `json:"item_id"`
	MTime  int64          `json:"mtime"`
	Result ModelRecResult `json:"result"`
}

type cacheFile struct {
	Entries map[string]cacheEntry `json:"entries"`
}

func cachePathFor(cfg *config.Config, opts ModelRecOpts) string {
	if opts.CacheDir != "" {
		return filepath.Join(opts.CacheDir, "model-rec-cache.json")
	}
	if cfg == nil {
		return ""
	}
	return filepath.Join(cfg.Root(), ".as", "runs", "model-rec-cache.json")
}

func readCache(path, itemID string, mtime int64) (ModelRecResult, bool) {
	if path == "" {
		return ModelRecResult{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ModelRecResult{}, false
	}
	var f cacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return ModelRecResult{}, false
	}
	entry, ok := f.Entries[itemID]
	if !ok || entry.MTime != mtime {
		return ModelRecResult{}, false
	}
	return entry.Result, true
}

func writeCache(path, itemID string, mtime int64, res ModelRecResult) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	// Read-modify-write so concurrent calls for different items don't clobber.
	// Failure is non-fatal: cache is an optimization, not correctness.
	var f cacheFile
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &f)
	}
	if f.Entries == nil {
		f.Entries = make(map[string]cacheEntry)
	}
	f.Entries[itemID] = cacheEntry{ItemID: itemID, MTime: mtime, Result: res}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// --- small helpers ---

func fileMTime(path string) int64 {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

func readItemString(item *model.Item, field string) string {
	if item.Doc == nil {
		return ""
	}
	val, _ := item.Doc.GetField(field)
	return strings.TrimSpace(val)
}

func readItemTags(item *model.Item) []string {
	if item.Doc == nil {
		return nil
	}
	val, ok := item.Doc.GetField("tags")
	if !ok || val == "" {
		return nil
	}
	// Tags can be either a YAML list (multi-line) or inline [a, b]; GetField
	// returns a single string. Split on commas + whitespace + brackets.
	cleaned := strings.NewReplacer("[", "", "]", "", "\n", " ", "-", " ").Replace(val)
	parts := strings.Fields(cleaned)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.Trim(p, ","))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func readItemDepCounts(item *model.Item) (depends, blocks int) {
	if item.Doc == nil {
		return 0, 0
	}
	depends = countListField(item, "depends_on")
	blocks = countListField(item, "blocks")
	return
}

func countListField(item *model.Item, field string) int {
	val, ok := item.Doc.GetField(field)
	if !ok || val == "" || val == "[]" {
		return 0
	}
	// Same parsing fallback as tags.
	cleaned := strings.NewReplacer("[", "", "]", "", "\n", " ", "-", " ").Replace(val)
	parts := strings.Fields(cleaned)
	n := 0
	for _, p := range parts {
		p = strings.TrimSpace(strings.Trim(p, ","))
		if p != "" {
			n++
		}
	}
	return n
}

func truncateForPrompt(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
