// sbar-heal is a one-shot structural repair for agent-state item files
// whose `sbar:` block was corrupted before the I-593 SetNestedField fix
// landed. Two damage classes are healed:
//
//   - I-487: the `summary -> sbar.background` migration wrote
//     un-indented, multi-line prose at column 0, breaking the YAML
//     mapping and stranding duplicate orphaned
//     situation:/background:/assessment:/recommendation: headers below
//     it.
//   - I-593: `st update <id> sbar.<field> --stdin` overwrote a sub-field
//     header line but left the prior block-scalar body lines as orphans.
//
// The parser still recovers the correct leading SBAR values into
// item.SBAR for these files (verified: `st show` / `st plan check`
// render clean), so the repair is: rebuild the `sbar:` block from the
// parsed item.SBAR via the hardened ParsedDocument.SetSBARBlock (which
// excises everything from `sbar:` up to the next recognized top-level
// schema key) and write the file back.
//
// Following the established one-shot pattern (cmd/migrate-status-vocab,
// cmd/migrate-priority): walk issues/tasks/archive, dry-run by default,
// --apply to write, idempotent (a second --apply is a no-op), and a
// final Markdown report.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/parse"
)

type fileResult struct {
	Path      string
	Action    string // healed | skipped_clean | skipped_empty_sbar | skipped_unsafe
	BeforeLn  int
	AfterLn   int
	Signature string // why it was flagged corrupt
	Unsafe    string // field that would change (skipped_unsafe only)
}

func main() {
	apply := flag.Bool("apply", false, "write changes (default: dry-run only)")
	root := flag.String("root", ".", "agent-state root (e.g. theraprac-workspace/agent-state)")
	scope := flag.String("scope", "all", "active | archive | all")
	only := flag.String("only", "", "comma-separated item IDs to restrict to (e.g. T-196,T-203); empty = no filter")
	report := flag.String("report", "", "report path (default: stdout)")
	flag.Parse()

	// --only restricts the run to specific item IDs. The corruption is
	// corpus-wide but most of it is I-595's judgment-heavy per-item
	// prose review; I-593 applies the structural heal only where it is
	// provably content-safe (operator-scoped), leaving the rest to the
	// purpose-built I-595 sweep.
	onlySet := map[string]bool{}
	for _, id := range strings.Split(*only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			onlySet[id] = true
		}
	}

	var dirs []string
	switch *scope {
	case "active":
		dirs = []string{filepath.Join(*root, "issues"), filepath.Join(*root, "tasks")}
	case "archive":
		dirs = []string{filepath.Join(*root, "archive")}
	case "all":
		dirs = []string{
			filepath.Join(*root, "issues"),
			filepath.Join(*root, "tasks"),
			filepath.Join(*root, "archive"),
		}
	default:
		fmt.Fprintf(os.Stderr, "invalid --scope %q (active|archive|all)\n", *scope)
		os.Exit(2)
	}

	var paths []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue // a scope dir may legitimately not exist
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "index.md" {
				continue
			}
			if len(onlySet) > 0 && !onlySet[itemIDFromFilename(e.Name())] {
				continue
			}
			paths = append(paths, filepath.Join(d, e.Name()))
		}
	}
	sort.Strings(paths)

	var results []fileResult
	healed := 0
	for _, p := range paths {
		r, err := healFile(p, *apply)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error %s: %v\n", p, err)
			continue
		}
		if r.Action == "skipped_clean" {
			continue
		}
		results = append(results, r)
		if r.Action == "healed" {
			healed++
		}
	}

	writeReport(*report, results, healed, len(paths), *apply)

	for _, r := range results {
		if r.Action == "skipped_unsafe" {
			// A flagged file whose typed content would change was
			// refused. Exit non-zero so an operator or CI sweep cannot
			// silently miss it.
			os.Exit(1)
		}
	}
}

