package command

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// QueueEntry represents an item in the user-controlled work queue.
type QueueEntry struct {
	ID       string
	AddedAt  string
	AddedBy  string // "user" or agent ID
	Reason   string
	Approved bool // agent-added items need user approval
}

// QueueOpts holds flags for queue commands.
type QueueOpts struct {
	Reason string
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
	return 0
}

func QueueShow(s *store.Store, cfg *config.Config) int {
	entries := LoadQueue(cfg)
	if len(entries) == 0 {
		fmt.Println("Queue is empty")
		return 0
	}

	g := deps.Build(s.All(), cfg)

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

		if e.Reason != "" {
			fmt.Printf("     %s%s%s\n", cDim, e.Reason, cReset)
		}
	}
	fmt.Println()
	return 0
}

func QueueNext(s *store.Store, cfg *config.Config) int {
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
		fmt.Printf("%s — %s\n", e.ID, item.Title)
		return 0
	}

	fmt.Println("No approved, unblocked items in queue")
	return 0
}

func QueueRm(cfg *config.Config, id string) int {
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
	return 0
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
	return 0
}

func QueueMove(cfg *config.Config, id string, position int) int {
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
	return 0
}

func QueueApprove(cfg *config.Config, id string) int {
	entries := LoadQueue(cfg)
	found := false
	for i, e := range entries {
		if e.ID == id {
			entries[i].Approved = true
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "%s not in queue\n", id)
		return 1
	}
	if err := SaveQueue(cfg, entries); err != nil {
		fmt.Fprintf(os.Stderr, "saving queue: %v\n", err)
		return 1
	}
	fmt.Printf("Approved %s\n", id)
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
