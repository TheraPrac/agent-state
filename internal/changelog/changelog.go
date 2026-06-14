// Package changelog provides append-only JSONL mutation logs for agent-state items.
// Each item gets its own file at .changelog/<id>.log with one JSON entry per line.
package changelog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
)

// Kind classifies a changelog entry by its role in cross-session continuity
// (I-679). It is orthogonal to Op: Op says *what* mutated; Kind says *which
// tape* a later session reads the entry from when it reconstructs context.
type Kind string

const (
	// KindTransition is a deliberate declarative lifecycle mutation
	// (create/update/start/close/tag/dep/plan/...). Low frequency, agent-
	// written via st workflow commands. The default for any entry whose
	// Kind cannot be derived as exec — including all pre-I-679 history.
	KindTransition Kind = "transition"
	// KindExec is an execution-tape event: the runtime record of work
	// actually happening (commit, test result, pr/deploy, pipeline step
	// completion). Captured as a side-effect of the command, never
	// something the agent must remember to write.
	KindExec Kind = "exec"
	// KindDecision is a forced inflection point + rationale + rejected
	// alternatives. NEVER derived from Op — only written explicitly by the
	// I-679 decision writers (Phase B native-structured / Phase C
	// extraction backstop). So an unknown Op can safely default away from it.
	KindDecision Kind = "decision"
	// KindHeuristic is an operational rule learned from experience: "when X,
	// do Y / don't do Z". Captures operator corrections, validated unusual
	// approaches, and cross-item behavioral guidance (I-804). Stored in a
	// per-agent heuristic file rather than per-item changelogs, so it
	// surfaces at every resume regardless of which item is active.
	KindHeuristic Kind = "heuristic"
)

// Source records provenance for KindDecision entries so a resuming session
// knows which lines it can stand on (I-679). Empty for transition/exec.
type Source string

const (
	// SourceStructured is captured verbatim from a structured channel
	// (AskUserQuestion answer, ExitPlanMode, st plan approve, push/pop
	// --reason). Authoritative and immutable once written.
	SourceStructured Source = "structured"
	// SourceExtracted is machine-inferred from a to-be-summarized window by
	// the Phase C extraction backstop (command.ExtractDecisions, via the
	// PreCompact/Stop hooks). May be lossy; always carries Confidence.
	// ENFORCED (Phase C, shipped): the extractor reconciles against existing
	// decision entries — structured AND prior extracted — and only appends
	// forks no entry covers yet, so it can never clobber a verbatim record
	// and re-runs are idempotent. Below-threshold extracted entries are
	// still advisory: `st resume` consolidates them into a single
	// boundary-confirm block rather than asserting them as fact.
	SourceExtracted Source = "extracted"
)

// Entry represents a single mutation in the changelog.
type Entry struct {
	Timestamp string `json:"timestamp"`
	Agent     string `json:"agent,omitempty"`
	SessionID string `json:"session,omitempty"` // groups entries from the same subprocess step
	Op        string `json:"op"`                // create, update, start, close, tag_add, tag_rm, dep_add, dep_rm, snapshot
	Field     string `json:"field,omitempty"`    // field that changed
	OldValue  string `json:"old,omitempty"`      // previous value
	NewValue  string `json:"new,omitempty"`      // new value
	Reason    string `json:"reason,omitempty"`   // human/agent explanation

	// I-679 cross-session continuity fields. All omitempty + backward
	// compatible: a pre-I-679 entry has empty Kind/Source/Confidence and is
	// given a stable Kind on read via EffectiveKind (derived from Op),
	// never crashing or silently dropping (operator silent-failure
	// principle). Append stamps Kind at write time for every new entry.
	Kind       Kind    `json:"kind,omitempty"`
	Source     Source  `json:"source,omitempty"`     // provenance; decision entries only
	Confidence float64 `json:"confidence,omitempty"` // 0<c≤1 for SourceExtracted; omitted ⇒ not applicable / fully trusted

	// I-804 heuristic fields. Only set on KindHeuristic entries.
	Scope         string   `json:"scope,omitempty"`          // "per-agent" (default) or "global"
	RelevanceTags []string `json:"relevance_tags,omitempty"` // tag/file affinity hints for resume filtering
}

// classifyKind derives the Kind for an entry from its Op. The exec set is
// small and explicit; everything else is a declarative transition. A
// decision is never derived — it is only ever written explicitly by the
// I-679 decision writers — so an unknown or brand-new Op safely defaults to
// transition. Consequence: adding a new declarative Op needs no change
// here; only a genuinely new execution-event Op does.
func classifyKind(op string) Kind {
	switch op {
	case "commit", "deploy_checked", "pr_recorded":
		return KindExec
	}
	switch {
	case strings.HasPrefix(op, "test_"): // test_executed/_failed/_recorded/_skipped
		return KindExec
	case strings.HasSuffix(op, "_completed"): // pipeline step completions (merge/deploy/smoke)
		return KindExec
	}
	return KindTransition
}

