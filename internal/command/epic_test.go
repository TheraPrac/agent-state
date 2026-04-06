package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// --- Epic ---

func TestEpicCreate(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := EpicCreate(cfg, "Test Epic")
	if code != 0 {
		t.Fatalf("EpicCreate returned %d, want 0", code)
	}

	// Verify saved
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if len(r.Epics) != 1 {
		t.Fatalf("expected 1 epic, got %d", len(r.Epics))
	}
	if r.Epics[0].Title != "Test Epic" {
		t.Errorf("title: got %q, want %q", r.Epics[0].Title, "Test Epic")
	}
	if r.Epics[0].Status != "active" {
		t.Errorf("status: got %q, want %q", r.Epics[0].Status, "active")
	}
	// ID should be 3-part
	parts := strings.Split(r.Epics[0].ID, "-")
	if len(parts) != 3 {
		t.Errorf("expected 3-part ID, got %d: %q", len(parts), r.Epics[0].ID)
	}
}

func TestEpicListEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := EpicList(s, cfg)
	if code != 0 {
		t.Errorf("EpicList returned %d, want 0", code)
	}
}

func TestEpicListWithItems(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Create an epic
	EpicCreate(cfg, "Test Epic")
	code := EpicList(s, cfg)
	if code != 0 {
		t.Errorf("EpicList returned %d, want 0", code)
	}
}

// --- Sprint ---

func TestSprintCreate(t *testing.T) {
	_, cfg := setupTestEnv(t)
	// Create epic first
	EpicCreate(cfg, "Parent Epic")
	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID

	code := SprintCreate(cfg, epicID, "Sprint 1")
	if code != 0 {
		t.Fatalf("SprintCreate returned %d, want 0", code)
	}

	r2, _ := registry.Load(cfg.EpicsPath())
	if len(r2.Sprints) != 1 {
		t.Fatalf("expected 1 sprint, got %d", len(r2.Sprints))
	}
	if r2.Sprints[0].Epic != epicID {
		t.Errorf("sprint epic: got %q, want %q", r2.Sprints[0].Epic, epicID)
	}
}

func TestSprintCreateBadEpic(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := SprintCreate(cfg, "nonexistent", "Bad Sprint")
	if code != 1 {
		t.Errorf("expected exit code 1 for bad epic, got %d", code)
	}
}

func TestSprintListEmpty(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := SprintList(cfg, "")
	if code != 0 {
		t.Errorf("SprintList returned %d, want 0", code)
	}
}

// --- Note ---

func TestNoteAdd(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := NoteAdd(cfg, "Test note message")
	if code != 0 {
		t.Fatalf("NoteAdd returned %d, want 0", code)
	}

	r, _ := registry.Load(cfg.NotesPath())
	if len(r.Notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(r.Notes))
	}
	if r.Notes[0].Message != "Test note message" {
		t.Errorf("message: got %q, want %q", r.Notes[0].Message, "Test note message")
	}
}

func TestNoteAddWithSession(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "test-agent")
	t.Setenv("AS_SESSION_ID", "session-uuid-123")

	code := NoteAdd(cfg, "Session note")
	if code != 0 {
		t.Fatalf("NoteAdd returned %d, want 0", code)
	}

	r, _ := registry.Load(cfg.NotesPath())
	if r.Notes[0].Author != "test-agent" {
		t.Errorf("author: got %q, want %q", r.Notes[0].Author, "test-agent")
	}
	if r.Notes[0].Session != "session-uuid-123" {
		t.Errorf("session: got %q, want %q", r.Notes[0].Session, "session-uuid-123")
	}
}

func TestNoteList(t *testing.T) {
	_, cfg := setupTestEnv(t)
	NoteAdd(cfg, "Note 1")
	NoteAdd(cfg, "Note 2")

	code := NoteList(cfg, 0)
	if code != 0 {
		t.Errorf("NoteList returned %d, want 0", code)
	}
}

func TestNoteListEmpty(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := NoteList(cfg, 10)
	if code != 0 {
		t.Errorf("NoteList returned %d, want 0", code)
	}
}

