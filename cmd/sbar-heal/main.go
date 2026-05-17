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
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/parse"
)

type fileResult struct {
	Path      string
	Action    string // "healed" | "skipped_clean" | "skipped_empty_sbar"
	BeforeLn  int
	AfterLn   int
	Signature string // why it was flagged corrupt
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

	before := strings.Count(string(orig), "\n") + 1
	item.Doc.SetSBARBlock(item.SBAR)
	healed := item.Doc.String()
	if !strings.HasSuffix(healed, "\n") {
		healed += "\n"
	}
	after := strings.Count(healed, "\n")

	if string(orig) == healed {
		// Signature matched but rebuild is byte-identical: already
		// canonical (idempotent second run). Treat as clean.
		return fileResult{Path: path, Action: "skipped_clean"}, nil
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

// corruptionSignature returns a non-empty reason if the document's
// sbar block is structurally corrupt, "" if it looks clean. The sbar
// region is [sbar:, next recognized top-level key | --- ). Within it
// the only legitimate lines are the `sbar:` header, the (<=4) indented
// sub-field headers, their deeper-indented body lines, and blanks.
// Corruption signatures: (a) any Indent==0 non-empty line in the
// region other than the `sbar:` header (dedented col-0 garbage, incl.
// garbage mis-parsed with a spurious Key), or (b) a sub-field header
// appearing more than once (duplicate orphan headers).
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
	for i := start + 1; i < len(d.Lines); i++ {
		l := d.Lines[i]
		if strings.TrimSpace(l.Raw) == "---" {
			break
		}
		if l.Indent == 0 && l.Key != "" && model.KnownTopLevelKeys[l.Key] {
			break // reached the next genuine field
		}
		if l.Indent == 0 && !l.IsEmpty {
			return fmt.Sprintf("col-0 line inside sbar block: %q", trunc(l.Raw))
		}
		if l.Indent == 2 {
			switch l.Key {
			case "situation", "background", "assessment", "recommendation":
				subCount[l.Key]++
			}
		}
	}
	for k, n := range subCount {
		if n > 1 {
			return fmt.Sprintf("duplicate sbar sub-field header %q (x%d)", k, n)
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
	fmt.Fprintf(&b, "# sbar-heal report\n\nMode: %s\n\n", mode)
	fmt.Fprintf(&b, "Scanned %d files; %d corrupt.\n\n", total, healed)
	if len(results) > 0 {
		fmt.Fprintf(&b, "| Item file | Action | Lines (before→after) | Signature |\n")
		fmt.Fprintf(&b, "|---|---|---|---|\n")
		for _, r := range results {
			fmt.Fprintf(&b, "| %s | %s | %d→%d | %s |\n",
				filepath.Base(r.Path), r.Action, r.BeforeLn, r.AfterLn, r.Signature)
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
