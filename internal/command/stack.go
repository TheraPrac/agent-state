package command

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// StackEntry represents an item on the per-agent work stack.
type StackEntry struct {
	ID       string
	Reason   string // why this was pushed (what blocked the parent)
	PushedAt string
	PushedBy string // agent ID
	Repos    map[string]StackRepoState
}

// StackRepoState tracks branch and last commit for a repo.
type StackRepoState struct {
	Branch     string
	LastCommit string
}

// --- Commands ---

// StackPushOpts holds flags for stack push.
type StackPushOpts struct {
	Reason string
	// FromPending lets the operator push a pending-approval item onto
	// the stack without first approving it. Useful when interrupting
	// current work to investigate an item that hasn't graduated through
	// the queue yet. Default behavior (FromPending=false) is to refuse
	// pending items per the I-490 gate.
	FromPending bool
}

func StackPush(s *store.Store, cfg *config.Config, id string, opts StackPushOpts) int {
	if _, ok := s.Get(id); !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	entries := LoadStack(cfg)

	// Check if already on stack
	for _, e := range entries {
		if e.ID == id {
			fmt.Fprintf(os.Stderr, "%s is already on the stack\n", id)
			return 1
		}
	}

	// I-490: stack push honors the queue approval gate by default. The
	// --from-pending flag lets the operator interrupt-mode an item
	// that hasn't yet graduated through the queue.
	if !opts.FromPending && IsQueuePending(cfg, id) {
		fmt.Fprintf(os.Stderr,
			"%s is pending operator approval — run `st queue approve %s` first (or `st push %s --from-pending` to push without approving)\n",
			id, id, id)
		return 1
	}

	// Capture repo state from the item's work_tracking
	repos := map[string]StackRepoState{}
	item, _ := s.Get(id)
	if branch, ok := item.WorkTracking["branch"]; ok {
		if bs, ok := branch.(string); ok && bs != "" && bs != "null" {
			// Parse branch — might be "feat/T-001-slug" for a single repo
			repos["default"] = StackRepoState{Branch: bs}
		}
	}

	agentID := cfg.AgentID()
	if agentID == "" {
		agentID = "user"
	}

	entry := StackEntry{
		ID:       id,
		Reason:   opts.Reason,
		PushedAt: time.Now().Format(time.RFC3339),
		PushedBy: agentID,
		Repos:    repos,
	}

	// Push onto top (end of slice = top of stack)
	entries = append(entries, entry)

	if err := SaveStack(cfg, entries); err != nil {
		fmt.Fprintf(os.Stderr, "saving stack: %v\n", err)
		return 1
	}

	depth := len(entries)
	fmt.Printf("Pushed %s onto stack (depth %d)\n", id, depth)
	if opts.Reason != "" {
		fmt.Printf("  reason: %s\n", opts.Reason)
	}
	autoSync(s, fmt.Sprintf("st push: %s (depth %d)", id, depth))
	return 0
}

func StackPop(s *store.Store, cfg *config.Config) int {
	entries := LoadStack(cfg)
	if len(entries) == 0 {
		fmt.Println("Stack is empty")
		return 0
	}

	// Pop from top
	popped := entries[len(entries)-1]
	entries = entries[:len(entries)-1]

	// Check if the popped item is already resolved
	resolved := false
	if item, ok := s.Get(popped.ID); ok {
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			resolved = true
		}
	}

	if err := SaveStack(cfg, entries); err != nil {
		fmt.Fprintf(os.Stderr, "saving stack: %v\n", err)
		return 1
	}

	if resolved {
		fmt.Printf("Popped %s (already resolved)\n", popped.ID)
	} else {
		fmt.Printf("Popped %s\n", popped.ID)
	}

	// Auto-pop resolved items and show what to return to
	for len(entries) > 0 {
		top := entries[len(entries)-1]
		topResolved := false
		if item, ok := s.Get(top.ID); ok {
			if cfg.IsTerminalStatus(item.Type, item.Status) {
				topResolved = true
			}
		}
		if !topResolved {
			item, _ := s.Get(top.ID)
			title := ""
			if item != nil {
				title = " — " + item.Title
			}
			fmt.Printf("Returning to %s%s\n", top.ID, title)
			autoSync(s, fmt.Sprintf("st pop: %s (returning to %s)", popped.ID, top.ID))
			return 0
		}
		fmt.Printf("  %s also resolved — skipping\n", top.ID)
		entries = entries[:len(entries)-1]
	}

	if err := SaveStack(cfg, entries); err != nil {
		fmt.Fprintf(os.Stderr, "saving stack: %v\n", err)
		return 1
	}

	fmt.Println("Stack is now empty")
	autoSync(s, fmt.Sprintf("st pop: %s", popped.ID))
	return 0
}