func TestNoteEdit(t *testing.T) {
	_, cfg := setupTestEnv(t)
	NoteAdd(cfg, "Original")
	r, _ := registry.Load(cfg.NotesPath())
	noteID := r.Notes[0].ID

	code := NoteEdit(cfg, noteID, "Updated")
	if code != 0 {
		t.Fatalf("NoteEdit returned %d, want 0", code)
	}

	r2, _ := registry.Load(cfg.NotesPath())
	if r2.Notes[0].Message != "Updated" {
		t.Errorf("expected updated message, got %q", r2.Notes[0].Message)
	}
}

func TestNoteEditNotFound(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := NoteEdit(cfg, "nonexistent", "x")
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

func TestNoteRm(t *testing.T) {
	_, cfg := setupTestEnv(t)
	NoteAdd(cfg, "Delete me")
	r, _ := registry.Load(cfg.NotesPath())
	noteID := r.Notes[0].ID

	code := NoteRm(cfg, noteID)
	if code != 0 {
		t.Fatalf("NoteRm returned %d, want 0", code)
	}

	r2, _ := registry.Load(cfg.NotesPath())
	if len(r2.Notes) != 0 {
		t.Error("expected 0 notes after remove")
	}
}

func TestNoteRmNotFound(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := NoteRm(cfg, "nonexistent")
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

// --- Index ---

func TestIndexDeterministic(t *testing.T) {
	s, cfg := setupTestEnv(t)
	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	code := Index(s, cfg)
	if code != 0 {
		t.Fatalf("Index returned %d, want 0", code)
	}
	content1, _ := os.ReadFile(indexPath)

	code = Index(s, cfg)
	if code != 0 {
		t.Fatalf("Index second run returned %d, want 0", code)
	}
	content2, _ := os.ReadFile(indexPath)

	if string(content1) != string(content2) {
		t.Error("index output is not deterministic")
	}
}

func TestIndexContainsSections(t *testing.T) {
	s, cfg := setupTestEnv(t)
	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	Index(s, cfg)
	content, _ := os.ReadFile(indexPath)
	str := string(content)

	for _, section := range []string{
		"# Agent State Index",
		"generated: auto",
		"## Active Work",
		"## Queued Tasks",
		"## Open Issues",
		"## Completed",
	} {
		if !strings.Contains(str, section) {
			t.Errorf("missing section: %q", section)
		}
	}
}

func TestIndexEpicGrouping(t *testing.T) {
	s, cfg := setupTestEnvWithEpics(t)
	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	code := Index(s, cfg)
	if code != 0 {
		t.Fatalf("Index returned %d, want 0", code)
	}

	content, _ := os.ReadFile(indexPath)
	str := string(content)

	if !strings.Contains(str, "### Epic:") {
		t.Error("missing epic header in index")
	}
}

func TestIndexBlockedSection(t *testing.T) {
	s, cfg := setupTestEnv(t)
	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	Index(s, cfg)
	content, _ := os.ReadFile(indexPath)
	str := string(content)

	// T-002 depends on T-001 which is queued (not resolved) → T-002 should be blocked
	if !strings.Contains(str, "## Blocked") {
		t.Error("missing Blocked section")
	}
	if !strings.Contains(str, "T-002") {
		t.Error("T-002 should be in blocked section")
	}
}

func TestIndexIssueSeverity(t *testing.T) {
	s, cfg := setupTestEnv(t)
	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	Index(s, cfg)
	content, _ := os.ReadFile(indexPath)
	str := string(content)

	// I-001 has severity: high → should be under "blocking"
	if !strings.Contains(str, "### blocking") {
		t.Error("missing blocking issue section")
	}
}

func TestIndexWithNotes(t *testing.T) {
	_, cfg := setupTestEnv(t)
	// Add a note
	NoteAdd(cfg, "Test note for index")

	// Reload store
	s2, _ := setupTestEnv(t)
	// Need to point to same config dir — use the original cfg
	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	// Write a note to the notes path the index will read
	r := &registry.Registry{}
	r.Notes = append(r.Notes, registry.Note{
		ID:        "test-note-id",
		Timestamp: time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC),
		Author:    "agent-a",
		Message:   "Test note for index",
	})
	r.Save(cfg.NotesPath())

	Index(s2, cfg)
	content, _ := os.ReadFile(indexPath)
	str := string(content)

	if !strings.Contains(str, "## Notes") {
		t.Error("missing Notes section in index")
	}
	if !strings.Contains(str, "Test note for index") {
		t.Error("note content missing from index")
	}
}

// --- Prime ---

func TestPrimeCompact(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Prime(s, cfg, PrimeOpts{Compact: true})
	if code != 0 {
		t.Fatalf("Prime --compact returned %d, want 0", code)
	}
}

func TestPrimeCompactOmitsCommands(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Capture output
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	Prime(s, cfg, PrimeOpts{Compact: true})
	w.Close()
	os.Stdout = old

	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if strings.Contains(output, "## Commands") {
		t.Error("compact mode should not include Commands section")
	}
}

func TestPrimeIssuesBySeverity(t *testing.T) {
	s, cfg := setupTestEnv(t)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	Prime(s, cfg, PrimeOpts{})
	w.Close()
	os.Stdout = old

	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "## Open Issues") {
		t.Error("missing Open Issues section in prime output")
	}
	// I-001 is high severity → 1 blocking
	if !strings.Contains(output, "1 blocking") {
		t.Error("expected 1 blocking issue in prime output")
	}
}

