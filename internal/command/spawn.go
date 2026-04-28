package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SpawnChildOpts holds inputs for `st spawn child <item>`.
type SpawnChildOpts struct {
	// Item is the item id the child will work on. v1 supports only
	// items the parent already claims (same-item spawn shares the
	// parent's worktree per T-312). Different-item spawn is a tracked
	// follow-up — until then the spawn returns an explanatory error.
	Item string
}

// SpawnChild materializes a child agent registration under the
// caller's identity. T-326 / T-312.
//
// Behavior:
//   - Resolves parent identity from the calling process via
//     cfg.Identity(). Refuses if no identity is bound.
//   - Enforces the depth-2 cap: if the caller is already a child
//     (Identity.ParentID is set), spawning again would reach
//     grandchild depth — rejected with a policy message.
//   - Calls agent.Register with ParentAgentID + RootAgentID set so
//     the child's session events roll up to the root for cost
//     attribution (I-369).
//   - For v1, only same-item spawn is supported. The parent's claim
//     covers the child's work; no new worktree is created. Different
//     item spawn is intentionally rejected with a follow-up pointer.
//
// Returns the child agent id + PID on stdout (one line, tab-separated)
// so the caller can pipe into a subprocess launcher. Registration is
// PID=os.Getpid() of THIS spawn-child invocation: the child agent
// inherits the parent's process tree until the caller exec's a real
// subprocess (out of scope for v1).
func SpawnChild(s *store.Store, cfg *config.Config, opts SpawnChildOpts) int {
	if opts.Item == "" {
		fmt.Fprintln(os.Stderr, "spawn child: item id is required")
		return 2
	}

	parent := cfg.Identity()
	if parent.ID == "" {
		fmt.Fprintln(os.Stderr,
			"spawn child: no agent identity in this shell. "+
				"Set AS_AGENT_ID, run from a per-agent dir, or write "+
				".as/local-agent.yaml.")
		return 1
	}

	// Depth-2 policy: a child cannot itself spawn another child.
	// Identity.ParentID being non-empty means the caller is already
	// a child (root agents have ParentID == "").
	if parent.ParentID != "" {
		fmt.Fprintf(os.Stderr,
			"spawn child: %s is already a child of %s — depth-2 cap reached. "+
				"Spawn from the root agent (%s) instead.\n",
			parent.ID, parent.ParentID, parent.RootID)
		return 1
	}

	// V1: same-item spawn only. The parent's claim on `Item` covers
	// the child's work. Verify the item exists; verify the parent
	// claimed it (or is unclaimed and the parent is going to start).
	item, ok := s.Get(opts.Item)
	if !ok {
		fmt.Fprintf(os.Stderr, "spawn child: item %s not found\n", opts.Item)
		return 1
	}
	parentSession := cfg.SessionID()
	if item.ClaimedBy != "" && item.ClaimedBy != parentSession {
		fmt.Fprintf(os.Stderr,
			"spawn child: %s is claimed by session %s, not by parent session %s\n",
			opts.Item, item.ClaimedBy, parentSession)
		return 1
	}

	// Compute the root: parent.RootID was set if the parent is a
	// child of yet-another-agent (we already rejected that above), so
	// here parent IS root. RootID falls back to parent.ID.
	rootID := parent.RootID
	if rootID == "" {
		rootID = parent.ID
	}

	// Inherit the spawning session so I-369's cost rollup walks the
	// chain from child → parent → root.
	reg, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID:      parent.ID,
		ParentAgentID:    parent.ID,
		RootAgentID:      rootID,
		Role:             "child",
		SessionID:        parentSession,
		SpawnedBySession: parentSession,
		Scope:            "item:" + opts.Item,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn child: register: %v\n", err)
		return 1
	}

	// Tab-separated so callers can pipe into `cut` / `read`.
	fmt.Printf("%s\t%d\n", reg.AgentID, reg.PID)
	return 0
}