// healFile inspects one item file. If its sbar block carries the
// corruption signature it rebuilds the block from the parsed
// item.SBAR. Returns the outcome; only writes when apply is true.
func healFile(path string, apply bool) (fileResult, error) {
	orig, err := os.ReadFile(path)
	if err != nil {
		return fileResult{}, err
	}
	item, err := parse.File(path)
	if err != nil {
		return fileResult{}, err
	}
	if item.Doc == nil {
		return fileResult{Path: path, Action: "skipped_clean"}, nil
	}

	sig := corruptionSignature(item.Doc)
	if sig == "" {
		return fileResult{Path: path, Action: "skipped_clean"}, nil
	}

	// Defensive: never overwrite a populated block with an empty one.
	// Every known-damaged item still parses with all four sub-fields
	// recovered; an empty parse here means an unexpected shape — skip
	// and surface it rather than risk data loss.
	s := item.SBAR
	if strings.TrimSpace(s.Situation+s.Background+s.Assessment+s.Recommendation) == "" {
		return fileResult{Path: path, Action: "skipped_empty_sbar", Signature: sig}, nil
	}

	// Ambiguity guard for the consumed region. In corrupt mode the
	// rebuild consumes col-0 keyed lines that are not canonical keys.
	// Genuine I-487 garbage is dedented PROSE that the parser mis-keys
	// — its "key" is a long phrase with spaces/punctuation. A real
	// legacy/freeform field (`design:`, `linked_tasks:`,
	// `current_state_assessment:`) is a short snake_case identifier and
	// is NOT stored in the typed model, so the firstChangedField guard
	// below cannot see its deletion. If such an identifier-like
	// non-canonical key sits in the region we would replace, we cannot
	// prove it is garbage — refuse and surface it (the I-595 sweep does
	// the per-item human judgment). Prose-keyed garbage (spaces, etc.)
	// is not identifier-like, so genuinely-corrupt items like T-196
	// still heal.
	if k := ambiguousConsumedField(item.Doc); k != "" {
		return fileResult{
			Path: path, Action: "skipped_unsafe", Signature: sig,
			Unsafe: "non-canonical field in consumed region: " + k,
		}, nil
	}

	before := countLines(string(orig))
	item.Doc.SetSBARBlock(item.SBAR)
	healed := item.Doc.String()
	if !strings.HasSuffix(healed, "\n") {
		healed += "\n"
	}
	after := countLines(healed)

	if string(orig) == healed {
		// Signature matched but rebuild is byte-identical: already
		// canonical (idempotent second run). Treat as clean.
		return fileResult{Path: path, Action: "skipped_clean"}, nil
	}

	// Content-preservation guard (definitive backstop). Re-parse the
	// healed text and compare the typed item model to the original. The
	// rebuild only restructures the sbar block; every real field —
	// including the four SBAR sub-fields and all non-sbar fields — must
	// be byte-identical after a round-trip. Garbage (col-0 prose, even
	// when mis-parsed with a spurious key) is not a typed field, so it
	// legitimately disappears and does not trip this. If ANY typed
	// field would change, refuse to write and surface it rather than
	// risk the data loss the boundary heuristics are designed to avoid.
	if field, ok := firstChangedField(item, healed); !ok {
		return fileResult{
			Path: path, Action: "skipped_unsafe", Signature: sig,
			BeforeLn: before, AfterLn: after,
			Unsafe: field,
		}, nil
	}

	res := fileResult{
		Path: path, Action: "healed",
		BeforeLn: before, AfterLn: after, Signature: sig,
	}
	if apply {
		if err := os.WriteFile(path, []byte(healed), 0644); err != nil {
			return fileResult{}, err
		}
	}
	return res, nil
}

