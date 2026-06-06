package command

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/coordinator"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Queue entry source values. Missing/empty = legacy "manual" semantics.
const (
	QueueSourceManual = "manual"
	QueueSourceSprint = "sprint"
)

// QueueEntry represents an item in the user-controlled work queue.
type QueueEntry struct {
	ID       string
	AddedAt  string
	AddedBy  string // "user" or agent ID
	Reason   string
	Approved bool // agent-added items need user approval
	// Source identifies what put the entry on the queue. "sprint" means
	// the entry was created as a side effect of a historical `st sprint add`
	// (pre-I-1322); sprint rm cascade-removes it. "manual" means the operator
	// placed or re-placed the entry explicitly (via `st queue add` or via
	// `st queue move`, which I-489 flips to "manual" on move) — sprint rm
	// leaves it alone. The empty string is a legacy on-disk form (pre-I-488)
	// treated as equivalent to "manual" at runtime: SaveQueue writes "manual"
	// explicitly when the field is set, but skips the line for empty so the
	// file format stays compact for entries that never carried the field.
	// Future readers must check `Source != QueueSourceSprint`
	// (not `== QueueSourceManual`) so legacy empty-source entries
	// behave identically to operator-pinned ones.
	Source string
}

// QueueOpts holds flags for queue commands.
type QueueOpts struct {
	Reason string
}

// QueueNextOpts filters queue next.
type QueueNextOpts struct {
	Sprint string // when non-empty, restrict to items whose item.Sprint matches
}

// QueueApproveOpts holds flags for queue approve.
type QueueApproveOpts struct {
	Sprint string // when non-empty (and ID empty), bulk-approve all pending sprint members
	// BypassPlan skips the I-491 plan-required gate. Each bypassed
	// approval writes a changelog entry so the override is auditable.
	// Use only for emergencies — the gate exists so approvals carry a
	// commitment to a method, not just a yes/no rubber-stamp.
	BypassPlan bool
}

// IsQueuePending always returns false. Queue entries are auto-approved on add
// (T-461: approval gate eliminated — candidates derive from item properties).
func IsQueuePending(_ *store.Store, _ *config.Config, _ string) bool {
	return false
}

// PendingApprovalCount always returns 0. Queue entries are auto-approved on
// add (T-461: approval gate eliminated — candidates derive from item properties).
func PendingApprovalCount(_ *store.Store, _ *config.Config) int {
	return 0
}

// IsGoalReachable reports whether id is listed in item.Goals for any active
// goal. When true, the item's operator-intent is already structurally
// encoded — the per-item queue approval gate is redundant (T-412, T-416).
func IsGoalReachable(s *store.Store, cfg *config.Config, id string) bool {
	if s == nil || id == "" {
		return false
	}
	item, ok := s.Get(id)
	if !ok || len(item.Goals) == 0 {
		return false
	}
	activeGoals := make(map[string]bool)
	for _, g := range s.List(store.TypeFilter("goal")) {
		if g.Status == "active" {
			activeGoals[g.ID] = true
		}
	}
	for _, goalID := range item.Goals {
		if activeGoals[goalID] {
			return true
		}
	}
	return false
}

// autoSync commits + pushes any working-tree changes left by a state-mutating
// command. Four behaviors:
//   - Gate sentinel (ErrI807MainBranchGate): prints the full actionable error
//     to stderr and returns the error so the caller can exit non-zero. The gate
//     is operator-actionable and won't self-resolve — silencing it hides a real
//     problem (I-821).
//   - ErrPushDiverged: prints an actionable warning naming the conflict and
//     returns nil. Operator must resolve the conflict before re-running st sync.
//   - ErrPushRejectedButOriginUnchanged: prints an actionable warning including
//     the remote rejection text (I-684) and returns nil. No retry will help.
//   - Any other error: prints a "warning:" line and returns nil (best-effort for
//     transient failures like network blips or git-lock contention that recover
//     on the next sync).
func autoSync(s *store.Store, msg string, newPaths ...string) error {
	if s == nil {
		return nil
	}
	if err := s.GitSync(msg, newPaths...); err != nil {
		if errors.Is(err, store.ErrI807MainBranchGate) {
			fmt.Fprint(os.Stderr, err)
			return err
		}
		if errors.Is(err, store.ErrPushDiverged) {
			fmt.Fprintf(os.Stderr, "warning: auto-sync: push diverged — a peer changed the same file(s); resolve the conflict, then run `st sync` (%v)\n", err)
			return nil
		}
		if errors.Is(err, store.ErrPushRejectedButOriginUnchanged) {
			fmt.Fprintf(os.Stderr, "warning: auto-sync: push blocked by a server-side gate (branch protection or pre-receive hook) — retrying won't help; check remote settings (%v)\n", err)
			return nil
		}
		fmt.Fprintf(os.Stderr, "warning: auto-sync failed: %v (run `st sync` manually)\n", err)
	}
	return nil
}

