package command

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/coordinator"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Claim stamps claimed_by/claimed_at on an item using the CAS Mutate
// guard. It is a pure claim primitive: no status change, no worktree,
// no git — that's st start's job. The session id comes from
// cfg.SessionID(). Exits:
//   0 — claim stamped
//   1 — live conflict (another session owns it)
//   2 — bad args / no session id
func Claim(s *store.Store, cfg *config.Config, itemID string) int {
	if itemID == "" {
		fmt.Fprintln(os.Stderr, "claim: item id is required")
		return 2
	}
	sessionID := cfg.SessionID()
	if sessionID == "" {
		fmt.Fprintln(os.Stderr,
			"claim: no AS_SESSION_ID set. A session id is required so the claim is unambiguous.")
		return 2
	}
	if _, ok := s.Get(itemID); !ok {
		fmt.Fprintf(os.Stderr, "claim: item %s not found\n", itemID)
		return 1
	}

	now := time.Now().Format(time.RFC3339)
	err := s.Mutate(itemID, func(it *model.Item) error {
		if it.ClaimedBy != "" && it.ClaimedBy != sessionID {
			if isSessionLive(cfg, it.ClaimedBy) {
				return store.ErrAlreadyClaimed
			}
			fmt.Printf("claim: releasing stale claim from session %s\n", it.ClaimedBy)
		}
		it.ClaimedBy = sessionID
		it.ClaimedAt = now
		it.Doc.SetField("claimed_by", sessionID)
		it.Doc.SetField("claimed_at", now)
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrAlreadyClaimed) {
			if it, ok := s.Get(itemID); ok {
				fmt.Fprintf(os.Stderr, "claim: %s is claimed by live session %s (since %s)\n",
					itemID, it.ClaimedBy, it.ClaimedAt)
			} else {
				fmt.Fprintf(os.Stderr, "claim: %s is claimed (owner no longer readable)\n", itemID)
			}
			return 1
		}
		fmt.Fprintf(os.Stderr, "claim: writing %s: %v\n", itemID, err)
		return 1
	}
	fmt.Printf("claimed %s (session %s)\n", itemID, sessionID)
	return 0
}

// spawnFn is the indirection seam for tests — overridden in dispatch tests
// to verify claim-before-spawn ordering and failure recovery without forking
// a real claude binary.
var spawnFn = Spawn

// DispatchOpts holds flags for `st dispatch`.
type DispatchOpts struct {
	// Parallelism is the number of items to launch (0 or negative → 1).
	Parallelism int
	// DryRun shows picks and spawn plans without launching anything.
	DryRun bool
	// BudgetOverride lowers the per-item cap (forwarded to Spawn; a value
	// above the coordinator cap is rejected there).
	BudgetOverride float64
}

