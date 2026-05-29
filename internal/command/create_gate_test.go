package command

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/model"
)

// substantiveSBAR is a valid SBAR with all fields above their minimum floors.
var substantiveSBAR = struct {
	situation, background, assessment, recommendation string
}{
	situation:      "POST /patients returns 500 for new practice sign-ups when RLS is active.",
	background:     "Tenant ID not propagated to DB connection in internal/db/pool.go SetContext(); pool.go line 42 missing ctx.Value(tenantKey) call.",
	assessment:     "RLS policy rejects queries running as `app` user with no tenant context — app/rls_policy.sql line 18.",
	recommendation: "Add SetContext(ctx, tenantID) call in internal/db/pool.go:42 before s.querier(ctx) invocation.",
}

// makeSemanticEngine creates a RunEngine whose RunClaude always returns a fixed
// sbarVerdict JSON. The verdict and findings simulate the Layer-2+3 validator.
func makeSemanticEngine(verdict string, findings []string) RunEngine {
	data, _ := json.Marshal(struct {
		Verdict  string   `json:"verdict"`
		Findings []string `json:"findings"`
	}{verdict, findings})
	return RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return data, 0, nil
		},
	}
}

// TestCreateLayer1RejectsScaffoldSBAR: EnforceGate=true with empty Situation
// must exit 1 without creating an item.
func TestCreateLayer1RejectsScaffoldSBAR(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	code := Create(s, cfg, "issue", "Test issue scaffold rejection", CreateOpts{
		Priority:    2,
		EnforceGate: true,
		Situation:   "",   // empty — Layer 1 must catch this
		Background:  "",
		Assessment:  "",
		Recommendation: "",
	})
	if code != 1 {
		t.Errorf("expected exit 1 for empty SBAR with EnforceGate, got %d", code)
	}

	// Verify item was NOT created.
	_, ok := s.Get("I-002")
	if ok {
		t.Error("item should not have been created when SBAR validation fails")
	}
}

// TestCreateLayer1RejectsShortSBAR: EnforceGate=true with situation "Hi" (too short)
// must exit 1.
func TestCreateLayer1RejectsShortSBAR(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	code := Create(s, cfg, "task", "Test short SBAR rejection", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		Situation:      "Hi",                           // 2 chars, under 20
		Background:     "Some background context here for the test.",
		Assessment:     "Assessment of the situation here for test.",
		Recommendation: "Recommendation for fixing the issue here.",
	})
	if code != 1 {
		t.Errorf("expected exit 1 for short situation with EnforceGate, got %d", code)
	}
}

// TestCreateLayer1AcceptsSubstantiveSBAR: valid SBAR with EnforceGate passes Layer 1
// and creates the item with the supplied SBAR content (not scaffold).
func TestCreateLayer1AcceptsSubstantiveSBAR(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	code := Create(s, cfg, "task", "Test substantive SBAR accepted", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		NoValidate:     true, // skip LLM layer so test is deterministic
		Situation:      substantiveSBAR.situation,
		Background:     substantiveSBAR.background,
		Assessment:     substantiveSBAR.assessment,
		Recommendation: substantiveSBAR.recommendation,
	})
	if code != 0 {
		t.Fatalf("expected exit 0 for substantive SBAR with EnforceGate, got %d", code)
	}

	item, ok := s.Get("T-005")
	if !ok {
		t.Fatal("item T-005 should have been created")
	}
	if item.SBAR.Situation != substantiveSBAR.situation {
		t.Errorf("SBAR.Situation = %q, want %q", item.SBAR.Situation, substantiveSBAR.situation)
	}
	if item.SBAR.Background != substantiveSBAR.background {
		t.Errorf("SBAR.Background = %q, want %q", item.SBAR.Background, substantiveSBAR.background)
	}
}