func StackShow(s *store.Store, cfg *config.Config) int {
	entries := LoadStack(cfg)
	if len(entries) == 0 {
		fmt.Println("Stack is empty")
		return 0
	}

	fmt.Printf("%sWork Stack%s (depth %d)\n\n", cBold, cReset, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		item, ok := s.Get(e.ID)
		title := "(not found)"
		status := ""
		if ok {
			title = truncate(item.Title, 45)
			status = item.Status
		}

		resolved := ""
		if ok && cfg.IsTerminalStatus(item.Type, item.Status) {
			resolved = fmt.Sprintf("  %s✓ resolved%s", cGreen, cReset)
		}

		marker := ""
		if i == len(entries)-1 {
			marker = " ← you are here"
		}

		fmt.Printf("  %d: %s%-8s%s %s  %s(%s)%s%s%s\n",
			i, cBold, e.ID, cReset, title, cDim, status, cReset, resolved, marker)
		if e.Reason != "" {
			fmt.Printf("     %s%s%s\n", cDim, e.Reason, cReset)
		}
	}
	fmt.Println()
	return 0
}

// --- Persistence ---

// LoadStack reads the stack file. Returns empty slice if not found.
// Stack is ordered bottom-to-top (last element = top of stack).
func LoadStack(cfg *config.Config) []StackEntry {
	path := cfg.StackPath()
	if cfg.AgentID() != "" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			legacy := filepath.Join(cfg.Root(), ".as", "stack.yaml")
			if _, legacyErr := os.Stat(legacy); legacyErr == nil {
				path = legacy
			}
		}
	}

	return loadStackFile(path)
}

func loadStackFile(path string) []StackEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []StackEntry
	var current *StackEntry
	inRepos := false
	var currentRepo string

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "stack:" {
			continue
		}

		if strings.HasPrefix(trimmed, "- id:") {
			if current != nil {
				entries = append(entries, *current)
			}
			current = &StackEntry{
				ID:    strings.TrimSpace(strings.TrimPrefix(trimmed, "- id:")),
				Repos: map[string]StackRepoState{},
			}
			inRepos = false
			continue
		}

		if current == nil {
			continue
		}

		if trimmed == "repos:" {
			inRepos = true
			continue
		}

		if idx := strings.Index(trimmed, ":"); idx >= 0 {
			key := strings.TrimSpace(trimmed[:idx])
			val := strings.TrimSpace(trimmed[idx+1:])
			val = strings.Trim(val, `"`)

			if inRepos {
				if val == "" {
					// Repo name as section header
					currentRepo = key
				} else if currentRepo != "" {
					state := current.Repos[currentRepo]
					switch key {
					case "branch":
						state.Branch = val
					case "last_commit":
						state.LastCommit = val
					}
					current.Repos[currentRepo] = state
				}
			} else {
				switch key {
				case "reason":
					current.Reason = val
				case "pushed_at":
					current.PushedAt = val
				case "pushed_by":
					current.PushedBy = val
				}
			}
		}
	}
	if current != nil {
		entries = append(entries, *current)
	}
	return entries
}

// SaveStack writes the stack file.
func SaveStack(cfg *config.Config, entries []StackEntry) error {
	path := cfg.StackPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("stack:\n")
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("  - id: %s\n", e.ID))
		if e.Reason != "" {
			reason := e.Reason
			if strings.ContainsAny(reason, ":{}[]&*?|>!%@`#") {
				reason = fmt.Sprintf("%q", reason)
			}
			sb.WriteString(fmt.Sprintf("    reason: %s\n", reason))
		}
		if e.PushedAt != "" {
			sb.WriteString(fmt.Sprintf("    pushed_at: %s\n", e.PushedAt))
		}
		if e.PushedBy != "" {
			sb.WriteString(fmt.Sprintf("    pushed_by: %s\n", e.PushedBy))
		}
		if len(e.Repos) > 0 {
			sb.WriteString("    repos:\n")
			for name, state := range e.Repos {
				sb.WriteString(fmt.Sprintf("      %s:\n", name))
				if state.Branch != "" {
					sb.WriteString(fmt.Sprintf("        branch: %s\n", state.Branch))
				}
				if state.LastCommit != "" {
					sb.WriteString(fmt.Sprintf("        last_commit: %s\n", state.LastCommit))
				}
			}
		}
	}
	return os.WriteFile(cfg.StackPath(), []byte(sb.String()), 0644)
}