// --- Commands ---

func QueueAdd(s *store.Store, cfg *config.Config, id string, opts QueueOpts) int {
	if _, ok := s.Get(id); !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	entries := LoadQueue(cfg)
	for _, e := range entries {
		if e.ID == id {
			fmt.Fprintf(os.Stderr, "%s is already pinned\n", id)
			return 1
		}
	}

	addedBy := "user"
	if agentID := cfg.AgentID(); agentID != "" {
		addedBy = agentID
	}

	entries = append(entries, QueueEntry{
		ID:      id,
		AddedAt: time.Now().Format(time.RFC3339),
		AddedBy: addedBy,
		Reason:  opts.Reason,
		Approved: true,
		Source:  QueueSourceManual,
	})

	if err := SaveQueue(cfg, entries); err != nil {
		fmt.Fprintf(os.Stderr, "saving queue: %v\n", err)
		return 1
	}

	fmt.Printf("Pinned %s — will boost scheduling priority within its priority band\n", id)
	if err := autoSync(s, fmt.Sprintf("st queue add: %s", id)); err != nil {
		return 1
	}
	return 0
}

// QueueShowOpts holds rendering flags for the queue listing.
//
// T-347 introduces AgentAll — from inside an agent workspace, queue
// rows visually distinguish current-agent / unassigned / peer items.
// AgentAll suppresses the visual treatment so the operator sees the
// raw global queue.
//
// I-838: Raw exposes the legacy positional queue (for debugging/approval
// workflows). Default behaviour is now aliased to `st recommend` — the
// goal-weighted authoritative work-ordering surface.
type QueueShowOpts struct {
	AgentAll bool
	Raw      bool
}

