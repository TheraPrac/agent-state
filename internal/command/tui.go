package command

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/theraprac/agent-state/internal/agent"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// tui.go (T-372 + T-373) — `st tui`: the Layout-A orchestration TUI.
//
// T-372 shipped the STATIC frame (`--once`); T-373 extends it into a
// LIVE Bubble Tea event loop driven by fsnotify (see tui_live.go).
// View(), the Lipgloss layout, and the four panel renderers are
// unchanged — only the event loop is new (the §7 invariant: each layer
// is glue on top of the stable primitive below).

// TuiOpts are the `st tui` flags.
type TuiOpts struct {
	Item  string // optional focused item id; default = next queue pick
	Width int    // optional render width; <=0 ⇒ DefaultWidth
	Once  bool   // T-372 static one-shot (no event loop, no watcher)
}

// DefaultWidth is the static fallback when the terminal width is
// unavailable. Live mode replaces it via tea.WindowSizeMsg on resize.
const DefaultWidth = 120

// Panel layout proportions (left composite : right planning), pre-borders.
const (
	compositeWidth = 78
	planningWidth  = 40
)

// tuiModel is the Layout-A model. Static (T-372) renders View once; live
// (T-373) calls View on every debounced refreshMsg; T-374 added the
// per-axis cursor state below.
type tuiModel struct {
	s       *store.Store
	cfg     *config.Config
	item    *model.Item
	agents  []*agent.Registration
	pending int                    // queue entries needing operator approval
	claimed map[string]*model.Item // session → claimed item (rebuilt on refresh)
	width   int

	// Live-mode wiring (zero for static / `--once`).
	refreshCh chan refreshMsg

	// T-374 §3 three-axis navigation state.
	focusAxis     int             // 0=agentStrip, 1=composite, 2=fullScreen
	agentCursor   int             // index into sortedAgents()
	sectionCursor int             // index into displayedOrder (T-375)
	expanded      map[string]bool // per-section toggle override; nil ⇒ use expandedByDefault

	// T-375 cache. Facet computation is non-trivial (changelog.Read,
	// plan.Load, mail.List, deps.Build…); every keypress runs View, so
	// without this cache an arrow would I/O 12 facets per stroke. We
	// recompute only on refresh / initial build, not per render.
	facetResults   map[string]facetResult
	displayedOrder []string // recency-reordered facetOrder (§5 #5)

	// T-379 (I-712): per-agent status rollup rendered as a 5th panel.
	// Cached on the same refresh cadence as facetResults; arc filter is
	// not in v1 (operator uses CLI --arc for filtered views — keeps the
	// TUI keyboard model at T-374's §5 minimum).
	statusMe statusMeReport
}

// Axis constants — readability for the focusAxis switch.
const (
	axisAgentStrip = 0
	axisComposite  = 1
	axisFullScreen = 2
)

// tea.Model satisfaction. T-373 wires the event loop; T-374 will add the
// §3/§5 navigation keys on top of these handlers.

func (m tuiModel) Init() tea.Cmd {
	if m.refreshCh == nil {
		return nil // static path — no event source to wait on
	}
	return waitForRefresh(m.refreshCh)
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case refreshMsg:
		m = doRefresh(m)
		return m, waitForRefresh(m.refreshCh) // re-arm for the next burst
	case tea.WindowSizeMsg:
		m.width = v.Width
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(v)
	}
	return m, nil
}

// handleKey is the §5 keyboard model: arrows move within an axis, Space
// toggles, Enter drills, Esc returns up the axis. q / Ctrl-C quit from
// any axis. No other keys — every affordance is visible in the hint
// line (the discoverability discipline).
func (m tuiModel) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	}
	switch m.focusAxis {
	case axisAgentStrip:
		switch k.String() {
		case "left":
			if m.agentCursor > 0 {
				m.agentCursor--
			}
		case "right":
			if m.agentCursor < len(m.sortedAgents())-1 {
				m.agentCursor++
			}
		case "enter":
			if a := m.cursoredAgent(); a != nil {
				if it, ok := m.claimed[a.SessionID]; ok && it != nil {
					m.item = it
				}
			}
			m.focusAxis = axisComposite
			m.sectionCursor = 0
		case "esc":
			// Top of the axis: Esc is "quit" (T-373 muscle memory).
			return m, tea.Quit
		}
	case axisComposite:
		// Cursor indexes into the displayed (recency-reordered) order, so
		// arrows + Space + Enter follow what the operator visually sees.
		order := m.displayedOrder
		if len(order) == 0 {
			order = facetOrder // pre-refresh fallback (rare; defensive)
		}
		switch k.String() {
		case "up":
			if m.sectionCursor > 0 {
				m.sectionCursor--
			}
		case "down":
			if m.sectionCursor < len(order)-1 {
				m.sectionCursor++
			}
		case " ":
			m = toggleSection(m, order[m.sectionCursor])
		case "enter":
			m.focusAxis = axisFullScreen
		case "esc":
			m.focusAxis = axisAgentStrip
		}
	case axisFullScreen:
		switch k.String() {
		case "esc":
			m.focusAxis = axisComposite
		}
	}
	return m, nil
}

