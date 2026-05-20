package command

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
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
	sectionCursor int             // index into facetOrder
	expanded      map[string]bool // per-section toggle override; nil ⇒ use expandedByDefault
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
		switch k.String() {
		case "up":
			if m.sectionCursor > 0 {
				m.sectionCursor--
			}
		case "down":
			if m.sectionCursor < len(facetOrder)-1 {
				m.sectionCursor++
			}
		case " ":
			m = toggleSection(m, facetOrder[m.sectionCursor])
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
	m.pending = 0
	for _, e := range LoadQueue(m.cfg) {
		if !e.Approved {
			m.pending++
		}
	}
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
	alerts := m.renderAlerts()

	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	top := panel.Width(w - 2).Render(agentStrip)
	left := panel.Width(compositeWidth).Render(composite)
	right := panel.Width(planningWidth).Render(planning)
	mid := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	bot := panel.Width(w - 2).Render(alerts)
	return lipgloss.JoinVertical(lipgloss.Left, top, mid, bot)
}

// renderFullScreen draws ONLY the cursored composite section (axis 2).
// Esc returns up; q quits. The hint line stays visible so the
// affordance is never lost (the §5 discoverability principle).
func (m tuiModel) renderFullScreen(w int) string {
	if m.item == nil || m.sectionCursor < 0 || m.sectionCursor >= len(facetOrder) {
		return "(no section to drill)"
	}
	kind := facetOrder[m.sectionCursor]
	fr := facets[kind](m.s, m.cfg, m.item)

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

func (m tuiModel) renderComposite() string {
	if m.item == nil {
		return "focused item: (none — workspace has no eligible item)"
	}
	// Walk facets here (not via showFull) so the TUI can apply the
	// per-section cursor highlight + per-toggle expanded override. The
	// per-section block itself is still rendered through the shared
	// renderSection helper — zero facet-rendering logic is duplicated
	// here (the §7 invariant).
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s — %s\n", m.item.ID, m.item.Title)
	fmt.Fprintln(&buf, strings.Repeat("─", 60))
	for i, kind := range facetOrder {
		fr := facets[kind](m.s, m.cfg, m.item)
		expanded := m.sectionExpanded(kind)
		highlighted := m.focusAxis == axisComposite && i == m.sectionCursor
		renderSection(&buf, kind, fr, expanded, highlighted)
	}
	return strings.TrimRight(buf.String(), "\n")
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
	pending := 0
	for _, e := range LoadQueue(cfg) {
		if !e.Approved {
			pending++
		}
	}
	m := tuiModel{
		s: s, cfg: cfg, item: it, agents: regs, pending: pending,
		claimed: buildClaimedIndex(s), width: opts.Width,
		focusAxis: initialFocusAxis(regs),
	}
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
	pending := 0
	for _, e := range LoadQueue(cfg) {
		if !e.Approved {
			pending++
		}
	}
	m := tuiModel{
		s: s, cfg: cfg, item: it, agents: regs, pending: pending,
		claimed: buildClaimedIndex(s), width: opts.Width, refreshCh: refreshCh,
		focusAxis: initialFocusAxis(regs),
	}
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
