package command

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// I-489 chain-position constants. The formula
// `epic_priority*epicMul + sprint_position*sprintMul + within-sprint`
// produces a deterministic insert key for sprint-sourced queue entries
// so the queue reflects epic→sprint→item priority by default. Manual
// `queue move` overrides aren't subject to this — see
// findChainInsertIndex for how operator-pinned entries are tolerated.
const (
	epicMul           = 1_000_000
	sprintMul         = 10_000
	unprioritizedEpic = 999 // sentinel: epics without Priority sort after numbered ones
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
	// the entry was created as a side effect of `st sprint add`; sprint
	// rm then cascade-removes it. "manual" means the operator placed
	// or re-placed the entry explicitly (via `st queue add` or via
	// `st queue move`, which I-489 flips to "manual" on move) — sprint
	// rm leaves it alone and the chain-position walk skips it. The
	// empty string is a legacy on-disk form (pre-I-488) treated as
	// equivalent to "manual" at runtime: SaveQueue writes "manual"
	// explicitly when the field is set, but skips the line for empty
	// so the file format stays compact for entries that never carried
	// the field. Future readers must check `Source != QueueSourceSprint`
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

// IsQueuePending reports whether `id` has a queue entry that is still
// awaiting operator approval (Approved=false). Returns false when the
// item is not on the queue at all — the I-490 gate only fires for
// pending entries, so items never queued can still be `st start`-ed.
func IsQueuePending(cfg *config.Config, id string) bool {
	for _, e := range LoadQueue(cfg) {
		if e.ID == id {
			return !e.Approved
		}
	}
	return false
}

// PendingApprovalCount returns how many queue entries are waiting on
// operator approval. Surfaced by `st prime` / `st status` so the
// session-start banner highlights pending items the operator needs to
// approve before agents can pick them up (I-490).
func PendingApprovalCount(cfg *config.Config) int {
	n := 0
	for _, e := range LoadQueue(cfg) {
		if !e.Approved {
			n++
		}
	}
	return n
}

// autoSync commits + pushes any working-tree changes left by a state-mutating
// command (queue/stack/sprint writes that touch .as/* files). Best-effort —
// failures log to stderr but don't fail the caller. Without this, every
// st-command write left the working tree dirty until the operator ran
// `st sync` manually, which the session-stop hook then flagged on every
// Stop event (I-415).
func autoSync(s *store.Store, msg string) {
	if s == nil {
		return
	}
	if err := s.GitSync(msg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: auto-sync failed: %v (run `st sync` manually)\n", err)
	}
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
			fmt.Fprintf(os.Stderr, "%s is already in the queue\n", id)
			return 1
		}
	}

	agentID := cfg.AgentID()
	addedBy := "user"
	approved := true
	if agentID != "" {
		addedBy = agentID
		approved = false
	}

	entries = append(entries, QueueEntry{
		ID:       id,
		AddedAt:  time.Now().Format(time.RFC3339),
		AddedBy:  addedBy,
		Reason:   opts.Reason,
		Approved: approved,
	})

	if err := SaveQueue(cfg, entries); err != nil {
		fmt.Fprintf(os.Stderr, "saving queue: %v\n", err)
		return 1
	}

	status := ""
	if !approved {
		status = " (pending approval)"
	}
	fmt.Printf("Added %s to queue at position %d%s\n", id, len(entries), status)
	autoSync(s, fmt.Sprintf("st queue add: %s", id))
	return 0
}

