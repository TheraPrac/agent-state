package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// I-731 integration tests: the active-envs sentinel is wired into
// Check, prints the active/torn-down summary, surfaces warnings for
// stale or empty declarations, respects the `quiet` flag, and
// silently skips when the file is absent.

func writeActiveEnvsForCheck(t *testing.T, root, content string) {
	t.Helper()
	asDir := filepath.Join(root, ".as")
	if err := os.MkdirAll(asDir, 0o755); err != nil {
		t.Fatalf("mkdir .as: %v", err)
	}
	if err := os.WriteFile(filepath.Join(asDir, "active-envs.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write active-envs.yaml: %v", err)
	}
}

func TestCheckActiveEnvs_PrintsSummaryAndNoWarnings(t *testing.T) {
	s, cfg, root := setupClaudeMdEnv(t)
	stubGitForCheck(t)

	// Use the current wall-clock so the freshness check passes
	// without needing to inject Now. The plan is to keep this
	// integration test free of Now-injection seams — they live
	// in active_envs_test.go.
	fresh := time.Now().UTC().Format(time.RFC3339)
	writeActiveEnvsForCheck(t, root,
		"declared_by: jfinlinson\n"+
			"declared_at: "+fresh+"\n"+
			"active_envs:\n  - demo\n"+
			"torn_down:\n  - prod\n  - dev\n")

	out := captureStdout(t, func() {
		_ = Check(s, cfg, false, false)
	})

	if !strings.Contains(out, "active_envs:") {
		t.Errorf("expected 'active_envs:' summary line; got: %s", out)
	}
	if !strings.Contains(out, "demo") {
		t.Errorf("expected 'demo' in active_envs summary; got: %s", out)
	}
	if !strings.Contains(out, "torn_down:") {
		t.Errorf("expected 'torn_down:' summary line; got: %s", out)
	}
	if strings.Contains(out, "active-envs declared_at:") {
		t.Errorf("fresh declaration should not warn on staleness; got: %s", out)
	}
}

func TestCheckActiveEnvs_StaleDeclarationWarns(t *testing.T) {
	s, cfg, root := setupClaudeMdEnv(t)
	stubGitForCheck(t)

	// 20 days ago is well past the 14-day default cap.
	stale := time.Now().UTC().Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	writeActiveEnvsForCheck(t, root,
		"declared_by: jfinlinson\n"+
			"declared_at: "+stale+"\n"+
			"active_envs:\n  - demo\n")

	out := captureStdout(t, func() {
		_ = Check(s, cfg, false, false)
	})

	if !strings.Contains(out, "active-envs declared_at") {
		t.Errorf("stale declaration should fire a declared_at warning; got: %s", out)
	}
}

func TestCheckActiveEnvs_EmptyActiveListWarns(t *testing.T) {
	s, cfg, root := setupClaudeMdEnv(t)
	stubGitForCheck(t)

	fresh := time.Now().UTC().Format(time.RFC3339)
	writeActiveEnvsForCheck(t, root,
		"declared_by: jfinlinson\n"+
			"declared_at: "+fresh+"\n"+
			"active_envs: []\n"+
			"torn_down:\n  - demo\n")

	out := captureStdout(t, func() {
		_ = Check(s, cfg, false, false)
	})

	if !strings.Contains(out, "active-envs active_envs") {
		t.Errorf("empty active_envs should fire a warning; got: %s", out)
	}
}

func TestCheckActiveEnvs_MissingFileSkipped(t *testing.T) {
	s, cfg, _ := setupClaudeMdEnv(t)
	stubGitForCheck(t)
	// No active-envs.yaml written.

	out := captureStdout(t, func() {
		_ = Check(s, cfg, false, false)
	})

	if strings.Contains(out, "active_envs:") {
		t.Errorf("missing active-envs.yaml should be silently skipped; got: %s", out)
	}
	if strings.Contains(out, "active-envs.yaml unreadable") {
		t.Errorf("missing-file should not emit an unreadable warning; got: %s", out)
	}
}

func TestCheckActiveEnvs_QuietSuppressed(t *testing.T) {
	s, cfg, root := setupClaudeMdEnv(t)
	stubGitForCheck(t)

	fresh := time.Now().UTC().Format(time.RFC3339)
	writeActiveEnvsForCheck(t, root,
		"declared_by: jfinlinson\n"+
			"declared_at: "+fresh+"\n"+
			"active_envs:\n  - demo\n")

	out := captureStdout(t, func() {
		_ = Check(s, cfg, true, false) // quiet=true
	})

	if strings.Contains(out, "active_envs:") {
		t.Errorf("quiet mode should suppress active_envs summary; got: %s", out)
	}
}

func TestCheckActiveEnvs_NeverFailsCheck(t *testing.T) {
	// Even with multiple warnings (stale + empty + cross-contam),
	// Check() returns 0 — active-envs is warn-only.
	s, cfg, root := setupClaudeMdEnv(t)
	stubGitForCheck(t)

	stale := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	writeActiveEnvsForCheck(t, root,
		"declared_by: jfinlinson\n"+
			"declared_at: "+stale+"\n"+
			"active_envs:\n  - demo\n  - prod\n"+
			"torn_down:\n  - prod\n")

	rc := Check(s, cfg, true, false)
	if rc != 0 {
		t.Errorf("active-envs warnings should not fail Check; got rc=%d", rc)
	}
}
