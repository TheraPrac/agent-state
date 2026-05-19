package command

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/mail"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// artifact.go (T-370) — `st artifact <id> <kind>`: the per-facet item
// introspection CLI, build-order layer 1 of the agent-orchestration TUI
// (TUI-design §4 taxonomy / §7 build order). It is COMPOSITION ONLY: each
// facet is a thin adapter over an accessor that already exists elsewhere
// in the tree (no new storage, no schema change, no duplicated logic), so
// `st show --full` (T-371, layer 2) and the TUI (T-348) reuse these
// verbatim instead of baking facet-gathering into rendering — the §7
// maintainability invariant.

// ArtifactOpts are the `st artifact` flags.
type ArtifactOpts struct {
	Kind   string // one of facetOrder, or "all"
	Format string // "text" (default) | "json"
}

// facetOrder is the FIXED deterministic sequence used by `all`. It is a
// slice, never Go map iteration — carrying the T-369 F1 lesson: a feature
// whose value is reproducible introspection cannot emit map-ordered output.
var facetOrder = []string{
	"item", "plan", "ac", "history", "testing", "pr",
	"uat", "commits", "deps", "bus", "worktree", "accounting",
}

type facetResult struct {
	Text string // human one-screen rendering
	JSON any    // stable machine shape (the T-371/TUI contract)
}

// facetFunc gathers ONE facet. It never returns an error / never panics:
// a missing facet (no plan, empty changelog) degrades to a clear "(none)"
// text + empty JSON — the operator silent-failure principle says say so,
// don't crash and don't hide.
type facetFunc func(s *store.Store, cfg *config.Config, it *model.Item) facetResult

var facets = map[string]facetFunc{
	"item":       facetItem,
	"plan":       facetPlan,
	"ac":         facetAC,
	"history":    facetHistory,
	"testing":    facetTesting,
	"pr":         facetPR,
	"uat":        facetUAT,
	"commits":    facetCommits,
	"deps":       facetDeps,
	"bus":        facetBus,
	"worktree":   facetWorktree,
	"accounting": facetAccounting,
}

// Artifact resolves the item, dispatches the kind (or every facet for
// "all"), and renders text or JSON. Unknown kind / bad format fail loudly
// with the valid set — never a silent empty.
func Artifact(s *store.Store, cfg *config.Config, id string, opts ArtifactOpts) int {
	it, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	format := opts.Format
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "json" {
		fmt.Fprintf(os.Stderr, "unknown --format %q (valid: text, json)\n", format)
		return 2
	}

	if opts.Kind == "all" {
		if format == "json" {
			out := map[string]any{}
			for _, k := range facetOrder {
				out[k] = facets[k](s, cfg, it).JSON
			}
			return emitJSON(out)
		}
		for _, k := range facetOrder {
			fmt.Printf("━━━ %s ━━━\n%s\n", k, facets[k](s, cfg, it).Text)
		}
		return 0
	}

	fn, ok := facets[opts.Kind]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown kind %q\nvalid kinds: %s, all\n",
			opts.Kind, strings.Join(facetOrder, ", "))
		return 2
	}
	res := fn(s, cfg, it)
	if format == "json" {
		return emitJSON(res.JSON)
	}
	fmt.Println(res.Text)
	return 0
}

func emitJSON(v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		return 1
	}
	fmt.Println(string(b))
	return 0
}

func emptyText(label string) string { return "(no " + label + ")" }

// --- facet adapters: thin reads over existing accessors only ---

