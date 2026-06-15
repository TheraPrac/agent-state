package command

import (
	"fmt"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/coordinator"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ReadyOpts holds flags for the ready command.
type ReadyOpts struct {
	Type  string
	Tag   string
	Limit int
}

func Ready(s *store.Store, cfg *config.Config, opts ReadyOpts) int {
	g := deps.Build(s.All(), cfg)
	sprints := loadSprintInfo(cfg, g)
	cands := recommendCandidates(s, cfg, g, RecommendOpts{}, sprints)
	leverage, names := unblockLeverage(g, cands)
	pins := loadQueuePins(cfg)
	priorityOverrides := buildPriorityOverrides(g, cands, pins)
	recs := coordinator.Recommend(cands, leverage, sprints, loadGoalWeights(s), priorityOverrides, time.Now(), pins)
	enrichUnblockDetail(recs, names)
	enrichPriorityDetail(recs, priorityOverrides, g.Items, pins)

	// Apply type/tag filters on the ranked slice, then apply limit.
	var filtered []coordinator.Recommendation
	for _, r := range recs {
		if opts.Type != "" && r.Item.Type != opts.Type {
			continue
		}
		if opts.Tag != "" && !hasTag(r.Item, opts.Tag) {
			continue
		}
		filtered = append(filtered, r)
	}

	if opts.Limit > 0 && len(filtered) > opts.Limit {
		filtered = filtered[:opts.Limit]
	}

	if len(filtered) == 0 {
		fmt.Println("No ready items.")
		return 0
	}

	for _, r := range filtered {
		fmt.Printf("%-8s p%d  %s\n", r.Item.ID, r.Priority, r.Item.Title)
	}

	return 0
}

func hasTag(item *model.Item, tag string) bool {
	for _, t := range item.Tags {
		if t == tag {
			return true
		}
	}
	return false
}
