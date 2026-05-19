package changelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestClassifyKind pins the Op→Kind derivation table. The exec set is
// explicit and small; everything else (including unknown/new Ops) is a
// transition; a decision is never derived from an Op.
func TestClassifyKind(t *testing.T) {
	tests := []struct {
		op   string
		want Kind
	}{
		// explicit exec ops
		{"commit", KindExec},
		{"deploy_checked", KindExec},
		{"pr_recorded", KindExec},
		// test_* prefix → exec
		{"test_executed", KindExec},
		{"test_failed", KindExec},
		{"test_recorded", KindExec},
		{"test_skipped", KindExec},
		// pipeline *_completed suffix → exec
		{"merge_completed", KindExec},
		{"deploy_completed", KindExec},
		{"smoke_completed", KindExec},
		// declarative lifecycle → transition
		{"create", KindTransition},
		{"update", KindTransition},
		{"start", KindTransition},
		{"start_force", KindTransition},
		{"close", KindTransition},
		{"tag_add", KindTransition},
		{"dep_rm", KindTransition},
		{"snapshot", KindTransition},
		{"plan_approve", KindTransition},
		{"release", KindTransition},
		{"split", KindTransition},
		{"awaiting_decision", KindTransition},
		// unknown / brand-new Op safely defaults to transition,
		// never to decision (decisions are only ever explicit)
		{"some_future_op", KindTransition},
		{"", KindTransition},
	}
	for _, tt := range tests {
		if got := classifyKind(tt.op); got != tt.want {
			t.Errorf("classifyKind(%q) = %q, want %q", tt.op, got, tt.want)
		}
		if classifyKind(tt.op) == KindDecision {
			t.Errorf("classifyKind(%q) derived a decision — must never happen", tt.op)
		}
	}
}

// TestEffectiveKind: an explicit Kind is authoritative; an empty Kind is
// derived from Op so the typed view is consistent across the pre/post-I-679
// boundary.
func TestEffectiveKind(t *testing.T) {
	// explicit Kind wins, even when Op would classify differently
	e := Entry{Op: "create", Kind: KindDecision}
	if got := e.EffectiveKind(); got != KindDecision {
		t.Errorf("explicit Kind not preserved: got %q, want decision", got)
	}
	// empty Kind on a legacy exec op derives exec
	legacyCommit := Entry{Op: "commit"}
	if got := legacyCommit.EffectiveKind(); got != KindExec {
		t.Errorf("legacy commit EffectiveKind = %q, want exec", got)
	}
	// empty Kind on a legacy declarative op derives transition
	legacyCreate := Entry{Op: "create"}
	if got := legacyCreate.EffectiveKind(); got != KindTransition {
		t.Errorf("legacy create EffectiveKind = %q, want transition", got)
	}
}

// TestAppendStampsKind: Append captures Kind as a write side-effect — no
// caller opted in — while preserving an explicitly-set Kind.
func TestAppendStampsKind(t *testing.T) {
	cfg := testCfg(t)

	if err := Append(cfg, "T-001", Entry{Op: "create", Timestamp: "2026-05-18T10:00:00-06:00"}); err != nil {
		t.Fatalf("Append create: %v", err)
	}
	if err := Append(cfg, "T-001", Entry{Op: "commit", Timestamp: "2026-05-18T11:00:00-06:00"}); err != nil {
		t.Fatalf("Append commit: %v", err)
	}
	// explicit decision entry — Append must not overwrite it
	if err := Append(cfg, "T-001", Entry{
		Op: "update", Timestamp: "2026-05-18T12:00:00-06:00",
		Kind: KindDecision, Source: SourceStructured, Reason: "chose Postgres over SQLite",
	}); err != nil {
		t.Fatalf("Append decision: %v", err)
	}

	entries, err := Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries[0].Kind != KindTransition {
		t.Errorf("create entry Kind = %q, want transition", entries[0].Kind)
	}
	if entries[1].Kind != KindExec {
		t.Errorf("commit entry Kind = %q, want exec", entries[1].Kind)
	}
	if entries[2].Kind != KindDecision || entries[2].Source != SourceStructured {
		t.Errorf("explicit decision not preserved: Kind=%q Source=%q", entries[2].Kind, entries[2].Source)
	}
}

