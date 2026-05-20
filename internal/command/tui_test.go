package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

func tasksFile(t *testing.T, cfg *config.Config, name string) string {
	t.Helper()
	return filepath.Join(cfg.ItemDir(), "tasks", name)
}

func appendByte(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n")
	return err
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

// tuiRender drives tuiTo against an in-memory writer for headless
// assertions — no tea.NewProgram, no TTY, no flakes.
func tuiRender(t *testing.T, opts TuiOpts) (string, int) {
	t.Helper()
	s, cfg := setupTestEnv(t)
	// setupTestEnv leaves the queue empty; add the seeded T-001 so the
	// default-focus path has something to anchor on (no --item flag).
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	var buf bytes.Buffer
	rc := tuiTo(&buf, s, cfg, opts)
	return buf.String(), rc
}

// All four Layout-A panels must appear in the rendered frame, and the
// renderer must reuse showFull's section glyphs / recommend's "why" line
// (the §7 invariant — no facet logic duplicated).
func TestTui_AllFourPanelsRender(t *testing.T) {
	out, rc := tuiRender(t, TuiOpts{Width: 140})
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	for _, want := range []string{
		"agents:",                          // agent-strip panel
		"▼ item",                           // composite reuses show --full glyphs
		"planning queue (st recommend top", // planning panel
		"awaiting approval",                // alerts band
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel marker %q missing\n--- output ---\n%s", want, out)
		}
	}
}

func TestTui_ItemFlagFocusesThatItem(t *testing.T) {
	out, rc := tuiRender(t, TuiOpts{Item: "I-001", Width: 140})
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(out, "I-001") {
		t.Errorf("--item I-001 must appear in the focused composite\n%s", out)
	}
}

func TestTui_NotFoundItemFailsLoudly(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	if rc := tuiTo(&buf, s, cfg, TuiOpts{Item: "NOPE-999"}); rc != 1 {
		t.Errorf("not-found --item must rc=1, got %d", rc)
	}
}

func TestTui_EmptyQueueNoItemFlagFailsLoudly(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// No QueueAdd here: the queue is empty AND no --item is given, so
	// the default-focus path must fail loudly per the operator
	// silent-failure principle.
	var buf bytes.Buffer
	if rc := tuiTo(&buf, s, cfg, TuiOpts{}); rc != 1 {
		t.Errorf("empty queue + no --item must rc=1, got %d", rc)
	}
}

// Determinism: agent strip + composite + recommend + alerts compose
// reproducibly across runs (the T-369 F1 / T-370 / T-371 discipline).
func TestTui_Deterministic(t *testing.T) {
	run := func() string {
		out, _ := tuiRender(t, TuiOpts{Width: 140})
		return out
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("tui View() must be deterministic\nA:\n%s\nB:\n%s", a, b)
	}
}

// --- T-373 live-mode tests (headless: drive Update directly) ---

// Quit keys (q / Ctrl-C / Esc) must return tea.Quit so the live event
// loop actually exits — otherwise the program hangs.
func TestTui_UpdateQuitKeys(t *testing.T) {
	m := tuiModel{}
	for _, key := range []string{"q", "ctrl+c", "esc"} {
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		// For the special keys, build the right tea.KeyMsg.
		switch key {
		case "ctrl+c":
			_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		case "esc":
			_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		}
		if cmd == nil {
			t.Errorf("quit key %q must return a tea.Quit Cmd, got nil", key)
		}
	}
}

// A resize message updates the model's width (drives the live panel
// re-layout) — Bubble Tea's tea.WindowSizeMsg is the substrate hook.
func TestTui_UpdateWindowSize(t *testing.T) {
	m := tuiModel{width: 120}
	out, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if got := out.(tuiModel).width; got != 80 {
		t.Errorf("width after resize = %d, want 80", got)
	}
}

// doRefresh re-reads the substrate AND updates derived fields. Picks up
// a queue change that happened out-of-band (the live-refresh whole point).
func TestTui_DoRefreshPicksUpQueueChanges(t *testing.T) {
	s, cfg := setupTestEnv(t)
	m := tuiModel{s: s, cfg: cfg, claimed: map[string]*model.Item{}}

	// Initial: empty queue, pending=0.
	m = doRefresh(m)
	if m.pending != 0 {
		t.Fatalf("initial pending = %d, want 0", m.pending)
	}
	// Simulate an out-of-band agent queue-add (Approved=false).
	t.Setenv("AS_AGENT_ID", "agent-bot") // non-empty ⇒ NOT auto-approved
	QueueAdd(s, cfg, "T-002", QueueOpts{})

	m = doRefresh(m)
	if m.pending != 1 {
		t.Errorf("after out-of-band agent QueueAdd, pending = %d, want 1\n"+
			"(refresh must reflect substrate changes — the trust-substrate point)",
			m.pending)
	}
}

