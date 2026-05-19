package command

import (
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/jfinlinson/agent-state/internal/config"
)

// tui_live.go (T-373) — fsnotify watcher driving the live event loop.
// Kept in its own file so the watcher can be unit-tested without
// spinning the Bubble Tea program (the same discipline that kept the
// coordinator decision-core pure in T-363).

// refreshMsg is a debounced "the substrate changed, re-pull data" signal.
// It carries no payload: refresh re-reads through the existing accessors
// — the same composition the static frame already uses (§7).
type refreshMsg struct{}

// debounceWindow groups bursty events (an item edit = changelog + index +
// item rewrite in quick succession) into a single refresh. 150ms is the
// TUI-doc's prescription; tune here as needed.
const debounceWindow = 150 * time.Millisecond

// watchDirs returns the substrate directories worth observing for the
// Layout-A panels: item files (the focused composite + planning queue),
// the queue + agent registry + changelog (alerts + agent strip), and
// the plan sidecars (composite's plan facet). Session JSONLs are NOT
// watched at v1 — they are the content-tail concern (contract §8.1)
// the conversation-channel work owns; the static substrate is enough
// for the four panels this layer renders.
func watchDirs(cfg *config.Config) []string {
	var dirs []string
	add := func(p string) {
		if p == "" {
			return
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			dirs = append(dirs, p)
		}
	}
	root := cfg.Root()
	itemRoot := cfg.ItemDir()
	add(itemRoot)
	if entries, err := os.ReadDir(itemRoot); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				add(filepath.Join(itemRoot, e.Name()))
			}
		}
	}
	add(cfg.AgentsDir())
	add(cfg.ChangelogDir())
	add(cfg.PlansDir())
	add(filepath.Join(root, ".as")) // queue.md, coordinator.yaml
	return dirs
}

// startWatcher opens an fsnotify watcher over the substrate dirs and
// runs a single-goroutine debouncer that emits one refreshMsg per
// burst on out. Returns the watcher (so the caller closes it on
// shutdown) and any setup error. The done channel signals the debouncer
// to exit.
func startWatcher(cfg *config.Config, out chan<- refreshMsg, done <-chan struct{}) (*fsnotify.Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	for _, d := range watchDirs(cfg) {
		// Best-effort: a missing/unwatchable dir is logged via Errors
		// upstream but should NEVER fail the whole TUI startup.
		_ = w.Add(d)
	}
	go debounceLoop(w, out, done)
	return w, nil
}

// debounceLoop is a single-goroutine select that collapses bursty
// events into one refreshMsg per debounceWindow. Single-goroutine ⇒ no
// data race on the `armed` state. On done, drains and exits cleanly.
func debounceLoop(w *fsnotify.Watcher, out chan<- refreshMsg, done <-chan struct{}) {
	// time.NewTimer is "armed" out of the gate; stop it so we start idle.
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop() // deterministic cleanup on every exit path (F1)
	armed := false
	for {
		select {
		case _, ok := <-w.Events:
			if !ok {
				return // watcher closed
			}
			if armed {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			timer.Reset(debounceWindow)
			armed = true
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
			// Watcher errors are non-fatal: keep going, the TUI is
			// observability — a missed event is preferable to a crashed
			// view (the operator silent-failure principle inverted:
			// stay LIVE, log the miss).
		case <-timer.C:
			armed = false
			select {
			case out <- refreshMsg{}:
			case <-done:
				return
			}
		case <-done:
			return
		}
	}
}
