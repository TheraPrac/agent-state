package command

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/session"
	"github.com/theraprac/agent-state/internal/store"
)

// PairOpts holds flags for `st pair`.
type PairOpts struct {
	Off bool // deactivate pairing on this session instead of activating it
}

// pairIDPattern matches item ids (I-1706, T-042, G-014, ...). A local copy
// of internal/validate's unexported idPattern — not worth a cross-package
// export for one regex.
var pairIDPattern = regexp.MustCompile(`^[A-Z]-\d{3,}$`)

// tpStatusFunc reports whether a live stack already exists for the given
// worktree id (`tp status --worktree=<id>` exit 0 = up). The returned error
// is non-nil ONLY when the check itself couldn't be completed (tp missing
// from PATH, not executable, ...) — distinct from a confirmed "down", which
// is (false, nil). Type of the package-level tpStatus var, swappable in
// tests — mirrors the existing nodeInstaller seam in start.go (I-526) so
// unit tests never shell out or need Docker.
type tpStatusFunc func(cfg *config.Config, worktreeID string) (bool, error)

// tpUpFunc brings up (or attaches to) the stack for the given worktree id
// (`tp up --worktree=<id>`). Type of the package-level tpUp var.
type tpUpFunc func(cfg *config.Config, worktreeID string) error

// tpStatus/tpUp are package-level so tests can swap in fakes (t.Cleanup
// restored), matching the nodeInstaller precedent.
var (
	tpStatus tpStatusFunc = defaultTPStatus
	tpUp     tpUpFunc     = defaultTPUp
)

// tpEnv builds the environment for a `tp` subprocess. `tp`'s own agents-root
// autodiscovery walks cwd looking for a directory theraprac-workspace/.git —
// but a worktree's .git is a FILE (git worktree pointer), not a directory, so
// autodiscovery fails when st's cwd is inside a per-item worktree (confirmed
// during I-1705 live acceptance). Set THERAPRAC_AGENTS_ROOT explicitly rather
// than relying on cwd inference. AS_AGENT_ID is also set explicitly since a
// per-command exec.Command doesn't inherit whatever env st itself was
// launched with beyond os.Environ() (which may or may not include it).
func tpEnv(cfg *config.Config) []string {
	env := os.Environ()
	env = append(env, "AS_AGENT_ID="+cfg.AgentID())
	if root := cfg.AgentRoot(); root != "" {
		env = append(env, "THERAPRAC_AGENTS_ROOT="+filepath.Dir(root))
	}
	return env
}

// defaultTPStatus shells out to `tp status --worktree=<id>`; output is
// discarded — this is a machine up/down check, not a user-facing report
// (I-1705's `tp status --worktree` exits 0 iff api+web are both running).
//
// A non-zero exit is tp's documented, deliberate signal for "not up" — not
// an error condition (tp_stack_status always exits 0/1 by design, never
// crashes for the down case). But if the command couldn't even be started
// (tp missing from PATH, not executable, ...), that's not a status
// determination at all — code review (bugbot rule 3) correctly flagged the
// original bare-bool version for collapsing these into the same "false" and
// letting ensureTPStack silently reinterpret a genuine check failure as an
// ordinary down-state. exec.Error (vs exec.ExitError) is Go's own signal for
// "never ran" and is what we surface distinctly here.
func defaultTPStatus(cfg *config.Config, worktreeID string) (bool, error) {
	cmd := exec.Command("tp", "status", "--worktree="+worktreeID)
	cmd.Env = tpEnv(cfg)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return false, err
	}
	// *exec.ExitError (or any other post-start failure) — tp ran and
	// reported "not up" per its contract.
	return false, nil
}

