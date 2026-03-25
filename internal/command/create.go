package command

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

func Create(s *store.Store, cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	priority := fs.Int("priority", 2, "priority (0-4)")
	tag := fs.String("tag", "", "tag to add")
	depends := fs.String("depends", "", "depends on ID")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: as create <type> \"<title>\" [--priority N] [--tag TAG] [--depends ID]")
		return 2
	}

	itemType := fs.Arg(0)
	title := fs.Arg(1)

	tc, ok := cfg.Types[itemType]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown type: %s\n", itemType)
		return 2
	}

	id, err := s.NextID(itemType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "allocating ID: %v\n", err)
		return 1
	}

	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	// Build the document
	doc := &model.ParsedDocument{}
	lines := []model.Line{
		{Raw: "id: " + id, Key: "id", Value: id},
		{Raw: "type: " + itemType, Key: "type", Value: itemType},
		{Raw: "status: " + tc.StartStatus, Key: "status", Value: tc.StartStatus},
		{Raw: "created: " + nowStr, Key: "created", Value: nowStr},
		{Raw: "last_touched: " + nowStr, Key: "last_touched", Value: nowStr},
		{Raw: ""},
		{Raw: "completed: null", Key: "completed", Value: "null"},
		{Raw: ""},
	}

	// Title
	titleLine := "title: " + title
	if strings.ContainsAny(title, ":`\"") {
		titleLine = fmt.Sprintf("title: %q", title)
	}
	lines = append(lines, model.Line{Raw: titleLine, Key: "title", Value: title})
	lines = append(lines, model.Line{Raw: ""})

	// Priority
	lines = append(lines, model.Line{
		Raw: fmt.Sprintf("priority: %d", *priority), Key: "priority", Value: fmt.Sprintf("%d", *priority),
	})

	// Tags
	if *tag != "" {
		lines = append(lines, model.Line{Raw: fmt.Sprintf("tags: [%s]", *tag)})
	}
	lines = append(lines, model.Line{Raw: ""})

	// Dependencies
	if *depends != "" {
		lines = append(lines, model.Line{Raw: "depends_on:", Key: "depends_on"})
		lines = append(lines, model.Line{Raw: "- " + *depends, IsList: true})
	} else {
		lines = append(lines, model.Line{Raw: "depends_on:", Key: "depends_on"})
		lines = append(lines, model.Line{Raw: "- []", IsList: true})
	}
	lines = append(lines, model.Line{Raw: ""})

	// Next actions
	lines = append(lines, model.Line{Raw: "next_actions:", Key: "next_actions"})
	lines = append(lines, model.Line{Raw: "- []", IsList: true})

	doc.Lines = lines

	item := &model.Item{
		ID:          id,
		Type:        itemType,
		Status:      tc.StartStatus,
		Title:       title,
		Created:     now,
		LastTouched: now,
		Priority:    priority,
		Doc:         doc,
	}

	if *depends != "" {
		item.DependsOn = []string{*depends}
	}
	if *tag != "" {
		item.Tags = []string{*tag}
	}

	item.WorkTracking = make(map[string]interface{})
	item.Delivery = make(map[string]interface{})
	item.TestingEvidence = make(map[string]interface{})
	item.TimeTracking = make(map[string]interface{})
	item.Manifest = make(map[string]interface{})

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	fmt.Printf("Created %s — %s\n", id, title)
	return 0
}