// countLines returns the number of text lines in s (newline-terminated
// or not), so before/after counts in the report are consistent.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// firstChangedField re-parses healed and compares the typed item model
// against orig. Returns ("", false) and the offending field name if a
// real field would change; ("", true) if every typed field is
// preserved. orig.Doc was mutated by SetSBARBlock but its typed fields
// were not, so orig still reflects the pre-heal parse.
func firstChangedField(orig *model.Item, healed string) (string, bool) {
	tmp, err := os.CreateTemp("", "sbar-heal-verify-*.md")
	if err != nil {
		return "tempfile:" + err.Error(), false
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(healed); err != nil {
		tmp.Close()
		return "tempwrite:" + err.Error(), false
	}
	tmp.Close()

	got, err := parse.File(tmp.Name())
	if err != nil {
		return "reparse:" + err.Error(), false
	}

	a, b := *orig, *got
	a.Doc, b.Doc = nil, nil
	if reflect.DeepEqual(a, b) {
		return "", true
	}
	// Identify the offending field for the report.
	for name, eq := range map[string]bool{
		"sbar":                reflect.DeepEqual(a.SBAR, b.SBAR),
		"id":                  a.ID == b.ID,
		"type":                a.Type == b.Type,
		"status":              a.Status == b.Status,
		"title":               a.Title == b.Title,
		"depends_on":          reflect.DeepEqual(a.DependsOn, b.DependsOn),
		"blocks":              reflect.DeepEqual(a.Blocks, b.Blocks),
		"acceptance_criteria": reflect.DeepEqual(a.AcceptanceCriteria, b.AcceptanceCriteria),
		"resolution":          reflect.DeepEqual(a.Resolution, b.Resolution),
		"tags":                reflect.DeepEqual(a.Tags, b.Tags),
		"sessions":            reflect.DeepEqual(a.Sessions, b.Sessions),
		"work_tracking":       reflect.DeepEqual(a.WorkTracking, b.WorkTracking),
		"testing_evidence":    reflect.DeepEqual(a.TestingEvidence, b.TestingEvidence),
	} {
		if !eq {
			return name, false
		}
	}
	return "other", false
}

// corruptionSignature returns a non-empty reason if the document's
// sbar block carries the I-487/I-593 structural-corruption signature,
// "" if it is clean. It uses the SAME two-regime scan as
// model.SetSBARBlock so detection and rebuild agree exactly:
//
//   - Indent==0, non-empty, NO key  -> I-487 dedented col-0 prose:
//     unambiguous corruption (a real field is always a `key:` line).
//   - A sbar sub-field header (situation/background/assessment/
//     recommendation) appearing more than once -> duplicate orphan
//     headers from the I-593 setter bug.
//
// Crucially, an Indent==0 *keyed* line seen BEFORE any dedented prose
// ends the scan and is NOT corruption — a structurally clean block
// followed by a legacy/non-canonical freeform field (e.g. `impact:`,
// `root_cause:`) is left untouched, never false-flagged. Once dedented
// prose has been seen, keyed garbage is consumed until a canonical
// schema key, mirroring the rebuild boundary.
func corruptionSignature(d *model.ParsedDocument) string {
	start := -1
	for i, l := range d.Lines {
		if l.Key == "sbar" && l.Indent == 0 {
			start = i
			break
		}
	}
	if start < 0 {
		return "" // no sbar block — not this tool's concern
	}
	subCount := map[string]int{}
	sawDedentedProse := false
	var proseSample string
	for i := start + 1; i < len(d.Lines); i++ {
		l := d.Lines[i]
		if strings.TrimSpace(l.Raw) == "---" {
			break
		}
		if l.Indent == 0 && !l.IsEmpty {
			if l.Key == "" {
				if !sawDedentedProse {
					proseSample = trunc(l.Raw)
				}
				sawDedentedProse = true
				continue
			}
			// Indent==0 keyed line.
			if !sawDedentedProse {
				break // clean block; this is the real next field.
			}
			if model.CanonicalTopLevelKeys[l.Key] {
				break // corrupt region ended at the real next field.
			}
			// corrupt block, keyed garbage — keep scanning.
			continue
		}
		if l.Indent == 2 {
			switch l.Key {
			case "situation", "background", "assessment", "recommendation":
				subCount[l.Key]++
			}
		}
	}
	if sawDedentedProse {
		return fmt.Sprintf("dedented col-0 prose inside sbar block: %q", proseSample)
	}
	for k, n := range subCount {
		if n > 1 {
			return fmt.Sprintf("duplicate sbar sub-field header %q (x%d)", k, n)
		}
	}
	return ""
}

// fieldIdentifierRe matches a plausible schema/legacy field key: a
// short snake_case identifier. Dedented I-487 prose that the parser
// mis-keys produces "keys" that are long phrases containing spaces,
// uppercase, punctuation, parentheses, etc. — those do NOT match, so
// they are correctly treated as consumable garbage.
var fieldIdentifierRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func looksLikeFieldIdentifier(key string) bool {
	return len(key) <= 40 && fieldIdentifierRe.MatchString(key)
}

// ambiguousConsumedField walks the same two-regime region SetSBARBlock
// would replace. If, in corrupt mode, it finds an Indent==0 keyed line
// that is (a) not canonical, (b) not one of the four sbar sub-keys,
// and (c) looks like a real field identifier, it returns that key.
// Such a line cannot be proven to be garbage vs a genuine legacy
// freeform field — and because legacy fields are not in the typed
// model, firstChangedField cannot detect their deletion. Returning
// non-empty makes healFile refuse (skipped_unsafe) so the I-595 sweep
// resolves it with human judgment. Prose-keyed garbage is not
// identifier-like, so genuinely-corrupt items still heal.
func ambiguousConsumedField(d *model.ParsedDocument) string {
	start := -1
	for i, l := range d.Lines {
		if l.Key == "sbar" && l.Indent == 0 {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	subKeys := map[string]bool{
		"situation": true, "background": true,
		"assessment": true, "recommendation": true,
	}
	sawDedentedProse := false
	for i := start + 1; i < len(d.Lines); i++ {
		l := d.Lines[i]
		if strings.TrimSpace(l.Raw) == "---" {
			break
		}
		if l.Indent == 0 && !l.IsEmpty {
			if l.Key == "" {
				sawDedentedProse = true
				continue
			}
			if !sawDedentedProse {
				break // clean block; real next field.
			}
			if model.CanonicalTopLevelKeys[l.Key] {
				break // corrupt region ended at the real next field.
			}
			// corrupt-mode keyed line that would be consumed.
			if !subKeys[l.Key] && looksLikeFieldIdentifier(l.Key) {
				return l.Key
			}
		}
	}
	return ""
}

// itemIDFromFilename derives the item ID from an agent-state filename.
// Files are named "<TYPE>-<NUM>-<slug>.md" (e.g.
// "T-196-claim-assembly.md" -> "T-196").
func itemIDFromFilename(name string) string {
	parts := strings.SplitN(name, "-", 3)
	if len(parts) >= 2 {
		return parts[0] + "-" + parts[1]
	}
	return strings.TrimSuffix(name, ".md")
}

func trunc(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

func writeReport(path string, results []fileResult, healed, total int, apply bool) {
	var b strings.Builder
	mode := "DRY-RUN (no files written; pass --apply to write)"
	if apply {
		mode = "APPLIED"
	}
	unsafe := 0
	for _, r := range results {
		if r.Action == "skipped_unsafe" {
			unsafe++
		}
	}
	fmt.Fprintf(&b, "# sbar-heal report\n\nMode: %s\n\n", mode)
	fmt.Fprintf(&b, "Scanned %d files; %d healed; %d refused (skipped_unsafe).\n\n", total, healed, unsafe)
	if unsafe > 0 {
		fmt.Fprintf(&b, "> WARNING: %d file(s) carried a corruption signature but the\n"+
			"> content-preservation guard found a typed field would change — they\n"+
			"> were NOT written. Investigate each before any broader sweep.\n\n", unsafe)
	}
	if len(results) > 0 {
		fmt.Fprintf(&b, "| Item file | Action | Lines (before→after) | Signature / Unsafe field |\n")
		fmt.Fprintf(&b, "|---|---|---|---|\n")
		for _, r := range results {
			detail := r.Signature
			if r.Action == "skipped_unsafe" {
				detail = fmt.Sprintf("WOULD CHANGE %q — %s", r.Unsafe, r.Signature)
			}
			fmt.Fprintf(&b, "| %s | %s | %d→%d | %s |\n",
				filepath.Base(r.Path), r.Action, r.BeforeLn, r.AfterLn, detail)
		}
	}
	out := b.String()
	if path == "" {
		fmt.Print(out)
		return
	}
	if err := os.WriteFile(path, []byte(out), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "writing report %s: %v\n", path, err)
		fmt.Print(out)
	}
}
