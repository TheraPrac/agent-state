package command

import (
	"flag"
	"fmt"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

func Ready(s *store.Store, cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	typeF := fs.String("type", "", "filter by type")
	tagF := fs.String("tag", "", "filter by tag")
	limit := fs.Int("limit", 0, "max items to show")
	fs.Parse(args)

	g := deps.Build(s.All(), cfg)
	items := g.Ready()

	// Apply additional filters
	var filtered []*model.Item
	for _, item := range items {
		if *typeF != "" && item.Type != *typeF {
			continue
		}
		if *tagF != "" && !hasTag(item, *tagF) {
			continue
		}
		filtered = append(filtered, item)
	}

	if *limit > 0 && len(filtered) > *limit {
		filtered = filtered[:*limit]
	}

	if len(filtered) == 0 {
		fmt.Println("No ready items.")
		return 0
	}

	for _, item := range filtered {
		p := 2
		if item.Priority != nil {
			p = *item.Priority
		}
		fmt.Printf("%-8s p%d  %s\n", item.ID, p, item.Title)
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
