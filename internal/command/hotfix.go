package command

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// hotfixIDPattern matches a bare item ID (task or issue).
var hotfixIDPattern = regexp.MustCompile(`^[TI]-[0-9]+$`)

// HotfixOpts holds flags for `st hotfix`.
type HotfixOpts struct {
	Off bool // clear the flag instead of setting it
}

// Hotfix sets/clears the st-owned `hotfix` flag on an item, creates a new
// flagged issue, or lists currently-flagged items.
//
// The hotfix flag is the durable, audited escape hatch for urgent fixes: the
// deny-capable workflow gates that resolve their active item from `st stack`
// (plan-before-code, tier2-before-push, and the bash-safety direct-push-to-main
// block) fall open for a flagged item. Force-push stays blocked and the
// build/lint/typecheck git pre-commit hooks are untouched — those are what stop
// a hotfix becoming a second outage. Every flip is changelog-logged and
// git-synced via autoSync, so a bypass is never silent.
//
// Dispatch on the positional args:
//
//	st hotfix                 -> list items currently in hotfix mode
//	st hotfix <ID>            -> flag an existing item (ID must resolve)
//	st hotfix --off <ID>      -> clear the flag
//	st hotfix <title...>      -> create a p0 issue with the flag set
func Hotfix(s *store.Store, cfg *config.Config, args []string, opts HotfixOpts) int {
	if len(args) == 0 {
		if opts.Off {
			fmt.Fprintln(os.Stderr, "hotfix --off: provide the item ID to clear (e.g. st hotfix --off I-123)")
			return 2
		}
		return hotfixList(s)
	}

	first := args[0]

	if opts.Off {
		if len(args) != 1 || !hotfixIDPattern.MatchString(first) {
			fmt.Fprintf(os.Stderr, "hotfix --off: expected a single item ID (e.g. I-123), got %q\n", strings.Join(args, " "))
			return 2
		}
		return setHotfix(s, cfg, first, false)
	}

	// A single bare ID that resolves to an existing item → flag it.
	if len(args) == 1 && hotfixIDPattern.MatchString(first) {
		if _, ok := s.Get(first); ok {
			return setHotfix(s, cfg, first, true)
		}
		fmt.Fprintf(os.Stderr, "hotfix: %s not found — pass an existing item ID, or a title to create a new hotfix issue\n", first)
		return 1
	}

	// Otherwise treat the args as a title and create a flagged p0 issue.
	title := strings.TrimSpace(strings.Join(args, " "))
	if title == "" {
		fmt.Fprintln(os.Stderr, "hotfix: provide an existing item ID to flag, or a title to create a new hotfix issue")
		return 2
	}
	return createHotfix(s, cfg, title)
}

// setHotfix flips the flag on an existing item, logs it, and syncs.
func setHotfix(s *store.Store, cfg *config.Config, id string, on bool) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Doc == nil {
		fmt.Fprintf(os.Stderr, "%s has no document\n", id)
		return 1
	}
	if item.Hotfix == on {
		fmt.Printf("%s: hotfix already %s — no change.\n", id, onOff(on))
		return 0
	}

	if err := s.Mutate(id, func(it *model.Item) error {
		it.Hotfix = on
		it.Doc.SetField("hotfix", boolStr(on))
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	op := "hotfix_off"
	if on {
		op = "hotfix_on"
	}
	changelog.Append(cfg, id, changelog.Entry{Op: op, Field: "hotfix", NewValue: boolStr(on)})

	if on {
		printHotfixBanner(id)
	} else {
		fmt.Printf("Hotfix mode CLEARED on %s — workflow gates re-enabled.\n", id)
	}

	// Commit + push immediately so the flag flip can't be silently reverted
	// by a later command's pre-run GitPull, and so the gates (which read the
	// on-disk markdown) and any peer see it at once. Mirrors `st tag`.
	if err := autoSync(s, fmt.Sprintf("st hotfix %s: %s", onOff(on), id)); err != nil {
		return 1
	}
	return 0
}

// createHotfix creates a p0 issue with the flag pre-set. It deliberately takes
// the fast path through Create — no LLM SBAR validation, no dedup, no
// post-create review sub-agent — because hotfixes are time-critical. The
// scaffold SBAR is seeded with hotfix-aware content the author can refine.
func createHotfix(s *store.Store, cfg *config.Config, title string) int {
	var newID string
	rc := Create(s, cfg, "issue", title, CreateOpts{
		Priority:       0,
		Situation:      "Urgent hotfix: " + title,
		Background:     "Created via `st hotfix`. Workflow gates (plan-before-code, tier2-before-push, direct-push-to-main) are bypassed for this item; build/lint/typecheck stay enforced. Replace this with real context if the fix outlives the immediate incident.",
		Assessment:     "Triage in progress — this item tracks an urgent production fix delivered with reduced ceremony.",
		Recommendation: "Apply the minimal fix, verify against the live system, push direct to main, deploy, and confirm. Keep build/lint/typecheck green and clear hotfix mode when done.",
		// Fast path: no gate, no LLM validation/dedup, no review engine.
		EnforceGate: false,
		NoValidate:  true,
		NoDedup:     true,
		IDOut:       &newID,
	})
	if rc != 0 {
		return rc
	}
	if newID == "" {
		// Defensive: NoDedup means a new item should always be allocated.
		fmt.Fprintln(os.Stderr, "hotfix: item created but ID was not captured — set the flag manually with `st hotfix <ID>`.")
		return 1
	}

	if code := setHotfix(s, cfg, newID, true); code != 0 {
		return code
	}
	fmt.Printf("\nNext:  st start %s --slug <branch-slug>\n", newID)
	return 0
}

// hotfixList prints every item currently in hotfix mode. Auditability: makes
// open bypass windows visible at a glance (`st prime` / a peer can spot them).
func hotfixList(s *store.Store) int {
	var ids []string
	items := s.All()
	for id, it := range items {
		if it.Hotfix {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		fmt.Println("No items in hotfix mode.")
		return 0
	}
	sort.Strings(ids)
	fmt.Printf("Items in HOTFIX mode (gates bypassed):\n")
	for _, id := range ids {
		fmt.Printf("  %s  %s\n", id, items[id].Title)
	}
	fmt.Printf("\nClear with:  st hotfix --off <ID>\n")
	return 0
}

func printHotfixBanner(id string) {
	fmt.Printf("⚠ HOTFIX MODE ON: %s\n", id)
	fmt.Printf("  Bypassed for this item: plan-before-code, tier2-before-push, direct-push-to-main (force-push still blocked).\n")
	fmt.Printf("  Kept: build / lint / typecheck (git pre-commit). Each bypass logs an advisory to stderr.\n")
	fmt.Printf("  Clear when done:  st hotfix --off %s\n", id)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