// defaultTPUp shells out to `tp up --worktree=<id>`, streaming to the
// current stdout/stderr since a fresh-DB first-up can take several minutes
// (I-1705's RxNorm-ingest health-timeout finding) and the operator should
// see progress, not a silent hang.
func defaultTPUp(cfg *config.Config, worktreeID string) error {
	cmd := exec.Command("tp", "up", "--worktree="+worktreeID)
	cmd.Env = tpEnv(cfg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ensureTPStack reuses a live stack for worktreeID if one is already up (M4
// attach-stack), otherwise brings one up (`tp up --worktree=<id>`). Shared by
// all three entry modes (M1/M2/M3) — M4 is not a separate branch, it's the
// reuse/no-reuse fork inside this one step, applying uniformly to whichever
// mode resolved the item id.
func ensureTPStack(cfg *config.Config, worktreeID string) error {
	up, err := tpStatus(cfg, worktreeID)
	if err != nil {
		return fmt.Errorf("checking stack status for %s (tp status --worktree=%s): %w", worktreeID, worktreeID, err)
	}
	if up {
		fmt.Printf("st pair: stack for %s is already up — reusing (no re-provisioning).\n", worktreeID)
		return nil
	}
	fmt.Printf("st pair: bringing up stack for %s (tp up --worktree=%s)...\n", worktreeID, worktreeID)
	if err := tpUp(cfg, worktreeID); err != nil {
		return fmt.Errorf("tp up --worktree=%s: %w", worktreeID, err)
	}
	return nil
}

// Pair activates or deactivates the I-1700 `/pair` live-iteration mode on the
// CURRENT session — a session-local, ephemeral marker written into this
// session's yaml (.as/sessions/<id>.yaml), never changelog-logged or synced
// (unlike `st hotfix`'s item-level flag). Hooks read it via the shared
// pairing-mode.sh bash fragment to relax in-session friction (plan-before-code,
// worktree-dirty exit, model-check, advisory nags) for the paired item.
//
// Slice 3 (I-1706) implements the full M1-M4 entry-mode resolution from the
// design (docs/design-pair-live-iteration.md §4):
//
//	st pair               -> M1 attach-current: mark the stack-top item as
//	                         paired. Presupposes it's already started — no
//	                         Start() call.
//	st pair <id>          -> M2 attach-item: start it first if not already
//	                         active. An id-shaped arg that doesn't resolve
//	                         to an item is a hard error, never silently
//	                         treated as a title.
//	st pair "<title>"     -> M3 fresh: create a new issue with this title,
//	                         then start it.
//	st pair --off         -> detach: clear the marker on this session.
//
// After M1/M2/M3 resolve an item id, a shared step (ensureTPStack) reuses an
// already-live stack for that worktree (M4 attach-stack) or brings one up via
// `tp up --worktree=<id>` — so pairing only activates once the stack is
// actually up.
func Pair(s *store.Store, cfg *config.Config, sessMgr *session.Manager, sessionID string, args []string, opts PairOpts) int {
	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "st pair: no current session id resolved (.as/session missing or empty)")
		return 1
	}

	if opts.Off {
		if len(args) != 0 {
			fmt.Fprintln(os.Stderr, "st pair --off: takes no arguments")
			return 2
		}

		// I-1707: capture the paired item BEFORE clearing, so the promotion
		// step below (run only when there was something to clear) knows
		// which item's audit log to read. A pure read — ClearPairing's own
		// signature is untouched.
		var pairedItem string
		if sess, err := sessMgr.Load(sessionID); err == nil && sess != nil && sess.Pairing != nil && sess.Pairing.Active {
			pairedItem = sess.Pairing.Item
		}

		cleared, err := sessMgr.ClearPairing(sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "st pair --off: %v\n", err)
			return 1
		}
		if !cleared {
			fmt.Println("st pair --off: no active pairing on this session — nothing to clear.")
			return 0
		}
		fmt.Println("Pairing OFF — gates re-enabled.")

		if pairedItem != "" {
			promotePairingEvidence(s, cfg, sessionID, pairedItem)
		}
		return 0
	}

	argStr := strings.TrimSpace(strings.Join(args, " "))

	var id string
	switch {
	case argStr == "":
		// M1 attach-current: resolve the stack-top item. Unchanged from
		// Slice 1 — its precondition is "session already has a started
		// item", so no Start() call here.
		entries := LoadStack(cfg)
		if len(entries) == 0 {
			fmt.Fprintln(os.Stderr, "st pair: no active item on stack — st start/resume one first")
			return 1
		}
		id = entries[len(entries)-1].ID
		if _, ok := s.Get(id); !ok {
			fmt.Fprintf(os.Stderr, "st pair: stack-top item %s not found\n", id)
			return 1
		}

	case pairIDPattern.MatchString(argStr):
		// M2 attach-item: an id-shaped arg must resolve to an existing item.
		// A typo'd/nonexistent id is a hard error — never silently
		// reinterpreted as a fresh title.
		item, ok := s.Get(argStr)
		if !ok {
			fmt.Fprintf(os.Stderr, "st pair: %s looks like an item id but does not exist — "+
				"create it explicitly, or pass a title to create one (e.g. `st pair \"<title>\"`).\n", argStr)
			return 1
		}
		id = argStr
		if code := startIfNotActive(s, cfg, item); code != 0 {
			return code
		}

	default:
		// M3 fresh: title matches no item (arg isn't id-shaped) — create it,
		// then start it. Fast path mirroring createHotfix's ad hoc-item
		// convention: no plan gate, no semantic validation/dedup — pairing
		// is meant to be low-friction, and the scaffold SBAR/dedup gap is
		// deliberately deferred to when the item leaves pairing mode.
		var newID string
		rc := Create(s, cfg, "issue", argStr, CreateOpts{
			Priority:       2,
			Situation:      "Started via `st pair \"" + argStr + "\"` (live-iteration mode).",
			Background:     "Created ad hoc from a /pair session with no upstream design doc.",
			Assessment:     "Not yet assessed — created to unblock immediate live-iteration work.",
			Recommendation: "Refine the SBAR and add acceptance criteria before this item leaves pairing mode.",
			EnforceGate:    false,
			NoValidate:     true,
			NoDedup:        true,
			IDOut:          &newID,
		})
		if rc != 0 {
			return rc
		}
		if newID == "" {
			fmt.Fprintln(os.Stderr, "st pair: item created but ID was not captured")
			return 1
		}
		id = newID

		item, ok := s.Get(id)
		if !ok {
			fmt.Fprintf(os.Stderr, "st pair: created %s but could not re-read it\n", id)
			return 1
		}
		if code := startItem(s, cfg, item); code != 0 {
			return code
		}
	}

	if err := ensureTPStack(cfg, id); err != nil {
		fmt.Fprintf(os.Stderr, "st pair: %v\n", err)
		return 1
	}

	// `st resume <id>` (the documented re-entry point for a continuing
	// session) never creates the session yaml — only `st start`/`st sprint
	// join` do, via EnsureSession(WithIdentity). Without this, a session
	// that resumed existing work and then ran `st pair` (its exact intended
	// use case) would fail with "session not found" on SetPairing below.
	if _, err := sessMgr.EnsureSession(sessionID, cfg.AgentID()); err != nil {
		fmt.Fprintf(os.Stderr, "st pair: %v\n", err)
		return 1
	}

	p := &session.Pairing{
		Active: true,
		Item:   id,
		// Worktree always equals Item — a worktree is created (I-1705) and
		// named after the item id it was started for.
		Worktree:    id,
		ActivatedAt: time.Now(),
	}
	if err := sessMgr.SetPairing(sessionID, p); err != nil {
		fmt.Fprintf(os.Stderr, "st pair: %v\n", err)
		return 1
	}

	fmt.Printf("Pairing ON for %s — in-session friction relaxed (plan gate, worktree-dirty exit, advisory nags).\n", id)
	fmt.Printf("  The merge gate (tier1/tier2/live-acceptance) is untouched and runs fresh at merge.\n")
	fmt.Printf("  Detach with:  st pair --off\n")
	return 0
}