// TestCreateWritesRealSBARFromFlags: verify the doc written to disk contains
// the real SBAR content and not the scaffold placeholders.
func TestCreateWritesRealSBARFromFlags(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	code := Create(s, cfg, "issue", "Test real SBAR in file", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		NoValidate:     true,
		Situation:      substantiveSBAR.situation,
		Background:     substantiveSBAR.background,
		Assessment:     substantiveSBAR.assessment,
		Recommendation: substantiveSBAR.recommendation,
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	item, ok := s.Get("I-002")
	if !ok {
		t.Fatal("item I-002 should have been created")
	}
	raw := item.Doc.String()

	// Verify real content is present.
	if !strings.Contains(raw, substantiveSBAR.situation) {
		t.Errorf("doc missing situation content; doc:\n%s", raw)
	}
	// Verify scaffold placeholders are NOT present.
	if strings.Contains(raw, model.SBARPlaceholders["situation"]) {
		t.Errorf("doc still contains scaffold situation placeholder; doc:\n%s", raw)
	}
	if strings.Contains(raw, model.SBARPlaceholders["background"]) {
		t.Errorf("doc still contains scaffold background placeholder; doc:\n%s", raw)
	}
}

// TestCreateNoValidateSkipsSemanticButRunsLayer1: --no-validate must still
// run Layer 1 (short SBAR fails) but not call LLM.
func TestCreateNoValidateSkipsSemanticButRunsLayer1(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	callCount := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			callCount++
			return []byte(`{"verdict":"PASS","findings":[]}`), 0, nil
		},
	}

	code := Create(s, cfg, "task", "Test no-validate still hits Layer 1", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		NoValidate:     true, // skip LLM
		Situation:      "Too short",   // 9 chars, under 20 — Layer 1 must catch
		Background:     "Some background context here for the test.",
		Assessment:     "Assessment of the situation here for test.",
		Recommendation: "Recommendation for fixing the issue here.",
		Engine:         engine,
	})
	if code != 1 {
		t.Errorf("expected exit 1 (Layer 1 failure despite NoValidate), got %d", code)
	}
	if callCount != 0 {
		t.Errorf("LLM should not have been called with NoValidate, but was called %d times", callCount)
	}
}

// TestCreateSemanticValidationFailBlocks: fake engine returning FAIL exits 1
// and does not create the item.
func TestCreateSemanticValidationFailBlocks(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	engine := makeSemanticEngine("FAIL", []string{
		"situation: vague — no file paths or function names",
		"assessment: missing root cause analysis",
	})

	code := Create(s, cfg, "task", "Test semantic FAIL blocks create", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		NoValidate:     false, // run LLM
		Situation:      substantiveSBAR.situation,
		Background:     substantiveSBAR.background,
		Assessment:     substantiveSBAR.assessment,
		Recommendation: substantiveSBAR.recommendation,
		Engine:         engine,
	})
	if code != 1 {
		t.Errorf("expected exit 1 for semantic FAIL, got %d", code)
	}

	// Verify item was NOT created.
	items := s.All()
	for _, it := range items {
		if it.Title == "Test semantic FAIL blocks create" {
			t.Error("item should not have been created when semantic validation fails")
		}
	}
}

// TestCreateSemanticValidationWarnAnnotates: WARN verdict exits 0, item created,
// warnings printed to stderr.
func TestCreateSemanticValidationWarnAnnotates(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	engine := makeSemanticEngine("WARN", []string{"recommendation could be more specific"})

	// Redirect stderr to capture warnings.
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	code := Create(s, cfg, "task", "Test semantic WARN creates item", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		NoValidate:     false,
		Situation:      substantiveSBAR.situation,
		Background:     substantiveSBAR.background,
		Assessment:     substantiveSBAR.assessment,
		Recommendation: substantiveSBAR.recommendation,
		Engine:         engine,
	})

	w.Close()
	os.Stderr = origStderr
	var stderrBuf strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			stderrBuf.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}

	if code != 0 {
		t.Errorf("expected exit 0 for semantic WARN, got %d", code)
	}

	// Verify item was created.
	found := false
	for _, it := range s.All() {
		if it.Title == "Test semantic WARN creates item" {
			found = true
			break
		}
	}
	if !found {
		t.Error("item should have been created despite WARN verdict")
	}

	// Verify warnings were printed.
	stderrStr := stderrBuf.String()
	if !strings.Contains(stderrStr, "warning") {
		t.Errorf("expected warning output for WARN verdict, got stderr: %q", stderrStr)
	}
}