// toggleSection flips the per-section expanded override, seeded from
// expandedByDefault when first toggled (so Space at a default-collapsed
// machine section expands it; at a default-expanded human section it
// collapses).
func toggleSection(m tuiModel, kind string) tuiModel {
	if m.expanded == nil {
		m.expanded = map[string]bool{}
	}
	cur, has := m.expanded[kind]
	if !has {
		cur = expandedByDefault[kind]
	}
	m.expanded[kind] = !cur
	return m
}

// sortedAgents returns the agent slice in the SAME order the strip
// renders, so the cursor index lines up with what the operator sees.
func (m tuiModel) sortedAgents() []*agent.Registration {
	out := append([]*agent.Registration(nil), m.agents...)
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

func (m tuiModel) cursoredAgent() *agent.Registration {
	regs := m.sortedAgents()
	if len(regs) == 0 || m.agentCursor < 0 || m.agentCursor >= len(regs) {
		return nil
	}
	return regs[m.agentCursor]
}

// sectionExpanded honours the per-toggle override, falling through to
// the §Move-5 default policy (item/plan/ac expanded; machine sections
// collapsed). One source of truth for both the composite and the
// full-screen renderer.
func (m tuiModel) sectionExpanded(kind string) bool {
	if v, ok := m.expanded[kind]; ok {
		return v
	}
	return expandedByDefault[kind]
}

// waitForRefresh is the tea.Cmd Bubble Tea uses to read the NEXT
// debounced refresh message from the fsnotify goroutine. After each
// refresh, Update re-arms by returning this Cmd again (the standard
// Bubble Tea "stream of messages from a channel" pattern).
func waitForRefresh(ch <-chan refreshMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// doRefresh re-reads the substrate. coordinate.go's freshItem uses the
// same pattern — the store caches on construction, so reopening it is
// the way to pick up out-of-band file changes. value-receiver transform
// to stay with the Bubble Tea idiom.
func doRefresh(m tuiModel) tuiModel {
	if fresh, err := store.New(m.cfg); err == nil {
		m.s = fresh
	}
	m.agents, _ = agent.ListRegistrations(m.cfg)
	m.pending = PendingApprovalCount(m.s, m.cfg)
	m.claimed = buildClaimedIndex(m.s)
	if m.item != nil {
		if fresh, ok := m.s.Get(m.item.ID); ok {
			m.item = fresh
		}
		// If the focused item vanished (closed + archived during the
		// session), leave m.item as-is rather than racing a re-resolve;
		// the composite renders the last-known state until the operator
		// retargets via --item.
	}
	// T-374 F1: clamp the agent cursor so a deregistration during live
	// mode does not leave the highlight pointing past the end (cursor
	// would visually disappear until the user nudges it).
	if n := len(m.sortedAgents()); n == 0 {
		m.agentCursor = 0
	} else if m.agentCursor >= n {
		m.agentCursor = n - 1
	}
	// T-375: rebuild facet cache + recency-reordered display order.
	// Done HERE (per refresh, not per render) so arrow keys don't
	// re-run 12 facets' worth of I/O per keystroke.
	m = recomputeFacets(m)
	return m
}

func buildClaimedIndex(s *store.Store) map[string]*model.Item {
	out := map[string]*model.Item{}
	for _, it := range s.All() {
		if it.ClaimedBy != "" {
			out[it.ClaimedBy] = it
		}
	}
	return out
}

func (m tuiModel) View() string {
	w := m.width
	if w <= 0 {
		w = DefaultWidth
	}
	if m.focusAxis == axisFullScreen {
		return m.renderFullScreen(w)
	}

	agentStrip := m.renderAgentStrip()
	composite := m.renderComposite()
	planning := m.renderPlanning()
	statusSurface := m.renderStatusSurface() // T-379
	alerts := m.renderAlerts()

	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	top := panel.Width(w - 2).Render(agentStrip)
	left := panel.Width(compositeWidth).Render(composite)
	right := panel.Width(planningWidth).Render(planning)
	mid := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	status := panel.Width(w - 2).Render(statusSurface)
	bot := panel.Width(w - 2).Render(alerts)
	return lipgloss.JoinVertical(lipgloss.Left, top, mid, status, bot)
}

// renderStatusSurface (T-379, I-712 #3) renders the 4-section status
// rollup as a compact panel between the composite/planning row and the
// alerts band. Reuses T-377's buildStatusMe output cached in m.statusMe
// — no facet logic duplicated; recomputed once per refresh.
func (m tuiModel) renderStatusSurface() string {
	if m.statusMe.Agent == "" {
		return "status: (no agent identity resolved — set --agent or run from a per-agent workspace)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "status (window: %s)", m.statusMe.Since)
	renderStatusLine(&b, "DONE", m.statusMe.Done)
	renderStatusLine(&b, "IN-FLIGHT", m.statusMe.InFlight)
	renderStatusLine(&b, "NEEDS-YOU", m.statusMe.NeedsYou)
	renderStatusLine(&b, "PROPOSED-NEXT", m.statusMe.ProposedNext)
	return b.String()
}

// renderStatusLine writes one section line: "LABEL  (N)  ID — title".
// Empty sections render as "LABEL  (0)  (none)". The top-1 item is the
// glanceable signal — operators can `st status --me` from the CLI for
// the full list.
func renderStatusLine(b *strings.Builder, label string, entries []statusMeEntry) {
	fmt.Fprintf(b, "\n  %-13s (%d)  ", label, len(entries))
	if len(entries) == 0 {
		b.WriteString("(none)")
		return
	}
	top := entries[0]
	fmt.Fprintf(b, "%s — %s", top.ID, truncate(top.Title, 60))
	if len(entries) > 1 {
		fmt.Fprintf(b, " (+%d more)", len(entries)-1)
	}
}

// renderFullScreen draws ONLY the cursored composite section (axis 2).
// Esc returns up; q quits. The hint line stays visible so the
// affordance is never lost (the §5 discoverability principle).
func (m tuiModel) renderFullScreen(w int) string {
	if m.item == nil || len(m.displayedOrder) == 0 ||
		m.sectionCursor < 0 || m.sectionCursor >= len(m.displayedOrder) {
		return "(no section to drill)"
	}
	kind := m.displayedOrder[m.sectionCursor]
	fr := m.facetResults[kind] // cached — already computed at refresh

	var body bytes.Buffer
	fmt.Fprintf(&body, "%s — %s\n", m.item.ID, m.item.Title)
	fmt.Fprintln(&body, strings.Repeat("─", 60))
	renderSection(&body, kind, fr, true, true) // expanded + highlighted

	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	top := panel.Width(w - 2).Render(strings.TrimRight(body.String(), "\n"))
	bot := panel.Width(w - 2).Render(m.renderAlerts())
	return lipgloss.JoinVertical(lipgloss.Left, top, bot)
}

// --- panel content (deterministic; no map iteration in render order) ---

func (m tuiModel) renderAgentStrip() string {
	regs := m.sortedAgents()
	if len(regs) == 0 {
		return "agents: (no registered agents in this workspace)"
	}
	var b strings.Builder
	b.WriteString("agents:")
	for i, r := range regs {
		focus := "(idle)"
		if it, ok := m.claimed[r.SessionID]; ok && it != nil {
			focus = it.ID + " " + truncate(it.Title, 40)
		}
		prefix := "  "
		if m.focusAxis == axisAgentStrip && i == m.agentCursor {
			prefix = "» " // visible affordance (matches the section highlight)
		}
		fmt.Fprintf(&b, "\n%s%s  pid:%d  %s", prefix, r.AgentID, r.PID, focus)
	}
	return b.String()
}

// recencyWindow is the §5 #5 "last hour" threshold beyond which a facet
// LastChange no longer floats to the top of the composite. In-code v1;
// T-348 capstone polish may surface it as operator-configurable.
const recencyWindow = 1 * time.Hour

func (m tuiModel) renderComposite() string {
	if m.item == nil || len(m.displayedOrder) == 0 {
		return "focused item: (none — workspace has no eligible item)"
	}
	// Walk the displayed order (not facetOrder) so the §5 #5 recency
	// reorder is what the operator sees AND what the cursor indexes
	// into. renderSection is the shared helper — zero facet-rendering
	// logic duplicated here (§7 invariant, inherited from T-371/T-374).
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s — %s\n", m.item.ID, m.item.Title)
	fmt.Fprintln(&buf, strings.Repeat("─", 60))
	for i, kind := range m.displayedOrder {
		fr := m.facetResults[kind]
		expanded := m.sectionExpanded(kind)
		highlighted := m.focusAxis == axisComposite && i == m.sectionCursor
		renderSection(&buf, kind, fr, expanded, highlighted)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// recomputeFacets rebuilds the facet-results cache + displayed order on
// the model. Called at construction (tuiTo / tuiLive) and from
// doRefresh — NEVER per render. Pure value-transform so it sits with
// the Bubble Tea idiom. With m.item == nil the maps are zeroed; the
// renderers handle the empty case.
func recomputeFacets(m tuiModel) tuiModel {
	// T-379: rebuild the status rollup ONCE per refresh too. It doesn't
	// depend on m.item (it's a per-agent view of the whole store), so
	// it's populated even when no item is focused.
	if agent := m.cfg.Identity().ID; agent != "" {
		m.statusMe = buildStatusMe(m.s, m.cfg, agent,
			time.Now().Add(-defaultSince), "")
	} else {
		m.statusMe = statusMeReport{}
	}

	if m.item == nil {
		m.facetResults = nil
		m.displayedOrder = nil
		return m
	}
	results := make(map[string]facetResult, len(facetOrder))
	for _, kind := range facetOrder {
		results[kind] = facets[kind](m.s, m.cfg, m.item)
	}
	m.facetResults = results
	m.displayedOrder = displayedSectionOrder(results, time.Now())
	// Clamp the cursor — a refresh can change displayed-order length
	// only in degenerate cases (we never add/remove sections), but the
	// defensive clamp matches T-374 F1's pattern for agentCursor.
	if n := len(m.displayedOrder); n == 0 {
		m.sectionCursor = 0
	} else if m.sectionCursor >= n {
		m.sectionCursor = n - 1
	}
	return m
}

// displayedSectionOrder is the §5 #5 recency-aware reorder: sections
// with LastChange within recencyWindow float to the top in
// reverse-chronological order; all other sections (including those
// with zero LastChange — "unknown / long ago") keep their facetOrder
// position. Stable: same input → same output across runs.
func displayedSectionOrder(results map[string]facetResult, now time.Time) []string {
	type recent struct {
		kind string
		when time.Time
	}
	var promoted []recent
	demoted := make([]string, 0, len(facetOrder))
	for _, kind := range facetOrder {
		fr := results[kind]
		if !fr.LastChange.IsZero() && now.Sub(fr.LastChange) < recencyWindow {
			promoted = append(promoted, recent{kind: kind, when: fr.LastChange})
		} else {
			demoted = append(demoted, kind)
		}
	}
	sort.SliceStable(promoted, func(i, j int) bool {
		// More recent first; ties (same instant) preserve facetOrder via
		// the stable sort of the initial walk.
		return promoted[i].when.After(promoted[j].when)
	})
	out := make([]string, 0, len(facetOrder))
	for _, r := range promoted {
		out = append(out, r.kind)
	}
	out = append(out, demoted...)
	return out
}

func (m tuiModel) renderPlanning() string {
	var buf bytes.Buffer
	buf.WriteString("planning queue (st recommend top 5):\n\n")
	recommendTo(&buf, m.s, m.cfg, RecommendOpts{Top: 5}) // §7: REUSES the engine
	return strings.TrimRight(buf.String(), "\n")
}

func (m tuiModel) renderAlerts() string {
	parts := []string{fmt.Sprintf("%d awaiting approval", m.pending)}
	live := ""
	if m.refreshCh != nil {
		live = " · live"
	}
	// Hint line: only the keys visible at THIS axis are shown — the §5
	// discoverability discipline ("no memorized keys"). Rendered on a
	// second line of the alerts band so the affordance is always
	// adjacent to the state.
	return "alerts: " + strings.Join(parts, "  ·  ") + live + "\n" + m.hintLine()
}

// hintLine lists the keys the operator can press from the current
// focus axis — that's the entire keyboard model, on screen.
func (m tuiModel) hintLine() string {
	switch m.focusAxis {
	case axisAgentStrip:
		return "keys: ← → agent  ·  Enter drill  ·  q quit"
	case axisComposite:
		return "keys: ↑ ↓ section  ·  Space toggle  ·  Enter full-screen  ·  Esc back  ·  q quit"
	case axisFullScreen:
		return "keys: Esc back  ·  q quit"
	}
	return ""
}

// --- entrypoints ---

// Tui dispatches to the static or live entrypoint. The cobra path uses
// this; tests can call tuiTo / doRefresh / Update directly for headless
// assertions.
func Tui(s *store.Store, cfg *config.Config, opts TuiOpts) int {
	if opts.Once {
		return tuiTo(os.Stdout, s, cfg, opts)
	}
	return tuiLive(s, cfg, opts)
}

// tuiTo renders ONCE to w. The cobra `--once` path and tests use this;
// behaviour is identical to T-372's static frame so no regression.
func tuiTo(w io.Writer, s *store.Store, cfg *config.Config, opts TuiOpts) int {
	it, rc := resolveFocus(s, cfg, opts.Item)
	if rc != 0 {
		return rc
	}
	regs, _ := agent.ListRegistrations(cfg)
	m := tuiModel{
		s: s, cfg: cfg, item: it, agents: regs, pending: PendingApprovalCount(s, cfg),
		claimed: buildClaimedIndex(s), width: opts.Width,
		focusAxis: initialFocusAxis(regs),
	}
	m = recomputeFacets(m) // T-375: prime the cache so the first View renders the reordered composite
	fmt.Fprintln(w, m.View())
	return 0
}

// initialFocusAxis: agent strip if there are registered agents (the
// natural entry point for "what is each agent doing"); composite
// otherwise so the operator can still interact with sections.
func initialFocusAxis(regs []*agent.Registration) int {
	if len(regs) == 0 {
		return axisComposite
	}
	return axisAgentStrip
}

// tuiLive starts the fsnotify watcher and runs the Bubble Tea program.
// The watcher lifecycle is bounded by the program: closed on exit so
// goroutines and file descriptors don't leak. q / Ctrl-C / Esc quits.
func tuiLive(s *store.Store, cfg *config.Config, opts TuiOpts) int {
	it, rc := resolveFocus(s, cfg, opts.Item)
	if rc != 0 {
		return rc
	}
	refreshCh := make(chan refreshMsg, 1)
	done := make(chan struct{})
	w, err := startWatcher(cfg, refreshCh, done)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: fsnotify watcher: %v (falling back to --once)\n", err)
		return tuiTo(os.Stdout, s, cfg, opts)
	}
	defer func() {
		close(done)
		_ = w.Close()
	}()

	regs, _ := agent.ListRegistrations(cfg)
	m := tuiModel{
		s: s, cfg: cfg, item: it, agents: regs, pending: PendingApprovalCount(s, cfg),
		claimed: buildClaimedIndex(s), width: opts.Width, refreshCh: refreshCh,
		focusAxis: initialFocusAxis(regs),
	}
	m = recomputeFacets(m) // T-375: prime cache before tea.NewProgram
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		return 1
	}
	return 0
}

// resolveFocus picks the focused item: explicit --item wins; default is
// the first item in the queue that exists in the store (the same
// dispatch source the coordinator uses — single source of truth,
// contract §4.2).
func resolveFocus(s *store.Store, cfg *config.Config, want string) (*model.Item, int) {
	if want != "" {
		it, ok := s.Get(want)
		if !ok {
			fmt.Fprintf(os.Stderr, "not found: %s\n", want)
			return nil, 1
		}
		return it, 0
	}
	for _, e := range LoadQueue(cfg) {
		if it, ok := s.Get(e.ID); ok {
			return it, 0
		}
	}
	fmt.Fprintln(os.Stderr,
		"no items in queue to focus; use --item <id> (or add to the queue)")
	return nil, 1
}

// (truncate lives in status.go — rune-safe, used here for title clipping.)
