package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Split executes the I-180 full-stack scope split. Given a parent
// item that classifies as full-stack, it:
//
//  1. Creates a Part A child (backend scope, inherits api-shaped ACs).
//  2. Creates a Part B child (frontend scope, inherits web-shaped ACs;
//     depends_on Part A).
//  3. Mutates the parent: scope_flags.full_stack=true,
//     scope_flags.split_recommended=true, scope_flags.split_decision="split",
//     resolution=split, and an SBAR note pointing at the children.
//
// New child IDs are allocated via store.NextID — NOT literal `-a` /
// `-b` suffixes. The "Part A / Part B" labels are display-only.
//
// Returns the (childA, childB) IDs on success.
func Split(s *store.Store, cfg *config.Config, parentID string) (string, string, error) {
	parent, ok := s.Get(parentID)
	if !ok {
		return "", "", fmt.Errorf("not found: %s", parentID)
	}

	// Refuse to split if the parent is already split or already done.
	if parent.Doc != nil {
		if v, ok := parent.Doc.GetNestedField("scope_flags.split_decision"); ok && v == "split" {
			return "", "", fmt.Errorf("%s is already split (scope_flags.split_decision=split)", parentID)
		}
	}

	// Load the parent's plan sidecar — required so we can partition
	// the AC list. Without a plan sidecar the operator hasn't run
	// `st prep <id>` yet, so the split has nothing to base AC
	// partitioning on.
	planFile, err := plan.Load(cfg.PlansDir(), parentID)
	if err != nil {
		return "", "", fmt.Errorf("loading plan: %w", err)
	}
	if planFile == nil {
		return "", "", fmt.Errorf("%s has no plan sidecar at .plans/%s.md — run `st prep %s` first", parentID, parentID, parentID)
	}

	if plan.Classify(planFile) != "full-stack" {
		return "", "", fmt.Errorf("%s is not full-stack (Classify=%q); split is only for the full-stack bucket",
			parentID, plan.Classify(planFile))
	}

	apiACs, webACs := plan.PartitionACsByLayer(planFile.ACs)

	idA, err := s.NextID(parent.Type)
	if err != nil {
		return "", "", fmt.Errorf("allocating ID for Part A: %w", err)
	}
	if err := createSplitChild(s, cfg, parent, idA, "", planFile,
		"theraprac-api", apiACs, "Part A: backend"); err != nil {
		return "", "", fmt.Errorf("creating Part A: %w", err)
	}

	idB, err := s.NextID(parent.Type)
	if err != nil {
		return "", "", fmt.Errorf("allocating ID for Part B: %w", err)
	}
	if err := createSplitChild(s, cfg, parent, idB, idA, planFile,
		"theraprac-web", webACs, "Part B: frontend"); err != nil {
		return "", "", fmt.Errorf("creating Part B: %w", err)
	}

	// Stamp parent with scope_flags + resolution=split, close the
	// parent, and note the children in the SBAR background.
	if err := s.Mutate(parentID, func(it *model.Item) error {
		// I-180: scope_flags is a nested map. SetNestedField creates
		// the parent block + indented child line so consumers reading
		// via GetNestedField (`st show`, retrospective analysis, etc.)
		// see the values. SetField with a dotted path would write a
		// flat top-level key with the literal "." in the name, which
		// nested readers wouldn't find.
		it.Doc.SetNestedField("scope_flags.full_stack", "true")
		it.Doc.SetNestedField("scope_flags.split_recommended", "true")
		it.Doc.SetNestedField("scope_flags.split_decision", "split")
		it.Doc.SetNestedField("scope_flags.split_into", fmt.Sprintf("%s, %s", idA, idB))
		// resolution is a list field; the renderer expects each value
		// to include the leading "- " marker since ReplaceList writes
		// Raw verbatim. Keep the in-memory typed field aligned with
		// what the parser would read back from that on-disk form.
		it.Resolution = []string{"split"}
		it.Doc.ReplaceList("resolution", []string{"- split"})
		// Close the parent so the active queue stops surfacing it.
		// Children carry the work going forward.
		it.Status = "done"
		it.Doc.SetField("status", "done")
		// Append a note to the SBAR background pointing at the
		// children so future readers see the lineage.
		note := fmt.Sprintf("\n\nI-180 split: replaced by %s (Part A: backend) + %s (Part B: frontend).",
			idA, idB)
		it.SBAR.Background = strings.TrimRight(it.SBAR.Background, "\n") + note
		it.Doc.SetSBARBlock(it.SBAR)
		return nil
	}); err != nil {
		return "", "", fmt.Errorf("stamping parent: %w", err)
	}

	_ = changelog.Append(cfg, parentID, changelog.Entry{
		Op:       "split",
		NewValue: fmt.Sprintf("%s,%s", idA, idB),
		Reason:   fmt.Sprintf("I-180: full-stack item split into linked items %s + %s", idA, idB),
	})

	return idA, idB, nil
}

