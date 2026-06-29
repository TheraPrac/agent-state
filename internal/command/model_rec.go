package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
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
	defaultTier              = "sonnet"
	defaultReason            = "default policy for standard work"
	fallbackReason           = "rec service unavailable — defaulting to sonnet"
	overrideReason           = "operator override via model_tier field"
	prepRecReason            = "prep-generated recommendation (model_tier_rec)"
	noItemReason             = "no active item — defaulting to sonnet"
	recommenderModel             = "claude-haiku-4-5"
	opusSecondOpinionModel       = "claude-opus-4-8"
	recommenderTimeout           = 30 // seconds; haiku one-shot is fast
	opusSecondOpinionTimeout     = 90 // seconds; opus is slower than haiku
)

// highRiskDomainTags are tag substrings that flag an item as high-risk domain.
var highRiskDomainTags = []string{"auth", "billing", "rbac", "migrations", "access"}

// highRiskDomainPaths are SBAR text substrings that flag high-risk domain work.
var highRiskDomainPaths = []string{"internal/auth", "internal/access", "db/changelog", "internal/billing"}

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

	// Pre-warmed recommendation stamped by `st plan prep/approve` — skip API
	// call. Use `st update <id> model_tier_rec` or re-prep to refresh.
	if rec := readItemTierRec(item); rec != "" {
		if _, valid := validTiers[rec]; valid {
			return ModelRecResult{Tier: rec, Reason: prepRecReason}
		}
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

	// Opus second-opinion gate: if Haiku recommended sonnet for a p0/p1
	// high-risk-domain item, confirm with Opus. On disagreement, escalate.
	if res.Tier == "sonnet" && opts.Engine.RunClaude != nil &&
		itemPriorityIsHighRisk(item) && isHighRiskDomain(item) {
		if opusRes, opusErr := runOpusSecondOpinion(item, cfg, opts.Engine); opusErr != nil {
			fmt.Fprintf(os.Stderr, "model-rec: opus second-opinion: %v — keeping primary recommendation\n", opusErr)
		} else if opusRes.Tier == "opus" {
			res = ModelRecResult{
				Tier:   "opus",
				Reason: fmt.Sprintf("SECOND-OPINION: opus recommends opus — %s", opusRes.Reason),
			}
		}
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

// readItemTierRec returns the value of the `model_tier_rec` field — the
// auto-generated recommendation stamped by `st plan prep/approve`.
func readItemTierRec(item *model.Item) string {
	if item.Doc == nil {
		return ""
	}
	val, ok := item.Doc.GetField("model_tier_rec")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(val))
}

// ModelRecPersist is the --persist entry point: runs the recommender for id,
// writes the result as model_tier_rec on the item, and prints the outcome to
// out. Returns 1 on error (item not found, Mutate failure). Sonnet fallback
// from the recommender is not an error. Operator override (model_tier) is
// preserved — only model_tier_rec is written.
//
// Unlike stampModelRec (which silently drops errors so plan approval is never
// blocked), this function propagates write failures to the caller — an
// operator-driven backfill must not silently misreport success.
func ModelRecPersist(s *store.Store, cfg *config.Config, id string, engine RunEngine, noCache bool, out io.Writer) int {
	if _, ok := s.Get(id); !ok {
		fmt.Fprintf(os.Stderr, "model-rec --persist: item %s not found\n", id)
		return 1
	}
	var buf strings.Builder
	ModelRec(s, cfg, ModelRecOpts{ItemID: id, Engine: engine, NoCache: noCache}, &buf)
	line := strings.TrimSpace(buf.String())
	parts := strings.SplitN(line, "|", 2)
	if len(parts) != 2 {
		fmt.Fprintf(os.Stderr, "model-rec --persist: unexpected recommender output %q\n", line)
		return 1
	}
	tier := strings.TrimPrefix(parts[0], "tier:")
	if _, valid := validTiers[tier]; !valid {
		fmt.Fprintf(os.Stderr, "model-rec --persist: invalid tier %q in recommender output\n", tier)
		return 1
	}
	if err := s.Mutate(id, func(it *model.Item) error {
		it.Doc.SetField("model_tier_rec", tier)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "model-rec --persist: write failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "[%s] model recommendation: %s\n", id, line)
	fmt.Fprintf(out, "persisted model_tier_rec=%s on %s\n", tier, id)
	return 0
}

// stampModelRec calls the recommender for id and writes the result as
// model_tier_rec on the item so `st start` model checks resolve without
// an API call. Called from plan prep/approve paths. Errors are silently
// dropped so plan approval is never blocked. For p0/p1 high-risk-domain
// items, decideTier runs an Opus second-opinion (up to opusSecondOpinionTimeout
// seconds) before returning — plan approve may take longer for those items.
func stampModelRec(s *store.Store, cfg *config.Config, id string, engine RunEngine) {
	var buf strings.Builder
	ModelRec(s, cfg, ModelRecOpts{ItemID: id, Engine: engine}, &buf)
	line := strings.TrimSpace(buf.String())
	parts := strings.SplitN(line, "|", 2)
	if len(parts) != 2 {
		return
	}
	tier := strings.TrimPrefix(parts[0], "tier:")
	if _, valid := validTiers[tier]; !valid {
		return
	}
	_ = s.Mutate(id, func(it *model.Item) error {
		it.Doc.SetField("model_tier_rec", tier)
		return nil
	})
	fmt.Fprintf(os.Stdout, "[%s] model recommendation: %s\n", id, line)
}

// isHighRiskDomain returns true when the item's tags or SBAR text reference
// auth, billing, migrations, or RBAC-adjacent domains.
func isHighRiskDomain(item *model.Item) bool {
	for _, tag := range readItemTags(item) {
		tagLow := strings.ToLower(tag)
		for _, keyword := range highRiskDomainTags {
			if strings.Contains(tagLow, keyword) {
				return true
			}
		}
	}
	sbarText := strings.ToLower(item.SBAR.Situation + " " + item.SBAR.Assessment + " " + item.SBAR.Recommendation)
	for _, path := range highRiskDomainPaths {
		if strings.Contains(sbarText, path) {
			return true
		}
	}
	return false
}

// itemPriorityIsHighRisk returns true when the item's priority is p0 or p1
// (ResolvedPriority 0 or 1). Priority is stored as a bare integer in YAML
// (e.g., "priority: 1"), not as "p1" — using item.ResolvedPriority() avoids
// the raw-string mismatch that readItemString("priority") would produce.
func itemPriorityIsHighRisk(item *model.Item) bool {
	return item.ResolvedPriority() <= 1
}

// runOpusSecondOpinion calls Opus with a high-risk-aware prompt variant and
// returns the parsed verdict. Any error is propagated so the caller can
// silently fall back to the original sonnet recommendation.
func runOpusSecondOpinion(item *model.Item, cfg *config.Config, engine RunEngine) (ModelRecResult, error) {
	if engine.RunClaude == nil {
		return ModelRecResult{}, fmt.Errorf("no RunClaude engine wired")
	}
	var sb strings.Builder
	sb.WriteString("You are a model-tier auditor for an agent runtime. ")
	sb.WriteString("A fast model already recommended 'sonnet' for this item, but it touches a high-risk domain ")
	sb.WriteString("(auth / access / billing / database migrations / RBAC). ")
	sb.WriteString("Your job: independently decide if 'sonnet' is sufficient, or if 'opus' is needed.\n\n")
	sb.WriteString("Reply ONLY with one line of JSON: {\"tier\":\"sonnet|opus\",\"reason\":\"<≤20 words>\"}\n\n")
	sb.WriteString("Tiers:\n")
	sb.WriteString("- sonnet: standard coding, SBAR review, docs, multi-file edits without architectural decisions\n")
	sb.WriteString("- opus: architecture decisions, hard debugging, multi-system coordination, cross-repo refactors, correctness-critical domain logic\n\n")
	sb.WriteString("If genuinely uncertain, prefer opus — this is the safety-net pass.\n\n")
	sb.WriteString("Item:\n")
	sb.WriteString(fmt.Sprintf("  id: %s\n", item.ID))
	sb.WriteString(fmt.Sprintf("  type: %s\n", item.Type))
	sb.WriteString(fmt.Sprintf("  title: %s\n", item.Title))
	if item.SBAR.Situation != "" {
		sb.WriteString(fmt.Sprintf("\nSituation:\n%s\n", truncateForPrompt(item.SBAR.Situation, 400)))
	}
	if item.SBAR.Assessment != "" {
		sb.WriteString(fmt.Sprintf("\nAssessment:\n%s\n", truncateForPrompt(item.SBAR.Assessment, 400)))
	}
	if item.SBAR.Recommendation != "" {
		sb.WriteString(fmt.Sprintf("\nRecommendation:\n%s\n", truncateForPrompt(item.SBAR.Recommendation, 400)))
	}

	args := buildClaudeArgs(cfg, sb.String(), RunOpts{
		PermissionMode: "plan",
		Model:          opusSecondOpinionModel,
	}, cfg.Root())

	sessionID := generateSessionID()
	env := classifierEnv(sessionID, opusSecondOpinionTimeout)

	output, exitCode, err := engine.RunClaude(cfg.Root(), args, env)
	if err != nil {
		return ModelRecResult{}, fmt.Errorf("opus second-opinion exec: %w", err)
	}
	if exitCode != 0 {
		return ModelRecResult{}, fmt.Errorf("opus second-opinion exit %d: %s", exitCode, truncateOutput(string(output)))
	}

	parsed, err := parseClaudeOutput(output)
	if err != nil {
		return ModelRecResult{}, fmt.Errorf("opus second-opinion parse envelope: %w", err)
	}
	if parsed.IsError {
		return ModelRecResult{}, fmt.Errorf("opus second-opinion reported error: %v", parsed.Errors)
	}
	if parsed.Subtype != "" && parsed.Subtype != "success" {
		return ModelRecResult{}, fmt.Errorf("opus second-opinion returned subtype %q: %v", parsed.Subtype, parsed.Errors)
	}

	res, err := parseRecVerdict(parsed.Result)
	if err != nil {
		return ModelRecResult{}, fmt.Errorf("opus second-opinion verdict parse: %w", err)
	}
	// Only sonnet and opus are valid responses from the second-opinion pass.
	if res.Tier != "sonnet" && res.Tier != "opus" {
		return ModelRecResult{}, fmt.Errorf("opus second-opinion returned unexpected tier %q", res.Tier)
	}
	return res, nil
}

// ModelRecConfirmOpus is the --confirm-opus entry point: runs the standard
// recommender, then forces an Opus second-opinion regardless of the predicate.
// If Opus recommends opus, the higher tier is persisted as model_tier_rec and
// printed. If the item already has an operator model_tier override, that takes
// precedence and no second-opinion is run. Returns 1 on item-not-found or
// write failure; 0 otherwise.
func ModelRecConfirmOpus(s *store.Store, cfg *config.Config, id string, engine RunEngine, noCache bool, out io.Writer) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "model-rec --confirm-opus: item %s not found\n", id)
		return 1
	}

	// Get the base recommendation first.
	var buf strings.Builder
	ModelRec(s, cfg, ModelRecOpts{ItemID: id, Engine: engine, NoCache: noCache}, &buf)
	baseOutput := strings.TrimSpace(buf.String())
	fmt.Fprintln(out, baseOutput)

	// If ModelRec already escalated to opus via the automatic gate (indicated by
	// "SECOND-OPINION" in the reason), skip the explicit second-opinion call to
	// avoid a redundant Opus API round-trip.
	if strings.HasPrefix(baseOutput, "tier:opus|") && strings.Contains(baseOutput, "SECOND-OPINION") {
		fmt.Fprintf(out, "confirm-opus: automatic gate already escalated to opus — skipping redundant second opinion\n")
		return 0
	}

	// If an operator override exists, respect it — no second opinion.
	if ov := readItemTierOverride(item); ov != "" {
		if _, valid := validTiers[ov]; valid {
			fmt.Fprintf(out, "confirm-opus: operator override (%s) in effect — skipping second opinion\n", ov)
			return 0
		}
	}

	opusRes, err := runOpusSecondOpinion(item, cfg, engine)
	if err != nil {
		fmt.Fprintf(os.Stderr, "confirm-opus: %v — no change\n", err)
		return 0
	}
	if opusRes.Tier != "opus" {
		fmt.Fprintf(out, "confirm-opus: Opus agrees sonnet is sufficient (%s)\n", opusRes.Reason)
		return 0
	}

	finalReason := fmt.Sprintf("SECOND-OPINION: opus recommends opus — %s", opusRes.Reason)
	if err := s.Mutate(id, func(it *model.Item) error {
		it.Doc.SetField("model_tier_rec", "opus")
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "confirm-opus: write failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "confirm-opus: escalated to opus — %s\n", finalReason)
	fmt.Fprintf(out, "persisted model_tier_rec=opus on %s\n", id)
	_ = autoSync(s, fmt.Sprintf("st model-rec --confirm-opus: %s escalated to opus", id))
	return 0
}

// classifierEnv builds the env slice for one-shot classifier calls (Haiku
// recommender and Opus second-opinion). All three variables are required by
// every classifier call site; centralising them here prevents silent drift
// when a fourth key is added.
func classifierEnv(sessionID string, wallTimeoutSec int) []string {
	return []string{
		"AS_SESSION_ID=" + sessionID,
		fmt.Sprintf("AS_CLAUDE_WALL_TIMEOUT=%ds", wallTimeoutSec),
		asClaudeSilentEnv, // suppress text echo — caller reads bare tier:X|reason:Y line
	}
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
	// I-985: wire the recommenderTimeout constant that was defined but never
	// used. Haiku one-shot responses arrive in <5s normally; 30s is generous
	// headroom without risking a 2h hang on a network stall.
	env := classifierEnv(sessionID, recommenderTimeout)

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
	// Match classify_model.go: a non-empty subtype that isn't "success" is
	// a model-side failure (e.g. error_during_execution) even when IsError
	// is false. Without this guard, those failures fall through to
	// parseRecVerdict("") and we lose the actual diagnostic.
	if parsed.Subtype != "" && parsed.Subtype != "success" {
		return ModelRecResult{}, fmt.Errorf("claude returned subtype %q: %v", parsed.Subtype, parsed.Errors)
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
	if item.SBAR.Situation != "" {
		sb.WriteString(fmt.Sprintf("\nSituation:\n%s\n", truncateForPrompt(item.SBAR.Situation, 400)))
	}
	if item.SBAR.Assessment != "" {
		sb.WriteString(fmt.Sprintf("\nAssessment:\n%s\n", truncateForPrompt(item.SBAR.Assessment, 400)))
	}

	return sb.String()
}

// parseRecVerdict extracts the {tier, reason} JSON from Claude's stdout.
// Claude may wrap it in code fences or add prose. Walks the string finding
// balanced top-level `{...}` objects and json.Unmarshal-tries each until one
// has a valid tier. Replaces a naive `[^{}]*` regex that failed on nested
// objects or strings containing `}`.
func parseRecVerdict(s string) (ModelRecResult, error) {
	for _, candidate := range extractBalancedObjects(s) {
		var res ModelRecResult
		if err := json.Unmarshal([]byte(candidate), &res); err != nil {
			continue
		}
		res.Tier = strings.ToLower(strings.TrimSpace(res.Tier))
		if _, valid := validTiers[res.Tier]; !valid {
			continue
		}
		res.Reason = strings.TrimSpace(res.Reason)
		if res.Reason == "" {
			res.Reason = defaultReason
		}
		return res, nil
	}
	// If we walked everything and nothing parsed to a valid verdict, surface
	// a parse error so the caller falls back to sonnet.
	return ModelRecResult{}, fmt.Errorf("no valid {tier:...} JSON in output: %s", truncateOutput(s))
}

// extractBalancedObjects returns every balanced top-level {…} substring in s,
// ignoring braces inside JSON strings (with backslash-escape awareness). This
// is intentionally non-recursive into nested objects — the verdict we want is
// shaped {"tier":"...","reason":"..."}, so nested objects we encounter (e.g.
// {"verdict":{"tier":...}}) get returned as the OUTER object and the inner
// object as a separate substring on the next pass through the string. Either
// candidate is given to json.Unmarshal in parseRecVerdict; the one that hits
// ModelRecResult cleanly wins.
func extractBalancedObjects(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		end := findBalancedClose(s, i)
		if end < 0 {
			break // unbalanced — bail
		}
		out = append(out, s[i:end+1])
		// Don't advance past the open brace's matching close — we still want
		// to discover nested {...} objects on the next loop iteration.
	}
	return out
}

// findBalancedClose returns the index of the } matching the { at start, or -1
// if not found. Treats `"..."` as opaque (including escaped quotes).
func findBalancedClose(s string, start int) int {
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	// Read existing cache (failures fall through to empty — non-fatal).
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
	// Atomic publish: write to a unique sibling tmp file, then rename onto
	// the canonical path. rename(2) is atomic on POSIX same-filesystem.
	// Without this, two concurrent writers race on os.WriteFile (which is
	// truncate + write, NOT atomic) and the loser silently corrupts the file
	// to a partial JSON — the next reader's json.Unmarshal then discards the
	// whole cache. Cross-process safety is best-effort: with concurrent
	// writers, the last rename still wins, which means we may drop the
	// other's new entry. That's an acceptable degradation — cache miss is
	// recoverable; cache corruption is not.
	tmp, err := os.CreateTemp(dir, ".model-rec-cache-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
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

// truncateForPrompt caps prompt input length while staying valid UTF-8.
// Byte-slicing a multi-byte rune in half produces invalid UTF-8 which can
// confuse Haiku's tokenizer or break downstream JSON encoders.
func truncateForPrompt(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// Walk back from max to the last full rune boundary.
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