// End-to-end (still headless, no tea.NewProgram): an out-of-band write
// in a watched directory must produce a debounced refreshMsg on the
// channel. Proves the fsnotify → debounce → refreshMsg pipe wires up.
func TestTui_WatcherEmitsRefreshOnFileWrite(t *testing.T) {
	_, cfg := setupTestEnv(t)
	ch := make(chan refreshMsg, 4)
	done := make(chan struct{})
	w, err := startWatcher(cfg, ch, done)
	if err != nil {
		t.Fatalf("startWatcher: %v", err)
	}
	defer func() { close(done); _ = w.Close() }()

	// Touch a file in a watched dir (the queued tasks subdir).
	target := tasksFile(t, cfg, "T-001-first.md")
	must(t, appendByte(target))
	must(t, appendByte(target)) // a burst → still ONE refresh after debounce

	select {
	case <-ch:
		// ok — at least one debounced refresh arrived
	case <-time.After(2 * time.Second):
		t.Fatal("no refreshMsg within 2s — fsnotify→debounce wiring broken")
	}
}

// refreshMsg must update fields AND re-arm a waitForRefresh Cmd so the
// stream of refreshes continues (the standard Bubble Tea pattern).
func TestTui_UpdateRefreshMsgReArms(t *testing.T) {
	s, cfg := setupTestEnv(t)
	ch := make(chan refreshMsg, 1)
	m := tuiModel{s: s, cfg: cfg, refreshCh: ch, claimed: map[string]*model.Item{}}
	_, cmd := m.Update(refreshMsg{})
	if cmd == nil {
		t.Fatal("refreshMsg must re-arm with a waitForRefresh Cmd, got nil")
	}
	// Feed the channel; the re-armed Cmd should consume that next message.
	ch <- refreshMsg{}
	if got := cmd(); got == nil {
		t.Error("re-armed Cmd must return the next refreshMsg, got nil")
	}
}

// --- T-374 §3/§5 navigation tests (headless, no tea.NewProgram) ---

