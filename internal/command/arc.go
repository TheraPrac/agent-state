package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// arc.go (T-378, I-712) — `st arc`: strategic-work-stream tagging.
//
// Arc is a sibling of sprint/epic at a longer horizon: any name an
// operator uses IS the arc — no predefined list, no registry. The data
// shape is a single string field on the item (model.Arc); v1 is one
// arc per item. The discovery model is "find arcs by enumerating
// items," matching how tags work.

// ArcAdd assigns <name> as the Arc of every <id…>. Existing arc value
// is overwritten — an item can be re-arc'd by `st arc add <new> <id>`.
// Same semantics as `st sprint add` overwriting an item's sprint.
func ArcAdd(s *store.Store, cfg *config.Config, name string, ids []string) int {
	if name == "" || len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "arc add: usage: st arc add <name> <id…>")
		return 2
	}
	rc := 0
	for _, id := range ids {
		err := s.Mutate(id, func(m *model.Item) error {
			old := m.Arc
			m.Arc = name
			if m.Doc != nil {
				m.Doc.SetField("arc", name)
			}
			// One changelog entry per arc move — substrate-of-record.
			_ = changelog.Append(cfg, id, changelog.Entry{
				Op: "arc_add", Field: "arc", OldValue: old, NewValue: name,
				Agent: cfg.Identity().ID,
			})
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "arc add %s: %v\n", id, err)
			rc = 1
		} else {
			fmt.Printf("Tagged %s with arc %q\n", id, name)
		}
	}
	return rc
}

// ArcRm clears the Arc on every <id…>. v1: one arc per item, so "rm"
// is unambiguous — no name argument needed; we just clear it.
func ArcRm(s *store.Store, cfg *config.Config, ids []string) int {
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "arc rm: usage: st arc rm <id…>")
		return 2
	}
	rc := 0
	for _, id := range ids {
		err := s.Mutate(id, func(m *model.Item) error {
			old := m.Arc
			if old == "" {
				return nil // already none
			}
			m.Arc = ""
			if m.Doc != nil {
				m.Doc.SetField("arc", "")
			}
			_ = changelog.Append(cfg, id, changelog.Entry{
				Op: "arc_rm", Field: "arc", OldValue: old, NewValue: "",
				Agent: cfg.Identity().ID,
			})
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "arc rm %s: %v\n", id, err)
			rc = 1
		} else {
			fmt.Printf("Cleared arc on %s\n", id)
		}
	}
	return rc
}

// ArcShow lists every item where Arc == name, sorted by ID for
// deterministic output. JSON shape is the stable contract that T-379
// (TUI status surface) consumes when surfacing per-arc rollups.
func ArcShow(s *store.Store, cfg *config.Config, name string, jsonOut bool) int {
	if name == "" {
		fmt.Fprintln(os.Stderr, "arc show: usage: st arc show <name>")
		return 2
	}
	return arcShowTo(os.Stdout, s, cfg, name, jsonOut)
}

func arcShowTo(w io.Writer, s *store.Store, _ *config.Config, name string, jsonOut bool) int {
	type row struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Status   string `json:"status"`
		Title    string `json:"title"`
		Priority *int   `json:"priority,omitempty"`
	}
	rows := make([]row, 0)
	for _, it := range s.All() {
		if it != nil && it.Arc == name {
			rows = append(rows, row{
				ID: it.ID, Type: it.Type, Status: it.Status,
				Title: it.Title, Priority: it.Priority,
			})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	if jsonOut {
		b, err := json.MarshalIndent(map[string]any{"arc": name, "items": rows}, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "arc show: marshal: %v\n", err)
			return 1
		}
		fmt.Fprintln(w, string(b))
		return 0
	}
	fmt.Fprintf(w, "arc: %s  (%d)\n", name, len(rows))
	if len(rows) == 0 {
		fmt.Fprintln(w, "  (no items)")
		return 0
	}
	for _, r := range rows {
		p := "—"
		if r.Priority != nil {
			p = fmt.Sprintf("p%d", *r.Priority)
		}
		fmt.Fprintf(w, "  %-8s %-3s  %-8s  %s\n", r.ID, p, r.Status, r.Title)
	}
	return 0
}

// ArcList enumerates every arc name that appears on any item, with a
// count. The discovery model — no registry, no predefined list.
func ArcList(s *store.Store, cfg *config.Config, jsonOut bool) int {
	return arcListTo(os.Stdout, s, cfg, jsonOut)
}

func arcListTo(w io.Writer, s *store.Store, _ *config.Config, jsonOut bool) int {
	counts := map[string]int{}
	for _, it := range s.All() {
		if it != nil && it.Arc != "" {
			counts[it.Arc]++
		}
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic — never map-iter order

	if jsonOut {
		entries := make([]map[string]any, 0, len(names))
		for _, n := range names {
			entries = append(entries, map[string]any{"name": n, "count": counts[n]})
		}
		b, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "arc list: marshal: %v\n", err)
			return 1
		}
		fmt.Fprintln(w, string(b))
		return 0
	}
	if len(names) == 0 {
		fmt.Fprintln(w, "(no arcs in use)")
		return 0
	}
	for _, n := range names {
		fmt.Fprintf(w, "  %-30s  %d\n", n, counts[n])
	}
	return 0
}