func TestPrimeGuidance(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Guidance = "Focus on billing tasks this session"

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	Prime(s, cfg, PrimeOpts{})
	w.Close()
	os.Stdout = old

	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "## Guidance") {
		t.Error("missing Guidance section")
	}
	if !strings.Contains(output, "Focus on billing") {
		t.Error("guidance text not in output")
	}
}

func TestPrimeJSONIncludesNewFields(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Guidance = "Test guidance"

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	Prime(s, cfg, PrimeOpts{Format: "json"})
	w.Close()
	os.Stdout = old

	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, `"issues_by_severity"`) {
		t.Error("JSON missing issues_by_severity field")
	}
	if !strings.Contains(output, `"guidance"`) {
		t.Error("JSON missing guidance field")
	}
}

// --- Helpers ---

func TestSetNestedFieldNew(t *testing.T) {
	item := &model.Item{
		TimeTracking: make(map[string]interface{}),
		Doc:          model.NewParsedDocument(),
	}
	item.Doc.Lines = []model.Line{
		{Raw: "id: T-001", Key: "id", Value: "T-001"},
	}

	setNestedField(item, "time_tracking", "started_at", "2026-03-26T10:00:00-06:00")

	val, ok := getNestedField(item, "time_tracking", "started_at")
	if !ok {
		t.Fatal("expected to find started_at")
	}
	if val != "2026-03-26T10:00:00-06:00" {
		t.Errorf("got %q, want %q", val, "2026-03-26T10:00:00-06:00")
	}
}

func TestSetNestedFieldUpdate(t *testing.T) {
	item := &model.Item{
		TimeTracking: make(map[string]interface{}),
		Doc:          model.NewParsedDocument(),
	}
	item.Doc.Lines = []model.Line{
		{Raw: "time_tracking:", Key: "time_tracking"},
		{Raw: "  started_at: old", Key: "started_at", Value: "old", Indent: 2, BlockKey: "time_tracking"},
	}
	item.TimeTracking["started_at"] = "old"

	setNestedField(item, "time_tracking", "started_at", "new")

	val, ok := getNestedField(item, "time_tracking", "started_at")
	if !ok || val != "new" {
		t.Errorf("expected 'new', got %q (ok=%v)", val, ok)
	}
}