// QueueShow renders the work priority list.
//
// Default (Raw=false): aliased to st recommend — shows goal-weighted
// candidates so `st queue show` produces the authoritative priority
// order instead of the stale positional queue.
//
// Raw=true: legacy positional queue (useful for st queue add/rm/approve
// inspection, not for deciding what to work on).
func QueueShow(s *store.Store, cfg *config.Config, opts QueueShowOpts) int {
	if !opts.Raw {
		fmt.Printf("%s⚠  DEPRECATED as a work-ordering surface%s — aliased to %sst recommend%s (use %sst next%s for single top item).\n",
			cYellow, cReset, cBold, cReset, cBold, cReset)
		fmt.Printf("   For raw queue internals (add/rm/approve inspection): %sst queue show --raw%s\n\n",
			cDim, cReset)
		return recommendTo(os.Stdout, s, cfg, RecommendOpts{})
	}

	// Raw mode: legacy positional queue view.
	fmt.Printf("%s⚠  Raw queue internals%s — not goal-weighted. For work ordering: %sst next%s\n\n",
		cYellow, cReset, cBold, cReset)

	entries := LoadQueue(cfg)
	if len(entries) == 0 {
		fmt.Println("Queue is empty")
		return 0
	}

	g := deps.Build(s.All(), cfg)

	// I-489: load the registry so we can render the epic→sprint chain
	// alongside each entry. Best-effort — if the registry can't load,
	// we just skip the chain row.
	r, _ := registry.Load(cfg.EpicsPath())

	scope := cfg.ResolveAgentContext()
	applyAgentClass := scope.Scoped && !opts.AgentAll

	headerSuffix := ""
	if applyAgentClass {
		headerSuffix = fmt.Sprintf(" %s(scoped: %s; --all to clear)%s", cDim, scope.CurrentAgent, cReset)
	}
	fmt.Printf("%sWork Queue%s (%d items)%s\n\n", cBold, cReset, len(entries), headerSuffix)
	for i, e := range entries {
		item, ok := s.Get(e.ID)
		title := "(not found)"
		status := ""
		if ok {
			title = truncate(item.Title, 50)
			status = item.Status
		}

		blocked := ""
		if ok && g.IsBlocked(e.ID) {
			blocked = fmt.Sprintf("  %s⊘ blocked%s", cRed, cReset)
		}

		approval := ""
		if !e.Approved {
			approval = fmt.Sprintf("  %s⏳ needs approval%s", cYellow, cReset)
		}

		active := ""
		if ok && item.Status == "active" {
			active = fmt.Sprintf("  %s● active%s", cGreen, cReset)
		}

		// T-347 visual treatment. Bright (cBold) is the default for
		// the current agent's items, peer-assigned items render
		// dimmed so the operator can scan their own work quickly;
		// unassigned items keep the legacy single-style rendering.
		// Outside an agent context (workspace root) every row is
		// "self" so behavior is unchanged.
		bullet := " "
		rowColor := cBold
		owner := ""
		if applyAgentClass && ok {
			switch classifyAgentOwnership(item, scope.CurrentAgent) {
			case "self":
				bullet = "▶"
			case "peer":
				rowColor = cDim
				bullet = "·"
				owner = fmt.Sprintf("  %s[%s]%s", cDim, item.AssignedTo, cReset)
			case "open":
				bullet = " "
			}
		}

		fmt.Printf("  %s %d. %s%-8s%s %s  %s(%s)%s%s%s%s%s\n",
			bullet, i+1, rowColor, e.ID, cReset, title, cDim, status, cReset, active, blocked, approval, owner)

		// Chain row — epic › sprint, only when at least one is set.
		if ok && (item.Epic != "" || item.Sprint != "") {
			chain := formatEpicSprintChain(r, item.Epic, item.Sprint)
			if chain != "" {
				fmt.Printf("     %s%s%s\n", cDim, chain, cReset)
			}
		}

		// Reason — suppress the auto-generated `sprint:<slug>` because
		// the chain row already conveys it; show operator-set reasons.
		autoReason := false
		if e.Reason != "" && strings.HasPrefix(e.Reason, "sprint:") && ok && item.Sprint != "" && e.Reason == "sprint:"+item.Sprint {
			autoReason = true
		}
		if e.Reason != "" && !autoReason {
			fmt.Printf("     %s%s%s\n", cDim, e.Reason, cReset)
		}
	}
	fmt.Println()
	return 0
}

// formatEpicSprintChain renders the epic→sprint context for an item as
// `<epic-id> [pN] › <sprint-id> [#K]`. Pieces are dropped when missing
// so unprioritized epics or items without a sprint render cleanly.
func formatEpicSprintChain(r *registry.Registry, epicID, sprintID string) string {
	if epicID == "" && sprintID == "" {
		return ""
	}
	parts := []string{}
	if epicID != "" {
		piece := epicID
		if r != nil {
			if e, ok := r.GetEpic(epicID); ok && e.Priority != nil {
				piece = fmt.Sprintf("%s p%d", epicID, *e.Priority)
			}
		}
		parts = append(parts, piece)
	}
	if sprintID != "" {
		piece := sprintID
		if r != nil {
			if sp, err := r.SprintByID(sprintID); err == nil && sp.Sequence > 0 {
				piece = fmt.Sprintf("%s #%d", sprintID, sp.Sequence)
			}
		}
		parts = append(parts, piece)
	}
	return strings.Join(parts, " › ")
}