// Dispatch is a one-shot N-item fan-out launcher: it picks up to
// Parallelism items from the recommend queue (honouring C1 conflict
// exclusions), claims each via the Mutate CAS guard, and spawns each
// as a budget-capped detached worker using the existing Spawn() path.
// It does NOT supervise — supervision is st coordinate's job.
// Returns a process exit code.
func Dispatch(s *store.Store, cfg *config.Config, opts DispatchOpts) int {
	// Read coordinator boundary — mandatory; no boundary = no dispatch.
	bPath := coordinator.CoordinatorYAMLPath(cfg.Root())
	b, err := coordinator.LoadBoundary(bPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dispatch: %v\n", err)
		return 1
	}

	n := opts.Parallelism
	if n <= 0 {
		n = 1
	}
	// Cap to the boundary's parallelism_cap; never exceed it.
	parCap := b.ParallelismCap
	if parCap < 1 {
		parCap = 1
	}
	if n > parCap {
		fmt.Fprintf(os.Stderr,
			"dispatch: --parallelism %d exceeds coordinator.yaml parallelism_cap %d; capping\n",
			n, parCap)
		n = parCap
	}

	sessionID := cfg.SessionID()
	if !opts.DryRun && sessionID == "" {
		fmt.Fprintln(os.Stderr,
			"dispatch: no AS_SESSION_ID set. A session id is required for claims.")
		return 1
	}

	// Sweep dead agents and stale claims before picking, so items orphaned by
	// crashed sessions appear dispatchable (mirrors Coordinate's primeClaimState call).
	primeClaimState(s, cfg)

	// Pick up to N items, serialised, tracking occupied IDs and in-flight
	// conflict classes so selectNextExcluding is consistent across picks.
	occupied := map[string]bool{}
	var inflightClasses []string
	type pick struct {
		item    *model.Item
		why     string
		classes []string
	}
	var picks []pick

	for len(picks) < n {
		item, why := selectNextExcluding(cfg, occupied, inflightClasses)
		if item == nil {
			if len(picks) == 0 {
				fmt.Fprintf(os.Stderr, "dispatch: nothing dispatchable (%s)\n", why)
				return 1
			}
			// Fewer than requested — not an error; just fewer items in queue.
			break
		}
		classes := loadConflictClasses(cfg, item.ID)
		occupied[item.ID] = true
		inflightClasses = append(inflightClasses, classes...)
		picks = append(picks, pick{item: item, why: why, classes: classes})
	}

	if opts.DryRun {
		fmt.Printf("DRY RUN — nothing claimed or launched\n")
		fmt.Printf("boundary: %s\n", bPath)
		fmt.Printf("  parallelism=%d per_item=$%g\n", b.ParallelismCap, b.PerItemUSD)
		fmt.Printf("picks (%d):\n", len(picks))
		for i, p := range picks {
			fmt.Printf("  [%d] %s — %s\n", i+1, p.item.ID, p.item.Title)
			fmt.Printf("       why: %s\n", p.why)
			fmt.Printf("       next: st spawn %s%s\n", p.item.ID, spawnBudgetSuffix(opts.BudgetOverride))
		}
		fmt.Println("\nMonitor with: st watch   |   st tui")
		return 0
	}

	// Claim-then-spawn each pick sequentially so concurrent coordinators
	// cannot race us for the same item between claim and spawn.
	launched := 0
	for _, p := range picks {
		// CAS claim — skip on conflict (another agent grabbed it).
		now := time.Now().Format(time.RFC3339)
		claimErr := s.Mutate(p.item.ID, func(it *model.Item) error {
			if it.ClaimedBy != "" && it.ClaimedBy != sessionID {
				if isSessionLive(cfg, it.ClaimedBy) {
					return store.ErrAlreadyClaimed
				}
			}
			it.ClaimedBy = sessionID
			it.ClaimedAt = now
			it.Doc.SetField("claimed_by", sessionID)
			it.Doc.SetField("claimed_at", now)
			return nil
		})
		if claimErr != nil {
			if errors.Is(claimErr, store.ErrAlreadyClaimed) {
				fmt.Fprintf(os.Stderr,
					"dispatch: %s claimed by another session between pick and spawn — skipping\n",
					p.item.ID)
				continue
			}
			fmt.Fprintf(os.Stderr, "dispatch: claim %s: %v\n", p.item.ID, claimErr)
			continue
		}

		rc := spawnFn(s, cfg, SpawnOpts{
			Item:           p.item.ID,
			BudgetOverride: opts.BudgetOverride,
		})
		if rc != 0 {
			// Spawn failed — release the claim so the item isn't orphaned.
			_ = s.Mutate(p.item.ID, func(it *model.Item) error {
				if it.ClaimedBy == sessionID {
					it.ClaimedBy = ""
					it.ClaimedAt = ""
					it.Doc.SetField("claimed_by", "")
					it.Doc.SetField("claimed_at", "")
				}
				return nil
			})
			fmt.Fprintf(os.Stderr, "dispatch: spawn %s failed (rc=%d) — claim released\n",
				p.item.ID, rc)
			continue
		}
		launched++
	}

	if launched == 0 {
		fmt.Fprintln(os.Stderr, "dispatch: no workers launched")
		return 1
	}

	fmt.Printf("\n%d worker(s) launched\n", launched)
	fmt.Printf("  monitor: st watch   |   st tui\n")
	return 0
}
