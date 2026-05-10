// Package awsauth bridges the agent identity (from .as/local-agent.yaml
// or path-derived agent-X) to the AWS session credentials minted by
// theraprac-infra/scripts/agent-aws-auth.sh.
//
// Why this exists: the operator's interactive `aws sso login` is the
// wrong path for a long-lived headless agent — every SSO token expires
// within hours and refreshing requires browser interaction the agent
// can't drive. agent-aws-auth.sh assumes an operator role with a
// long-lived access key (chmod 600 at ~/.theraprac/aws-<name>.json)
// and caches a 12h session for reuse, which IS the right shape.
//
// EnsureAgentSession reads that cache (refreshing via the script when
// less than the threshold remains) and exports the resulting
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN into
// the current process so as/internal/evidence/exec.go's runAWSCLI
// inherits them. After EnsureAgentSession returns nil, every `aws`
// command spawned by this process picks up the agent's role
// automatically.
//
// Companion to I-507 (which made evidence/exec.go strip stale
// AWS_PROFILE when env-var creds are present): I-586 closes the loop
// by actually loading the env-var creds for the agent at the top of
// `st test --run`.
package awsauth

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// MinTTL is how much wall-clock the cached session must have left for
// EnsureAgentSession to use it without refreshing. Mirrors the 600s
// _REFRESH_THRESHOLD_SECONDS constant in agent-aws-auth.sh so the two
// stay in lockstep — if the shell would refresh, so would we.
const MinTTL = 10 * time.Minute

// SessionCache is the on-disk shape written by agent-aws-auth.sh.
// Field names match the JSON the script emits via jq.
type SessionCache struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	Expiration      string `json:"expiration"` // ISO 8601 UTC
	AssumedRoleARN  string `json:"assumed_role_arn,omitempty"`
}

// EnsureAgentSession sets AWS env vars on the current process for the
// named agent. It is a no-op when:
//   - agentName is empty (no agent identity resolved)
//   - AWS_ACCESS_KEY_ID is already set in the environment (the
//     operator sourced agent-aws-auth.sh themselves, or the agent
//     parent process did)
//
// `workspaceRoot` is the agent's workspace path (e.g., the result of
// cfg.Root() — typically `/.../theraprac-agent-X/theraprac-workspace`).
// theraprac-infra is expected as a sibling of that root, and the
// refresh script is resolved as `<sibling-dir>/theraprac-infra/scripts/agent-aws-auth.sh`.
// Pass "" to fall back to a CWD walk (used by tests; risks finding a
// stray theraprac-infra elsewhere in the path, so callers should
// supply the workspace root whenever they have one).
//
// Returns nil success, or an error describing why the session
// couldn't be loaded. Callers decide whether to fail-loud or fall
// through (e.g., the evidence preflight in testrecord.go fails the
// run; an interactive `st show` might choose to keep going).
func EnsureAgentSession(agentName, workspaceRoot string) error {
	if agentName == "" {
		return nil
	}
	if os.Getenv("AWS_ACCESS_KEY_ID") != "" {
		// Caller environment already has creds — don't clobber.
		return nil
	}

	cache, ok, err := readSessionCache(agentName)
	if err != nil {
		return fmt.Errorf("read agent session cache: %w", err)
	}
	if !ok || needsRefresh(cache) {
		if err := refreshSession(agentName, workspaceRoot); err != nil {
			return fmt.Errorf("refresh agent session via agent-aws-auth.sh: %w", err)
		}
		cache, ok, err = readSessionCache(agentName)
		if err != nil {
			return fmt.Errorf("re-read agent session cache after refresh: %w", err)
		}
		if !ok {
			return fmt.Errorf("agent session cache missing after refresh — agent-aws-auth.sh failed silently?")
		}
	}

	if cache.AccessKeyID == "" || cache.SecretAccessKey == "" || cache.SessionToken == "" {
		return fmt.Errorf("agent session cache for %q is missing required fields", agentName)
	}

	// Set on the current process so any spawned `aws` cmd inherits.
	// Don't unset AWS_PROFILE here — evidence/exec.go::runAWSCLI does
	// that on the spawned child only, which preserves the operator's
	// shell state for `st show`/etc. that don't go through runAWSCLI.
	if err := os.Setenv("AWS_ACCESS_KEY_ID", cache.AccessKeyID); err != nil {
		return fmt.Errorf("set AWS_ACCESS_KEY_ID: %w", err)
	}
	if err := os.Setenv("AWS_SECRET_ACCESS_KEY", cache.SecretAccessKey); err != nil {
		return fmt.Errorf("set AWS_SECRET_ACCESS_KEY: %w", err)
	}
	if err := os.Setenv("AWS_SESSION_TOKEN", cache.SessionToken); err != nil {
		return fmt.Errorf("set AWS_SESSION_TOKEN: %w", err)
	}
	return nil
}