// TestCreateSemanticValidationPass: PASS verdict exits 0 and creates item.
func TestCreateSemanticValidationPass(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	engine := makeSemanticEngine("PASS", nil)

	code := Create(s, cfg, "task", "Test semantic PASS creates item", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		NoValidate:     false,
		Situation:      substantiveSBAR.situation,
		Background:     substantiveSBAR.background,
		Assessment:     substantiveSBAR.assessment,
		Recommendation: substantiveSBAR.recommendation,
		Engine:         engine,
	})
	if code != 0 {
		t.Errorf("expected exit 0 for semantic PASS, got %d", code)
	}

	found := false
	for _, it := range s.All() {
		if it.Title == "Test semantic PASS creates item" {
			found = true
			break
		}
	}
	if !found {
		t.Error("item should have been created on PASS verdict")
	}
}

// TestCreateGateOffForIdeasAndPromotions: EnforceGate=true on type=idea must
// not fire the SBAR gate (ideas don't carry SBAR).
func TestCreateGateOffForIdeasAndPromotions(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	// Ideas have no SBAR requirement — gate must not fire even with EnforceGate.
	code := Create(s, cfg, "idea", "Test gate off for ideas", CreateOpts{
		Priority:    3,
		EnforceGate: true,
		// No SBAR fields supplied at all.
	})
	if code != 0 {
		t.Errorf("expected exit 0 for idea with EnforceGate (no SBAR requirement), got %d", code)
	}
}

// TestCreateGateOffWhenEnforceGateFalse: EnforceGate=false (in-process caller)
// skips all SBAR validation even for tasks.
func TestCreateGateOffWhenEnforceGateFalse(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	// Zero CreateOpts — no SBAR, no EnforceGate. Must succeed (legacy path).
	code := Create(s, cfg, "task", "Test gate off when EnforceGate false", CreateOpts{
		Priority: 2,
		// EnforceGate: false (zero value)
	})
	if code != 0 {
		t.Errorf("expected exit 0 when EnforceGate is false, got %d", code)
	}
}

// TestCreateSemanticValidationEngineErrorDegrades: a subprocess error must
// degrade gracefully — item is created, not blocked.
func TestCreateSemanticValidationEngineErrorDegrades(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	errEngine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return nil, 1, fmt.Errorf("timeout: subprocess failed")
		},
	}

	code := Create(s, cfg, "task", "Test engine error degrades", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		NoValidate:     false,
		Situation:      substantiveSBAR.situation,
		Background:     substantiveSBAR.background,
		Assessment:     substantiveSBAR.assessment,
		Recommendation: substantiveSBAR.recommendation,
		Engine:         errEngine,
	})
	if code != 0 {
		t.Errorf("expected exit 0 when engine errors (graceful degrade), got %d", code)
	}

	found := false
	for _, it := range s.All() {
		if it.Title == "Test engine error degrades" {
			found = true
			break
		}
	}
	if !found {
		t.Error("item should have been created when engine degrades")
	}
}

// TestCreateIdeaPromotionSkipSBARGate: EnforceGate=true on type=idea must skip
// all SBAR validation — ideas/promotions have no SBAR requirement.
func TestCreateIdeaPromotionSkipSBARGate(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	code := Create(s, cfg, "idea", "Test idea promotion skips SBAR gate", CreateOpts{
		Priority:    3,
		EnforceGate: true,
		// No SBAR fields — gate must be skipped for ideas.
	})
	if code != 0 {
		t.Errorf("expected exit 0 for idea type with EnforceGate (gate must be skipped), got %d", code)
	}
}

// TestCreateEnforceGateRunsSemanticValidationOnce: with EnforceGate=true the
// Layer-2+3 semantic validator is called exactly once per create invocation.
func TestCreateEnforceGateRunsSemanticValidationOnce(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	calls := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			calls++
			data, _ := json.Marshal(struct {
				Verdict  string   `json:"verdict"`
				Findings []string `json:"findings"`
			}{"PASS", nil})
			return data, 0, nil
		},
	}

	code := Create(s, cfg, "task", "Test gate runs semantic once", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		NoValidate:     false,
		Situation:      substantiveSBAR.situation,
		Background:     substantiveSBAR.background,
		Assessment:     substantiveSBAR.assessment,
		Recommendation: substantiveSBAR.recommendation,
		Engine:         engine,
	})
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	if calls != 1 {
		t.Errorf("expected semantic validation to run exactly once, got %d calls", calls)
	}
}