func QueueShow(s *store.Store, cfg *config.Config) int {
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

	fmt.Printf("%sWork Queue%s (%d items)\n\n", cBold, cReset, len(entries))
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

		fmt.Printf("  %d. %s%-8s%s %s  %s(%s)%s%s%s%s\n",
			i+1, cBold, e.ID, cReset, title, cDim, status, cReset, active, blocked, approval)

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
	entries := LoadQueue(cfg)
	g := deps.Build(s.All(), cfg)

	for _, e := range entries {
		if !e.Approved {
			continue
		}
		if g.IsBlocked(e.ID) {
			continue
		}
		item, ok := s.Get(e.ID)
		if !ok {
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}
		if opts.Sprint != "" && item.Sprint != opts.Sprint {
			continue
		}
		fmt.Printf("%s — %s\n", e.ID, item.Title)
		return 0
	}

	if opts.Sprint != "" {
		fmt.Printf("No approved, unblocked items in queue for sprint %s\n", opts.Sprint)
	} else {
		fmt.Println("No approved, unblocked items in queue")
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
	autoSync(s, fmt.Sprintf("st queue rm: %s", id))
	return 0
}

// upsertQueueSprintEntry ensures `id` has a sprint-sourced queue entry.
// When the entry is absent we insert a new one with Approved=false (so a
// 30-item sprint doesn't flood the operator's "next" view) and
// Source="sprint" so a later `st sprint rm` cascades the removal. The
// insert position is computed from the epic→sprint→within-sprint chain
// (I-489) so high-priority epics' sprints land ahead of lower ones by
// default. Operator-pinned entries (Source != "sprint") are tolerated —
// the chain walk skips them — so prior `queue move` overrides survive.
//
// When an entry already exists we leave it alone — operator-queued
// entries (empty or "manual" Source) keep their origin so sprint rm
// won't cascade-remove them, matching the "track origin" contract in
// I-488.
//
// `s` and `r` are optional; when nil, the function falls back to
// appending at the end of the queue (used by tests / pathological
// states where the chain can't be resolved).
//
// Returns true if a new queue entry was added.
func upsertQueueSprintEntry(cfg *config.Config, s *store.Store, r *registry.Registry, id, sprintID string) (bool, error) {
	entries := LoadQueue(cfg)
	for _, e := range entries {
		if e.ID == id {
			return false, nil
		}
	}

	addedBy := cfg.AgentID()
	if addedBy == "" {
		addedBy = "user"
	}
	newEntry := QueueEntry{
		ID:       id,
		AddedAt:  time.Now().Format(time.RFC3339),
		AddedBy:  addedBy,
		Reason:   fmt.Sprintf("sprint:%s", sprintID),
		Approved: false,
		Source:   QueueSourceSprint,
	}

	insertIdx := len(entries)
	if s != nil && r != nil {
		targetPos := computeSprintQueuePosition(r, sprintID, id)
		insertIdx = findChainInsertIndex(entries, s, r, targetPos)
	}
	updated := make([]QueueEntry, 0, len(entries)+1)
	updated = append(updated, entries[:insertIdx]...)
	updated = append(updated, newEntry)
	updated = append(updated, entries[insertIdx:]...)

	if err := SaveQueue(cfg, updated); err != nil {
		return false, err
	}
	return true, nil
}

// computeSprintQueuePosition returns the deterministic chain-position
// key for a sprint-sourced item: epic priority dominates, sprint
// Sequence within the epic breaks ties, item index within the sprint
// breaks the next tie. Items in unprioritized epics sort after every
// numbered one. Used at insert time (I-489); not stored on the queue
// entry.
func computeSprintQueuePosition(r *registry.Registry, sprintID, itemID string) int {
	sp, err := r.SprintByID(sprintID)
	if err != nil {
		return unprioritizedEpic * epicMul
	}

	epicPrio := unprioritizedEpic
	if e, ok := r.GetEpic(sp.Epic); ok && e.Priority != nil {
		epicPrio = *e.Priority
	}

	sprintPos := sp.Sequence

	withinIdx := len(sp.Items) // not yet appended → land at the tail of the sprint band
	for i, sid := range sp.Items {
		if sid == itemID {
			withinIdx = i
			break
		}
	}

	return epicPrio*epicMul + sprintPos*sprintMul + withinIdx
}

// findChainInsertIndex walks the queue and returns the first index whose
// sprint-sourced predecessor has a computed chain-position greater than
// targetPos. Manual / operator-pinned entries (Source != "sprint") are
// tolerated and skipped — the operator put them there explicitly, so a
// future sprint add should not displace them. When no entry's position
// dominates targetPos, the function returns len(entries) (= append).
func findChainInsertIndex(entries []QueueEntry, s *store.Store, r *registry.Registry, targetPos int) int {
	for i, e := range entries {
		if e.Source != QueueSourceSprint {
			continue
		}
		item, ok := s.Get(e.ID)
		if !ok || item.Sprint == "" {
			continue
		}
		otherPos := computeSprintQueuePosition(r, item.Sprint, e.ID)
		if otherPos > targetPos {
			return i
		}
	}
	return len(entries)
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
	autoSync(s, fmt.Sprintf("st queue prune: dropped %d terminal item(s)", len(dropped)))
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

	// I-489: an operator move is an explicit override of the
	// epic→sprint chain ordering. Flip Source to "manual" so a future
	// `st sprint add` won't compare its chain-position against this
	// entry and silently displace it. As a side effect, `st sprint rm`
	// no longer cascade-removes this entry — also intentional, since
	// the operator's manual placement signals "I want this in the
	// queue regardless of sprint membership."
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
	autoSync(s, fmt.Sprintf("st queue move: %s -> %d", id, position))
	return 0
}

func QueueApprove(s *store.Store, cfg *config.Config, id string, opts QueueApproveOpts) int {
	if id == "" && opts.Sprint == "" {
		fmt.Fprintln(os.Stderr, "queue approve requires <id> or --sprint <slug>")
		return 2
	}
	if id != "" && opts.Sprint != "" {
		fmt.Fprintln(os.Stderr, "queue approve: <id> and --sprint are mutually exclusive")
		return 2
	}

	entries := LoadQueue(cfg)

	if opts.Sprint != "" {
		// I-491: pre-pass — collect candidates and check the plan gate.
		// Without --bypass-plan, refuse the whole bulk-approve if any
		// candidate lacks an approved plan, so the operator gets a
		// "fix these N items first" signal rather than a partial commit.
		var candidates []int
		var planless []string
		for i, e := range entries {
			if entries[i].Approved {
				continue
			}
			item, ok := s.Get(e.ID)
			if !ok {
				continue
			}
			if item.Sprint != opts.Sprint {
				continue
			}
			candidates = append(candidates, i)
			if !item.PlanApproved {
				planless = append(planless, e.ID)
			}
		}
		if len(candidates) == 0 {
			fmt.Printf("No pending sprint-%s items in queue\n", opts.Sprint)
			return 0
		}
		if !opts.BypassPlan && len(planless) > 0 {
			fmt.Fprintf(os.Stderr,
				"refusing to approve sprint %s — %d item(s) have no approved plan: %s\n",
				opts.Sprint, len(planless), strings.Join(planless, ", "))
			fmt.Fprintln(os.Stderr,
				"run `st prep <id>` (Accept) or `st plan approve <id>` for each, or pass --bypass-plan to override")
			return 1
		}
		approved := 0
		var approvedIDs []string
		for _, i := range candidates {
			entries[i].Approved = true
			approved++
			approvedIDs = append(approvedIDs, entries[i].ID)
		}
		if err := SaveQueue(cfg, entries); err != nil {
			fmt.Fprintf(os.Stderr, "saving queue: %v\n", err)
			return 1
		}
		// Audit per-bypass so each override is traceable.
		if opts.BypassPlan {
			for _, id := range planless {
				_ = changelog.Append(cfg, id, changelog.Entry{
					Op:     "approve_bypass_plan",
					Reason: fmt.Sprintf("I-491 plan gate bypassed via --bypass-plan (sprint %s bulk approve)", opts.Sprint),
				})
			}
		}
		fmt.Printf("Approved %d item(s) for sprint %s: %s\n", approved, opts.Sprint, strings.Join(approvedIDs, ", "))
		if opts.BypassPlan && len(planless) > 0 {
			fmt.Fprintf(os.Stderr, "warning: --bypass-plan overrode the I-491 plan gate for: %s\n", strings.Join(planless, ", "))
		}
		autoSync(s, fmt.Sprintf("st queue approve --sprint %s: %d item(s)", opts.Sprint, approved))
		return 0
	}

	// entryIdx defaults to -1 so a future refactor that drops the
	// `if entryIdx < 0` guard below can't silently mutate entries[0].
	entryIdx := -1
	for i, e := range entries {
		if e.ID == id {
			entryIdx = i
			break
		}
	}
	if entryIdx < 0 {
		fmt.Fprintf(os.Stderr, "%s not in queue\n", id)
		return 1
	}

	// I-491: per-item plan gate. Refuse approval when the item's plan
	// has not been operator-approved (PlanApproved=false). Bypass via
	// --bypass-plan logs to the changelog so the override is auditable.
	item, ok := s.Get(id)
	if ok && !item.PlanApproved {
		if !opts.BypassPlan {
			fmt.Fprintf(os.Stderr,
				"%s has no approved plan — run `st prep %s` (Accept) or `st plan approve %s` first (or `--bypass-plan` to override)\n",
				id, id, id)
			return 1
		}
		_ = changelog.Append(cfg, id, changelog.Entry{
			Op:     "approve_bypass_plan",
			Reason: "I-491 plan gate bypassed via --bypass-plan",
		})
		fmt.Fprintf(os.Stderr, "warning: --bypass-plan overrode the I-491 plan gate for %s\n", id)
	}

	entries[entryIdx].Approved = true
	if err := SaveQueue(cfg, entries); err != nil {
		fmt.Fprintf(os.Stderr, "saving queue: %v\n", err)
		return 1
	}
	fmt.Printf("Approved %s\n", id)
	autoSync(s, fmt.Sprintf("st queue approve: %s", id))
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
	hasTests := hasItemTests(item.TestingEvidence, cfg)
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

func hasItemTests(te map[string]interface{}, cfg *config.Config) bool {
	if cfg.Testing == nil {
		return true
	}
	for name := range cfg.Testing.RequiredSuites {
		val, ok := te[name]
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
