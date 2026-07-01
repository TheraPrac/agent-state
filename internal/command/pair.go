package command

import (
	"fmt"
	"os"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/session"
	"github.com/theraprac/agent-state/internal/store"
)

// PairOpts holds flags for `st pair`.
type PairOpts struct {
	Off bool // deactivate pairing on this session instead of activating it
}

// Pair activates or deactivates the I-1700 `/pair` live-iteration mode on the
// CURRENT session — a session-local, ephemeral marker written into this
// session's yaml (.as/sessions/<id>.yaml), never changelog-logged or synced
// (unlike `st hotfix`'s item-level flag). Hooks read it via the shared
// pairing-mode.sh bash fragment to relax in-session friction (plan-before-code,
// worktree-dirty exit, model-check, advisory nags) for the paired item.
//
// Slice 1 (I-1704) scope is deliberately narrow: only the bare "attach to the
// current stack-top item" form and "--off" are implemented. Attaching by id
// or title (M2/M3/M4 in the design) requires tp reuse-or-start semantics that
// don't exist until I-1705/I-1706 — passing an argument here returns a clear,
// honest error rather than silently no-op'ing.
//
//	st pair          -> attach: mark the current stack-top item as paired
//	st pair --off    -> detach: clear the marker on this session
//	st pair <arg>    -> not yet implemented (tracked: I-1706); exit 2
func Pair(s *store.Store, cfg *config.Config, sessMgr *session.Manager, sessionID string, args []string, opts PairOpts) int {
	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "st pair: no current session id resolved (.as/session missing or empty)")
		return 1
	}

	if opts.Off {
		if len(args) != 0 {
			fmt.Fprintln(os.Stderr, "st pair --off: takes no arguments")
			return 2
		}
		cleared, err := sessMgr.ClearPairing(sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "st pair --off: %v\n", err)
			return 1
		}
		if cleared {
			fmt.Println("Pairing OFF — gates re-enabled.")
		} else {
			fmt.Println("st pair --off: no active pairing on this session — nothing to clear.")
		}
		return 0
	}

	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "st pair: attach-by-id/title is not implemented yet (tracked: I-1706) — "+
			"run bare `st pair` to attach the current stack-top item, or `st pair --off` to detach.\n")
		return 2
	}

	entries := LoadStack(cfg)
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "st pair: no active item on stack — st start/resume one first")
		return 1
	}
	id := entries[len(entries)-1].ID

	if _, ok := s.Get(id); !ok {
		fmt.Fprintf(os.Stderr, "st pair: stack-top item %s not found\n", id)
		return 1
	}

	// `st resume <id>` (the documented re-entry point for a continuing
	// session) never creates the session yaml — only `st start`/`st sprint
	// join` do, via EnsureSession(WithIdentity). Without this, a session
	// that resumed existing work and then ran `st pair` (its exact intended
	// use case) would fail with "session not found" on SetPairing below.
	if _, err := sessMgr.EnsureSession(sessionID, cfg.AgentID()); err != nil {
		fmt.Fprintf(os.Stderr, "st pair: %v\n", err)
		return 1
	}

	p := &session.Pairing{
		Active: true,
		Item:   id,
		// Worktree currently always equals Item (Slice 1 only supports
		// attaching the stack-top item, whose worktree name is its own ID).
		// It's kept as a distinct field because Slice 3 (I-1706) attach
		// modes may resolve a worktree that differs from the item ID.
		Worktree:    id,
		ActivatedAt: time.Now(),
	}
	if err := sessMgr.SetPairing(sessionID, p); err != nil {
		fmt.Fprintf(os.Stderr, "st pair: %v\n", err)
		return 1
	}

	fmt.Printf("Pairing ON for %s — in-session friction relaxed (plan gate, worktree-dirty exit, advisory nags).\n", id)
	fmt.Printf("  The merge gate (tier1/tier2/live-acceptance) is untouched and runs fresh at merge.\n")
	fmt.Printf("  Detach with:  st pair --off\n")
	return 0
}
