package sprintinherit

import (
	"reflect"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/deps"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/registry"
)

// env builds the (items, graph, registry) triple Resolve/Drift consume.
func env(items map[string]*model.Item, sprints []registry.Sprint) (map[string]*model.Item, *deps.Graph, *registry.Registry) {
	cfg := config.Defaults()
	return items, deps.Build(items, cfg), &registry.Registry{Sprints: sprints}
}

func it(id, status, sprint string, dependsOn, blocks []string) *model.Item {
	return &model.Item{ID: id, Type: "issue", Status: status, Sprint: sprint, DependsOn: dependsOn, Blocks: blocks}
}

func TestResolve_InheritsActiveSprintViaDependsOn(t *testing.T) {
	// Y depends_on X (so X blocks Y); Y is in an active sprint; X has none.
	items := map[string]*model.Item{
		"X": it("X", "active", "", nil, nil),
		"Y": it("Y", "active", "sp-a", []string{"X"}, nil),
	}
	all, g, reg := env(items, []registry.Sprint{{ID: "sp-a", Status: "active", Epic: "ep-1", Items: []string{"Y"}}})

	target, amb := Resolve("X", all, g, reg)
	if amb != nil {
		t.Fatalf("unexpected ambiguous: %v", amb)
	}
	if target == nil || target.SprintID != "sp-a" || target.EpicID != "ep-1" || target.Via != "Y" {
		t.Fatalf("target = %+v, want {sp-a ep-1 Y}", target)
	}
}

func TestResolve_InheritsViaBlocksFieldWhenReciprocalMissing(t *testing.T) {
	// Only X.blocks is set (Y.depends_on not yet synced) — must still resolve.
	items := map[string]*model.Item{
		"X": it("X", "active", "", nil, []string{"Y"}),
		"Y": it("Y", "active", "sp-a", nil, nil),
	}
	all, g, reg := env(items, []registry.Sprint{{ID: "sp-a", Status: "active", Epic: "ep-1"}})

	target, _ := Resolve("X", all, g, reg)
	if target == nil || target.SprintID != "sp-a" {
		t.Fatalf("target = %+v, want sp-a via blocks field", target)
	}
}

func TestResolve_IgnoresNonActiveSprint(t *testing.T) {
	items := map[string]*model.Item{
		"X": it("X", "active", "", nil, nil),
		"Y": it("Y", "active", "sp-done", []string{"X"}, nil),
	}
	all, g, reg := env(items, []registry.Sprint{{ID: "sp-done", Status: "completed", Epic: "ep-1"}})

	target, amb := Resolve("X", all, g, reg)
	if target != nil || amb != nil {
		t.Fatalf("expected no inherit for completed sprint; got target=%+v amb=%v", target, amb)
	}
}

func TestResolve_NothingBlocked(t *testing.T) {
	items := map[string]*model.Item{"X": it("X", "active", "", nil, nil)}
	all, g, reg := env(items, []registry.Sprint{{ID: "sp-a", Status: "active"}})
	if target, amb := Resolve("X", all, g, reg); target != nil || amb != nil {
		t.Fatalf("expected (nil,nil); got target=%+v amb=%v", target, amb)
	}
}

func TestResolve_SameActiveSprintTwiceIsNotAmbiguous(t *testing.T) {
	items := map[string]*model.Item{
		"X":  it("X", "active", "", nil, nil),
		"Y1": it("Y1", "active", "sp-a", []string{"X"}, nil),
		"Y2": it("Y2", "active", "sp-a", []string{"X"}, nil),
	}
	all, g, reg := env(items, []registry.Sprint{{ID: "sp-a", Status: "active", Epic: "ep-1"}})

	target, amb := Resolve("X", all, g, reg)
	if amb != nil || target == nil || target.SprintID != "sp-a" {
		t.Fatalf("target=%+v amb=%v, want single sp-a", target, amb)
	}
}

func TestResolve_MultipleActiveSprintsIsAmbiguous(t *testing.T) {
	items := map[string]*model.Item{
		"X":  it("X", "active", "", nil, nil),
		"Yb": it("Yb", "active", "sp-b", []string{"X"}, nil),
		"Ya": it("Ya", "active", "sp-a", []string{"X"}, nil),
	}
	all, g, reg := env(items, []registry.Sprint{
		{ID: "sp-a", Status: "active", Epic: "ep-1"},
		{ID: "sp-b", Status: "active", Epic: "ep-2"},
	})

	target, amb := Resolve("X", all, g, reg)
	if target != nil {
		t.Fatalf("expected nil target on ambiguity, got %+v", target)
	}
	if !reflect.DeepEqual(amb, []string{"sp-a", "sp-b"}) {
		t.Fatalf("ambiguous = %v, want sorted [sp-a sp-b]", amb)
	}
}

func TestDrift_FlagsSprintlessBlockerOfActiveSprint(t *testing.T) {
	cfg := config.Defaults()
	items := map[string]*model.Item{
		"X": it("X", "active", "", nil, nil),
		"Y": it("Y", "active", "sp-a", []string{"X"}, nil),
	}
	all, g, reg := env(items, []registry.Sprint{{ID: "sp-a", Status: "active", Epic: "ep-1", Items: []string{"Y"}}})

	errs := Drift(all, g, reg, cfg)
	if len(errs) != 1 || errs[0].ItemID != "X" || errs[0].Field != "sprint" {
		t.Fatalf("Drift = %+v, want one sprint error on X", errs)
	}
}

func TestDrift_SkipsTerminalAndAlreadyPlacedItems(t *testing.T) {
	cfg := config.Defaults()
	items := map[string]*model.Item{
		"Done":    it("Done", "done", "", nil, []string{"Y"}),         // terminal → skip
		"Placed":  it("Placed", "active", "sp-a", nil, []string{"Y"}), // already in a sprint → skip
		"Y":       it("Y", "active", "sp-a", []string{"Done", "Placed"}, nil),
		"sprintY": it("sprintY", "active", "sp-a", nil, nil),
	}
	all, g, reg := env(items, []registry.Sprint{{ID: "sp-a", Status: "active", Epic: "ep-1"}})

	if errs := Drift(all, g, reg, cfg); len(errs) != 0 {
		t.Fatalf("Drift = %+v, want none (terminal + already-placed both skipped)", errs)
	}
}

func TestDrift_AmbiguousProducesGuidanceError(t *testing.T) {
	cfg := config.Defaults()
	items := map[string]*model.Item{
		"X":  it("X", "active", "", nil, nil),
		"Ya": it("Ya", "active", "sp-a", []string{"X"}, nil),
		"Yb": it("Yb", "active", "sp-b", []string{"X"}, nil),
	}
	all, g, reg := env(items, []registry.Sprint{
		{ID: "sp-a", Status: "active"},
		{ID: "sp-b", Status: "active"},
	})

	errs := Drift(all, g, reg, cfg)
	if len(errs) != 1 || errs[0].ItemID != "X" {
		t.Fatalf("Drift = %+v, want one ambiguity error on X", errs)
	}
}