// TestReadNormalizesLegacyEntries is the load-bearing backward-compat test:
// raw pre-I-679 JSON lines (no kind/source/confidence fields) must read back
// with a stable, correctly-typed Kind and never crash or be dropped.
func TestReadNormalizesLegacyEntries(t *testing.T) {
	cfg := testCfg(t)
	path := filepath.Join(cfg.ChangelogDir(), "T-LEGACY.log")
	// Two genuine pre-I-679 lines: a declarative one and an exec one,
	// neither carrying the new fields.
	legacy := `{"timestamp":"2026-01-01T09:00:00-06:00","agent":"agent-a","op":"create","field":"status","new":"queued"}
{"timestamp":"2026-01-01T10:00:00-06:00","agent":"agent-a","op":"commit","new":"abc1234"}
`
	if err := os.WriteFile(path, []byte(legacy), 0644); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}

	entries, err := Read(cfg, "T-LEGACY")
	if err != nil {
		t.Fatalf("Read legacy: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Kind != KindTransition {
		t.Errorf("legacy create normalized Kind = %q, want transition", entries[0].Kind)
	}
	if entries[1].Kind != KindExec {
		t.Errorf("legacy commit normalized Kind = %q, want exec", entries[1].Kind)
	}
	// Provenance fields stay zero-valued for non-decision history.
	if entries[1].Source != "" || entries[1].Confidence != 0 {
		t.Errorf("legacy entry gained spurious provenance: Source=%q Confidence=%v", entries[1].Source, entries[1].Confidence)
	}
}

// TestDecisionProvenanceRoundTrip: structured vs extracted decision entries
// serialize/deserialize losslessly, and omitempty keeps non-decision rows clean.
func TestDecisionProvenanceRoundTrip(t *testing.T) {
	cfg := testCfg(t)

	structured := Entry{
		Op: "update", Timestamp: "2026-05-18T13:00:00-06:00",
		Kind: KindDecision, Source: SourceStructured,
		Reason: "operator chose parallel over sequence (AskUserQuestion)",
	}
	extracted := Entry{
		Op: "update", Timestamp: "2026-05-18T14:00:00-06:00",
		Kind: KindDecision, Source: SourceExtracted, Confidence: 0.42,
		Reason: "rejected: rebuild storage subsystem — substrate already exists",
	}
	if err := Append(cfg, "I-679", structured); err != nil {
		t.Fatalf("Append structured: %v", err)
	}
	if err := Append(cfg, "I-679", extracted); err != nil {
		t.Fatalf("Append extracted: %v", err)
	}

	entries, err := Read(cfg, "I-679")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Source != SourceStructured || entries[0].Confidence != 0 {
		t.Errorf("structured round-trip wrong: Source=%q Confidence=%v", entries[0].Source, entries[0].Confidence)
	}
	if entries[1].Source != SourceExtracted || entries[1].Confidence != 0.42 {
		t.Errorf("extracted round-trip wrong: Source=%q Confidence=%v", entries[1].Source, entries[1].Confidence)
	}

	// omitempty: a transition row must not emit kind=transition noise...
	// (kept human-diff-clean) — Append stamps it but JSON stays minimal
	// only for the zero-ish fields; Kind is intentionally persisted.
	raw, err := json.Marshal(Entry{Op: "create"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["source"]; ok {
		t.Errorf("non-decision entry leaked a source field: %s", raw)
	}
	if _, ok := m["confidence"]; ok {
		t.Errorf("non-decision entry leaked a confidence field: %s", raw)
	}
}
