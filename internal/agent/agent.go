// Package agent provides process-level agent registration so the workspace
// has a durable record of which st processes are alive, who spawned them,
// and what scope they own. T-311.
//
// Each live `st run` or `st start` writes <AgentsDir>/<agent-id>.yaml
// containing the resolved identity (from internal/config), the process
// PID, and lineage fields when the agent inherits from a parent. The
// file is removed on clean exit (defer) and swept on the next startup
// if the recorded PID no longer exists.
//
// Sibling layer T-310 uses the registry as its source of truth for
// stale-claim detection: an item whose claimed_by names a session that
// is no longer in the registry can be safely released.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
)

// Registration is the on-disk record for one live st process.
type Registration struct {
	AgentID          string `yaml:"agent_id"`
	Parent           string `yaml:"parent,omitempty"`
	Root             string `yaml:"root"`
	PID              int    `yaml:"pid"`
	Started          string `yaml:"started"` // RFC3339
	Scope            string `yaml:"scope,omitempty"`
	SessionID        string `yaml:"session_id,omitempty"`
	Role             string `yaml:"role,omitempty"`
	SpawnedBySession string `yaml:"spawned_by_session,omitempty"`
}

// Options carry the inputs Register needs that aren't on the Config.
// Identity comes pre-resolved from cfg.Identity() at the call site so
// this package doesn't depend on the resolution chain.
type Options struct {
	BaseAgentID      string // resolved id, e.g. "agent-a" — suffix is appended
	ParentAgentID    string // empty for root agents
	RootAgentID      string // defaults to BaseAgentID when empty
	Role             string // "worker" / "reviewer" / "explorer" / "child"
	SpawnedBySession string // session id that spawned this process
	SessionID        string // this process's session id
	Scope            string // free-form, e.g. "sprint:naturally-picked-buck"
	PID              int    // 0 → use os.Getpid()
}

// Register writes a registration file for this process and returns the
// Registration plus an unregister cleanup. Callers should `defer`
// the cleanup to remove the file on clean exit; on crash the file is
// left behind and Sweep on the next startup will remove it.
//
// The cleanup is idempotent (safe to call multiple times) and never
// returns an error in deferred contexts — it logs to stderr instead.
func Register(cfg *config.Config, opts Options) (*Registration, func(), error) {
	if opts.BaseAgentID == "" {
		return nil, noop, fmt.Errorf("agent.Register: BaseAgentID required")
	}
	if opts.PID == 0 {
		opts.PID = os.Getpid()
	}
	if opts.RootAgentID == "" {
		opts.RootAgentID = opts.BaseAgentID
	}

	dir := cfg.AgentsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, noop, fmt.Errorf("agent.Register: mkdir %s: %w", dir, err)
	}

	// Compute the suffixed agent id under the root prefix. Children
	// hang off the parent's id (parent + "-N"); root agents get
	// "<base>-N" so multiple sibling roots don't collide either.
	prefix := opts.BaseAgentID
	if opts.ParentAgentID != "" {
		prefix = opts.ParentAgentID
	}
	id, err := nextSuffix(dir, prefix)
	if err != nil {
		return nil, noop, fmt.Errorf("agent.Register: suffix: %w", err)
	}

	reg := &Registration{
		AgentID:          id,
		Parent:           opts.ParentAgentID,
		Root:             opts.RootAgentID,
		PID:              opts.PID,
		Started:          time.Now().Format(time.RFC3339),
		Scope:            opts.Scope,
		SessionID:        opts.SessionID,
		Role:             opts.Role,
		SpawnedBySession: opts.SpawnedBySession,
	}

	path := filepath.Join(dir, id+".yaml")
	if err := writeRegistration(path, reg); err != nil {
		return nil, noop, fmt.Errorf("agent.Register: write %s: %w", path, err)
	}

	cleanup := func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "agent: cleanup %s: %v\n", path, err)
		}
	}
	return reg, cleanup, nil
}

