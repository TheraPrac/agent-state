package quality

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// writeActiveEnvsFile is a test helper that drops content at <dir>/active-envs.yaml
// and returns the path so each test can run against a tempdir of its own.
func writeActiveEnvsFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "active-envs.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestParseActiveEnvs_WellFormed covers the canonical shape:
// declared_by, declared_at, last_reconciled, active_envs list,
// torn_down list, with interleaved comments + blank lines.
func TestParseActiveEnvs_WellFormed(t *testing.T) {
	path := writeActiveEnvsFile(t, t.TempDir(), `# header comment
declared_by: jfinlinson
declared_at: 2026-05-20T19:30:00-06:00
last_reconciled: 2026-05-20T19:32:00-06:00

active_envs:
  - demo

torn_down:
  - prod   # destroyed 2026-05-20
  - dev
  - test
`)
	ae, err := ParseActiveEnvs(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ae.DeclaredBy != "jfinlinson" {
		t.Errorf("DeclaredBy = %q, want jfinlinson", ae.DeclaredBy)
	}
	wantDeclared := time.Date(2026, 5, 20, 19, 30, 0, 0, time.FixedZone("", -6*60*60))
	if !ae.DeclaredAt.Equal(wantDeclared) {
		t.Errorf("DeclaredAt = %v, want %v", ae.DeclaredAt, wantDeclared)
	}
	if !reflect.DeepEqual(ae.Active, []string{"demo"}) {
		t.Errorf("Active = %v, want [demo]", ae.Active)
	}
	if !reflect.DeepEqual(ae.TornDown, []string{"prod", "dev", "test"}) {
		t.Errorf("TornDown = %v, want [prod dev test]", ae.TornDown)
	}
}

// TestParseActiveEnvs_MissingFile returns the os.IsNotExist error.
func TestParseActiveEnvs_MissingFile(t *testing.T) {
	_, err := ParseActiveEnvs(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error on missing file, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist error, got %v", err)
	}
}

// TestParseActiveEnvs_EmptyActiveList: `active_envs: []` inline form
// produces a zero-length (but non-nil) slice. Used when the operator
// declares everything torn down.
func TestParseActiveEnvs_EmptyActiveList(t *testing.T) {
	path := writeActiveEnvsFile(t, t.TempDir(), `declared_by: jfinlinson
declared_at: 2026-05-21T08:00:00-06:00
active_envs: []
torn_down:
  - demo
`)
	ae, err := ParseActiveEnvs(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ae.Active) != 0 {
		t.Errorf("Active = %v, want empty", ae.Active)
	}
	if !reflect.DeepEqual(ae.TornDown, []string{"demo"}) {
		t.Errorf("TornDown = %v, want [demo]", ae.TornDown)
	}
}

// TestParseActiveEnvs_UnparseableTimestamp: a malformed declared_at
// produces ae.DeclaredAt == zero time with no parse error returned —
// the validator surfaces it.
func TestParseActiveEnvs_UnparseableTimestamp(t *testing.T) {
	path := writeActiveEnvsFile(t, t.TempDir(), `declared_by: jfinlinson
declared_at: not-a-timestamp
active_envs:
  - demo
`)
	ae, err := ParseActiveEnvs(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ae.DeclaredAt.IsZero() {
		t.Errorf("DeclaredAt = %v, want zero time on unparseable input", ae.DeclaredAt)
	}
}

// TestParseActiveEnvs_QuotedValues: operators sometimes quote
// strings; the parser should strip surrounding double/single quotes.
func TestParseActiveEnvs_QuotedValues(t *testing.T) {
	path := writeActiveEnvsFile(t, t.TempDir(), `declared_by: "jfinlinson"
active_envs:
  - "demo"
  - 'staging'
`)
	ae, err := ParseActiveEnvs(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ae.DeclaredBy != "jfinlinson" {
		t.Errorf("DeclaredBy quote-strip failed: %q", ae.DeclaredBy)
	}
	if !reflect.DeepEqual(ae.Active, []string{"demo", "staging"}) {
		t.Errorf("Active = %v, want [demo staging]", ae.Active)
	}
}

// TestValidateActiveEnvs_NilInput surfaces the "no declaration found"
// finding as a top-level warning.
func TestValidateActiveEnvs_NilInput(t *testing.T) {
	vios := ValidateActiveEnvs(nil, ActiveEnvsValidateOpts{})
	if len(vios) != 1 {
		t.Fatalf("expected 1 violation on nil input; got %d (%v)", len(vios), vios)
	}
	if vios[0].Severity != SeverityWarn {
		t.Errorf("nil-input finding should be Warn, got %v", vios[0].Severity)
	}
}

// TestValidateActiveEnvs_FreshDeclarationNoFindings: an active,
// recently-declared envset within the staleness window emits no
// findings.
func TestValidateActiveEnvs_FreshDeclarationNoFindings(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	ae := &ActiveEnvs{
		Active:     []string{"demo"},
		DeclaredAt: now.Add(-1 * time.Hour),
	}
	vios := ValidateActiveEnvs(ae, ActiveEnvsValidateOpts{Now: func() time.Time { return now }})
	if len(vios) != 0 {
		t.Errorf("fresh declaration should have no findings; got %v", vios)
	}
}

// TestValidateActiveEnvs_StaleDeclaration: a declared_at older than
// StaleAfter (default 14 days) surfaces the freshness warning.
func TestValidateActiveEnvs_StaleDeclaration(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	ae := &ActiveEnvs{
		Active:     []string{"demo"},
		DeclaredAt: now.Add(-15 * 24 * time.Hour),
	}
	vios := ValidateActiveEnvs(ae, ActiveEnvsValidateOpts{Now: func() time.Time { return now }})
	foundStale := false
	for _, v := range vios {
		if v.Field == "declared_at" && v.Severity == SeverityWarn {
			foundStale = true
		}
	}
	if !foundStale {
		t.Errorf("expected declared_at staleness warning; got %v", vios)
	}
}

// TestValidateActiveEnvs_FreshnessThresholdExact: exactly the cap
// (14 days) does NOT trigger; one second over does.
func TestValidateActiveEnvs_FreshnessThresholdExact(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	cap := 14 * 24 * time.Hour

	atEdge := &ActiveEnvs{Active: []string{"demo"}, DeclaredAt: now.Add(-cap)}
	vios := ValidateActiveEnvs(atEdge, ActiveEnvsValidateOpts{Now: func() time.Time { return now }})
	for _, v := range vios {
		if v.Field == "declared_at" {
			t.Errorf("declaration at exactly cap should NOT fire; got %v", v)
		}
	}

	overEdge := &ActiveEnvs{Active: []string{"demo"}, DeclaredAt: now.Add(-cap - time.Second)}
	vios = ValidateActiveEnvs(overEdge, ActiveEnvsValidateOpts{Now: func() time.Time { return now }})
	found := false
	for _, v := range vios {
		if v.Field == "declared_at" {
			found = true
		}
	}
	if !found {
		t.Error("declaration one second over the cap should fire the freshness warning")
	}
}

// TestValidateActiveEnvs_EmptyActiveList: empty active_envs is
// surfaced as a warning (operator has nothing declared active).
func TestValidateActiveEnvs_EmptyActiveList(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	ae := &ActiveEnvs{Active: []string{}, DeclaredAt: now}
	vios := ValidateActiveEnvs(ae, ActiveEnvsValidateOpts{Now: func() time.Time { return now }})
	found := false
	for _, v := range vios {
		if v.Field == "active_envs" {
			found = true
		}
	}
	if !found {
		t.Error("empty active_envs should fire a warning")
	}
}

// TestValidateActiveEnvs_CrossListContamination: an env in BOTH
// active_envs and torn_down is a data error worth surfacing.
func TestValidateActiveEnvs_CrossListContamination(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	ae := &ActiveEnvs{
		Active:     []string{"demo", "prod"},
		TornDown:   []string{"prod"},
		DeclaredAt: now,
	}
	vios := ValidateActiveEnvs(ae, ActiveEnvsValidateOpts{Now: func() time.Time { return now }})
	count := 0
	for _, v := range vios {
		if v.Field == "active_envs" {
			count++
		}
	}
	if count == 0 {
		t.Error("cross-list contamination should fire a warning")
	}
}

// TestActiveEnvs_IsActive: lookup helper.
func TestActiveEnvs_IsActive(t *testing.T) {
	ae := &ActiveEnvs{Active: []string{"demo", "staging"}}
	if !ae.IsActive("demo") {
		t.Error("IsActive(demo) should be true")
	}
	if ae.IsActive("prod") {
		t.Error("IsActive(prod) should be false")
	}
	// Nil receiver returns false without panicking.
	var nilAE *ActiveEnvs
	if nilAE.IsActive("demo") {
		t.Error("nil receiver should return false")
	}
}

// TestActiveEnvs_IsTornDown: lookup helper.
func TestActiveEnvs_IsTornDown(t *testing.T) {
	ae := &ActiveEnvs{TornDown: []string{"prod", "dev"}}
	if !ae.IsTornDown("prod") {
		t.Error("IsTornDown(prod) should be true")
	}
	if ae.IsTornDown("demo") {
		t.Error("IsTornDown(demo) should be false")
	}
}
