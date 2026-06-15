package command

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// CoShipOpts holds flags for `st coship`.
type CoShipOpts struct {
	Off       bool   // clear the flag instead of setting it
	APIRef    string // the paired api git ref to resolve the web OpenAPI check against
	ActiveRef bool   // print the active (stack-top) item's coship ref, machine-readable
}

// CoShip sets/clears the st-owned `coship_api_ref` flag on an item, prints the
// active item's ref for the web OpenAPI check to consume, or lists flagged items.
//
// Co-ship mode is the durable, audited escape hatch for paired api+web contract
// changes (I-1476). The web pre-commit `check-openapi-sync.sh` normally requires
// `openapi.yml` to byte-match the api spec on origin/main; a paired web change
// carries the new contract before the api PR merges, deadlocking the commit. When
// an item is flagged, the web check resolves the backend spec against the paired
// api ref (a local branch in the sibling api worktree) instead of origin/main, so
// both sides can commit/push before either merges. Default stays strict. Every
// flip is changelog-logged and git-synced via autoSync, so a bypass is never
// silent.
//
// Dispatch on the flags/args:
//
//	st coship                       -> list items currently in co-ship mode
//	st coship --active-ref          -> print the stack-top item's ref (or nothing)
//	st coship <ID> --api-ref <ref>  -> flag an existing item with the paired ref
//	st coship --off <ID>            -> clear the flag
func CoShip(s *store.Store, cfg *config.Config, args []string, opts CoShipOpts) int {
	// Reject conflicting flags rather than silently resolving by check order
	// (e.g. `--off --api-ref foo` must not quietly clear and ignore the ref).
	set := 0
	if opts.Off {
		set++
	}
	if opts.APIRef != "" {
		set++
	}
	if opts.ActiveRef {
		set++
	}
	if set > 1 {
		fmt.Fprintln(os.Stderr, "coship: --off, --api-ref, and --active-ref are mutually exclusive")
		return 2
	}

	// Machine accessor for check-openapi-sync.sh: print only the active item's
	// ref (or nothing) and always exit 0 so a `$(...)` capture stays clean.
	if opts.ActiveRef {
		return coshipActiveRef(s, cfg)
	}

	if opts.Off {
		if len(args) != 1 || !hotfixIDPattern.MatchString(args[0]) {
			fmt.Fprintf(os.Stderr, "coship --off: expected a single item ID (e.g. I-123), got %q\n", strings.Join(args, " "))
			return 2
		}
		return setCoShip(s, cfg, args[0], "")
	}

	if opts.APIRef != "" {
		if len(args) != 1 || !hotfixIDPattern.MatchString(args[0]) {
			fmt.Fprintf(os.Stderr, "coship: --api-ref requires a single item ID (e.g. st coship I-123 --api-ref fix/api-branch)\n")
			return 2
		}
		ref := strings.TrimSpace(opts.APIRef)
		if ref == "" {
			fmt.Fprintln(os.Stderr, "coship: --api-ref must be a non-empty git ref (the paired api branch)")
			return 2
		}
		return setCoShip(s, cfg, args[0], ref)
	}

	// No flags, no args → list.
	if len(args) == 0 {
		return coshipList(s)
	}

	fmt.Fprintln(os.Stderr, "coship: provide --api-ref <ref> to flag an item, --off <ID> to clear, or no args to list")
	return 2
}

// coshipActiveRef prints the coship ref of the active item, or nothing if none
// is in co-ship mode. It scans the stack top-down so the flag keeps working
// even when an unrelated blocker is pushed on top of the co-shipped item (a
// realistic interruption in this workflow); the topmost flagged item wins.
// Always exits 0 — the web check captures stdout and must not be broken by a
// non-zero status when co-ship is simply inactive.
func coshipActiveRef(s *store.Store, cfg *config.Config) int {
	entries := LoadStack(cfg)
	for i := len(entries) - 1; i >= 0; i-- {
		if item, ok := s.Get(entries[i].ID); ok && item.CoShipAPIRef != "" {
			fmt.Println(item.CoShipAPIRef)
			return 0
		}
	}
	return 0
}

// setCoShip sets (ref != "") or clears (ref == "") the flag on an existing item,
// logs it, and syncs. Mirrors setHotfix.
func setCoShip(s *store.Store, cfg *config.Config, id, ref string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Doc == nil {
		fmt.Fprintf(os.Stderr, "%s has no document\n", id)
		return 1
	}
	if item.CoShipAPIRef == ref {
		if ref == "" {
			fmt.Printf("%s: co-ship already off — no change.\n", id)
		} else {
			fmt.Printf("%s: co-ship already pinned to %q — no change.\n", id, ref)
		}
		return 0
	}

	if err := s.Mutate(id, func(it *model.Item) error {
		it.CoShipAPIRef = ref
		it.Doc.SetField("coship_api_ref", ref)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	op := "coship_off"
	if ref != "" {
		op = "coship_on"
	}
	changelog.Append(cfg, id, changelog.Entry{Op: op, Field: "coship_api_ref", NewValue: ref})

	if ref != "" {
		printCoShipBanner(id, ref)
	} else {
		fmt.Printf("Co-ship mode CLEARED on %s — web OpenAPI check back to strict origin/main.\n", id)
	}

	// Commit + push immediately so the flag flip can't be silently reverted and
	// the web check (and any peer) sees it at once. Mirrors `st hotfix`.
	if err := autoSync(s, fmt.Sprintf("st coship %s: %s", id, op)); err != nil {
		return 1
	}
	return 0
}

// coshipList prints every item currently in co-ship mode. Auditability: makes
// open escape windows visible at a glance, mirroring `st hotfix` with no args.
func coshipList(s *store.Store) int {
	type entry struct{ id, ref, title string }
	var rows []entry
	for id, it := range s.All() {
		if it.CoShipAPIRef != "" {
			rows = append(rows, entry{id, it.CoShipAPIRef, it.Title})
		}
	}
	if len(rows) == 0 {
		fmt.Println("No items in co-ship mode.")
		return 0
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	fmt.Println("Items in CO-SHIP mode (web OpenAPI check resolves against the paired api ref):")
	for _, r := range rows {
		fmt.Printf("  %s  api-ref=%s  %s\n", r.id, r.ref, r.title)
	}
	fmt.Printf("\nClear with:  st coship --off <ID>\n")
	return 0
}

func printCoShipBanner(id, ref string) {
	fmt.Printf("⚠ CO-SHIP MODE ON: %s (api-ref=%s)\n", id, ref)
	fmt.Printf("  The web OpenAPI check (check-openapi-sync.sh) resolves the backend spec against\n")
	fmt.Printf("  %q instead of api origin/main, so a paired api+web contract change can commit\n", ref)
	fmt.Printf("  before the api PR merges. Commit the api spec change first — the check reads the\n")
	fmt.Printf("  committed spec at that ref. Default stays strict for every other item.\n")
	fmt.Printf("  Clear when both PRs are ready to merge:  st coship --off %s\n", id)
}