// LoadRegistration reads a single registration file by agent id.
// Returns (nil, nil) when the file is missing.
func LoadRegistration(cfg *config.Config, agentID string) (*Registration, error) {
	path := filepath.Join(cfg.AgentsDir(), agentID+".yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseRegistration(body)
}

// ListRegistrations returns every registration in AgentsDir. Files that
// fail to parse are skipped (best-effort, with a warning to stderr) so
// one bad file can't break a sweep or a status read.
func ListRegistrations(cfg *config.Config) ([]*Registration, error) {
	dir := cfg.AgentsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := []*Registration{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent: read %s: %v\n", e.Name(), err)
			continue
		}
		reg, err := parseRegistration(body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent: parse %s: %v\n", e.Name(), err)
			continue
		}
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}

// Sweep removes registration files whose recorded PID is no longer
// alive. Returns the agent ids it cleaned up and any errors that
// surfaced (one error doesn't stop the sweep).
func Sweep(cfg *config.Config) ([]string, error) {
	regs, err := ListRegistrations(cfg)
	if err != nil {
		return nil, err
	}
	var cleaned []string
	var firstErr error
	for _, reg := range regs {
		if isPIDLive(reg.PID) {
			continue
		}
		path := filepath.Join(cfg.AgentsDir(), reg.AgentID+".yaml")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
			fmt.Fprintf(os.Stderr, "agent.Sweep: remove %s: %v\n", path, err)
			continue
		}
		cleaned = append(cleaned, reg.AgentID)
	}
	return cleaned, firstErr
}

// IsPIDLive reports whether the given PID corresponds to a running
// process. Exposed for callers (T-310 stale-claim sweep) that want
// the same liveness test the registry uses.
func IsPIDLive(pid int) bool { return isPIDLive(pid) }

func isPIDLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds; Signal(0) is the actual probe.
	// ESRCH means the PID doesn't exist; EPERM means it does but is owned
	// by another user (still alive for our purposes).
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// nextSuffix returns "<prefix>-<n>" where n is the lowest positive integer
// not already taken by a file in dir. Examples:
//   - dir empty:                     prefix-1
//   - prefix-1.yaml present:         prefix-2
//   - prefix-1.yaml + prefix-3.yaml: prefix-2
func nextSuffix(dir, prefix string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	taken := map[int]bool{}
	pat := regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + `-(\d+)\.yaml$`)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := pat.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		taken[n] = true
	}
	for n := 1; ; n++ {
		if !taken[n] {
			return fmt.Sprintf("%s-%d", prefix, n), nil
		}
	}
}

func noop() {}

// --- minimal YAML serialization ---
//
// Keep the format hand-rolled and stable so external tooling (and the
// stale-sweep on a future binary) can read these without depending on
// any YAML library version. The schema is fixed; keys are emitted in
// declaration order.
func writeRegistration(path string, reg *Registration) error {
	var b strings.Builder
	fmt.Fprintf(&b, "agent_id: %s\n", reg.AgentID)
	if reg.Parent != "" {
		fmt.Fprintf(&b, "parent: %s\n", reg.Parent)
	}
	fmt.Fprintf(&b, "root: %s\n", reg.Root)
	fmt.Fprintf(&b, "pid: %d\n", reg.PID)
	fmt.Fprintf(&b, "started: %s\n", reg.Started)
	if reg.Scope != "" {
		fmt.Fprintf(&b, "scope: %s\n", reg.Scope)
	}
	if reg.SessionID != "" {
		fmt.Fprintf(&b, "session_id: %s\n", reg.SessionID)
	}
	if reg.Role != "" {
		fmt.Fprintf(&b, "role: %s\n", reg.Role)
	}
	if reg.SpawnedBySession != "" {
		fmt.Fprintf(&b, "spawned_by_session: %s\n", reg.SpawnedBySession)
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func parseRegistration(body []byte) (*Registration, error) {
	reg := &Registration{}
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, `"'`)
		switch key {
		case "agent_id":
			reg.AgentID = val
		case "parent":
			reg.Parent = val
		case "root":
			reg.Root = val
		case "pid":
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("pid: %w", err)
			}
			reg.PID = n
		case "started":
			reg.Started = val
		case "scope":
			reg.Scope = val
		case "session_id":
			reg.SessionID = val
		case "role":
			reg.Role = val
		case "spawned_by_session":
			reg.SpawnedBySession = val
		}
	}
	if reg.AgentID == "" {
		return nil, fmt.Errorf("missing agent_id")
	}
	return reg, nil
}