// startIfNotActive starts item only if it isn't already active — the M2
// "start if not started" behavior. A no-op (exit 0) when already started.
func startIfNotActive(s *store.Store, cfg *config.Config, item *model.Item) int {
	if tc, ok := cfg.Types[item.Type]; ok && item.Status == tc.ActiveStatus {
		return 0
	}
	return startItem(s, cfg, item)
}

// startItem runs Start() unconditionally with a slug derived from the item's
// title (deriveSlug, shared with spawn.go), for M2 (not-yet-started) and M3
// (always-fresh) callers.
func startItem(s *store.Store, cfg *config.Config, item *model.Item) int {
	slug := deriveSlug(item)
	if code := Start(s, cfg, item.ID, StartOpts{Slug: slug}); code != 0 {
		fmt.Fprintf(os.Stderr, "st pair: starting %s: exit %d\n", item.ID, code)
		return code
	}
	return 0
}

// promotePairingEvidence implements /pair --off's promotion step (I-1707,
// design §4 exit flow + §6): the session's audit log seeds a plan draft when
// the item has none approved yet, and any browser-verification observation
// is durably credited via the changelog — regardless of whether a plan was
// seeded. Best-effort by design: a failure here is reported loudly but never
// changes --off's own exit code — the marker is already cleared, and losing
// the promotion is strictly worse than losing --off itself.
func promotePairingEvidence(s *store.Store, cfg *config.Config, sessionID, itemID string) {
	events, err := readPairLogEvents(cfg, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "st pair --off: reading pairing audit log: %v\n", err)
		return
	}
	if len(events) == 0 {
		fmt.Println("  (no audit-log events recorded this session — nothing to promote)")
		return
	}

	item, ok := s.Get(itemID)
	if !ok {
		fmt.Fprintf(os.Stderr, "st pair --off: %s not found — cannot promote pairing evidence\n", itemID)
		return
	}

	var edits, commands, observations []PairLogEvent
	for _, ev := range events {
		switch ev.Type {
		case "edit":
			edits = append(edits, ev)
		case "command", "server":
			commands = append(commands, ev)
		case "observation":
			observations = append(observations, ev)
		}
	}

	if item.PlanApproved {
		fmt.Printf("  %s already has an approved plan — not overwriting; recording pairing evidence instead.\n", itemID)
		_ = changelog.Append(cfg, itemID, changelog.Entry{
			Op:     "pairing_evidence",
			Reason: fmt.Sprintf("session %s: %d edit(s), %d command(s) recorded during pairing (not seeded into a plan — plan already approved)", sessionID, len(edits), len(commands)),
		})
	} else {
		body := buildPlanSeed(item, edits, commands, observations)
		if code := PlanWrite(s, cfg, itemID, body, false); code != 0 {
			fmt.Fprintf(os.Stderr, "st pair --off: seeding plan for %s failed (exit %d) — audit log preserved at %s\n", itemID, code, pairingLogPath(cfg, sessionID))
		} else {
			fmt.Printf("  Seeded a plan draft for %s from %d edit(s), %d command(s) — review and approve with `st plan approve %s` (or `st plan write %s --self-approve` after refining).\n", itemID, len(edits), len(commands), itemID, itemID)
		}
	}

	for _, obs := range observations {
		_ = changelog.Append(cfg, itemID, changelog.Entry{
			Op:     "pairing_browser_verified",
			Reason: obs.Text,
		})
	}
	if len(observations) > 0 {
		fmt.Printf("  Credited %d browser-verification observation(s).\n", len(observations))
	}
}