// SessionCachePath is exposed (lowercase, package-internal) so tests
// can swap it. Always under HOME so chmod 600 stays meaningful.
var sessionCachePath = func(agentName string) string {
	return filepath.Join(os.Getenv("HOME"), ".theraprac", "aws-"+agentName+"-session.json")
}

func readSessionCache(agentName string) (SessionCache, bool, error) {
	path := sessionCachePath(agentName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SessionCache{}, false, nil
		}
		return SessionCache{}, false, err
	}
	var c SessionCache
	if err := json.Unmarshal(data, &c); err != nil {
		return SessionCache{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, true, nil
}

func needsRefresh(c SessionCache) bool {
	if c.Expiration == "" {
		return true
	}
	exp, err := time.Parse(time.RFC3339, c.Expiration)
	if err != nil {
		// Unparseable expiry — refresh defensively rather than risk
		// using a credential we can't reason about.
		return true
	}
	return time.Until(exp) < MinTTL
}

// refreshScript is the path (relative to the agent workspace root)
// where agent-aws-auth.sh lives. Indirection so tests can swap it.
var refreshScript = "theraprac-infra/scripts/agent-aws-auth.sh"

// agentWorkspaceRoot is the CWD-walk fallback used when the caller
// can't supply an explicit workspaceRoot. Risk: if the developer has
// a personal theraprac-infra checkout above CWD on the path, the
// walker will find it and exec the wrong script. Prefer passing
// workspaceRoot to refreshSession when the caller has access to
// cfg.Root().
var agentWorkspaceRoot = func() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := cwd; dir != "/" && dir != ""; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, refreshScript)); err == nil {
			return dir
		}
	}
	return ""
}

// resolveScriptDir picks the directory to look for theraprac-infra in.
// Prefers the caller-supplied workspaceRoot's parent (the canonical
// agent root, e.g., /.../theraprac-agent-X) so we never resolve to a
// stray theraprac-infra elsewhere on the filesystem path. Falls back
// to the CWD walk only when no workspaceRoot is supplied (tests).
func resolveScriptDir(workspaceRoot string) string {
	if workspaceRoot != "" {
		// theraprac-infra is a sibling of theraprac-workspace under
		// the agent root, so candidate = parent of workspaceRoot.
		candidate := filepath.Dir(workspaceRoot)
		if _, err := os.Stat(filepath.Join(candidate, refreshScript)); err == nil {
			return candidate
		}
		// workspaceRoot was supplied but infra isn't a sibling —
		// fall through to the CWD walk rather than fail; lets a
		// nonstandard layout still work, with the documented risk.
	}
	return agentWorkspaceRoot()
}

func refreshSession(agentName, workspaceRoot string) error {
	root := resolveScriptDir(workspaceRoot)
	if root == "" {
		return fmt.Errorf("could not locate %s — is theraprac-infra checked out as a sibling of the agent workspace?", refreshScript)
	}
	scriptPath := filepath.Join(root, refreshScript)

	// Run the script in --show mode would skip the export, but we
	// don't need its exports — we re-read the cache file after. So
	// just run normally and let it write the cache.
	cmd := exec.Command("bash", scriptPath, "--name", agentName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("agent-aws-auth.sh: %w (output: %s)", err, string(out))
	}
	return nil
}
