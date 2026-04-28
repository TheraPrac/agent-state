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

// docWithFields creates a ParsedDocument containing the given field keys.
func docWithFields(fields ...string) *model.ParsedDocument {
	doc := model.NewParsedDocument()
	for _, f := range fields {
		doc.Lines = append(doc.Lines, model.Line{
			Raw:   f + ":",
			Key:   f,
			Value: "",
		})
	}
	return doc
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

// I-433: tasks and issues share the unified vocabulary
// (queued/active/done/abandoned/archived). The old per-type asymmetry
// (where "open" was issue-only, "completed" was task-only) is gone.
func TestItemStatusValidForType(t *testing.T) {
	cfg := config.Defaults()

	// `queued` is the start status for both types post-I-433.
	for _, typ := range []string{"task", "issue"} {
		item := validItem()
		item.Type = typ
		if typ == "issue" {
			item.ID = "I-001"
		}
		item.Status = "queued"
		r := Item(item, cfg)
		if !r.OK() {
			t.Errorf("queued should be valid for %s: %v", typ, r.Errors)
		}
	}

	// Legacy values (open/completed/resolved/wontfix) are no longer in
	// the type's status set, so validation should reject them.
	for _, typ := range []string{"task", "issue"} {
		item := validItem()
		item.Type = typ
		if typ == "issue" {
			item.ID = "I-001"
		}
		item.Status = "open"
		if Item(item, cfg).OK() {
			t.Errorf("legacy 'open' should be invalid for %s post-I-433", typ)
		}
	}
}

func TestDirectoryConsistency(t *testing.T) {
	cfg := config.Defaults()

	// I-433: unified DirectoryMap — queued/active in original dir,
	// done/abandoned/archived in archive/.
	tests := []struct {
		name      string
		itemType  string
		status    string
		dir       string
		wantError bool
	}{
		{"task queued in tasks", "task", "queued", "/path/to/tasks", false},
		{"task active in tasks", "task", "active", "/path/to/tasks", false},
		{"task done in archive", "task", "done", "/path/to/archive", false},
		{"task queued in archive", "task", "queued", "/path/to/archive", true},
		{"task done in tasks", "task", "done", "/path/to/tasks", true},
		{"issue queued in issues", "issue", "queued", "/path/to/issues", false},
		{"issue done in archive", "issue", "done", "/path/to/archive", false},
		{"issue queued in archive", "issue", "queued", "/path/to/archive", true},
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

func TestTypeSpecificRequiredFields(t *testing.T) {
	cfg := config.Defaults()

	t.Run("task missing depends_on and blocks", func(t *testing.T) {
		item := validItem()
		item.Doc = docWithFields("id", "type", "status", "title")
		r := Item(item, cfg)
		fieldErrors := map[string]bool{}
		for _, e := range r.Errors {
			fieldErrors[e.Field] = true
		}
		if !fieldErrors["depends_on"] {
			t.Error("expected error for missing depends_on")
		}
		if !fieldErrors["blocks"] {
			t.Error("expected error for missing blocks")
		}
	})

	t.Run("task with all required fields", func(t *testing.T) {
		item := validItem()
		item.Doc = docWithFields("id", "type", "status", "title", "depends_on", "blocks")
		r := Item(item, cfg)
		if !r.OK() {
			t.Errorf("expected no errors, got: %v", r.Errors)
		}
	})

	// I-406: severity is no longer required on issues. The new field
	// list is just depends_on + blocks. Validation should succeed when
	// those are present even without severity.
	t.Run("issue without severity is valid post-I-406", func(t *testing.T) {
		item := validItem()
		item.Type = "issue"
		item.ID = "I-001"
		item.Status = "queued"
		item.Doc = docWithFields("id", "type", "status", "title", "depends_on", "blocks")
		r := Item(item, cfg)
		if !r.OK() {
			t.Errorf("expected no errors post-I-406, got: %v", r.Errors)
		}
	})

	t.Run("issue with all required fields", func(t *testing.T) {
		item := validItem()
		item.Type = "issue"
		item.ID = "I-001"
		item.Status = "queued"
		item.Doc = docWithFields("id", "type", "status", "title", "depends_on", "blocks")
		r := Item(item, cfg)
		if !r.OK() {
			t.Errorf("expected no errors, got: %v", r.Errors)
		}
	})

	t.Run("no doc skips required field check", func(t *testing.T) {
		item := validItem()
		// No Doc set — should still pass schema checks
		r := Item(item, cfg)
		if !r.OK() {
			t.Errorf("expected no errors without doc, got: %v", r.Errors)
		}
	})
}

func TestIndexCoverage(t *testing.T) {
	cfg := config.Defaults()
	now := time.Now()

	items := map[string]*model.Item{
		"T-001": {ID: "T-001", Type: "task", Status: "queued", Created: now},
		"T-002": {ID: "T-002", Type: "task", Status: "queued", Created: now},
		"T-003": {ID: "T-003", Type: "task", Status: "done", Created: now}, // terminal
		"I-001": {ID: "I-001", Type: "issue", Status: "queued", Created: now},
	}

	t.Run("all non-archived items listed", func(t *testing.T) {
		index := "## Active\n- T-001 something\n- T-002 another\n- I-001 issue\n"
		errs := IndexCoverage(items, index, cfg)
		if len(errs) != 0 {
			t.Errorf("expected no errors, got: %v", errs)
		}
	})

	t.Run("missing items flagged", func(t *testing.T) {
		index := "## Active\n- T-001 something\n"
		errs := IndexCoverage(items, index, cfg)
		// T-002 and I-001 should be flagged (T-003 is terminal, skipped)
		if len(errs) != 2 {
			t.Errorf("expected 2 errors, got %d: %v", len(errs), errs)
		}
	})

	t.Run("terminal items not checked", func(t *testing.T) {
		// Only T-003 (completed) should be skipped
		index := "## Active\n- T-001\n- T-002\n- I-001\n"
		errs := IndexCoverage(items, index, cfg)
		if len(errs) != 0 {
			t.Errorf("expected no errors, got: %v", errs)
		}
	})
}

func TestDeliveryGate(t *testing.T) {
	cfg := config.Defaults()
	cfg.Delivery = &config.DeliveryConfig{
		Stages:      []string{"coding", "committed", "pushed", "pr_open", "merged", "deployed_dev", "uat_approved", "closed"},
		ArchiveGate: "uat_approved",
	}
	now := time.Now()

	t.Run("archived task without UAT approval", func(t *testing.T) {
		item := &model.Item{
			ID: "T-001", Type: "task", Status: "done",
			Created: now, LastTouched: now, Title: "Test",
			Delivery: map[string]interface{}{"stage": "merged"},
		}
		r := DeliveryGate(item, cfg)
		if r.OK() {
			t.Error("expected delivery gate error")
		}
	})

	t.Run("archived task with UAT approval", func(t *testing.T) {
		item := &model.Item{
			ID: "T-001", Type: "task", Status: "done",
			Created: now, LastTouched: now, Title: "Test",
			Delivery: map[string]interface{}{"stage": "uat_approved"},
		}
		r := DeliveryGate(item, cfg)
		if !r.OK() {
			t.Errorf("expected no errors, got: %v", r.Errors)
		}
	})

	t.Run("archived task with closed stage", func(t *testing.T) {
		item := &model.Item{
			ID: "T-001", Type: "task", Status: "done",
			Created: now, LastTouched: now, Title: "Test",
			Delivery: map[string]interface{}{"stage": "closed"},
		}
		r := DeliveryGate(item, cfg)
		if !r.OK() {
			t.Errorf("expected no errors, got: %v", r.Errors)
		}
	})

	t.Run("non-terminal item skipped", func(t *testing.T) {
		item := &model.Item{
			ID: "T-001", Type: "task", Status: "queued",
			Created: now, LastTouched: now, Title: "Test",
			Delivery: map[string]interface{}{"stage": "coding"},
		}
		r := DeliveryGate(item, cfg)
		if !r.OK() {
			t.Errorf("expected no errors for non-terminal item, got: %v", r.Errors)
		}
	})

	t.Run("no delivery data skipped", func(t *testing.T) {
		item := &model.Item{
			ID: "T-001", Type: "task", Status: "done",
			Created: now, LastTouched: now, Title: "Test",
		}
		r := DeliveryGate(item, cfg)
		if !r.OK() {
			t.Errorf("expected no errors without delivery data, got: %v", r.Errors)
		}
	})

	t.Run("no delivery config skipped", func(t *testing.T) {
		noCfg := config.Defaults()
		item := &model.Item{
			ID: "T-001", Type: "task", Status: "done",
			Created: now, LastTouched: now, Title: "Test",
			Delivery: map[string]interface{}{"stage": "coding"},
		}
		r := DeliveryGate(item, noCfg)
		if !r.OK() {
			t.Errorf("expected no errors without delivery config, got: %v", r.Errors)
		}
	})

	t.Run("null delivery stage", func(t *testing.T) {
		item := &model.Item{
			ID: "T-001", Type: "task", Status: "done",
			Created: now, LastTouched: now, Title: "Test",
			Delivery: map[string]interface{}{"stage": ""},
		}
		r := DeliveryGate(item, cfg)
		if r.OK() {
			t.Error("expected delivery gate error for null stage")
		}
	})

	t.Run("non-task-issue type skipped", func(t *testing.T) {
		item := &model.Item{
			ID: "D-001", Type: "idea", Status: "promoted",
			Created: now, LastTouched: now, Title: "Test",
			Delivery: map[string]interface{}{"stage": "coding"},
		}
		r := DeliveryGate(item, cfg)
		if !r.OK() {
			t.Errorf("expected no errors for idea type, got: %v", r.Errors)
		}
	})
}

func TestHasField(t *testing.T) {
	doc := docWithFields("id", "type", "depends_on")

	if !HasField(doc, "depends_on") {
		t.Error("expected depends_on to be found")
	}
	if HasField(doc, "blocks") {
		t.Error("expected blocks to not be found")
	}
}