// keyFor builds a tea.KeyMsg whose .String() matches the §5 model.
func keyFor(s string) tea.KeyMsg {
	switch s {
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func pressKey(m tuiModel, key string) tuiModel {
	out, _ := m.Update(keyFor(key))
	return out.(tuiModel)
}

// q quits from every axis — the basic event-loop necessity is honoured
// no matter where the cursor is.
func TestTui_QQuitsFromEveryAxis(t *testing.T) {
	for _, axis := range []int{axisAgentStrip, axisComposite, axisFullScreen} {
		m := tuiModel{focusAxis: axis}
		_, cmd := m.Update(keyFor("q"))
		if cmd == nil {
			t.Errorf("q at axis %d must return tea.Quit, got nil", axis)
		}
	}
}

// Axis 0 (agent strip): arrows move the cursor; Enter drills to
// composite + retargets m.item; Esc quits (top-of-axis).
func TestTui_AxisAgentStripArrowsAndDrill(t *testing.T) {
	a := &agent.Registration{AgentID: "agent-a", PID: 1, SessionID: "sess-a"}
	b := &agent.Registration{AgentID: "agent-b", PID: 2, SessionID: "sess-b"}
	work := &model.Item{ID: "T-W", Title: "agent-b's work", ClaimedBy: "sess-b"}
	m := tuiModel{
		focusAxis:   axisAgentStrip,
		agentCursor: 0,
		agents:      []*agent.Registration{a, b},
		claimed:     map[string]*model.Item{"sess-b": work},
	}
	m = pressKey(m, "right")
	if m.agentCursor != 1 {
		t.Errorf("right should move cursor 0→1, got %d", m.agentCursor)
	}
	m = pressKey(m, "right") // edge clamp — only 2 agents
	if m.agentCursor != 1 {
		t.Errorf("right at edge must clamp, got %d", m.agentCursor)
	}
	m = pressKey(m, "left")
	if m.agentCursor != 0 {
		t.Errorf("left should move 1→0, got %d", m.agentCursor)
	}
	m = pressKey(m, "left") // edge clamp at 0
	if m.agentCursor != 0 {
		t.Errorf("left at edge must clamp, got %d", m.agentCursor)
	}

	// Drill: cursor on agent-b → Enter retargets m.item to T-W and
	// switches to axisComposite.
	m.agentCursor = 1
	m = pressKey(m, "enter")
	if m.focusAxis != axisComposite {
		t.Errorf("Enter on axis 0 must move to axis 1, got %d", m.focusAxis)
	}
	if m.item == nil || m.item.ID != "T-W" {
		t.Errorf("Enter must retarget m.item to cursored agent's claim, got %v", m.item)
	}

	// Esc on axis 0 quits.
	m2 := tuiModel{focusAxis: axisAgentStrip}
	_, cmd := m2.Update(keyFor("esc"))
	if cmd == nil {
		t.Error("Esc on axis 0 must quit, got nil cmd")
	}
}

// Axis 1 (composite): up/down move sectionCursor; Space toggles
// per-section expanded override; Enter → axisFullScreen; Esc → axis 0.
func TestTui_AxisCompositeNavToggleAndDrill(t *testing.T) {
	m := tuiModel{focusAxis: axisComposite, sectionCursor: 0}
	m = pressKey(m, "down")
	if m.sectionCursor != 1 {
		t.Errorf("down should move 0→1, got %d", m.sectionCursor)
	}
	// Move to the bottom and prove edge clamp.
	for i := 0; i < len(facetOrder)+5; i++ {
		m = pressKey(m, "down")
	}
	if m.sectionCursor != len(facetOrder)-1 {
		t.Errorf("down at edge must clamp at %d, got %d",
			len(facetOrder)-1, m.sectionCursor)
	}
	m = pressKey(m, "up")
	if m.sectionCursor != len(facetOrder)-2 {
		t.Errorf("up should decrement, got %d", m.sectionCursor)
	}
	// Up at the top is a no-op.
	m.sectionCursor = 0
	m = pressKey(m, "up")
	if m.sectionCursor != 0 {
		t.Errorf("up at 0 must clamp, got %d", m.sectionCursor)
	}

	// Space toggles the cursored section's expanded state, seeded from
	// the default-policy (item is expanded-by-default ⇒ Space collapses
	// it; the override map records false).
	m.sectionCursor = 0 // "item" — expanded by default
	if !m.sectionExpanded("item") {
		t.Fatal("test premise: item expanded by default")
	}
	m = pressKey(m, " ")
	if m.sectionExpanded("item") {
		t.Errorf("Space must toggle item expanded → collapsed")
	}
	m = pressKey(m, " ")
	if !m.sectionExpanded("item") {
		t.Errorf("Space again must toggle collapsed → expanded")
	}

	// Enter drills to full-screen.
	m = pressKey(m, "enter")
	if m.focusAxis != axisFullScreen {
		t.Errorf("Enter on axis 1 must move to axis 2, got %d", m.focusAxis)
	}

	// Esc from axis 1 returns to axis 0.
	m2 := tuiModel{focusAxis: axisComposite}
	m2 = pressKey(m2, "esc")
	if m2.focusAxis != axisAgentStrip {
		t.Errorf("Esc on axis 1 must return to axis 0, got %d", m2.focusAxis)
	}
}

// Axis 2 (full-screen): Esc returns to composite.
func TestTui_AxisFullScreenEscReturns(t *testing.T) {
	m := tuiModel{focusAxis: axisFullScreen}
	m = pressKey(m, "esc")
	if m.focusAxis != axisComposite {
		t.Errorf("Esc on axis 2 must return to axis 1, got %d", m.focusAxis)
	}
}

// The hint line lists ONLY the keys visible at the current axis — the
// §5 "header is the at-a-glance" / no-memorization discipline.
func TestTui_HintLinePerAxis(t *testing.T) {
	cases := map[int]string{
		axisAgentStrip: "← →",
		axisComposite:  "Space toggle",
		axisFullScreen: "Esc back",
	}
	for axis, want := range cases {
		got := tuiModel{focusAxis: axis}.hintLine()
		if !strings.Contains(got, want) {
			t.Errorf("axis %d hint missing %q: got %q", axis, want, got)
		}
	}
}
