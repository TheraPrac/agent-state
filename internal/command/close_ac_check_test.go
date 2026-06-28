package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/testutil"
)

func acItem(id string, acs ...string) *model.Item {
	return &model.Item{ID: id, AcceptanceCriteria: acs}
}

// runner factory: every cmd returns the given exit code.
func acRunner(exit int) func(string) ([]byte, int, error) {
	return func(string) ([]byte, int, error) { return []byte("output"), exit, nil }
}

func TestCloseACGateBlocksFailingCmd(t *testing.T) {
	item := acItem("T-1", "cmd: go test ./...")
	msg := closeACCheck(item, &config.Config{}, CloseOpts{ACRunCmd: acRunner(1)})
	if msg == "" {
		t.Fatalf("expected a block message when a cmd AC fails, got empty")
	}
	if !strings.Contains(msg, "did not pass") {
		t.Errorf("block message should mention failing AC, got: %q", msg)
	}
}

func TestCloseACGatePassesAllGreen(t *testing.T) {
	item := acItem("T-1", "cmd: go build ./...", "cmd: go test ./...")
	msg := closeACCheck(item, &config.Config{}, CloseOpts{ACRunCmd: acRunner(0)})
	if msg != "" {
		t.Fatalf("expected pass (empty message) when all cmd AC pass, got: %q", msg)
	}
}

func TestCloseACGateSkipACBypassesAndAudits(t *testing.T) {
	env := testutil.NewEnv(t)
	item := acItem("T-1", "cmd: go test ./...")
	msg := closeACCheck(item, env.Cfg, CloseOpts{
		SkipACRequested: true,
		SkipAC:          "AC needs the live API which is unavailable in CI",
		ACRunCmd:        acRunner(1), // would fail, but skip bypasses
	})
	if msg != "" {
		t.Fatalf("skip-ac with reason should bypass (empty message), got: %q", msg)
	}
	logPath := filepath.Join(env.Cfg.Root(), ".as", "close-ac-skip.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("close-ac-skip.log not written: %v", err)
	}
	if !strings.Contains(string(data), "T-1") || !strings.Contains(string(data), "live API") {
		t.Errorf("audit log missing item/reason: %q", string(data))
	}
}

func TestCloseACGateSkipACRequiresReason(t *testing.T) {
	item := acItem("T-1", "cmd: go test ./...")
	msg := closeACCheck(item, &config.Config{}, CloseOpts{SkipACRequested: true, SkipAC: "   "})
	if msg == "" {
		t.Fatalf("skip-ac with empty reason should be rejected, got empty (allowed)")
	}
	if !strings.Contains(msg, "non-empty reason") {
		t.Errorf("rejection should mention the required reason, got: %q", msg)
	}
}

func TestCloseACGateZeroCmdACsRequiresNoAC(t *testing.T) {
	// Item with only a prose AC — no `cmd:` to verify.
	item := acItem("T-1", "the dashboard renders correctly")
	// Without --no-ac → blocked.
	if msg := closeACCheck(item, &config.Config{}, CloseOpts{ACRunCmd: acRunner(0)}); msg == "" {
		t.Fatalf("zero cmd AC should be blocked without --no-ac")
	}
	// With --no-ac → allowed.
	if msg := closeACCheck(item, &config.Config{}, CloseOpts{NoAC: true, ACRunCmd: acRunner(0)}); msg != "" {
		t.Fatalf("zero cmd AC with --no-ac should pass, got: %q", msg)
	}
}
