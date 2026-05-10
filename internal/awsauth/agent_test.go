package awsauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureAgentSession_NoAgent_NoOp(t *testing.T) {
	// agentName empty: should be a no-op even with no AWS env, no
	// cache file, no infra script. Validates the early return that
	// keeps developer workflows (no agent identity resolved) from
	// requiring any agent-AWS plumbing.
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	if err := EnsureAgentSession("", ""); err != nil {
		t.Fatalf("expected nil err for empty agent, got %v", err)
	}
	if v := os.Getenv("AWS_ACCESS_KEY_ID"); v != "" {
		t.Errorf("AWS_ACCESS_KEY_ID was set when no agent provided: %q", v)
	}
}

func TestEnsureAgentSession_AlreadyHasCreds_NoClobber(t *testing.T) {
	// Caller env carries credentials (operator sourced agent-aws-auth
	// themselves, OR a parent test runner already exported them).
	// EnsureAgentSession must not clobber.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA-CALLER")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-caller")
	if err := EnsureAgentSession("agent-test", ""); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if v := os.Getenv("AWS_ACCESS_KEY_ID"); v != "AKIA-CALLER" {
		t.Errorf("AWS_ACCESS_KEY_ID was clobbered: got %q want AKIA-CALLER", v)
	}
}

func TestEnsureAgentSession_FreshCacheLoaded(t *testing.T) {
	// Valid, non-expired cache → exports the env vars without
	// invoking the refresh script. Proves the happy path: agent is
	// authenticated, session is fresh, env vars get set.
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")

	tmp := t.TempDir()
	swapCachePath(t, tmp)
	mustWriteCache(t, tmp, "agent-fresh", SessionCache{
		AccessKeyID:     "AKIA-FRESH",
		SecretAccessKey: "secret-fresh",
		SessionToken:    "token-fresh",
		Expiration:      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	})

	if err := EnsureAgentSession("agent-fresh", ""); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if v := os.Getenv("AWS_ACCESS_KEY_ID"); v != "AKIA-FRESH" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want AKIA-FRESH", v)
	}
	if v := os.Getenv("AWS_SECRET_ACCESS_KEY"); v != "secret-fresh" {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want secret-fresh", v)
	}
	if v := os.Getenv("AWS_SESSION_TOKEN"); v != "token-fresh" {
		t.Errorf("AWS_SESSION_TOKEN = %q, want token-fresh", v)
	}
}

func TestResolveScriptDir_PrefersWorkspaceParent(t *testing.T) {
	// Build a fake layout: /tmp/.../theraprac-agent-x/{theraprac-workspace,theraprac-infra}
	// resolveScriptDir(workspaceRoot) should return the parent dir
	// (theraprac-agent-x) without walking elsewhere.
	tmp := t.TempDir()
	agentRoot := filepath.Join(tmp, "theraprac-agent-x")
	wsRoot := filepath.Join(agentRoot, "theraprac-workspace")
	scriptDir := filepath.Join(agentRoot, "theraprac-infra", "scripts")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "agent-aws-auth.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := resolveScriptDir(wsRoot)
	if got != agentRoot {
		t.Errorf("resolveScriptDir = %q, want %q (sibling-of-workspace path)", got, agentRoot)
	}
}

func TestResolveScriptDir_FallbackToCWDWhenNoWorkspaceRoot(t *testing.T) {
	// Empty workspaceRoot triggers the agentWorkspaceRoot CWD walker.
	// Swap the walker to a deterministic stub so the test isn't
	// dependent on the real CWD.
	orig := agentWorkspaceRoot
	agentWorkspaceRoot = func() string { return "/walked/to/here" }
	t.Cleanup(func() { agentWorkspaceRoot = orig })

	if got := resolveScriptDir(""); got != "/walked/to/here" {
		t.Errorf("resolveScriptDir(\"\") = %q, want /walked/to/here (CWD-walk fallback)", got)
	}
}

func TestNeedsRefresh(t *testing.T) {
	tests := []struct {
		name string
		c    SessionCache
		want bool
	}{
		{"no expiration", SessionCache{}, true},
		{"unparseable", SessionCache{Expiration: "yesterday"}, true},
		{"already expired", SessionCache{Expiration: time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)}, true},
		{"under threshold", SessionCache{Expiration: time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)}, true},
		{"over threshold", SessionCache{Expiration: time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsRefresh(tc.c); got != tc.want {
				t.Errorf("needsRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- helpers ---

func swapCachePath(t *testing.T, tmpHome string) {
	t.Helper()
	orig := sessionCachePath
	sessionCachePath = func(agent string) string {
		return filepath.Join(tmpHome, ".theraprac", "aws-"+agent+"-session.json")
	}
	t.Cleanup(func() { sessionCachePath = orig })
}

func mustWriteCache(t *testing.T, tmpHome, agent string, c SessionCache) {
	t.Helper()
	dir := filepath.Join(tmpHome, ".theraprac")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "aws-"+agent+"-session.json")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
