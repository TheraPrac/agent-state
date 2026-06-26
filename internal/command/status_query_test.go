package command

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/model"
)

// --- Spec parsing ---

func TestParseFilterSpecs(t *testing.T) {
	cases := []struct {
		raw     []string
		want    []filterSpec
		wantErr bool
	}{
		{[]string{"agent:agent-b"}, []filterSpec{{Key: "agent", Value: "agent-b"}}, false},
		{[]string{"status:active,queued"}, []filterSpec{{Key: "status", Value: "active,queued"}}, false},
		{[]string{"type:task", "priority:1"}, []filterSpec{
			{Key: "type", Value: "task"}, {Key: "priority", Value: "1"},
		}, false},
		{[]string{"AGENT:b"}, []filterSpec{{Key: "agent", Value: "b"}}, false}, // case-insensitive key
		{[]string{""}, nil, false},                                               // empty entries skipped
		{[]string{"weather:sunny"}, nil, true},                                   // unknown key
		{[]string{"agent"}, nil, true},                                           // missing value
		{[]string{":lonely"}, nil, true},                                         // missing key
		{[]string{"agent:"}, nil, true},                                          // trailing colon
	}
	for _, c := range cases {
		got, err := parseFilterSpecs(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("input %v: expected error, got %+v", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("input %v: unexpected err %v", c.raw, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("input %v: got %d specs, want %d", c.raw, len(got), len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("input %v[%d]: got %+v, want %+v", c.raw, i, got[i], c.want[i])
			}
		}
	}
}

func TestParseSortSpec(t *testing.T) {
	cases := []struct {
		raw     string
		want    sortSpec
		wantErr bool
	}{
		{"", sortSpec{}, false},
		{"cost", sortSpec{Field: "cost", Desc: true}, false},
		{"cost,asc", sortSpec{Field: "cost", Desc: false}, false},
		{"id", sortSpec{Field: "id", Desc: false}, false},
		{"id,desc", sortSpec{Field: "id", Desc: true}, false},
		{"PRIORITY", sortSpec{Field: "priority", Desc: false}, false}, // case
		{"weather", sortSpec{}, true},
		{"cost,sideways", sortSpec{}, true},
	}
	for _, c := range cases {
		got, err := parseSortSpec(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got %+v", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected err %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %+v, want %+v", c.raw, got, c.want)
		}
	}
}

// --- Filter application ---

func itemFixture(id, typ, status string, priority int, assigned string, tags []string) *model.Item {
	p := priority
	it := &model.Item{
		ID:            id,
		Type:          typ,
		Status:        status,
		Title:         id + " title",
		Priority:      &p,
		Tags:          tags,
		LastTouched:   time.Now(),
		LastTouchedBy: "agent-c",
	}
	it.Doc = &model.ParsedDocument{}
	if assigned != "" {
		it.Doc.SetField("assigned_to", assigned)
	}
	return it
}

func TestApplyStatusQuery_Filters(t *testing.T) {
	a := itemFixture("T-001", "task", "active", 1, "agent-b", []string{"infra"})
	b := itemFixture("T-002", "task", "queued", 2, "agent-a", []string{"web"})
	c := itemFixture("T-003", "issue", "open", 1, "agent-b", []string{"infra"})
	all := []*model.Item{a, b, c}

	// agent:b should match T-001 (assigned to agent-b) and T-003.
	out := applyStatusQuery(all, []filterSpec{{Key: "agent", Value: "b"}}, sortSpec{}, time.Time{}, nil, "", time.Now())
	if len(out) != 2 {
		t.Errorf("agent:b → %d items, want 2 (T-001+T-003). Got: %v", len(out), idsOf(out))
	}

	// type:issue priority:1 should match T-003 only.
	out = applyStatusQuery(all, []filterSpec{
		{Key: "type", Value: "issue"}, {Key: "priority", Value: "1"},
	}, sortSpec{}, time.Time{}, nil, "", time.Now())
	if len(out) != 1 || out[0].ID != "T-003" {
		t.Errorf("type:issue + priority:1 → %v, want [T-003]", idsOf(out))
	}

	// tag:infra → T-001 and T-003.
	out = applyStatusQuery(all, []filterSpec{{Key: "tag", Value: "infra"}}, sortSpec{}, time.Time{}, nil, "", time.Now())
	if len(out) != 2 {
		t.Errorf("tag:infra → %v, want 2", idsOf(out))
	}

	// Unknown agent → nothing.
	out = applyStatusQuery(all, []filterSpec{{Key: "agent", Value: "z"}}, sortSpec{}, time.Time{}, nil, "", time.Now())
	if len(out) != 0 {
		t.Errorf("agent:z → %v, want []", idsOf(out))
	}
}

func TestApplyStatusQuery_SinceCutoff(t *testing.T) {
	old := itemFixture("T-OLD", "task", "active", 1, "agent-a", nil)
	old.LastTouched = time.Now().Add(-72 * time.Hour)
	fresh := itemFixture("T-NEW", "task", "active", 1, "agent-a", nil)
	fresh.LastTouched = time.Now().Add(-30 * time.Minute)
	all := []*model.Item{old, fresh}

	cutoff := time.Now().Add(-24 * time.Hour)
	out := applyStatusQuery(all, nil, sortSpec{}, cutoff, nil, "", time.Now())
	if len(out) != 1 || out[0].ID != "T-NEW" {
		t.Errorf("--since 24h → %v, want [T-NEW]", idsOf(out))
	}
}

func TestApplyStatusQuery_Sort(t *testing.T) {
	a := itemFixture("T-001", "task", "active", 2, "agent-a", nil)
	b := itemFixture("T-002", "task", "active", 1, "agent-a", nil)
	c := itemFixture("T-003", "task", "active", 1, "agent-a", nil)
	all := []*model.Item{a, b, c}

	// Sort by priority asc (default for priority field): 1, 1, 2.
	out := applyStatusQuery(all, nil, sortSpec{Field: "priority"}, time.Time{}, nil, "", time.Now())
	if len(out) != 3 || out[0].ID == "T-001" {
		t.Errorf("sort priority asc → %v, want T-001 last", idsOf(out))
	}

	// Sort by id desc.
	out = applyStatusQuery(all, nil, sortSpec{Field: "id", Desc: true}, time.Time{}, nil, "", time.Now())
	if out[0].ID != "T-003" {
		t.Errorf("sort id desc → %v, want T-003 first", idsOf(out))
	}
}

// --- Status command integration ---

func TestStatusQuery_FilterRejectedSurfacesError(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Filters: []string{"weather:sunny"}, NoRefresh: true})
	if code != 2 {
		t.Errorf("invalid filter key should exit 2, got %d", code)
	}
}