func QueueNext(s *store.Store, cfg *config.Config, opts QueueNextOpts) int {
	g := deps.Build(s.All(), cfg)
	sprints := loadSprintInfo(cfg, g)
	cands := recommendCandidates(s, cfg, g, RecommendOpts{}, sprints)
	lev, _ := unblockLeverage(g, cands)
	recs := coordinator.Recommend(cands, lev, sprints, loadGoalWeights(s), loadQueuePins(cfg), time.Now())

	for _, r := range recs {
		if opts.Sprint != "" && r.Item.Sprint != opts.Sprint {
			continue
		}
		fmt.Printf("%s — %s\n", r.Item.ID, r.Item.Title)
		return 0
	}

	if opts.Sprint != "" {
		fmt.Printf("No unblocked items for sprint %s\n", opts.Sprint)
	} else {
		fmt.Println("No unblocked, unassigned items ready")
	}
	return 0
}

func QueueRm(s *store.Store, cfg *config.Config, id string) int {
	removed, err := removeFromQueueSilently(cfg, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "saving queue: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(os.Stderr, "%s not in queue\n", id)
		return 1
	}
	fmt.Printf("Removed %s from queue\n", id)
	if err := autoSync(s, fmt.Sprintf("st queue rm: %s", id)); err != nil {
		return 1
	}
	return 0
}

// removeSprintSourcedQueueEntry drops the entry for `id` IFF it was
// added by `st sprint add` (Source="sprint"). Operator-queued entries
// ("manual" or empty Source) are left in place so removing an item from
// a sprint doesn't yank work the operator explicitly queued.
//
// Returns (removed, err). Missing entries return (false, nil).
func removeSprintSourcedQueueEntry(cfg *config.Config, id string) (bool, error) {
	entries := LoadQueue(cfg)
	found := false
	var updated []QueueEntry
	for _, e := range entries {
		if e.ID == id && e.Source == QueueSourceSprint {
			found = true
			continue
		}
		updated = append(updated, e)
	}
	if !found {
		return false, nil
	}
	if err := SaveQueue(cfg, updated); err != nil {
		return false, err
	}
	return true, nil
}

// removeFromQueueSilently drops the entry with the given ID from the queue
// if present. Returns (removed, err). Safe to call when the ID isn't in the
// queue — that case returns (false, nil). Callers that want the user-facing
// "not in queue" message should use QueueRm; internal callers (e.g. auto-
// cleanup on st close) use this helper to stay quiet on a miss.
func removeFromQueueSilently(cfg *config.Config, id string) (bool, error) {
	entries := LoadQueue(cfg)
	found := false
	var updated []QueueEntry
	for _, e := range entries {
		if e.ID == id {
			found = true
			continue
		}
		updated = append(updated, e)
	}
	if !found {
		return false, nil
	}
	if err := SaveQueue(cfg, updated); err != nil {
		return false, err
	}
	return true, nil
}

// QueueAutoApprove bulk-approves every pending queue entry whose ID is
// goal-reachable (T-412). Clears the existing approval backlog in one
// shot without requiring per-item `st queue approve` calls.
func QueueAutoApprove(s *store.Store, cfg *config.Config) int {
	entries := LoadQueue(cfg)
	if len(entries) == 0 {
		fmt.Println("Queue is empty — nothing to auto-approve")
		return 0
	}

	var flipped []string
	for i, e := range entries {
		if !e.Approved && IsGoalReachable(s, cfg, e.ID) {
			entries[i].Approved = true
			flipped = append(flipped, e.ID)
		}
	}

	if len(flipped) == 0 {
		fmt.Println("No pending goal-reachable items — nothing to auto-approve")
		return 0
	}

	if err := SaveQueue(cfg, entries); err != nil {
		fmt.Fprintf(os.Stderr, "saving queue: %v\n", err)
		return 1
	}
	fmt.Printf("Auto-approved %d item(s): %s\n", len(flipped), strings.Join(flipped, ", "))
	if err := autoSync(s, fmt.Sprintf("st queue auto-approve: %d item(s)", len(flipped))); err != nil {
		return 1
	}
	if orphans := GoalOrphans(s, cfg); len(orphans) > 0 {
		fmt.Printf("⚠ %d orphan queue item(s) not in any active goal — run `st goal review` to reconcile\n", len(orphans))
	}
	return 0
}

