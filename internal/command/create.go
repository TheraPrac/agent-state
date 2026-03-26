package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// CreateOpts holds flags for the create command.
type CreateOpts struct {
	Priority int
	Severity string // issues only: critical, high, medium, low
	Tag      string
	Depends  string
}

func Create(s *store.Store, cfg *config.Config, itemType, title string, opts CreateOpts) int {
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
		Raw: fmt.Sprintf("priority: %d", opts.Priority), Key: "priority", Value: fmt.Sprintf("%d", opts.Priority),
	})

	// Severity (issues only)
	if opts.Severity != "" {
		lines = append(lines, model.Line{
			Raw: "severity: " + opts.Severity, Key: "severity", Value: opts.Severity,
		})
	}

	// Tags
	if opts.Tag != "" {
		lines = append(lines, model.Line{Raw: fmt.Sprintf("tags: [%s]", opts.Tag)})
	}
	lines = append(lines, model.Line{Raw: ""})

	// Dependencies
	if opts.Depends != "" {
		lines = append(lines, model.Line{Raw: "depends_on:", Key: "depends_on"})
		lines = append(lines, model.Line{Raw: "- " + opts.Depends, IsList: true})
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
		Priority:    &opts.Priority,
		Doc:         doc,
	}

	if opts.Severity != "" {
		item.Severity = opts.Severity
	}
	if opts.Depends != "" {
		item.DependsOn = []string{opts.Depends}
	}
	if opts.Tag != "" {
		item.Tags = []string{opts.Tag}
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

	// Record in changelog
	changelog.Append(cfg, id, changelog.Entry{
		Op: "create", Field: "status", NewValue: tc.StartStatus,
		Reason: title,
	})

	fmt.Printf("Created %s — %s\n", id, title)
	return 0
}