func TestGetNestedFieldMissing(t *testing.T) {
	item := &model.Item{
		TimeTracking: make(map[string]interface{}),
	}
	_, ok := getNestedField(item, "time_tracking", "nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestGetNestedFieldBadParent(t *testing.T) {
	item := &model.Item{}
	_, ok := getNestedField(item, "nonexistent_parent", "key")
	if ok {
		t.Error("expected not found for bad parent")
	}
}

func TestCompletionReport_NumericTimeTracking(t *testing.T) {
	// Simulates the extraction logic from printCompletionReport (run.go:5980-5997)
	// when time_tracking values are stored as numeric types (int/float64) by YAML.
	item := &model.Item{
		TimeTracking: map[string]interface{}{
			"run_wall_seconds":   3600,
			"ai_duration_seconds": 2284,
			"ai_cost_usd":        10.4589,
			"input_tokens":       3000000,
			"output_tokens":      2196078,
		},
	}

	// run_wall_seconds
	if v, ok := getNestedField(item, "time_tracking", "run_wall_seconds"); !ok {
		t.Fatal("run_wall_seconds: expected ok=true")
	} else {
		var secs int
		fmt.Sscanf(v, "%d", &secs)
		if secs != 3600 {
			t.Errorf("run_wall_seconds: got %d, want 3600", secs)
		}
	}

	// ai_duration_seconds
	if v, ok := getNestedField(item, "time_tracking", "ai_duration_seconds"); !ok {
		t.Fatal("ai_duration_seconds: expected ok=true")
	} else {
		var secs int
		fmt.Sscanf(v, "%d", &secs)
		if secs != 2284 {
			t.Errorf("ai_duration_seconds: got %d, want 2284", secs)
		}
	}

	// ai_cost_usd
	if v, ok := getNestedField(item, "time_tracking", "ai_cost_usd"); !ok {
		t.Fatal("ai_cost_usd: expected ok=true")
	} else {
		var cost float64
		fmt.Sscanf(v, "%f", &cost)
		if cost < 10.45 || cost > 10.46 {
			t.Errorf("ai_cost_usd: got %f, want ~10.4589", cost)
		}
	}

	// input_tokens
	if v, ok := getNestedField(item, "time_tracking", "input_tokens"); !ok {
		t.Fatal("input_tokens: expected ok=true")
	} else {
		var tok int
		fmt.Sscanf(v, "%d", &tok)
		if tok != 3000000 {
			t.Errorf("input_tokens: got %d, want 3000000", tok)
		}
	}

	// output_tokens
	if v, ok := getNestedField(item, "time_tracking", "output_tokens"); !ok {
		t.Fatal("output_tokens: expected ok=true")
	} else {
		var tok int
		fmt.Sscanf(v, "%d", &tok)
		if tok != 2196078 {
			t.Errorf("output_tokens: got %d, want 2196078", tok)
		}
	}
}

func TestGetNestedField_NumericTypes(t *testing.T) {
	item := &model.Item{
		TimeTracking: map[string]interface{}{
			"ai_duration_seconds": 2284,
			"ai_cost_usd":        10.4589,
			"total_tokens":       5196078,
			"run_wall_seconds":   3600,
			"a_string_field":     "hello",
			"large_float_tokens": float64(5196078), // YAML edge case: large int stored as float64
		},
	}

	tests := []struct {
		key  string
		want string
	}{
		{"ai_duration_seconds", "2284"},
		{"ai_cost_usd", "10.4589"},
		{"total_tokens", "5196078"},
		{"run_wall_seconds", "3600"},
		{"a_string_field", "hello"},
		{"large_float_tokens", "5196078"}, // must not produce scientific notation
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			val, ok := getNestedField(item, "time_tracking", tc.key)
			if !ok {
				t.Fatalf("expected ok=true for key %q", tc.key)
			}
			if val != tc.want {
				t.Errorf("key %q: got %q, want %q", tc.key, val, tc.want)
			}
		})
	}
}

func TestIssueSeverityCategory(t *testing.T) {
	tests := []struct {
		sev  string
		want string
	}{
		{"critical", "blocking"},
		{"high", "blocking"},
		{"medium", "important"},
		{"low", "tech-debt"},
		{"", "important"},
	}
	for _, tt := range tests {
		got := issueSeverityCategory(tt.sev)
		if got != tt.want {
			t.Errorf("issueSeverityCategory(%q) = %q, want %q", tt.sev, got, tt.want)
		}
	}
}

// --- Time Tracking ---

