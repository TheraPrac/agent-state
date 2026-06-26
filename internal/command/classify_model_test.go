package command

import (
	"fmt"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/classify"
	"github.com/theraprac/agent-state/internal/config"
)

// makeStubEngine returns a RunEngine whose RunClaude returns the
// given envelope output. The envelope wraps the model's stdout text
// in the same JSON shape that real claude -p --output-format
// stream-json produces.
func makeStubEngine(result string, exitCode int) RunEngine {
	envelope := fmt.Sprintf(`{"type":"result","result":%q,"is_error":false,"total_cost_usd":0.001,"duration_ms":42}`, result)
	return RunEngine{
		RunClaude: func(cwd string, args, env []string) ([]byte, int, error) {
			return []byte(envelope), exitCode, nil
		},
	}
}

// TestClaudeModel_HappyPath verifies the full chain — prompt build,
// subprocess invocation, envelope parse, verdict parse — produces a
// well-formed Result.
func TestClaudeModel_HappyPath(t *testing.T) {
	cfg := &config.Config{}
	m := &ClaudeModel{
		Cfg:    cfg,
		Engine: makeStubEngine(`{"verdict":"green","reason":"doc-only change, low risk","confidence":0.9}`, 0),
	}

	in := classify.Inputs{
		ItemID:       "T-345",
		Title:        "test",
		Type:         "task",
		TouchedFiles: []string{"docs/README.md"},
	}
	res, err := m.Classify(in)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Verdict != classify.VerdictGreen {
		t.Errorf("verdict = %s; want green", res.Verdict)
	}
	if !strings.Contains(res.Reason, "doc-only") {
		t.Errorf("reason = %q; want substring 'doc-only'", res.Reason)
	}
	if res.ClassifiedBy != "model:claude" {
		t.Errorf("classified_by = %q; want model:claude", res.ClassifiedBy)
	}
	if res.Confidence != 0.9 {
		t.Errorf("confidence = %g; want 0.9", res.Confidence)
	}
}

// TestClaudeModel_NilEngineErrors covers the misconfigured case —
// ClaudeModel constructed without a RunClaude function fails fast
// with a clear error instead of a nil-call panic.
func TestClaudeModel_NilEngineErrors(t *testing.T) {
	m := &ClaudeModel{Cfg: &config.Config{}}
	_, err := m.Classify(classify.Inputs{ItemID: "T-345"})
	if err == nil {
		t.Fatal("expected error for nil engine, got nil")
	}
	if !strings.Contains(err.Error(), "Engine.RunClaude is nil") {
		t.Errorf("err = %v; want substring 'Engine.RunClaude is nil'", err)
	}
}

// TestClaudeModel_SubprocessNonzeroExitErrors covers the case where
// claude -p exits non-zero (auth failure, budget exceeded, etc.).
// The error message should include the captured output so operators
// can see what claude said.
func TestClaudeModel_SubprocessNonzeroExitErrors(t *testing.T) {
	cfg := &config.Config{}
	m := &ClaudeModel{
		Cfg: cfg,
		Engine: RunEngine{
			RunClaude: func(cwd string, args, env []string) ([]byte, int, error) {
				return []byte("ERROR: budget exceeded\n"), 2, nil
			},
		},
	}
	_, err := m.Classify(classify.Inputs{ItemID: "T-345"})
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "claude exit 2") {
		t.Errorf("err = %v; want substring 'claude exit 2'", err)
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("err = %v; want substring 'budget exceeded' (captured output)", err)
	}
}

// TestClaudeModel_SubtypeNonSuccessErrors verifies that an envelope
// with a non-"success" subtype is rejected, matching the behavior of
// proposePlan (run.go:3371) and executeClaude (run.go:2008). Without
// this check a "subtype":"error_during_execution" + is_error=false
// envelope with a partial Result would be silently treated as a
// successful classification.
func TestClaudeModel_SubtypeNonSuccessErrors(t *testing.T) {
	envelope := `{"type":"result","subtype":"error_during_execution","result":"{\"verdict\":\"green\",\"reason\":\"x\",\"confidence\":0.5}","is_error":false}`
	cfg := &config.Config{}
	m := &ClaudeModel{
		Cfg: cfg,
		Engine: RunEngine{
			RunClaude: func(cwd string, args, env []string) ([]byte, int, error) {
				return []byte(envelope), 0, nil
			},
		},
	}
	_, err := m.Classify(classify.Inputs{ItemID: "T-345"})
	if err == nil {
		t.Fatal("expected error for non-success subtype, got nil")
	}
	if !strings.Contains(err.Error(), "subtype") {
		t.Errorf("err = %v; want substring 'subtype'", err)
	}
}

// TestClaudeModel_EnvelopeErrorPropagates verifies that claude
// reporting is_error=true (a model-level failure, distinct from a
// subprocess crash) surfaces as an error rather than a phantom
// success.
func TestClaudeModel_EnvelopeErrorPropagates(t *testing.T) {
	envelope := `{"type":"result","result":"","is_error":true,"errors":["context window exceeded"]}`
	cfg := &config.Config{}
	m := &ClaudeModel{
		Cfg: cfg,
		Engine: RunEngine{
			RunClaude: func(cwd string, args, env []string) ([]byte, int, error) {
				return []byte(envelope), 0, nil
			},
		},
	}
	_, err := m.Classify(classify.Inputs{ItemID: "T-345"})
	if err == nil {
		t.Fatal("expected error for is_error envelope, got nil")
	}
	if !strings.Contains(err.Error(), "context window exceeded") {
		t.Errorf("err = %v; want substring 'context window exceeded'", err)
	}
}

// TestClaudeModel_MalformedVerdictErrors verifies that a model that
// returns an envelope with invalid verdict JSON inside is rejected
// at parse-time, not silently turned into a green.
func TestClaudeModel_MalformedVerdictErrors(t *testing.T) {
	cfg := &config.Config{}
	m := &ClaudeModel{
		Cfg:    cfg,
		Engine: makeStubEngine("the model refused to answer", 0),
	}
	_, err := m.Classify(classify.Inputs{ItemID: "T-345"})
	if err == nil {
		t.Fatal("expected error for malformed verdict, got nil")
	}
	if !strings.Contains(err.Error(), "parse verdict") {
		t.Errorf("err = %v; want substring 'parse verdict'", err)
	}
}

// TestClaudeModel_PassesPromptWithItemContext verifies the prompt
// handed to RunClaude carries the item context — the subprocess is
// otherwise opaque.
func TestClaudeModel_PassesPromptWithItemContext(t *testing.T) {
	var capturedArgs []string
	cfg := &config.Config{}
	m := &ClaudeModel{
		Cfg: cfg,
		Engine: RunEngine{
			RunClaude: func(cwd string, args, env []string) ([]byte, int, error) {
				capturedArgs = args
				return []byte(`{"type":"result","result":"{\"verdict\":\"green\",\"reason\":\"ok\",\"confidence\":0.5}","is_error":false}`), 0, nil
			},
		},
	}
	in := classify.Inputs{
		ItemID:       "T-345",
		Title:        "unique-item-title-for-test",
		TouchedFiles: []string{"unique/file/path.go"},
	}
	if _, err := m.Classify(in); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	// claude -p <prompt> ... — args[1] is the assembled prompt.
	if len(capturedArgs) < 2 {
		t.Fatalf("captured args too short: %v", capturedArgs)
	}
	prompt := capturedArgs[1]
	for _, want := range []string{"T-345", "unique-item-title-for-test", "unique/file/path.go"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