// createSplitChild constructs a new Item that's a child of parent.
// scope is "theraprac-api" or "theraprac-web". dependsOnID is empty
// for Part A and the Part A id for Part B.
func createSplitChild(
	s *store.Store, cfg *config.Config, parent *model.Item, id, dependsOnID string,
	planFile *plan.Plan, scope string, acs []string, label string,
) error {
	tc, ok := cfg.Types[parent.Type]
	if !ok {
		return fmt.Errorf("unknown type: %s", parent.Type)
	}

	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	title := fmt.Sprintf("%s (%s)", parent.Title, label)

	doc := &model.ParsedDocument{}
	lines := []model.Line{
		{Raw: "id: " + id, Key: "id", Value: id},
		{Raw: "type: " + parent.Type, Key: "type", Value: parent.Type},
		{Raw: "status: " + tc.StartStatus, Key: "status", Value: tc.StartStatus},
		{Raw: "created: " + nowStr, Key: "created", Value: nowStr},
		{Raw: "last_touched: " + nowStr, Key: "last_touched", Value: nowStr},
		{Raw: ""},
		{Raw: "completed: null", Key: "completed", Value: "null"},
		{Raw: ""},
	}

	titleLine := "title: " + title
	if strings.ContainsAny(title, ":`\"") {
		titleLine = fmt.Sprintf("title: %q", title)
	}
	lines = append(lines, model.Line{Raw: titleLine, Key: "title", Value: title})
	lines = append(lines, model.Line{Raw: ""})

	priority := 2
	if parent.Priority != nil {
		priority = *parent.Priority
	}
	lines = append(lines, model.Line{Raw: fmt.Sprintf("priority: %d", priority), Key: "priority", Value: fmt.Sprintf("%d", priority)})
	lines = append(lines, model.Line{Raw: ""})

	// scope_repos
	lines = append(lines, model.Line{Raw: "scope_repos: " + scope, Key: "scope_repos", Value: scope})
	lines = append(lines, model.Line{Raw: ""})

	// depends_on
	lines = append(lines, model.Line{Raw: "depends_on:", Key: "depends_on"})
	if dependsOnID != "" {
		lines = append(lines, model.Line{Raw: "- " + dependsOnID, IsList: true})
	} else {
		lines = append(lines, model.Line{Raw: "- []", IsList: true})
	}
	lines = append(lines, model.Line{Raw: ""})

	// blocks
	hasBlocksRequired := false
	for _, rf := range tc.RequiredFields {
		if rf == "blocks" {
			hasBlocksRequired = true
			break
		}
	}
	if hasBlocksRequired {
		lines = append(lines, model.Line{Raw: "blocks:", Key: "blocks"})
		lines = append(lines, model.Line{Raw: "- []", IsList: true})
		lines = append(lines, model.Line{Raw: ""})
	}

	lines = append(lines, model.Line{Raw: "next_actions:", Key: "next_actions"})
	lines = append(lines, model.Line{Raw: "- []", IsList: true})
	lines = append(lines, model.Line{Raw: ""})

	// scope_flags pointing back at the parent for lineage.
	lines = append(lines, model.Line{Raw: "scope_flags:", Key: "scope_flags"})
	lines = append(lines, model.Line{Raw: "  split_from: " + parent.ID})
	lines = append(lines, model.Line{Raw: ""})

	// SBAR — background carries the lineage note. Other sections
	// inherit from the parent verbatim so the new item starts with
	// real context, not the I-492 placeholder scaffold.
	parentSBAR := parent.SBAR
	childSBAR := model.SBAR{
		Situation:      parentSBAR.Situation,
		Background:     fmt.Sprintf("%s\n\nI-180 split: %s of parent %s.", strings.TrimRight(parentSBAR.Background, "\n"), label, parent.ID),
		Assessment:     parentSBAR.Assessment,
		Recommendation: parentSBAR.Recommendation,
	}
	if parent.Type == "task" || parent.Type == "issue" {
		lines = append(lines, model.Line{Raw: "sbar:", Key: "sbar"})
		// Use SetSBARBlock after Create to write the SBAR; for
		// initial scaffold seed the sub-keys with placeholders so
		// validation accepts them, then replace via SetSBARBlock.
		for _, key := range []string{"situation", "background", "assessment", "recommendation"} {
			lines = append(lines, model.Line{Raw: "  " + key + ": |-"})
			lines = append(lines, model.Line{Raw: "    " + model.SBARPlaceholders[key]})
		}
	}

	doc.Lines = lines

	child := &model.Item{
		ID:                 id,
		Type:               parent.Type,
		Status:             tc.StartStatus,
		Title:              title,
		Created:            now,
		LastTouched:        now,
		Priority:           &priority,
		AcceptanceCriteria: acs,
		Doc:                doc,
	}
	if dependsOnID != "" {
		child.DependsOn = []string{dependsOnID}
	}
	child.WorkTracking = make(map[string]interface{})
	child.Delivery = make(map[string]interface{})
	child.TestingEvidence = make(map[string]interface{})
	child.TimeTracking = make(map[string]interface{})
	child.Manifest = make(map[string]interface{})

	if err := s.Create(child); err != nil {
		return err
	}

	// Now Mutate to write the real SBAR (replacing placeholders) and
	// the AC list.
	return s.Mutate(id, func(it *model.Item) error {
		it.SBAR = childSBAR
		it.Doc.SetSBARBlock(it.SBAR)
		if len(acs) > 0 {
			// ReplaceList stores `Raw` verbatim — prefix with `- ` so
			// the rendered YAML reads back as a list (not bare strings
			// under the key).
			prefixed := make([]string, len(acs))
			for i, a := range acs {
				prefixed[i] = "- " + a
			}
			it.Doc.ReplaceList("acceptance_criteria", prefixed)
			it.AcceptanceCriteria = acs
		}
		return nil
	})
}

// SplitCommand is the CLI entry point for `st split <id>`. Prints the
// two child IDs on success or a non-zero exit on failure.
//
// SPLIT RECOMMENDATION: this command is designed to be invoked
// programmatically from `st prep`'s review-gate fifth-option ("Split:
// create child items and reject this plan"), but is also exposed as
// a standalone command so an operator can split a previously-prepped
// item without re-entering prep.
//
// linked items: the two children carry depends_on so Part B is
// blocked on Part A's completion. The parent is closed as
// resolution=split with scope_flags pointing at both children.
func SplitCommand(s *store.Store, cfg *config.Config, id string) int {
	idA, idB, err := Split(s, cfg, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "split: %v\n", err)
		return 1
	}
	fmt.Printf("Split %s into linked items: %s (backend) + %s (frontend, depends_on %s)\n",
		id, idA, idB, idA)
	autoSync(s, fmt.Sprintf("st split: %s -> %s + %s", id, idA, idB))
	return 0
}