// EffectiveKind returns the entry's Kind for filtering/replay. An explicit
// Kind (stamped by Append under I-679, or written by a decision writer) is
// authoritative and returned as-is. An empty Kind (pre-I-679 history, or
// any path that did not set it) is derived from Op via classifyKind so the
// typed view is consistent across the pre/post-I-679 boundary — a
// historical commit reads back as exec, a historical create as transition.
// Callers must filter on EffectiveKind(), never on a raw Kind == "".
func (e Entry) EffectiveKind() Kind {
	if e.Kind != "" {
		return e.Kind
	}
	return classifyKind(e.Op)
}

// ActiveSessionID is set by st run subprocess steps to group changelog entries.
// When set, all Append calls include this session ID automatically.
var ActiveSessionID string

const (
	// sizeGuardBytes is the per-item changelog size cap. Entries are dropped
	// (with a stderr warning) once the file reaches this size, bounding the
	// blast radius of a runaway snapshot loop (I-1454).
	sizeGuardBytes int64 = 20 * 1024 * 1024

	// tailReadBytes is the window scanned from the file tail when deduplicating
	// snapshot entries. Large enough to hold several full item YAML snapshots.
	tailReadBytes int64 = 128 * 1024
)

// Append adds an entry to the changelog for the given item ID.
func Append(cfg *config.Config, id string, entry Entry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().Format(time.RFC3339)
	}
	if entry.Agent == "" {
		entry.Agent = cfg.AgentID()
	}
	if entry.SessionID == "" && ActiveSessionID != "" {
		entry.SessionID = ActiveSessionID
	}
	// I-679: stamp Kind as a side-effect of the write so the typed tape is
	// captured without any caller having to opt in. An explicit Kind (e.g.
	// a decision writer) is preserved; only an unset Kind is derived.
	if entry.Kind == "" {
		entry.Kind = classifyKind(entry.Op)
	}

	dir := cfg.ChangelogDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating changelog dir: %w", err)
	}

	path := filepath.Join(dir, id+".log")

	// Size guard: once a file exceeds sizeGuardBytes, drop subsequent entries
	// rather than letting a runaway loop fill the disk (I-1454).
	if info, statErr := os.Stat(path); statErr == nil && info.Size() >= sizeGuardBytes {
		fmt.Fprintf(os.Stderr, "st: changelog size guard: %s exceeds %d MB — entry dropped\n",
			filepath.Base(path), sizeGuardBytes/(1024*1024))
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling entry: %w", err)
	}
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing entry: %w", err)
	}

	return nil
}

// Read returns all changelog entries for an item, oldest first.
func Read(cfg *config.Config, id string) ([]Entry, error) {
	path := filepath.Join(cfg.ChangelogDir(), id+".log")
	return readFile(path)
}

// ReadAll returns changelog entries for all items that have changelogs.
func ReadAll(cfg *config.Config) (map[string][]Entry, error) {
	dir := cfg.ChangelogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	result := make(map[string][]Entry)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".log")
		path := filepath.Join(dir, entry.Name())
		items, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		result[id] = items
	}

	return result, nil
}

func readFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	// I-679: read with io.ReadAll + split, NOT bufio.Scanner. A changelog
	// line embeds a full item document via Snapshot()'s NewValue; a single
	// line can exceed bufio.Scanner's 64 KB token cap, which would make the
	// whole changelog silently unreadable (scanner.Err() = ErrTooLong) and
	// strand cross-session replay — the exact silent-drop class I-673
	// (#108) fixed in internal/registry. The read error is subsumed by
	// io.ReadAll; per-line JSON parse errors are still surfaced explicitly.
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("parsing line %q: %w", line, err)
		}
		// Give every read entry a stable, non-empty Kind so filters/replay
		// never have to special-case pre-I-679 history.
		e.Kind = e.EffectiveKind()
		entries = append(entries, e)
	}

	return entries, nil
}

// tailSnapshotContent reads up to tailReadBytes from the tail of path and
// returns the NewValue of the last snapshot entry matching field.
// Returns ("", false) when the file doesn't exist or no matching entry is found.
func tailSnapshotContent(path, field string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", false
	}

	start := info.Size() - tailReadBytes
	partial := start > 0
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", false
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return "", false
	}

	lines := strings.Split(string(data), "\n")
	if partial && len(lines) > 0 {
		lines = lines[1:] // first line may be truncated JSON at the seek boundary; skip it
	}

	var last string
	found := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.Op == "snapshot" && e.Field == field {
			last = e.NewValue
			found = true
		}
	}
	return last, found
}

// Snapshot records the full document state before a subprocess step.
// Returns the snapshot content for later diff comparison.
// Dedup: if the last snapshot for stepName has identical content, the write is
// skipped to prevent runaway loops from inflating the changelog (I-1454).
func Snapshot(cfg *config.Config, id, stepName, content string) (string, error) {
	path := filepath.Join(cfg.ChangelogDir(), id+".log")
	if last, found := tailSnapshotContent(path, stepName); found && last == content {
		return content, nil // identical to previous snapshot — skip
	}
	entry := Entry{
		Op:       "snapshot",
		Field:    stepName,
		NewValue: content,
		Reason:   "pre-step snapshot",
	}
	err := Append(cfg, id, entry)
	return content, err
}

