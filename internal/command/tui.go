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

// tui.go (T-372) — `st tui`: the STATIC Layout-A frame, sub-layer (a) of
// the T-348 capstone. A Bubble Tea program whose v1 just composes the
// merged CLI primitives (st watch's agent data · st show --full's
// composite · st recommend's planning queue · §7 alerts) into one
// Lipgloss-laid-out screen, renders ONCE, and exits. The tea.Model
// interface is satisfied here even though v1 never calls
// tea.NewProgram — that interface IS the substrate T-373 (live fsnotify)
// and T-374 (keyboard) plug into without reshaping the code (TUI-design
// §7: "TUI is glue on stable CLI primitives").

// TuiOpts are the `st tui` flags.
type TuiOpts struct {
	Item  string // optional focused item id; default = next queue pick
	Width int    // optional render width; <=0 ⇒ DefaultWidth
}

// DefaultWidth is the static fallback when the terminal width is
// unavailable. T-373 will replace this with a live tea.WindowSizeMsg.
const DefaultWidth = 120

// Panel layout proportions (left composite : right planning), pre-borders.
const (
	compositeWidth = 78
	planningWidth  = 40
)

// tuiModel is the v1 STATIC model. Init/Update are no-ops; T-373/T-374
// fill them in. The whole frame is a function of these fields.
type tuiModel struct {
	s       *store.Store
	cfg     *config.Config
	item    *model.Item
	agents  []*agent.Registration
	pending int // queue entries needing operator approval (alerts band)
	width   int
}

// Init / Update are tea.Model stubs for v1 — the substrate, not yet wired.
func (m tuiModel) Init() tea.Cmd                         { return nil }
func (m tuiModel) Update(_ tea.Msg) (tea.Model, tea.Cmd) { return m, nil }

func (m tuiModel) View() string {
	w := m.width
	if w <= 0 {
		w = DefaultWidth
	}

	agentStrip := m.renderAgentStrip()
	composite := m.renderComposite()
	planning := m.renderPlanning()
	alerts := m.renderAlerts()

	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)

	// Top + bottom panels span the full width; middle row is two columns.
	top := panel.Width(w - 2).Render(agentStrip)
	left := panel.Width(compositeWidth).Render(composite)
	right := panel.Width(planningWidth).Render(planning)
	mid := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	bot := panel.Width(w - 2).Render(alerts)
	return lipgloss.JoinVertical(lipgloss.Left, top, mid, bot)
}

// --- panel content (deterministic; no map iteration in render order) ---

func (m tuiModel) renderAgentStrip() string {
	if len(m.agents) == 0 {
		return "agents: (no registered agents in this workspace)"
	}
	// Sort by AgentID for deterministic order — the T-369 F1 lesson.
	regs := append([]*agent.Registration(nil), m.agents...)
	sort.Slice(regs, func(i, j int) bool { return regs[i].AgentID < regs[j].AgentID })

	// Build a session→claimed-item index. O(N items): fine at static v1
	// (View runs once per `st tui` invocation), but T-373's fsnotify loop
	// will call View on every change — at that point this should move
	// into the tuiModel as a cached index invalidated on store events,
	// not recomputed per render.
	claimed := map[string]*model.Item{}
	for _, it := range m.s.All() {
		if it.ClaimedBy != "" {
			claimed[it.ClaimedBy] = it
		}
	}

	var b strings.Builder
	b.WriteString("agents:")
	for _, r := range regs {
		focus := "(idle)"
		if it, ok := claimed[r.SessionID]; ok {
			focus = it.ID + " " + truncate(it.Title, 40)
		}
		fmt.Fprintf(&b, "\n  %s  pid:%d  %s", r.AgentID, r.PID, focus)
	}
	return b.String()
}

func (m tuiModel) renderComposite() string {
	if m.item == nil {
		return "focused item: (none — workspace has no eligible item)"
	}
	var buf bytes.Buffer
	showFull(&buf, m.s, m.cfg, m.item, false) // §7: REUSES the renderer, not a copy
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
	// Room for future signals (§7 escalations, stale-active) — kept honest
	// in v1 as just the pending count; T-373 surfaces live escalations.
	return "alerts: " + strings.Join(parts, "  ·  ")
}

// --- entrypoints ---

// Tui builds the static model and renders it to stdout once. The cobra
// path uses this; tests use tuiTo with a buffer for headless assertions.
func Tui(s *store.Store, cfg *config.Config, opts TuiOpts) int {
	return tuiTo(os.Stdout, s, cfg, opts)
}

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
		width: opts.Width,
	}
	fmt.Fprintln(w, m.View())
	return 0
}

// resolveFocus picks the focused item: explicit --item wins; default is
// the next eligible queue pick (the SAME selectNext semantics the
// coordinator's dispatch uses — single source of truth, contract §4.2).
func resolveFocus(s *store.Store, cfg *config.Config, want string) (*model.Item, int) {
	if want != "" {
		it, ok := s.Get(want)
		if !ok {
			fmt.Fprintf(os.Stderr, "not found: %s\n", want)
			return nil, 1
		}
		return it, 0
	}
	// Default: the first item in the queue that exists in the store. For
	// v1 (static) this is intentionally cheaper than a full
	// EligibleForDispatch walk — the focus is just the "what item are we
	// looking at first" anchor; the operator overrides with --item.
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