type itemCore struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	Title      string   `json:"title"`
	Priority   *int     `json:"priority,omitempty"`
	Epic       string   `json:"epic,omitempty"`
	Sprint     string   `json:"sprint,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	DependsOn  []string `json:"depends_on,omitempty"`
	AssignedTo string   `json:"assigned_to,omitempty"`
}

func facetItem(_ *store.Store, _ *config.Config, it *model.Item) facetResult {
	c := itemCore{
		ID: it.ID, Type: it.Type, Status: it.Status, Title: it.Title,
		Priority: it.Priority, Epic: it.Epic, Sprint: it.Sprint,
		Tags: it.Tags, DependsOn: it.DependsOn, AssignedTo: it.AssignedTo,
	}
	p := "—"
	if it.Priority != nil {
		p = fmt.Sprintf("p%d", *it.Priority)
	}
	head := fmt.Sprintf("%s [%s] %s", it.ID, it.Type, it.Title)
	if sum := firstLine(it.Summary); sum != "" {
		head += " — " + sum
	}
	txt := fmt.Sprintf("%s\nstatus: %s  priority: %s  sprint: %s\ntags: %s  depends_on: %s",
		head, it.Status, p, orDash(it.Sprint),
		joinOrDash(it.Tags), joinOrDash(it.DependsOn))
	return facetResult{Text: txt, JSON: c}
}

func facetPlan(_ *store.Store, cfg *config.Config, it *model.Item) facetResult {
	p, _ := plan.Load(cfg.PlansDir(), it.ID)
	if p == nil {
		return facetResult{Text: emptyText("plan"), JSON: nil}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "approved: %t  scope: %s\n", p.Approved, joinOrDash(p.ScopeRepos))
	if p.Approach != "" {
		fmt.Fprintf(&b, "approach: %s\n", p.Approach)
	}
	for i, s := range p.Steps {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
	}
	if len(p.ACs) > 0 {
		fmt.Fprintf(&b, "acceptance:\n")
		for _, a := range p.ACs {
			fmt.Fprintf(&b, "  - %s\n", a)
		}
	}
	return facetResult{Text: strings.TrimRight(b.String(), "\n"), JSON: p}
}

func facetAC(_ *store.Store, _ *config.Config, it *model.Item) facetResult {
	if len(it.AcceptanceCriteria) == 0 {
		return facetResult{Text: emptyText("acceptance criteria"), JSON: []string{}}
	}
	var b strings.Builder
	for i, a := range it.AcceptanceCriteria {
		fmt.Fprintf(&b, "%d. %s\n", i+1, a)
	}
	return facetResult{Text: strings.TrimRight(b.String(), "\n"), JSON: it.AcceptanceCriteria}
}

func facetHistory(_ *store.Store, cfg *config.Config, it *model.Item) facetResult {
	entries, _ := changelog.Read(cfg, it.ID)
	if len(entries) == 0 {
		return facetResult{Text: emptyText("changelog history"), JSON: []changelog.Entry{}}
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintln(&b, e.Format())
	}
	return facetResult{Text: strings.TrimRight(b.String(), "\n"), JSON: entries}
}

func facetTesting(_ *store.Store, _ *config.Config, it *model.Item) facetResult {
	return mapFacet(it.TestingEvidence, "testing evidence")
}

func facetPR(_ *store.Store, _ *config.Config, it *model.Item) facetResult {
	out := map[string]any{}
	if len(it.Manifest) > 0 {
		out["manifest"] = it.Manifest
	}
	if pr, ok := it.WorkTracking["pr"]; ok {
		out["pr"] = pr
	}
	if len(out) == 0 {
		return facetResult{Text: emptyText("PR manifest"), JSON: map[string]any{}}
	}
	return facetResult{Text: kvText(out), JSON: out}
}

func facetUAT(_ *store.Store, cfg *config.Config, it *model.Item) facetResult {
	rep, _ := plan.LoadReport(cfg.PlansDir(), it.ID)
	if strings.TrimSpace(rep) == "" {
		return facetResult{Text: emptyText("stored UAT / plan-review report"), JSON: map[string]any{}}
	}
	return facetResult{Text: rep, JSON: map[string]any{"report": rep}}
}

func facetCommits(_ *store.Store, _ *config.Config, it *model.Item) facetResult {
	c, ok := it.WorkTracking["commits"]
	if !ok || c == nil {
		return facetResult{Text: emptyText("recorded commits"), JSON: []any{}}
	}
	// Glance text: one entry per line when it's a list (the common
	// shape); structured form is --format json (consistent with kvText).
	txt := fmt.Sprintf("%v", c)
	if list, isList := c.([]interface{}); isList {
		var b strings.Builder
		for _, e := range list {
			fmt.Fprintf(&b, "%v\n", e)
		}
		txt = strings.TrimRight(b.String(), "\n")
	}
	return facetResult{Text: txt, JSON: c}
}

func facetDeps(s *store.Store, cfg *config.Config, it *model.Item) facetResult {
	tree := deps.Build(s.All(), cfg).Tree(it.ID, 10)
	// For any real item Tree emits at least the root line (an isolated
	// item legitimately shows just its own node — that IS the dep view).
	// The empty branch only guards an unknown id (Tree returns "").
	if strings.TrimSpace(tree) == "" {
		return facetResult{Text: emptyText("dependencies"), JSON: map[string]any{"tree": ""}}
	}
	return facetResult{Text: strings.TrimRight(tree, "\n"), JSON: map[string]any{"tree": tree}}
}

// facetBus: v1 surfaces the CURRENT agent's mailbox messages that
// reference this item. Cross-agent / threaded per-item aggregation is the
// mailbox→conversation-channel evolution (operating-contract §8.2), a
// known downstream item — not silently overclaimed here.
func facetBus(_ *store.Store, cfg *config.Config, it *model.Item) facetResult {
	rcpt := cfg.Identity().ID
	if rcpt == "" {
		return facetResult{
			Text: "(no agent identity resolved; cross-agent bus is the conversation-channel downstream)",
			JSON: []mail.Message{},
		}
	}
	all, _ := mail.List(cfg, rcpt)
	var mine []mail.Message
	for _, m := range all {
		if m.Item == it.ID {
			mine = append(mine, m)
		}
	}
	if len(mine) == 0 {
		return facetResult{Text: emptyText("bus messages for this item (this mailbox)"), JSON: []mail.Message{}}
	}
	var b strings.Builder
	for _, m := range mine {
		fmt.Fprintf(&b, "%s  %s→%s [%s] %s\n", m.At, m.From, m.To, m.Kind, firstLine(m.Body))
	}
	return facetResult{Text: strings.TrimRight(b.String(), "\n"), JSON: mine}
}

// facetWorktree: the RECORDED substrate truth (work_tracking.branch +
// related fields). Live git-status of a worktree path is env-specific and
// brittle; the substrate record is the stable ground truth, consistent
// with the coordinator's substrate-over-self-report philosophy.
func facetWorktree(_ *store.Store, _ *config.Config, it *model.Item) facetResult {
	wt := map[string]any{}
	for _, k := range []string{"branch", "worktree", "pr"} {
		if v, ok := it.WorkTracking[k]; ok && v != nil {
			wt[k] = v
		}
	}
	if len(wt) == 0 {
		return facetResult{Text: emptyText("recorded worktree state"), JSON: map[string]any{}}
	}
	return facetResult{Text: kvText(wt), JSON: wt}
}

// facetAccounting: v1 = the structured time_tracking map as-is. Deep
// per-session JSONL cost rollup is explicitly the existing cost-rollup
// work (I-369 / T-365) — NOT grown here (TUI-design §4 flags the unified
// run-history view as net-new; this facet does not overclaim it).
func facetAccounting(_ *store.Store, _ *config.Config, it *model.Item) facetResult {
	return mapFacet(it.TimeTracking, "accounting / time tracking")
}

// --- tiny shared renderers ---

func mapFacet(m map[string]interface{}, label string) facetResult {
	if len(m) == 0 {
		return facetResult{Text: emptyText(label), JSON: map[string]any{}}
	}
	return facetResult{Text: kvText(m), JSON: m}
}

// kvText is the human GLANCE for a map facet: nested values render
// Go-style (`map[...]`) by design — the structured contract is --format
// json, and the composite section rendering is T-371's job (layer 2).
func kvText(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic (the T-369 F1 lesson, applied)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %v\n", k, m[k])
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func joinOrDash(xs []string) string {
	if len(xs) == 0 {
		return "—"
	}
	return strings.Join(xs, ", ")
}
