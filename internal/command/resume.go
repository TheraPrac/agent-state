package command

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ResumeOpts controls `st resume` / `st prime --resume` (I-679).
type ResumeOpts struct {
	ID string // explicit item; empty ⇒ stack top, then first active
}

// tapeAudit is the self-attestation result. The dangerous failure mode is a
// record that LOOKS complete but is not, so the audit is rendered FIRST and
// degrades to a loud, explicit "unverified/gap" rather than a confident
// clean line (operator silent-failure principle, I-679 design decision #3).
type tapeAudit struct {
	verified bool   // true only if the tape could be checked AND matched ground truth
	gap      bool   // true if git/PR ground truth is ahead of the recorded exec tape
	message  string // human-readable; always non-empty
}

// Resume regenerates the paste-able session-start prompt for a long-running
// item LIVE from the changelog — it never stores-and-trusts a snapshot (that
// is the T-300 failure the breadcrumb hack caused). Output = typed changelog
// replay (decisions verbatim, exec tape, transitions summarized) + the plan
// + declarative item state + a self-attestation banner.
func Resume(s *store.Store, cfg *config.Config, opts ResumeOpts) int {
	id := strings.TrimSpace(opts.ID)
	if id == "" {
		id = resolveResumeTarget(s, cfg)
	}
	if id == "" {
		fmt.Println("st resume: no item to resume — pass an id (`st resume I-679`), or push/start an item first.")
		return 1
	}
	item, ok := s.Get(id)
	if !ok {
		fmt.Printf("st resume: unknown item %q\n", id)
		return 1
	}

	entries, err := changelog.Read(cfg, id)
	if err != nil {
		// Never silently proceed as if the tape were empty.
		fmt.Printf("st resume: cannot read changelog for %s: %v\n", id, err)
		return 1
	}

	sessionID := lastSessionID(item)

	// Plan fold-in. A missing/unreadable/empty plan must be LOUD, never a
	// silent omit (operator silent-failure principle, I-690): the plan body
	// is one of the two artifacts a cold session most needs, so its absence
	// has to be impossible to miss rather than a quietly empty section.
	plansDir := cfg.PlansDir()
	planBody, planNote := "", ""
	switch p, err := plan.Load(plansDir, id); {
	case err != nil:
		planNote = "load error: " + err.Error()
	case p == nil:
		planNote = "NOT FOUND — expected " + filepath.Join(plansDir, id+".md")
	default:
		if planBody = strings.TrimSpace(p.RawText); planBody == "" {
			planNote = "file present but EMPTY at " + filepath.Join(plansDir, id+".md")
		}
	}

	audit := auditExecTape(cfg, item, entries, sessionID)

	fmt.Print(renderResume(item, entries, sessionID, planBody, planNote, audit))
	return 0
}

// resolveResumeTarget mirrors prime's "stack beats active" precedence so
// `st resume` with no argument resumes whatever the session was doing.
//
// The active-item fallback is AGENT-SCOPED: in the shared multi-agent
// workspace several peers' items are "active" simultaneously. s.List() sorts
// by ID, so an un-scoped "first active" deterministically returns the
// LOWEST-ID active item — which is frequently a PEER's item, not this
// agent's. For the PostToolUse capture path (CaptureDecision, no explicit
// id) that meant a decision being appended to a peer's changelog, violating
// the coordination rule "never edit a peer's item" (caught live,
// 2026-05-19, before wiring).
// The stack is already per-agent (LoadStack is the local agent's), so only
// the active fallback needs the guard. When no agent identity is resolvable
// (the `as`-CLI-only repo, a plain checkout), there are no peers to collide
// with, so the original global "first active" behavior is preserved.
func resolveResumeTarget(s *store.Store, cfg *config.Config) string {
	stack := LoadStack(cfg)
	if len(stack) > 0 {
		top := stack[len(stack)-1]
		if item, ok := s.Get(top.ID); ok && !cfg.IsTerminalStatus(item.Type, item.Status) {
			return top.ID
		}
	}
	me := cfg.AgentID()
	for _, it := range s.List() {
		if it.Status != "active" {
			continue
		}
		// Agent-scoped: never resolve onto a peer's item. Only relax to
		// global-first-active when this process has no agent identity.
		if me != "" && it.AssignedTo != me {
			continue
		}
		return it.ID
	}
	return ""
}

