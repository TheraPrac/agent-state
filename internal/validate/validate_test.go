package validate

import (
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

func validItem() *model.Item {
	now := time.Now()
	return &model.Item{
		ID:          "T-001",
		Type:        "task",
		Status:      "queued",
		Title:       "Test task",
		Created:     now,
		LastTouched: now,
	}
}

func TestItemValid(t *testing.T) {
	cfg := config.Defaults()
	r := Item(validItem(), cfg)
	if !r.OK() {
		t.Errorf("valid item had errors: %v", r.Errors)
	}
}

func TestItemMissingRequired(t *testing.T) {
	cfg := config.Defaults()
	tests := []struct {
		name   string
		modify func(*model.Item)
		field  string
	}{
		{"missing id", func(i *model.Item) { i.ID = "" }, "id"},
		{"missing type", func(i *model.Item) { i.Type = "" }, "type"},
		{"missing status", func(i *model.Item) { i.Status = "" }, "status"},
		{"missing title", func(i *model.Item) { i.Title = "" }, "title"},
		{"missing created", func(i *model.Item) { i.Created = time.Time{} }, "created"},
		{"missing last_touched", func(i *model.Item) { i.LastTouched = time.Time{} }, "last_touched"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := validItem()
			tt.modify(item)
			r := Item(item, cfg)
			if r.OK() {
				t.Error("expected validation error")
			}
			found := false
			for _, e := range r.Errors {
				if e.Field == tt.field {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected error on field %q, got: %v", tt.field, r.Errors)
			}
		})
	}
}

func TestItemInvalidID(t *testing.T) {
	cfg := config.Defaults()
	tests := []struct {
		id    string
		valid bool
	}{
		{"T-001", true},
		{"I-042", true},
		{"D-100", true},
		{"T-1234", true},
		{"T-01", false},  // too few digits
		{"task-1", false}, // wrong prefix
		{"T001", false},   // no dash
		{"", false},       // empty (caught by required check)
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			item := validItem()
			item.ID = tt.id
			r := Item(item, cfg)
			hasIDError := false
			for _, e := range r.Errors {
				if e.Field == "id" && e.Message != "required" {
					hasIDError = true
				}
			}
			if tt.valid && hasIDError {
				t.Errorf("ID %q should be valid", tt.id)
			}
			if !tt.valid && tt.id != "" && !hasIDError {
				t.Errorf("ID %q should be invalid", tt.id)
			}
		})
	}
}

func TestItemInvalidType(t *testing.T) {
	cfg := config.Defaults()
	item := validItem()
	item.Type = "banana"
	r := Item(item, cfg)
	if r.OK() {
		t.Error("expected error for unknown type")
	}
}

func TestItemInvalidStatus(t *testing.T) {
	cfg := config.Defaults()
	item := validItem()
	item.Status = "flying"
	r := Item(item, cfg)
	if r.OK() {
		t.Error("expected error for invalid status")
	}
}

func TestItemStatusValidForType(t *testing.T) {
	cfg := config.Defaults()

	// "open" is valid for issue, not for task
	item := validItem()
	item.Type = "issue"
	item.ID = "I-001"
	item.Status = "open"
	r := Item(item, cfg)
	if !r.OK() {
		t.Errorf("open should be valid for issue: %v", r.Errors)
	}

	item2 := validItem()
	item2.Status = "open" // task can't be "open"
	r2 := Item(item2, cfg)
	if r2.OK() {
		t.Error("open should be invalid for task")
	}
}

func TestDirectoryConsistency(t *testing.T) {
	cfg := config.Defaults()

	tests := []struct {
		name      string
		itemType  string
		status    string
		dir       string
		wantError bool
	}{
		{"task queued in tasks", "task", "queued", "/path/to/tasks", false},
		{"task active in tasks", "task", "active", "/path/to/tasks", false},
		{"task completed in archive", "task", "completed", "/path/to/archive", false},
		{"task queued in archive", "task", "queued", "/path/to/archive", true},
		{"task completed in tasks", "task", "completed", "/path/to/tasks", true},
		{"issue open in issues", "issue", "open", "/path/to/issues", false},
		{"issue resolved in archive", "issue", "resolved", "/path/to/archive", false},
		{"issue open in archive", "issue", "open", "/path/to/archive", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := validItem()
			item.Type = tt.itemType
			item.Status = tt.status
			if tt.itemType == "issue" {
				item.ID = "I-001"
			}
			r := DirectoryConsistency(item, tt.dir, cfg)
			if tt.wantError && r.OK() {
				t.Error("expected directory consistency error")
			}
			if !tt.wantError && !r.OK() {
				t.Errorf("unexpected error: %v", r.Errors)
			}
		})
	}
}

func TestReciprocalDeps(t *testing.T) {
	now := time.Now()

	// Good: A depends on B, B blocks A
	items := map[string]*model.Item{
		"T-001": {ID: "T-001", Type: "task", Status: "queued", Created: now, DependsOn: []string{"T-002"}},
		"T-002": {ID: "T-002", Type: "task", Status: "queued", Created: now, Blocks: []string{"T-001"}},
	}
	errs := ReciprocalDeps(items)
	if len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}

	// Bad: A depends on B, B doesn't block A
	items2 := map[string]*model.Item{
		"T-001": {ID: "T-001", Type: "task", Status: "queued", Created: now, DependsOn: []string{"T-002"}},
		"T-002": {ID: "T-002", Type: "task", Status: "queued", Created: now, Blocks: []string{}},
	}
	errs2 := ReciprocalDeps(items2)
	if len(errs2) == 0 {
		t.Error("expected reciprocal dependency error")
	}

	// Bad: A depends on nonexistent C
	items3 := map[string]*model.Item{
		"T-001": {ID: "T-001", Type: "task", Status: "queued", Created: now, DependsOn: []string{"T-003"}},
	}
	errs3 := ReciprocalDeps(items3)
	if len(errs3) == 0 {
		t.Error("expected missing dependency error")
	}
}