// DiffSnapshot compares a pre-step snapshot with the current content
// and returns a human-readable summary of what changed.
func DiffSnapshot(before, after string) string {
	if before == after {
		return "(no changes)"
	}

	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")

	// Build line sets for simple diff
	beforeSet := make(map[string]bool, len(beforeLines))
	for _, l := range beforeLines {
		beforeSet[strings.TrimSpace(l)] = true
	}
	afterSet := make(map[string]bool, len(afterLines))
	for _, l := range afterLines {
		afterSet[strings.TrimSpace(l)] = true
	}

	var added, removed []string
	for _, l := range afterLines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" && !beforeSet[trimmed] {
			added = append(added, trimmed)
		}
	}
	for _, l := range beforeLines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" && !afterSet[trimmed] {
			removed = append(removed, trimmed)
		}
	}

	var sb strings.Builder
	if len(removed) > 0 {
		for _, l := range removed {
			if len(l) > 80 {
				l = l[:77] + "..."
			}
			sb.WriteString("  - " + l + "\n")
		}
	}
	if len(added) > 0 {
		for _, l := range added {
			if len(l) > 80 {
				l = l[:77] + "..."
			}
			sb.WriteString("  + " + l + "\n")
		}
	}

	if sb.Len() == 0 {
		return "(whitespace-only changes)"
	}
	return sb.String()
}

// LastSnapshot returns the most recent snapshot content for a given step, if any.
func LastSnapshot(cfg *config.Config, id, stepName string) string {
	entries, err := Read(cfg, id)
	if err != nil {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Op == "snapshot" && entries[i].Field == stepName {
			return entries[i].NewValue
		}
	}
	return ""
}

// heuristicPath returns the path to the per-agent heuristic log file.
func heuristicPath(cfg *config.Config, agentID string) string {
	return filepath.Join(cfg.ChangelogDir(), "_heuristic-"+agentID+".log")
}

// HeuristicAppend writes a KindHeuristic entry to the per-agent heuristic log.
// entry.Kind is forced to KindHeuristic; Agent and Timestamp are auto-stamped
// if absent. Writes to .changelog/_heuristic-<agentID>.log, creating the file
// if it does not exist.
func HeuristicAppend(cfg *config.Config, entry Entry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().Format(time.RFC3339)
	}
	agentID := cfg.AgentID()
	if entry.Agent == "" {
		entry.Agent = agentID
	}
	entry.Kind = KindHeuristic

	dir := cfg.ChangelogDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating changelog dir: %w", err)
	}

	path := heuristicPath(cfg, agentID)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling entry: %w", err)
	}
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing entry: %w", err)
	}

	return nil
}

// HeuristicList reads heuristic entries for a given agent.
//
// filterTags controls relevance filtering:
//   - Empty filterTags (nil or zero-length): return ALL entries. An item
//     with no tags is a signal that any operational rule may be relevant —
//     surfacing everything is the conservative "superset beats silent drop"
//     choice and matches the approved plan (I-804 §HeuristicList).
//   - Non-empty filterTags: include only entries where len(RelevanceTags)==0
//     (universal) OR at least one RelevanceTags element matches a filterTags
//     element. This narrows to rules relevant to the item's context.
//
// Returns nil, nil if the heuristic file does not exist.
func HeuristicList(cfg *config.Config, agentID string, filterTags []string) ([]Entry, error) {
	path := heuristicPath(cfg, agentID)
	entries, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if len(filterTags) == 0 || len(entries) == 0 {
		return entries, nil
	}

	tagSet := make(map[string]bool, len(filterTags))
	for _, t := range filterTags {
		tagSet[t] = true
	}

	var filtered []Entry
	for _, e := range entries {
		if len(e.RelevanceTags) == 0 {
			filtered = append(filtered, e)
			continue
		}
		for _, rt := range e.RelevanceTags {
			if tagSet[rt] {
				filtered = append(filtered, e)
				break
			}
		}
	}
	return filtered, nil
}

// Format renders a changelog entry as a human-readable string.
func (e Entry) Format() string {
	ts := e.Timestamp
	if len(ts) > 19 {
		ts = ts[:19] // trim timezone for readability
	}

	var parts []string
	parts = append(parts, ts)
	if e.Agent != "" {
		parts = append(parts, fmt.Sprintf("[%s]", e.Agent))
	}
	parts = append(parts, e.Op)

	if e.Field != "" {
		if e.OldValue != "" && e.NewValue != "" {
			parts = append(parts, fmt.Sprintf("%s: %s → %s", e.Field, e.OldValue, e.NewValue))
		} else if e.NewValue != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", e.Field, e.NewValue))
		} else if e.OldValue != "" {
			parts = append(parts, fmt.Sprintf("%s: (removed %s)", e.Field, e.OldValue))
		} else {
			parts = append(parts, e.Field)
		}
	}

	if e.Reason != "" {
		parts = append(parts, fmt.Sprintf("— %s", e.Reason))
	}

	return strings.Join(parts, "  ")
}