func TestStartRecordsSession(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_SESSION_ID", "test-session-uuid")

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 0 {
		t.Fatalf("Start returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if len(item.Sessions) != 1 || item.Sessions[0] != "test-session-uuid" {
		t.Errorf("expected session recorded, got %v", item.Sessions)
	}
}

func TestStartRecordsStartedAt(t *testing.T) {
	s, cfg := setupTestEnv(t)

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 0 {
		t.Fatalf("Start returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	startedAt, ok := item.TimeTracking["started_at"]
	if !ok || startedAt == "" {
		t.Error("expected started_at in time_tracking")
	}
}

func TestStartSetsLastTouchedBy(t *testing.T) {
	s, cfg := setupTestEnv(t)

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 0 {
		t.Fatalf("Start returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	ltb, ok := item.Doc.GetField("last_touched_by")
	if !ok || ltb == "" {
		t.Error("expected last_touched_by to be set after Start")
	}
	// Without AS_AGENT_ID set, should default to "user"
	if ltb != "user" {
		t.Errorf("last_touched_by = %q, want 'user'", ltb)
	}
}

func TestStartNoSessionWhenUnset(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_SESSION_ID", "")

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 0 {
		t.Fatalf("Start returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if len(item.Sessions) != 0 {
		t.Errorf("expected no sessions, got %v", item.Sessions)
	}
}

func TestCloseRecordsTimeTracking(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Start first to set started_at
	Start(s, cfg, "T-001", StartOpts{})

	// Close
	code := Close(s, cfg, "T-001", "completed", CloseOpts{Force: true})
	if code != 0 {
		t.Fatalf("Close returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	completedAt, ok := item.TimeTracking["completed_at"]
	if !ok || completedAt == "" {
		t.Error("expected completed_at in time_tracking")
	}

	wallHours, ok := item.TimeTracking["wall_time_hours"]
	if !ok || wallHours == "" {
		t.Error("expected wall_time_hours in time_tracking")
	}
}

// --- Helpers for extended test env ---

func setupTestEnvWithEpics(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	_, cfg := setupTestEnv(t)

	// Create an epic
	r := &registry.Registry{}
	e := r.AddEpic("Test Epic")
	r.Save(cfg.EpicsPath())

	// Add a task with that epic
	root := cfg.Root()
	writeFile(t, filepath.Join(root, "tasks", "T-005-epic-task.md"), `id: T-005
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Epic task
epic: `+e.ID+`
tags: [billing]

depends_on:
- []

next_actions:
- []
`)

	// Reload store
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s2, cfg
}

// --- Coverage boosters ---

func TestNestedMapAllParents(t *testing.T) {
	item := &model.Item{
		WorkTracking:    make(map[string]interface{}),
		Delivery:        make(map[string]interface{}),
		TestingEvidence: make(map[string]interface{}),
		TimeTracking:    make(map[string]interface{}),
		Manifest:        make(map[string]interface{}),
	}

	tests := []string{"work_tracking", "delivery", "testing_evidence", "time_tracking", "manifest"}
	for _, parent := range tests {
		m := nestedMap(item, parent)
		if m == nil {
			t.Errorf("nestedMap(%q) returned nil", parent)
		}
	}

	// Unknown parent
	m := nestedMap(item, "unknown")
	if m != nil {
		t.Error("expected nil for unknown parent")
	}
}

func TestNestedMapNilTimeTracking(t *testing.T) {
	item := &model.Item{} // TimeTracking is nil
	m := nestedMap(item, "time_tracking")
	if m == nil {
		t.Error("expected time_tracking to be auto-initialized")
	}
}

func TestWritePendingUAT(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Set delivery config so writePendingUAT runs
	cfg.Delivery = &config.DeliveryConfig{
		Stages:      []string{"coding", "committed", "pushed", "pr_open", "merged", "deployed_dev", "uat_approved", "closed"},
		ArchiveGate: "uat_approved",
	}

	// Set T-003 (active) to have a delivery stage of "deployed_dev"
	item, _ := s.Get("T-003")
	item.Delivery["stage"] = "deployed_dev"

	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	Index(s, cfg)
	content, _ := os.ReadFile(indexPath)
	str := string(content)

	if !strings.Contains(str, "## Pending Deploy/UAT") {
		t.Error("missing Pending Deploy/UAT section")
	}
}

func TestWritePendingUATNoDeliveryConfig(t *testing.T) {
	// When delivery config is nil, writePendingUAT should be a no-op
	var b strings.Builder
	s, cfg := setupTestEnv(t)
	cfg.Delivery = nil
	writePendingUAT(&b, s, cfg)
	if b.Len() != 0 {
		t.Error("expected no output when delivery config is nil")
	}
}

func TestBuildGroupsSorting(t *testing.T) {
	p1 := intPtr(1)
	p2 := intPtr(2)
	items := []*model.Item{
		{ID: "T-010", Title: "No epic, no tags", Priority: p2},
		{ID: "T-011", Title: "Has tag", Tags: []string{"infra"}, Priority: p1},
		{ID: "T-012", Title: "Has epic", Epic: "epic-a", Tags: []string{"billing"}, Priority: p1},
		{ID: "T-013", Title: "Has epic and sprint", Epic: "epic-a", Sprint: "sprint-a", Tags: []string{"api"}, Priority: p2},
	}

	r := &registry.Registry{}
	r.Epics = append(r.Epics, registry.Epic{ID: "epic-a", Title: "Alpha Epic", Status: "active"})
	r.Sprints = append(r.Sprints, registry.Sprint{ID: "sprint-a", Title: "Sprint 1", Epic: "epic-a", Status: "active"})

	groups := buildGroups(items, r)

	// Epic items should come first
	if groups[0].EpicID != "epic-a" {
		t.Errorf("first group should be epic-a, got %q", groups[0].EpicID)
	}
	// Uncategorized should be last
	lastGroup := groups[len(groups)-1]
	if lastGroup.Tag != "uncategorized" {
		t.Errorf("last group should be uncategorized, got %q", lastGroup.Tag)
	}
}

func TestIndexEmptyStore(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	code := Index(s, cfg)
	if code != 0 {
		t.Fatalf("Index returned %d, want 0", code)
	}

	content, _ := os.ReadFile(indexPath)
	if !strings.Contains(string(content), "## Active Work") {
		t.Error("missing Active Work section in empty index")
	}
}

func TestPriorityOf(t *testing.T) {
	p3 := intPtr(3)
	item1 := &model.Item{Priority: p3}
	if priorityOf(item1) != 3 {
		t.Errorf("expected 3, got %d", priorityOf(item1))
	}

	item2 := &model.Item{}
	if priorityOf(item2) != 2 {
		t.Errorf("expected default 2, got %d", priorityOf(item2))
	}
}

func TestCapitalize(t *testing.T) {
	if capitalize("task") != "Task" {
		t.Errorf("got %q", capitalize("task"))
	}
	if capitalize("") != "" {
		t.Errorf("got %q for empty", capitalize(""))
	}
}

func intPtr(i int) *int {
	return &i
}

func TestSprintListWithSprints(t *testing.T) {
	_, cfg := setupTestEnv(t)
	EpicCreate(cfg, "Parent")
	r, _ := registry.Load(cfg.EpicsPath())
	SprintCreate(cfg, r.Epics[0].ID, "Sprint 1")

	code := SprintList(cfg, "")
	if code != 0 {
		t.Errorf("SprintList returned %d, want 0", code)
	}
}

func TestWriteQueuedTasksWithSprint(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	r := &registry.Registry{}
	e := r.AddEpic("Test Epic")
	sp, _ := r.AddSprint(e.ID, "Sprint 1")
	os.MkdirAll(filepath.Join(root, ".as"), 0755)
	r.Save(filepath.Join(root, ".as", "epics.yaml"))

	writeFile2(t, filepath.Join(root, "tasks", "T-001-sprint-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Sprint task
epic: `+e.ID+`
sprint: `+sp.ID+`
tags: [billing]
depends_on:
- []
next_actions:
- []
`)

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	code := Index(s, cfg)
	if code != 0 {
		t.Fatalf("Index returned %d, want 0", code)
	}

	content, _ := os.ReadFile(indexPath)
	str := string(content)

	if !strings.Contains(str, "Sprint:") {
		t.Error("missing Sprint header in index")
	}
}

func writeFile2(t *testing.T, path, content string) {
	t.Helper()
	os.WriteFile(path, []byte(content), 0644)
}
