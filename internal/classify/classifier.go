package classify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Verdict is the binary autonomy decision: green ⇒ auto-run the full
// delivery loop; red ⇒ stop and surface to the operator.
type Verdict string

const (
	VerdictGreen Verdict = "green"
	VerdictRed   Verdict = "red"
)

// Result is the structured output of one classification — what gets
// persisted on the item and printed for downstream callers.
type Result struct {
	Verdict      Verdict   `json:"verdict"`
	Reason       string    `json:"reason"`
	Confidence   float64   `json:"confidence"`
	ClassifiedAt time.Time `json:"classified_at"`
	ClassifiedBy string    `json:"classified_by"` // "deny-list", "model:<name>", or "stub"
	InputHash    string    `json:"input_hash"`
}

// SBARInput mirrors model.SBAR but lives in this package to keep
// classify decoupled from the model package — the classifier should
// not depend on item-storage concerns.
type SBARInput struct {
	Situation, Background, Assessment, Recommendation string
}

// Inputs are the read-only inputs the classifier consults. Its content
// hash drives the cache: if the hash matches a persisted prior Result,
// the model is not called again (unless --force).
type Inputs struct {
	ItemID        string
	Title         string
	Type          string
	Repo          string
	Tags          []string
	SBAR          SBARInput
	PlanContent   string
	TouchedFiles  []string
	DiffLineCount int
}

// Hash returns a stable content hash over the Inputs. Slices are
// sorted before hashing so caller-side ordering noise doesn't bust
// the cache.
func (in Inputs) Hash() string {
	tags := append([]string(nil), in.Tags...)
	sort.Strings(tags)
	files := append([]string(nil), in.TouchedFiles...)
	sort.Strings(files)

	snap := struct {
		ItemID        string    `json:"item_id"`
		Title         string    `json:"title"`
		Type          string    `json:"type"`
		Repo          string    `json:"repo"`
		Tags          []string  `json:"tags"`
		SBAR          SBARInput `json:"sbar"`
		PlanContent   string    `json:"plan_content"`
		TouchedFiles  []string  `json:"touched_files"`
		DiffLineCount int       `json:"diff_line_count"`
	}{in.ItemID, in.Title, in.Type, in.Repo, tags, in.SBAR, in.PlanContent, files, in.DiffLineCount}

	b, _ := json.Marshal(snap)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Model is the LLM-judge interface. Phase 1 of T-345 ships with no
// production wiring — the Classifier returns ErrModelNotWired when no
// deny-list pattern matches and Model is nil. Phase 2 (I-590) plugs
// in a real claude -p subprocess implementation.
type Model interface {
	Classify(in Inputs) (Result, error)
}

// ErrModelNotWired is returned when the Classifier has no Model and
// the deny-list does not short-circuit the call. Phase 2 (I-590)
// replaces nil Model with the production claude subprocess.
var ErrModelNotWired = fmt.Errorf("classifier: Model not wired (T-345 phase 2 / I-590 pending)")

// Classifier orchestrates deny-list → cache → model. Pure logic; no
// IO. Callers handle reading inputs and persisting results.
type Classifier struct {
	DenyList []DenyPattern
	Model    Model

	// Now is injected for tests. Production leaves it nil and uses
	// time.Now via the helper below.
	Now func() time.Time
}

func (c *Classifier) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// Classify returns the verdict for the given inputs.
//
// Evaluation order:
//  1. Deny-list — any match returns red immediately with reason
//     "static deny-list match: <pattern> — <reason>". The deny-list
//     runs FIRST and is checked on every call, even cache hits, so
//     newly-added deny-list patterns can never be silently overridden
//     by a stale cached green.
//  2. Cache — if cached.InputHash matches the current Inputs.Hash()
//     and force is false, return cached verbatim (timestamp unchanged
//     so callers can detect a cache hit).
//  3. Model — call c.Model.Classify(in). The returned Result has its
//     ClassifiedAt and InputHash stamped by the Classifier (callers
//     should not set those).
//
// If c.Model is nil and the deny-list does not match, Classify returns
// ErrModelNotWired — this is the phase-1 ship state.
func (c *Classifier) Classify(in Inputs, cached Result, force bool) (Result, error) {
	h := in.Hash()

	if d := Match(in.TouchedFiles, c.DenyList); d != nil {
		return Result{
			Verdict:      VerdictRed,
			Reason:       fmt.Sprintf("static deny-list match: %s — %s", d.Label(), d.Reason),
			Confidence:   1.0,
			ClassifiedAt: c.now(),
			ClassifiedBy: "deny-list",
			InputHash:    h,
		}, nil
	}

	if !force && cached.InputHash == h && cached.Verdict != "" {
		return cached, nil
	}

	if c.Model == nil {
		return Result{}, ErrModelNotWired
	}

	res, err := c.Model.Classify(in)
	if err != nil {
		return Result{}, fmt.Errorf("model classify: %w", err)
	}
	res.ClassifiedAt = c.now()
	res.InputHash = h
	if res.ClassifiedBy == "" {
		res.ClassifiedBy = "model"
	}
	return res, nil
}