// QueuePrune drops every queue entry whose underlying item has a terminal
// status (resolved/completed/wontfix/abandoned/etc per the type config).
// Keeps entries for items that no longer exist in the store (so broken
// references still surface in queue show) — only terminal items are
// dropped.
func QueuePrune(s *store.Store, cfg *config.Config) int {
	entries := LoadQueue(cfg)
	if len(entries) == 0 {
		fmt.Println("Queue is empty — nothing to prune")
		return 0
	}

	var kept []QueueEntry
	var dropped []string
	for _, e := range entries {
		item, ok := s.Get(e.ID)
		if !ok {
			kept = append(kept, e)
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			dropped = append(dropped, fmt.Sprintf("%s (%s)", e.ID, item.Status))
			continue
		}
		kept = append(kept, e)
	}

	if len(dropped) == 0 {
		fmt.Println("No terminal items in queue — nothing to prune")
		return 0
	}

	if err := SaveQueue(cfg, kept); err != nil {
		fmt.Fprintf(os.Stderr, "saving queue: %v\n", err)
		return 1
	}

	fmt.Printf("Pruned %d terminal item(s) from queue:\n", len(dropped))
	for _, d := range dropped {
		fmt.Printf("  - %s\n", d)
	}
	if err := autoSync(s, fmt.Sprintf("st queue prune: dropped %d terminal item(s)", len(dropped))); err != nil {
		return 1
	}
	return 0
}

func QueueMove(s *store.Store, cfg *config.Config, id string, position int) int {
	entries := LoadQueue(cfg)

	idx := -1
	var entry QueueEntry
	for i, e := range entries {
		if e.ID == id {
			idx = i
			entry = e
			break
		}
	}
	if idx < 0 {
		fmt.Fprintf(os.Stderr, "%s not in queue\n", id)
		return 1
	}

	// I-489: an operator move is an explicit override. Flip Source to
	// "manual" so `st sprint rm` no longer cascade-removes this entry —
	// intentional, since the operator's manual placement signals "I want
	// this in the queue regardless of sprint membership." Pre-I-1322
	// this also prevented a sprint-add chain walk from displacing the
	// entry; that chain walk no longer exists but the cascade-remove
	// guard is still the right behaviour.
	entry.Source = QueueSourceManual

	entries = append(entries[:idx], entries[idx+1:]...)

	pos := position - 1
	if pos < 0 {
		pos = 0
	}
	if pos > len(entries) {
		pos = len(entries)
	}

	updated := make([]QueueEntry, 0, len(entries)+1)
	updated = append(updated, entries[:pos]...)
	updated = append(updated, entry)
	updated = append(updated, entries[pos:]...)

	if err := SaveQueue(cfg, updated); err != nil {
		fmt.Fprintf(os.Stderr, "saving queue: %v\n", err)
		return 1
	}
	fmt.Printf("Moved %s to position %d\n", id, position)
	if err := autoSync(s, fmt.Sprintf("st queue move: %s -> %d", id, position)); err != nil {
		return 1
	}
	return 0
}

func QueueApprove(_ *store.Store, _ *config.Config, _ string, _ QueueApproveOpts) int {
	fmt.Println("Queue entries are auto-approved — no action needed (T-461: approval gate removed).")
	fmt.Println("Use `st queue add <id>` to pin an item to boost its scheduling priority.")
	return 0
}

// --- Persistence ---

// LoadQueue reads the queue file. Returns empty slice if not found.
func LoadQueue(cfg *config.Config) []QueueEntry {
	path := cfg.QueuePath()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []QueueEntry
	var current *QueueEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "queue:" {
			continue
		}

		if strings.HasPrefix(trimmed, "- id:") {
			if current != nil {
				entries = append(entries, *current)
			}
			current = &QueueEntry{
				ID:       strings.TrimSpace(strings.TrimPrefix(trimmed, "- id:")),
				Approved: true,
			}
			continue
		}

		if current == nil {
			continue
		}

		if idx := strings.Index(trimmed, ":"); idx >= 0 {
			key := strings.TrimSpace(trimmed[:idx])
			val := strings.TrimSpace(trimmed[idx+1:])
			val = strings.Trim(val, `"`)
			switch key {
			case "added_at":
				current.AddedAt = val
			case "added_by":
				current.AddedBy = val
			case "reason":
				current.Reason = val
			case "approved":
				current.Approved = val == "true"
			case "source":
				current.Source = val
			}
		}
	}
	if current != nil {
		entries = append(entries, *current)
	}
	return entries
}