// lastSessionID returns the most recent Claude Code session for the item:
// the last element of Sessions (append-ordered), falling back to the
// claiming session. Empty ⇒ replay is item-wide rather than session-scoped.
func lastSessionID(item *model.Item) string {
	if n := len(item.Sessions); n > 0 {
		if v := strings.TrimSpace(item.Sessions[n-1]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(item.ClaimedBy)
}

// renderResume is the pure, table-tested core: (item, typed changelog,
// session, plan, audit) → the paste-able prompt. No I/O.
//
// planNote is the loud fallback for the plan section: "" means planBody is
// authoritative; non-empty means the plan could not be loaded (missing /
// unreadable / empty) and the section renders a ⚠️ block instead of silently
// vanishing (I-690 — operator silent-failure principle).
func renderResume(item *model.Item, entries []changelog.Entry, sessionID, planBody, planNote string, audit tapeAudit) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# RESUME %s — %s\n\n", item.ID, item.Title)

	// (1) Self-attestation FIRST — a gap/unverified state must be impossible
	// to miss and must precede anything that reads like a complete record.
	switch {
	case audit.gap:
		b.WriteString("## ⚠️  EXECUTION-TAPE GAP — DO NOT TRUST THE TAPE BELOW AS COMPLETE\n")
		b.WriteString("  " + audit.message + "\n")
		b.WriteString("  The capture path may not be firing. Reconcile against `git log` / `gh pr` before acting.\n\n")
	case !audit.verified:
		b.WriteString("## ⚠️  EXECUTION TAPE UNVERIFIED\n")
		b.WriteString("  " + audit.message + "\n\n")
	default:
		b.WriteString("## ✓ Execution tape verified against ground truth\n")
		b.WriteString("  " + audit.message + "\n\n")
	}

	// (2) Declarative state — formal lifecycle, trusted (deliberate transitions).
	b.WriteString("## State\n")
	fmt.Fprintf(&b, "  status: %s   type: %s\n", item.Status, item.Type)
	if v := nestedString(item.Delivery, "stage"); v != "" {
		fmt.Fprintf(&b, "  delivery.stage: %s\n", v)
	}
	if br := nestedString(item.WorkTracking, "branch"); br != "" {
		fmt.Fprintf(&b, "  branch: %s\n", br)
	}
	if item.PlanApproved {
		fmt.Fprintf(&b, "  plan: approved by %s\n", item.PlanApprovedBy)
	} else {
		b.WriteString("  plan: NOT approved (code edits gated)\n")
	}
	if sessionID != "" {
		fmt.Fprintf(&b, "  last session: %s\n", sessionID)
	}
	b.WriteString("\n")

	// (2b) Next — the single highest-value "what to do" line for a cold
	// resume (I-690). Placed immediately after State, ahead of the
	// historical record, because a resuming session needs the forward
	// directive before the backward narrative.
	var nexts []string
	for _, n := range item.NextActions {
		if s := strings.TrimSpace(n); s != "" {
			nexts = append(nexts, s)
		}
	}
	if len(nexts) > 0 {
		b.WriteString("## Next\n")
		for _, n := range nexts {
			fmt.Fprintf(&b, "  → %s\n", n)
		}
		b.WriteString("\n")
	}

	scoped := filterSession(entries, sessionID)

	// (3) Decisions — the non-re-derivable content. Verbatim, with
	// provenance + confidence so the reader knows which lines to stand on.
	var decisions []changelog.Entry
	for _, e := range scoped {
		if e.EffectiveKind() == changelog.KindDecision {
			decisions = append(decisions, e)
		}
	}
	if len(decisions) > 0 {
		b.WriteString("## Decisions (do NOT re-litigate — verbatim)\n")
		for _, e := range decisions {
			prov := string(e.Source)
			if prov == "" {
				prov = "structured"
			}
			tag := prov
			if e.Source == changelog.SourceExtracted {
				tag = fmt.Sprintf("extracted, confidence %.2f — CONFIRM if acting on this", e.Confidence)
			}
			fmt.Fprintf(&b, "  • [%s] %s\n", tag, flattenLine(e.Reason))
		}
		b.WriteString("\n")
	}

	// (4) Execution tape — what the doing actually got to.
	var execs []changelog.Entry
	for _, e := range scoped {
		if e.EffectiveKind() == changelog.KindExec {
			execs = append(execs, e)
		}
	}
	if len(execs) > 0 {
		b.WriteString("## Execution tape\n")
		for _, e := range execs {
			fmt.Fprintf(&b, "  %s  %s\n", shortTS(e.Timestamp), execLine(e))
		}
		b.WriteString("\n")
	}

	// (5) Transitions — summarized, not a firehose: the most recent N
	// declarative entries, rendered oldest→newest (chronological reading
	// for the resume narrative), each as a one-line summary (no full
	// old→new value dump — see transitionLine).
	var trans []changelog.Entry
	for _, e := range scoped {
		if e.EffectiveKind() == changelog.KindTransition {
			trans = append(trans, e)
		}
	}
	if len(trans) > 0 {
		b.WriteString("## Recent transitions\n")
		const cap = 8
		start := 0
		if len(trans) > cap {
			start = len(trans) - cap
		}
		for _, e := range trans[start:] {
			fmt.Fprintf(&b, "  %s  %s\n", shortTS(e.Timestamp), transitionLine(e))
		}
		b.WriteString("\n")
	}

	// (6) Plan — folded in live, never a stored snapshot. ALWAYS emitted:
	// a missing/unreadable/empty plan renders a loud ⚠️ block rather than
	// silently vanishing, so a cold session can never mistake "no plan
	// shown" for "no plan needed" (I-690, operator silent-failure principle).
	if planNote == "" {
		b.WriteString("## Plan (.plans/" + item.ID + ".md)\n")
		b.WriteString(indent(planBody, "  ") + "\n\n")
	} else {
		b.WriteString("## ⚠️  PLAN " + planNote + "\n")
		b.WriteString("  Resume cannot fold in the plan body — author/repair .plans/" + item.ID + ".md, then re-run `st resume " + item.ID + "`.\n\n")
	}

	b.WriteString("---\n")
	fmt.Fprintf(&b, "Regenerated live from the changelog at %s. Nothing above is a stored breadcrumb — re-run `st resume %s` any time.\n",
		time.Now().Format(time.RFC3339), item.ID)
	return b.String()
}

// filterSession keeps entries for the given session. Empty session ⇒ all
// entries (item-wide replay): a visible superset beats silently dropping.
func filterSession(entries []changelog.Entry, sessionID string) []changelog.Entry {
	if sessionID == "" {
		return entries
	}
	var out []changelog.Entry
	for _, e := range entries {
		if e.SessionID == "" || e.SessionID == sessionID {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return entries // never strand the reader with an empty replay
	}
	return out
}

// execLine renders one execution-tape entry as a compact one-liner.
func execLine(e changelog.Entry) string {
	parts := []string{e.Op}
	if e.Field != "" {
		parts = append(parts, e.Field)
	}
	if e.NewValue != "" {
		parts = append(parts, "→ "+flattenLine(e.NewValue))
	}
	if e.Reason != "" {
		parts = append(parts, "— "+flattenLine(e.Reason))
	}
	return strings.Join(parts, " ")
}

// transitionLine renders one declarative transition as a SUMMARY, not a
// dump. It deliberately does NOT echo full old→new values (an sbar.* update
// carries multi-paragraph prose — replaying it verbatim is the firehose the
// design forbids). The reader gets "what field moved", not its contents;
// the live item is the source of truth for the value. No leading timestamp
// (the caller prints it once — Entry.Format would duplicate it).
func transitionLine(e changelog.Entry) string {
	parts := []string{}
	if e.Agent != "" {
		parts = append(parts, "["+e.Agent+"]")
	}
	parts = append(parts, e.Op)
	if e.Field != "" {
		parts = append(parts, e.Field)
	}
	if e.Reason != "" {
		parts = append(parts, "— "+clip(flattenLine(e.Reason), 100))
	}
	return strings.Join(parts, " ")
}

// clip truncates with an ellipsis at most n runes-ish (bytes; values here
// are flattened ASCII-ish so byte length is adequate and avoids a rune walk).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// auditExecTape compares the recorded exec tape against git ground truth for
// the item's branch. It NEVER reports clean unless it actually verified;
// an unresolvable repo degrades to an explicit "unverified", a git-ahead
// state to a loud "gap" (I-679 design decision #3).
func auditExecTape(cfg *config.Config, item *model.Item, entries []changelog.Entry, sessionID string) tapeAudit {
	recorded := countRecordedCommits(entries, sessionID)

	dir := resolveItemRepoDir(cfg, item)
	if dir == "" || !isGitDir(dir) {
		return tapeAudit{
			verified: false,
			message: fmt.Sprintf("no resolvable git worktree for %s — exec tape is UNVERIFIED (recorded %d commit event(s); could not cross-check git).",
				item.ID, recorded),
		}
	}
	branch := nestedString(item.WorkTracking, "branch")
	since := sessionWindowStart(entries, sessionID)
	if since == "" {
		return tapeAudit{
			verified: false,
			message:  fmt.Sprintf("no changelog entries for %s — nothing to audit yet.", item.ID),
		}
	}
	gitN, ok := countBranchCommitsSince(dir, branch, since)
	if !ok {
		return tapeAudit{
			verified: false,
			message: fmt.Sprintf("git log unavailable for branch %q in %s — exec tape UNVERIFIED (recorded %d commit event(s)).",
				branch, dir, recorded),
		}
	}
	return detectTapeGap(recorded, gitN, branch)
}

// sessionWindowStart returns the timestamp of the earliest in-scope
// changelog entry — the start of the work window the audit measures
// against. Total branch commits are meaningless (the branch inherits
// main's deep history); only commits authored *since this item's work
// began* are the tape's responsibility.
func sessionWindowStart(entries []changelog.Entry, sessionID string) string {
	earliest := ""
	for _, e := range filterSession(entries, sessionID) {
		if e.Timestamp == "" {
			continue
		}
		if earliest == "" || e.Timestamp < earliest {
			earliest = e.Timestamp
		}
	}
	return earliest
}

// detectTapeGap is the pure comparison (table-tested). git ahead of the
// recorded tape ⇒ gap; equal/ahead-tape ⇒ verified.
func detectTapeGap(recordedCommits, gitCommits int, branch string) tapeAudit {
	if gitCommits > recordedCommits {
		return tapeAudit{
			gap: true,
			message: fmt.Sprintf("branch %q has %d commit(s) but only %d are on the recorded exec tape — %d uncaptured.",
				branch, gitCommits, recordedCommits, gitCommits-recordedCommits),
		}
	}
	return tapeAudit{
		verified: true,
		message:  fmt.Sprintf("branch %q: %d commit(s), %d on the recorded exec tape — consistent.", branch, gitCommits, recordedCommits),
	}
}

func countRecordedCommits(entries []changelog.Entry, sessionID string) int {
	n := 0
	for _, e := range filterSession(entries, sessionID) {
		if e.EffectiveKind() == changelog.KindExec && e.Op == "commit" {
			n++
		}
	}
	return n
}

// countBranchCommitsSince counts commits UNIQUE to branch (excluding
// inherited origin/main history) authored since the work window opened.
// --since alone is NOT sufficient: a feature branch shares main's history,
// and a main commit whose committer date is after `since` would be
// miscounted as item work — firing a loud but spurious gap (noise == no
// signal). `--not origin/main` bounds the count to branch-unique commits;
// `--since` bounds it to the work window. If main cannot be excluded on
// any known name the count would be unreliable, so this returns ok=false
// ("unverified") rather than risk a false alarm — never confident-but-
// wrong. Returns ok=false if git could not answer.
func countBranchCommitsSince(dir, branch, since string) (int, bool) {
	if branch == "" || since == "" {
		return 0, false
	}
	for _, base := range []string{"origin/main", "origin/master"} {
		out, err := runGit(dir, "rev-list", "--count", "--since="+since, branch, "--not", base)
		if err != nil {
			// Branch ref may not exist in this clone yet (worktree-only
			// HEAD): fall back to HEAD, still excluding inherited main.
			out, err = runGit(dir, "rev-list", "--count", "--since="+since, "HEAD", "--not", base)
			if err != nil {
				continue // try the next base name before giving up
			}
		}
		n := 0
		if _, scanErr := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); scanErr != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false // could not exclude main anywhere — honest unverified, never a false gap
}

// resolveItemRepoDir resolves the worktree the item is actually worked in:
// the repo whose checkout carries the item's branch. It is item-scoped
// (resolveRepoDirForItem keys on the item's own worktree) AND
// branch-verified, so the audit can never silently run against the wrong
// repo (e.g. a theraprac-web item audited against `as` — the silent-
// wrong-answer the tapeAudit design explicitly forbids). Returns "" when
// the branch is unknown or not found in any known repo; the caller renders
// that as an explicit "unverified", never a wrong "clean".
func resolveItemRepoDir(cfg *config.Config, item *model.Item) string {
	branch := nestedString(item.WorkTracking, "branch")
	if branch == "" {
		return "" // cannot attribute the tape to a repo without the item's branch
	}
	for _, repo := range []string{"as", "theraprac-api", "theraprac-web", "theraprac-infra"} {
		d := resolveRepoDirForItem(cfg, item.ID, repo)
		if d == "" || !isGitDir(d) {
			continue
		}
		if _, err := runGit(d, "rev-parse", "--verify", "--quiet", branch+"^{commit}"); err == nil {
			return d
		}
	}
	return ""
}

func nestedString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func flattenLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

func shortTS(ts string) string {
	if len(ts) >= 19 {
		return ts[:19]
	}
	return ts
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