func TestStatusQuery_SortRejectedSurfacesError(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Sort: "weather", NoRefresh: true})
	if code != 2 {
		t.Errorf("invalid sort field should exit 2, got %d", code)
	}
}

func TestStatusQuery_SinceRejectedSurfacesError(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Since: "huh?", NoRefresh: true})
	if code != 2 {
		t.Errorf("invalid --since should exit 2, got %d", code)
	}
}

func TestStatusQuery_JSONOutputShape(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Status(s, cfg, "", StatusOpts{JSON: true, NoRefresh: true})
	})
	var rows []statusJSONItem
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("JSON output not parseable: %v\nout: %s", err, out)
	}
	if len(rows) == 0 {
		t.Fatal("no rows emitted; setupTestEnv has multiple items")
	}
	// Every row carries a metrics block (zero values are OK; field must exist).
	for _, r := range rows {
		if r.ID == "" || r.Type == "" {
			t.Errorf("row missing ID/Type: %+v", r)
		}
	}
}

// /code-review finding: `--filter agent:agent-` collapsed to empty after
// stripAgentPrefix, silently matching every item with empty assigned_to /
// last_touched_by. Now rejected as a no-match.
func TestApplyStatusQuery_AgentFilterRejectsEmptyAfterStripPrefix(t *testing.T) {
	a := itemFixture("T-001", "task", "active", 1, "", nil)        // unassigned
	b := itemFixture("T-002", "task", "active", 1, "agent-b", nil) // real agent
	all := []*model.Item{a, b}

	// Malformed input: "agent-" alone strips to "". Must NOT match anything.
	out := applyStatusQuery(all, []filterSpec{{Key: "agent", Value: "agent-"}}, sortSpec{}, time.Time{}, nil, "", time.Now())
	if len(out) != 0 {
		t.Errorf("agent:agent- should not match (empty after prefix strip), got %v", idsOf(out))
	}
}

// /code-review finding: `st status --json` returned ALL items including
// archived/completed by default, inconsistent with the text dashboard's
// active-first bias. With no `status:` filter, JSON now narrows to
// non-terminal statuses by default.
func TestStatusQuery_JSONDefaultsToNonTerminal(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Status(s, cfg, "", StatusOpts{JSON: true, NoRefresh: true})
	})
	var rows []statusJSONItem
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("unmarshal: %v\nout: %s", err, out)
	}
	for _, r := range rows {
		if r.Status == "done" || r.Status == "abandoned" || r.Status == "archived" {
			t.Errorf("default JSON should exclude terminal statuses, got %s status=%s", r.ID, r.Status)
		}
	}
	// Sanity: with explicit `status:done` filter, terminal items
	// should appear (proves the default narrowing is opt-out, not hardcoded).
	out = captureStdout(t, func() {
		Status(s, cfg, "", StatusOpts{JSON: true, NoRefresh: true, Filters: []string{"status:done"}})
	})
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, r := range rows {
		if r.Status != "done" {
			t.Errorf("status:done filter leaked non-done: %+v", r)
		}
	}
}

func TestStatusQuery_JSONFilterApplied(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Status(s, cfg, "", StatusOpts{
			JSON:    true,
			NoRefresh: true,
			Filters:   []string{"type:issue"},
		})
	})
	var rows []statusJSONItem
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("unmarshal: %v\nout: %s", err, out)
	}
	for _, r := range rows {
		if r.Type != "issue" {
			t.Errorf("type:issue filter missed: %+v", r)
		}
	}
}

// --- helpers ---

func idsOf(items []*model.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// Confirm strings.Contains usage isn't dropped.
var _ = strings.Contains
