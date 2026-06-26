package command

import (
	"fmt"
	"io"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// show_full.go (T-371) — `st show --full <id>`: the composite item view,
// TUI build-order layer 2 (TUI-design Move 5 / §7). It is a PURE RENDERER
// over T-370's facets: it calls the same facets/facetOrder/facetResult and
// adds nothing but layout, so the section taxonomy can be validated as
// plain stdout BEFORE any Bubble Tea commitment (§7: "get the taxonomy
// wrong and the TUI is unusable"). No facet logic is re-derived here — the
// self-documenting header text comes from facetResult.Summary, computed by
// the facet in one pass (§7 maintainability invariant; the reason T-370
// was split out first).

// expandedByDefault is the §Move-5 collapse policy: the human sections
// show their full body; machine sections show only the self-documenting
// header. `--full --all` overrides this and expands everything.
var expandedByDefault = map[string]bool{
	"item": true, "plan": true, "ac": true, "observations": true,
}

// showFull writes the composite view to w. Taking an io.Writer (instead
// of hard-coding os.Stdout) is the additive refactor T-372 needs to
// compose this renderer into the TUI panel WITHOUT duplicating the facet
// rendering logic (the §7 maintainability invariant). The cobra path
// passes os.Stdout (identical behaviour); the TUI passes a bytes.Buffer.
func showFull(w io.Writer, s *store.Store, cfg *config.Config, item *model.Item, all bool) int {
	title := item.Title
	if title == "" {
		title = "(untitled)"
	}
	// Orienting document header. The id/title also appear in the `item`
	// facet body below — that is intentional: the banner says "what am I
	// looking at", the `item` facet is the identity entry of the §4
	// taxonomy. The scout deliberately shows the whole taxonomy.
	fmt.Fprintf(w, "%s — %s\n", item.ID, title)
	fmt.Fprintln(w, strings.Repeat("─", 60))

	for _, kind := range facetOrder { // fixed slice ⇒ deterministic order
		fr := facets[kind](s, cfg, item)
		expanded := all || expandedByDefault[kind]
		renderSection(w, kind, fr, expanded, false)
	}
	return 0
}

// renderSection writes ONE composite section to w — a self-documenting
// header line plus (if expanded) the indented body. Extracted from
// showFull so both the static CLI (`st show --full`) and the
// interactive TUI's per-section cursor renderer can call it without
// duplicating the §4/§5 glyph/header/body logic (the §7 maintainability
// invariant; T-374 added this). `highlighted=true` marks the section
// under the TUI cursor — static callers pass false.
func renderSection(w io.Writer, kind string, fr facetResult, expanded, highlighted bool) {
	glyph := "▶"
	if expanded {
		glyph = "▼"
	}
	summary := fr.Summary
	if summary == "" {
		summary = "—"
	}
	header := fmt.Sprintf("%s %s  (%s)", glyph, kind, summary)
	if highlighted {
		header = "» " + header // visible affordance (no color/ANSI needed);
		// the highlighted line shifting left is the standard "→ here"
		// selection idiom (Charm/gh/k9s); not jitter.
	}
	fmt.Fprintln(w, header)

	if !expanded {
		return
	}
	body := strings.TrimRight(fr.Text, "\n")
	if body == "" {
		return
	}
	for _, line := range strings.Split(body, "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
}