// buildPlanSeed renders a plan draft body (the same section format
// internal/plan.Parse expects: `## Approach`, `## Tests`, etc.) from a
// pairing session's recorded events. This is a SEED for the operator to
// refine, not a finished plan — Out-of-scope/Risks are left as explicit
// placeholders since the audit log has no way to know either.
func buildPlanSeed(item *model.Item, edits, commands, observations []PairLogEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s Plan — %s (seeded from a paired session)\n\n", item.ID, item.Title)

	b.WriteString("## Approach\n\n")
	fmt.Fprintf(&b, "Seeded from a `/pair` live-iteration session: %d file edit(s) and %d command(s) were recorded while working on this item. Files touched:\n", len(edits), len(commands))
	seenFiles := map[string]bool{}
	for _, e := range edits {
		key := e.Repo + ":" + e.File
		if e.File == "" || seenFiles[key] {
			continue
		}
		seenFiles[key] = true
		if e.Repo != "" {
			fmt.Fprintf(&b, "- %s (%s)\n", e.File, e.Repo)
		} else {
			fmt.Fprintf(&b, "- %s\n", e.File)
		}
	}
	b.WriteString("\nRefine this into a real approach description before approval — this section is a session-evidence summary, not yet a plan.\n\n")

	b.WriteString("## Tests\n\n")
	if len(observations) > 0 {
		b.WriteString("Browser-verification observed during pairing:\n")
		for _, o := range observations {
			fmt.Fprintf(&b, "- %s\n", o.Text)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("TBD — describe what tests cover this work before approval.\n\n")
	}

	b.WriteString("## Out-of-scope\n\nTBD — refine before approval.\n\n")
	b.WriteString("## Risks\n\nTBD — refine before approval.\n")

	return b.String()
}