// SaveQueue writes the queue file.
func SaveQueue(cfg *config.Config, entries []QueueEntry) error {
	var sb strings.Builder
	sb.WriteString("queue:\n")
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("  - id: %s\n", e.ID))
		if e.AddedAt != "" {
			sb.WriteString(fmt.Sprintf("    added_at: %s\n", e.AddedAt))
		}
		if e.AddedBy != "" {
			sb.WriteString(fmt.Sprintf("    added_by: %s\n", e.AddedBy))
		}
		if e.Reason != "" {
			reason := e.Reason
			if strings.ContainsAny(reason, ":{}[]&*?|>!%@`#") {
				reason = fmt.Sprintf("%q", reason)
			}
			sb.WriteString(fmt.Sprintf("    reason: %s\n", reason))
		}
		if !e.Approved {
			sb.WriteString("    approved: false\n")
		}
		// I-489: persist Source verbatim when set. Empty stays empty
		// (skipped) so the file format for entries that never carried
		// Source remains compact. "manual" is written explicitly so a
		// `queue move`-created pin survives reload — without this the
		// round-trip would collapse "manual" back to empty and a future
		// `sprint add` would treat the entry as an unrecognized source.
		if e.Source != "" {
			sb.WriteString(fmt.Sprintf("    source: %s\n", e.Source))
		}
	}
	return os.WriteFile(cfg.QueuePath(), []byte(sb.String()), 0644)
}

// --- Next Action ---

// NextAction computes the advisory next action for an active item.
func NextAction(s *store.Store, cfg *config.Config, id string) string {
	item, ok := s.Get(id)
	if !ok {
		return ""
	}
	return nextActionForItem(item, id, cfg)
}

func nextActionForItem(item *model.Item, id string, cfg *config.Config) string {
	stage := deliveryStage(item)
	hasTests := hasItemTests(item, cfg)
	hasManifest := hasItemManifest(item)

	switch {
	case item.Status != "active":
		return fmt.Sprintf("st start %s — activate this item", id)
	case stage == "" || stage == "coding":
		if !hasTests {
			return fmt.Sprintf("Run tests, then: st test %s <suite> --run", id)
		}
		if !hasManifest {
			return fmt.Sprintf("Create PR, then: st pr %s --repo <repo> --pr <N>", id)
		}
		return "Continue coding — tests pass, PR recorded"
	case stage == "pushed" || stage == "pr_open":
		if !hasManifest {
			return fmt.Sprintf("st pr %s --repo <repo> --pr <N>", id)
		}
		return "Waiting for CI / merge"
	case stage == "merged":
		return fmt.Sprintf("Verify deployment: st deploy-check %s", id)
	case stage == "deployed_dev":
		return "Ready for UAT — present evidence to user"
	default:
		return ""
	}
}

func hasItemTests(item *model.Item, cfg *config.Config) bool {
	if cfg.Testing == nil {
		return true
	}
	// I-776: query the item's class-scoped required-suite set so the queue
	// advisor agrees with the gate on what "tests recorded" means. An item
	// with workspace_test=pass should not be flagged as "tests not passing"
	// just because it lacks api_unit evidence.
	requiredSuites, classOK := cfg.Testing.RequiredSuitesFor(item.ScopeClass)
	if !classOK {
		// Unknown scope_class — treat as "needs attention" so the next-action
		// hint surfaces something rather than silently passing.
		return false
	}
	for name := range requiredSuites {
		val, ok := item.TestingEvidence[name]
		if !ok {
			return false
		}
		s, ok := val.(string)
		if !ok || !strings.HasPrefix(s, "pass") {
			return false
		}
	}
	return true
}

func hasItemManifest(item *model.Item) bool {
	if item.Manifest == nil {
		return false
	}
	prs, ok := item.Manifest["prs"]
	if !ok {
		return false
	}
	str, ok := prs.(string)
	return ok && str != "" && str != "null"
}
