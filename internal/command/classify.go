package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/classify"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ClassifyOpts holds flags for the `st classify` command.
type ClassifyOpts struct {
	// Force bypasses the cache: the classifier ignores any previously
	// persisted Result with a matching InputHash and re-runs.
	Force bool
	// Files overrides the touched-files list. Without this flag, the
	// command reads files from the item's manifest. Used for testing
	// the deny-list path without staging a real branch diff.
	Files []string
	// Model injects a Model implementation. Production wiring leaves
	// this nil — phase 1 of T-345 ships without a real Model, so any
	// non-deny-list classification returns ErrModelNotWired until
	// I-590 (phase 2) lands.
	Model classify.Model
}

// Classify runs the binary autonomy classifier on the given item and
// persists the verdict to the item's classification.* nested fields.
// The verdict is also printed as one JSON line on stdout for piping.
//
// Return codes:
//
//	0 — verdict computed and persisted
//	1 — error (item not found, persist failed, model error)
//	2 — Model not wired (phase-2 sentinel; not a hard error, see
//	    classify.ErrModelNotWired)
func Classify(s *store.Store, cfg *config.Config, id string, opts ClassifyOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	in := buildClassifyInputs(cfg, id, item, opts.Files)
	cached := readCachedResult(item)

	classifier := classify.Classifier{
		DenyList: classify.HardRedPatterns,
		Model:    opts.Model,
	}

	res, err := classifier.Classify(in, cached, opts.Force)
	if err != nil {
		if errors.Is(err, classify.ErrModelNotWired) {
			fmt.Fprintln(os.Stderr, "classify: Model not wired — deny-list did not match and phase-2 LLM wiring is pending (I-590)")
			return 2
		}
		fmt.Fprintf(os.Stderr, "classify %s: %v\n", id, err)
		return 1
	}

	if err := persistClassification(s, id, res); err != nil {
		fmt.Fprintf(os.Stderr, "persist %s: %v\n", id, err)
		return 1
	}

	_ = changelog.Append(cfg, id, changelog.Entry{
		Op:       "classify",
		Field:    "classification.verdict",
		NewValue: string(res.Verdict),
		Reason:   res.Reason,
	})

	line, err := json.Marshal(struct {
		ID           string  `json:"id"`
		Verdict      string  `json:"verdict"`
		Reason       string  `json:"reason"`
		Confidence   float64 `json:"confidence"`
		ClassifiedBy string  `json:"classified_by"`
		InputHash    string  `json:"input_hash"`
	}{id, string(res.Verdict), res.Reason, res.Confidence, res.ClassifiedBy, res.InputHash})
	if err != nil {
		// Should never happen — the struct above marshals cleanly.
		fmt.Fprintf(os.Stderr, "marshal verdict: %v\n", err)
		return 1
	}
	fmt.Println(string(line))
	return 0
}

// buildClassifyInputs assembles the classifier inputs from the item
// file, sidecar plan, and (optionally) an explicit --files override.
//
// Phase 1: touched files come from --files or item.Manifest. Phase 2
// (I-590) will derive them from the live branch diff against main.
func buildClassifyInputs(cfg *config.Config, id string, item *model.Item, filesOverride []string) classify.Inputs {
	in := classify.Inputs{
		ItemID: id,
		Title:  item.Title,
		Type:   item.Type,
		Repo:   item.Repo,
		Tags:   append([]string(nil), item.Tags...),
		SBAR: classify.SBARInput{
			Situation:      item.SBAR.Situation,
			Background:     item.SBAR.Background,
			Assessment:     item.SBAR.Assessment,
			Recommendation: item.SBAR.Recommendation,
		},
	}

	if p, err := plan.Load(cfg.PlansDir(), id); err == nil && p != nil {
		in.PlanContent = p.RawText
	}

	switch {
	case len(filesOverride) > 0:
		in.TouchedFiles = append([]string(nil), filesOverride...)
	default:
		in.TouchedFiles = touchedFilesFromManifest(item)
	}
	in.DiffLineCount = len(in.TouchedFiles)
	return in
}

// touchedFilesFromManifest extracts file paths from the item's
// manifest. The manifest stores file_details as a multiline string
// of the form "M path/to/file +X/-Y (...) [tag]" per line. We pull
// the path token (third field) from each line.
func touchedFilesFromManifest(item *model.Item) []string {
	if item.Manifest == nil {
		return nil
	}
	raw, _ := item.Manifest["file_details"].(string)
	if raw == "" {
		return nil
	}
	var files []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// Expected: <repo> <M|A|D> <path> +X -Y (net) [tag]
		// Some legacy lines start with just <M|A|D> <path> — handle both.
		if len(fields) >= 3 && (fields[0] == "M" || fields[0] == "A" || fields[0] == "D") {
			files = append(files, fields[1])
			continue
		}
		if len(fields) >= 4 {
			files = append(files, fields[2])
		}
	}
	return files
}

// readCachedResult reads any previously-persisted classification.*
// fields off the item and returns them as a classify.Result. A zero
// Result (no Verdict) means "no cache".
func readCachedResult(item *model.Item) classify.Result {
	if item.Doc == nil {
		return classify.Result{}
	}
	get := func(key string) string {
		v, _ := item.Doc.GetNestedField("classification." + key)
		return v
	}
	verdict := get("verdict")
	if verdict == "" {
		return classify.Result{}
	}
	res := classify.Result{
		Verdict:      classify.Verdict(verdict),
		Reason:       get("reason"),
		ClassifiedBy: get("classified_by"),
		InputHash:    get("input_hash"),
	}
	if conf := get("confidence"); conf != "" {
		if f, err := strconv.ParseFloat(conf, 64); err == nil {
			res.Confidence = f
		}
	}
	if at := get("classified_at"); at != "" {
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			res.ClassifiedAt = t
		}
	}
	return res
}

// persistClassification writes the Result to the item's
// classification.* nested fields and records a changelog entry.
func persistClassification(s *store.Store, id string, res classify.Result) error {
	return s.Mutate(id, func(item *model.Item) error {
		item.SetNested("classification", "verdict", string(res.Verdict))
		item.SetNested("classification", "reason", res.Reason)
		item.SetNested("classification", "confidence", strconv.FormatFloat(res.Confidence, 'g', -1, 64))
		item.SetNested("classification", "classified_at", res.ClassifiedAt.Format(time.RFC3339))
		item.SetNested("classification", "classified_by", res.ClassifiedBy)
		item.SetNested("classification", "input_hash", res.InputHash)
		return nil
	})
}
