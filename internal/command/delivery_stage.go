package command

import (
	"github.com/theraprac/agent-state/internal/model"
)

// advanceDeliveryStage moves item.delivery.stage forward only — never
// regresses past a later stage already in place. The stage ordering is
// the canonical 8-position lifecycle (see stageIndex in run.go).
//
// I-447: each `st` verb that completes a lifecycle phase calls this so
// items being worked interactively (no `st run` driving the pipeline)
// still reflect their progress on `st status` / `st run status`. The
// "advance only" guarantee means the verb-side calls are safe even
// when a hook (T-447 future work) already inferred the same stage from
// filesystem state.
func advanceDeliveryStage(item *model.Item, target string) {
	if item == nil {
		return
	}
	current, _ := getNestedField(item, "delivery", "stage")
	if stageIndex(target) <= stageIndex(current) {
		return
	}
	item.SetNested("delivery", "stage", target)
}
