// Package config provides configuration for the as CLI.
//
// The tool works with sensible defaults out of the box. An optional
// .as/config.yaml in the project root can override any default.
// Config discovery: walk up from CWD looking for .as/config.yaml or
// .st-root (a redirect file containing a path to the project root),
// or use --config flag. If none found, use defaults.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Config holds all configuration for the as CLI.
type Config struct {
	// Project metadata
	Project ProjectConfig

	// Paths relative to the config file (or CWD if no config)
	Paths PathsConfig

	// Item types and their allowed statuses
	Types map[string]TypeConfig

	// ID patterns per type
	IDPatterns map[string]string

	// Fields configuration
	Fields FieldsConfig

	// Testing configuration (optional)
	Testing *TestingConfig

	// Delivery pipeline (optional)
	Delivery *DeliveryConfig

	// Git sync (optional)
	Git *GitConfig

	// Worktree integration (optional)
	Worktree *WorktreeConfig

	// Gate definitions per transition (optional)
	Gates map[string][]GateConfig // keyed by transition name (e.g. "close", "start")

	// Multi-agent support (optional)
	Agents *AgentsConfig

	// Sprint configuration (optional)
	Sprints *SprintsConfig

	// Run configuration for st run/advance (optional)
	Run *RunConfig

	// Pipeline steps (optional)
	Pipeline *PipelineConfig

	// Evidence storage (optional)
	Evidence *EvidenceConfig

	// Autonomy classifier configuration (optional)
	Classify *ClassifyConfig

	// Session guidance text (optional, shown in prime output)
	Guidance string

	// Root directory (where .as/ lives, or CWD)
	root string

	// startDir is the directory Load() was originally called from, before
	// any discover() walk-up or .st-root / ST_ROOT redirection. AgentRoot()
	// walks up from here to find a per-agent .as/agent-workspace.yaml so
	// the correct per-agent root is recoverable even when c.root resolves
	// to a peer agent's workspace under an ST_ROOT env leak. I-778.
	startDir string

	// agentRootCache memoizes the resolved AgentRoot() value (filesystem
	// walks are amortized across many call sites per session). I-778.
	agentRootCache string
	agentRootResolved bool

	// Discovered is true when a .as/config.yaml was found (vs using defaults)
	Discovered bool
}

type ProjectConfig struct {
	Name        string
	Description string
}

type PathsConfig struct {
	Root      string // item storage root (default: ".")
	Templates string // template directory (default: "templates")
	Changelog string // changelog directory (default: ".changelog")
	Index     string // index file (default: "index.md")
}

type TypeConfig struct {
	IDPrefix         string            // e.g., "T" for tasks
	Statuses         []string          // allowed statuses
	DirectoryMap     map[string]string // status -> directory
	StartStatus      string            // status items are created with
	ActiveStatus     string            // status that means "in progress"
	TerminalStatuses []string          // these can't transition further
	RequiredFields   []string          // fields that must be present for this type
}

type FieldsConfig struct {
	Required []string
	Optional []OptionalField
	Computed []ComputedField
}

type OptionalField struct {
	Name      string
	Type      string   // string, int, enum, list_string, list_id, multiline, bool
	Values    []string // for enum type
	Range     [2]int   // for int type
	AppliesTo []string // item types (empty = all)
}

type ComputedField struct {
	Name   string
	Source string // e.g., "inverse(depends_on)"
}

type TestingConfig struct {
	RequiredSuites     map[string]SuiteConfig
	ScopeSuites        map[string]ScopeSuiteConfig
	CoverageThresholds *CoverageThresholds

	// I-776: ScopeClasses maps a class name (e.g. "workspace-config") to a
	// class-specific required-suite set. When an item declares scope_class,
	// the testing-complete gate iterates that class's RequiredSuites instead
	// of the default TestingConfig.RequiredSuites.
	//
	// Config shape is flat (4 levels deep, matching the line parser's cap):
	//
	//   testing:
	//     scope_classes:
	//       workspace-config:
	//         workspace_test: bash claude-config/hooks/run-changed-hook-tests.sh
	//
	// Direct key/value entries under the class name ARE the class's required
	// suites — there is no inner `required_suites:` key.
	ScopeClasses map[string]ScopeClassConfig
}

// ScopeClassConfig is one named scope class's required-suite set.
// Suite names follow the same conventions as TestingConfig.RequiredSuites;
// SuiteConfig values are reused as-is.
type ScopeClassConfig struct {
	RequiredSuites map[string]SuiteConfig
	// I-830: goal slugs (e.g. "st-tooling") that auto-assign this class on
	// create/start when the item carries a matching "goal:<slug>" tag.
	AppliesToGoals []string
}

// RequiredSuitesFor returns the required-suite set that applies to a given
// item, plus a "class resolved" flag. I-776: every code path that needs to
// know which suites are required for an item MUST route through this helper
// so the gate, st test, st uat, st run auto-runner, the queue advisor, and
// the canonical-emit slot all stay in lock-step on what counts as required.
//
//   - scopeClass == ""           → default RequiredSuites, ok=true.
//   - scopeClass set + found     → class's RequiredSuites, ok=true.
//   - scopeClass set + not found → nil, ok=false (caller decides: fail fast,
//                                  or surface "unknown scope_class").
//
// A nil receiver returns nil/true (no testing config → nothing required).
func (t *TestingConfig) RequiredSuitesFor(scopeClass string) (map[string]SuiteConfig, bool) {
	if t == nil {
		return nil, true
	}
	if scopeClass == "" {
		return t.RequiredSuites, true
	}
	class, ok := t.ScopeClasses[scopeClass]
	if !ok {
		return nil, false
	}
	return class.RequiredSuites, true
}

// ScopeClassForItem returns the first scope class (by sorted class name, for
// deterministic precedence) whose AppliesToGoals list matches the item. A class
// target matches when it appears in the item's goal IDs (e.g. "G-014") OR is the
// slug of a "goal:<slug>" tag. Returns "" if none match. I-830 introduced
// goal-tag matching; I-987 added goal-ID membership (item.Goals) because items
// reliably carry their goal IDs but inconsistently carry goal:<slug> tags,
// leaving auto-assign dormant.
//
// Bare (non-"goal:"-prefixed) tags are deliberately NOT matched: applies_to_goals
// entries are goal identifiers, not the free-form tag namespace, so a label tag
// that merely happens to equal a goal slug must not silently swap an item's
// required-suite set (I-987 review finding D1).
func (t *TestingConfig) ScopeClassForItem(tags, goals []string) string {
	if t == nil {
		return ""
	}
	// Match set is goal IDs plus goal:-stripped tag slugs — never bare tags.
	matchSet := make(map[string]bool, len(tags)+len(goals))
	for _, g := range goals {
		matchSet[g] = true
	}
	for _, tag := range tags {
		if slug, ok := strings.CutPrefix(tag, "goal:"); ok {
			matchSet[slug] = true
		}
	}
	classNames := make([]string, 0, len(t.ScopeClasses))
	for cn := range t.ScopeClasses {
		classNames = append(classNames, cn)
	}
	sort.Strings(classNames)
	for _, className := range classNames {
		for _, target := range t.ScopeClasses[className].AppliesToGoals {
			if matchSet[target] {
				return className
			}
		}
	}
	return ""
}

type CoverageThresholds struct {
	Lines     float64
	Branches  float64
	Functions float64
}

type SuiteConfig struct {
	Command   string
	Artifacts []string // glob patterns for artifacts to upload after execution
}

type ScopeSuiteConfig struct {
	Command       string
	Triggers      []string // file glob patterns that activate this suite
	RepoTrigger   string   // repo name (e.g. "theraprac-api") — suite applicable when that repo has any changed files
	Artifacts     []string // glob patterns for artifacts to upload after execution
	PostDeployCmd string   // command to run post-deploy verification (e.g., E2E against dev)
	PostMergeCmd  string   // command to run post-merge verification against merged main (I-696, e.g., full local E2E)
	// I-757: optional env/target/vendor-tier capture. Resolved at run time and
	// stamped into testSummary so live_acceptance evidence is interpretable.
	EnvFrom     string   // env-var reference (e.g. "$TARGET_ENV"); resolved value → testSummary.Env
	TargetFrom  []string // list of "key=$VAR" pairs → testSummary.Target
	VendorTiers []string // list of "key=$VAR" pairs → testSummary.VendorTiers
}

type PipelineConfig struct {
	Merge       *PipelineStepConfig
	DeployCheck *PipelineStepConfig
	Smoke       *PipelineStepConfig
}

type PipelineStepConfig struct {
	Command    string
	PreChecks  []string
	PostRecord string
	HealthURL  string   // single health URL (backward compatible)
	HealthURLs []string // multiple health URLs — all must pass
	Timeout    int      // seconds, 0 = default 300
	Artifacts  []string
	WatchCI    bool // if true, watch the latest GH Actions run on main before health checks
}

type EvidenceConfig struct {
	Backend   string // "local" (default) or "s3"
	LocalDir  string // for local: directory path (default: .evidence)
	S3Bucket  string // for s3: bucket name
	S3Region  string // for s3: AWS region
	S3Prefix  string // for s3: key prefix
	S3Profile string // for s3: AWS CLI profile name (optional)
}

type DeliveryConfig struct {
	Stages      []string
	ArchiveGate string // cannot archive without reaching this stage
}

type GitConfig struct {
	AutoCommit bool
	AutoPush   bool
	LockFile   string
}

type WorktreeConfig struct {
	Enabled   bool
	BaseDir   string            // worktree root relative to AGENT root, one level above the workspace (e.g. "worktrees" → <agent>/worktrees). I-407.
	ParentDir string            // parent of all repos (e.g. "/Users/x/Dev/project")
	Repos     []string          // short repo names in default order (e.g. ["api", "web"])
	RepoMap   map[string]string // short name → directory name (e.g. "api" → "theraprac-api")
}

type GateConfig struct {
	Type   string   // deps_resolved, testing_complete, field_nonempty, stage_reached, agent_assigned, manifest_exists
	Fields []string // for field_nonempty
	Stage  string   // for stage_reached
}

type AgentsConfig struct {
	// Agent identity comes from $AS_AGENT_ID env var
}

// ClassifyConfig holds project-specific autonomy classifier settings.
// Project-specific path prefixes that should always force a red verdict
// go here (via .as/config.yaml) instead of HardRedPatterns, which is
// reserved for universally-appropriate generic patterns.
type ClassifyConfig struct {
	DenyPathPrefixes  []string // path-prefix deny patterns: any touched file with this prefix forces red
	DenyBasenameGlobs []string // basename-glob deny patterns: e.g. "private_*.tf"
}

type SprintsConfig struct {
	StaleClaimTTL   int // seconds before a claim is stale (default 7200)
	StaleActiveHours int // hours before an active item with no changelog activity is released (default 6). I-874.
}

// RunConfig holds settings for st run / st advance.
type RunConfig struct {
	PermissionMode   string // "dangerously-skip-permissions" (default) or "auto"
	DefaultModel     string // e.g. "sonnet", "opus"
	MaxParallelism   int    // max concurrent claude processes (default 1)
	DefaultBudgetUSD float64
	StepOrder        []string              // ordered step names
	Steps            map[string]RunStepDef // step name → definition
	Breakpoints      []string              // step names where the pipeline pauses for user input
	AutoParallel     bool                  // auto-determine parallelism based on repo overlap
}

// RunStepDef defines a single pipeline step for st run.
type RunStepDef struct {
	Type       string   // claude, test, pr, merge, merge_precheck, deploy, smoke, uat, gate, close, command
	Command    string   // for command type
	Prompt     string   // for claude type (optional, uses default)
	Resolution string   // for close type (e.g. "completed")
	Timeout    int      // for watch/deploy (seconds, default 600)
	Coverage   bool     // for test type
	Budget     float64  // per-step budget override (USD, 0 = use default)
	ExtraEnv   []string // I-752: extra KEY=VAL env vars appended in executeClaude (e.g. AS_CLAUDE_WALL_TIMEOUT for plan-review)
	name       string   // set by RunPipeline(), not from config
}

// Name returns the step's name (set when building the pipeline from config).
func (s RunStepDef) Name() string { return s.name }

// SetName sets the step name (for dynamically created steps like ci_fix).
func (s *RunStepDef) SetName(name string) { s.name = name }

// Root returns the root directory for this config.
func (c *Config) Root() string {
	return c.root
}

// ItemDir returns the absolute path to the item storage root.
func (c *Config) ItemDir() string {
	return filepath.Join(c.root, c.Paths.Root)
}

// TemplatesDir returns the absolute path to templates.
func (c *Config) TemplatesDir() string {
	return filepath.Join(c.root, c.Paths.Templates)
}

// ChangelogDir returns the absolute path to changelogs.
func (c *Config) ChangelogDir() string {
	return filepath.Join(c.root, c.Paths.Changelog)
}

// IndexPath returns the absolute path to the index file.
func (c *Config) IndexPath() string {
	return filepath.Join(c.root, c.Paths.Index)
}

// Identity describes the resolved agent identity for the current st invocation.
// It captures both the executing agent (ID) and any parent/root heritage
// inherited from a spawning agent so that work attribution and usage rollups
// can credit the full chain.
type Identity struct {
	ID               string
	DisplayName      string
	Source           string // "env", "local-config", "path", "inherited", ""
	WorkspacePath    string
	ParentID         string
	RootID           string
	SpawnedBySession string
	DelegatedItemID  string
	Role             string
}

// HasHeritage reports whether this identity carries sub-agent heritage from
// a spawning parent. Role alone (which can come from local-agent.yaml) does
// not count — heritage requires at least one parent/spawning marker.
func (i Identity) HasHeritage() bool {
	return i.ParentID != "" || i.SpawnedBySession != "" || i.DelegatedItemID != ""
}

// Identity resolves the current agent identity using this precedence chain
// for the ID field:
//  1. $AS_AGENT_ID env var
//  2. CWD-anchored .as/agent-workspace.yaml (walked up from c.startDir — immune
//     to ST_ROOT leaks because startDir is set from CWD before Load() applies
//     any ST_ROOT redirect). I-936.
//  3. <root>/.as/local-agent.yaml (gitignored, per-workspace)
//  4. parent directory named theraprac-agent-<suffix> (I-383 path derivation)
//
// Heritage env vars (AS_AGENT_PARENT_ID, AS_AGENT_ROOT_ID,
// AS_AGENT_SPAWNED_BY_SESSION, AS_AGENT_DELEGATED_ITEM, AS_AGENT_ROLE) are
// layered on top regardless of how the ID was resolved. When any
// parent/spawning marker is present (see HasHeritage — Role alone does NOT
// count, since it can come from local-agent.yaml), Source is reported as
// "inherited" so the chain is obvious in `st agent identity show`. RootID
// defaults to ParentID if unset, then to ID if there is no parent.
func (c *Config) Identity() Identity {
	id := Identity{WorkspacePath: c.root}

	if envID := os.Getenv("AS_AGENT_ID"); envID != "" {
		id.ID = envID
		id.Source = "env"
	} else if markerID := agentIDFromWorkspaceMarker(c.startDir); markerID != "" {
		id.ID = markerID
		id.Source = "local-config"
		// agent-workspace.yaml is an infrastructure marker with no display/role
		// fields; compose with local-agent.yaml for those if it agrees on the ID.
		if la, err := loadLocalAgent(c.root); err == nil && la.ID == markerID {
			id.DisplayName = la.DisplayName
			id.Role = la.Role
		}
	} else if la, err := loadLocalAgent(c.root); err == nil && la.ID != "" {
		id.ID = la.ID
		id.DisplayName = la.DisplayName
		id.Role = la.Role
		id.Source = "local-config"
	} else {
		parent := filepath.Base(filepath.Dir(c.root))
		if suffix := strings.TrimPrefix(parent, "theraprac-"); suffix != parent && strings.HasPrefix(suffix, "agent-") {
			id.ID = suffix
			id.Source = "path"
		}
	}

	if v := os.Getenv("AS_AGENT_PARENT_ID"); v != "" {
		id.ParentID = v
	}
	if v := os.Getenv("AS_AGENT_ROOT_ID"); v != "" {
		id.RootID = v
	}
	if v := os.Getenv("AS_AGENT_SPAWNED_BY_SESSION"); v != "" {
		id.SpawnedBySession = v
	}
	if v := os.Getenv("AS_AGENT_DELEGATED_ITEM"); v != "" {
		id.DelegatedItemID = v
	}
	if v := os.Getenv("AS_AGENT_ROLE"); v != "" {
		id.Role = v
	}

	if id.RootID == "" {
		if id.ParentID != "" {
			id.RootID = id.ParentID
		} else {
			id.RootID = id.ID
		}
	}

	if id.HasHeritage() {
		id.Source = "inherited"
	}

	return id
}

// AgentID returns the current agent identity ID. Equivalent to
// c.Identity().ID; retained for backward compatibility with the many call
// sites that only need the bare id.
func (c *Config) AgentID() string {
	return c.Identity().ID
}

// LocalAgentID returns the agent ID sourced from the filesystem only
// (agent-workspace.yaml or local-agent.yaml), ignoring the AS_AGENT_ID
// env var. Used by st whoami to detect mismatches between the env var
// and the on-disk configuration. I-877.
func (c *Config) LocalAgentID() string {
	if markerID := agentIDFromWorkspaceMarker(c.startDir); markerID != "" {
		return markerID
	}
	if la, err := loadLocalAgent(c.root); err == nil && la.ID != "" {
		return la.ID
	}
	return ""
}

// AgentContext is the resolved (current-agent, scoped?) pair that
// agent-facing st renderers consult to decide their default filter.
//
//   - From inside an agent workspace (cwd under theraprac-agents/
//     theraprac-agent-<x>/, or anywhere AS_AGENT_ID resolves),
//     CurrentAgent is the agent id and Scoped is true. Renderers
//     default to "show this agent's items only".
//   - From the workspace root (or any cwd where no agent identity
//     can be resolved), CurrentAgent is "" and Scoped is false.
//     Renderers default to the global view.
//
// `--all` flags on commands flip Scoped to false even inside an
// agent workspace so the operator can see the whole picture on
// demand. T-347.
type AgentContext struct {
	CurrentAgent string
	Scoped       bool
}

// ResolveAgentContext returns the rendering scope for the current st
// invocation. T-347 introduces this so every agent-facing renderer
// shares a single rule for "show me only items relevant to where I
// am" — see the AgentContext docstring for the exact semantics.
func (c *Config) ResolveAgentContext() AgentContext {
	id := c.AgentID()
	return AgentContext{
		CurrentAgent: id,
		Scoped:       id != "",
	}
}

// EpicsPath returns the path to the epics/sprints registry file.
func (c *Config) EpicsPath() string {
	return filepath.Join(c.root, ".as", "epics.yaml")
}

// NotesPath returns the path to the notes registry file.
func (c *Config) NotesPath() string {
	return filepath.Join(c.root, ".as", "notes.yaml")
}

// ManifestDir returns the absolute path to manifest sidecar files.
func (c *Config) ManifestDir() string {
	return filepath.Join(c.root, c.Paths.Root, ".manifest")
}

// PlansDir returns the path to the plans sidecar directory: the item-store
// root's `.plans/` (e.g. `<root>/agent-state/.plans`). This is the canonical
// location the `st` tooling authoritatively reads and writes — `st prep`,
// `st plan approve`, `st plan show`, `st run`, `st split`, `st classify` —
// and the directory `internal/store` git-auto-stages (I-575). Keep it
// consistent with the sibling accessors (ItemDir/ManifestDir/…), which all
// nest under `c.Paths.Root`.
//
// NOTE (I-690 / I-693): the human-facing PROSE drifts from this path —
// CLAUDE.md instructs agents to author `.plans/<id>.md` (read as
// workspace-root relative) and plan-before-code-guard.sh's rejection
// message likewise references `.plans/<id>.md`. The hook's GATE is fine
// (it shells `st plan check`, which uses THIS accessor); the drift is
// purely in the prose, so hand-authored plans land at the workspace root
// and the tooling never sees them. Tracked in I-693. Do NOT "fix" it by
// relocating this accessor — that silently orphans every plan the tooling
// already wrote here (incl. peer agents' active in-sprint work).
func (c *Config) PlansDir() string {
	return filepath.Join(c.root, c.Paths.Root, ".plans")
}

// RunPermissionMode returns the configured claude permission mode for st run.
func (c *Config) RunPermissionMode() string {
	if c.Run != nil && c.Run.PermissionMode != "" {
		return c.Run.PermissionMode
	}
	return "dangerously-skip-permissions"
}

// RunPipeline returns the ordered pipeline steps for st run.
func (c *Config) RunPipeline() []RunStepDef {
	if c.Run == nil || len(c.Run.StepOrder) == 0 {
		return nil
	}
	var steps []RunStepDef
	for _, name := range c.Run.StepOrder {
		step, ok := c.Run.Steps[name]
		if !ok {
			continue
		}
		// Carry the step name into the struct
		step.name = name
		steps = append(steps, step)
	}
	return steps
}

// QueuePath returns the path to the work queue file.
func (c *Config) QueuePath() string {
	return filepath.Join(c.root, ".as", "queue.yaml")
}

// StackPath returns the path to the per-agent work stack file.
func (c *Config) StackPath() string {
	agentID := c.AgentID()
	if agentID != "" {
		return filepath.Join(c.root, ".as", "stacks", agentID+".yaml")
	}
	return filepath.Join(c.root, ".as", "stack.yaml")
}

// EvidenceDir returns the default local evidence directory.
func (c *Config) EvidenceDir() string {
	if c.Evidence != nil && c.Evidence.LocalDir != "" {
		if filepath.IsAbs(c.Evidence.LocalDir) {
			return c.Evidence.LocalDir
		}
		return filepath.Join(c.root, c.Evidence.LocalDir)
	}
	return filepath.Join(c.root, c.Paths.Root, ".evidence")
}

// SessionID returns the current Claude Code session ID.
// Checks in order: $AS_SESSION_ID env var, then the .as/session file (written
// by the SessionStart hook) in the project root, CWD, the per-agent root, and
// finally $CLAUDE_PROJECT_DIR.
//
// I-1631: the per-agent root (cfg.AgentRoot()) is where the hook actually
// writes the file — it equals $CLAUDE_PROJECT_DIR but is resolvable without the
// env var, which the Bash-tool shell does not inherit. c.root and "." both
// resolve to the shared/symlinked workspace (no per-session file), so before
// this entry every fallback missed and SessionID() returned empty, forcing
// agents to hand-export a fabricated id. AgentRoot() walks up to the
// .as/agent-workspace.yaml marker (ST_ROOT-leak immune) and is read-only, so
// adding it only resolves more cases; empty/unresolvable dirs are skipped.
func (c *Config) SessionID() string {
	if id := os.Getenv("AS_SESSION_ID"); id != "" {
		return id
	}
	// Fallback: read from session file (written by startup hook).
	// Project root (st workspace) and CWD first for back-compat, then the
	// per-agent root (AgentRoot) where the hook actually writes the file.
	for _, dir := range []string{c.root, ".", c.AgentRoot()} {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, ".as", "session")
		data, err := os.ReadFile(path)
		if err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id
			}
		}
	}
	// Also check $CLAUDE_PROJECT_DIR/.as/session (agent's project directory)
	if projDir := os.Getenv("CLAUDE_PROJECT_DIR"); projDir != "" {
		path := filepath.Join(projDir, ".as", "session")
		data, err := os.ReadFile(path)
		if err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id
			}
		}
	}
	return ""
}

// SessionsDir returns the path to the sessions metadata directory.
func (c *Config) SessionsDir() string {
	return filepath.Join(c.root, ".as", "sessions")
}

// AgentsDir returns the path to the per-process agent registration
// directory (.as/agents/). Each live st run / st start writes a
// <agent-id>.yaml here on startup; the file is removed on clean exit
// and swept on the next startup if the PID is dead. T-311.
func (c *Config) AgentsDir() string {
	return filepath.Join(c.root, ".as", "agents")
}

// WorktreeBase returns the directory under which `<id>` worktree dirs live
// for this agent. I-407: placed under the agent root (one level up from
// the workspace) rather than inside the workspace itself, because the
// workspace is symlinked across agents (I-418). Putting worktrees under
// the workspace would mean agent-a and agent-b share a single physical
// worktree dir and collide on any item id. Returns "" when worktree
// integration is disabled or BaseDir is empty (so callers don't
// accidentally write into the agent root).
//
// I-778: routed through AgentRoot() rather than filepath.Dir(c.root) so
// WorktreeBase and the worktree call sites stay consistent under an
// ST_ROOT-leaked cfg.root — otherwise createWorktrees would mkdir the
// worktree under the peer agent's tree while git worktree-add ran
// against the real agent's clone.
func (c *Config) WorktreeBase() string {
	if c.Worktree == nil || !c.Worktree.Enabled || c.Worktree.BaseDir == "" {
		return ""
	}
	agentRoot := c.AgentRoot()
	if agentRoot == "" {
		return ""
	}
	return filepath.Join(agentRoot, c.Worktree.BaseDir)
}

// AgentRoot returns the per-agent root directory — the dir that holds
// per-agent worktrees, .as/agent-workspace.yaml, and (by default) the
// per-agent source repo clones. NEVER overridden by worktree.parent_dir;
// that field overrides where REPOS live, which is a separate concept
// (see RepoParent). I-778.
//
// Resolution order:
//  1. Walk up from c.startDir looking for the per-agent marker
//     `.as/agent-workspace.yaml`. The marker MUST satisfy all three
//     I-778 sanity checks:
//       (a) top-level `path:` field — indented `path:` keys nested
//           inside other YAML mappings are rejected,
//       (b) `agent_id:` matching c.trustedAgentID() — only env-var or
//           local-config sourced IDs are trusted (path-inferred IDs
//           derive from cfg.root, which is the exact thing the leak
//           corrupts, so they would defeat recovery),
//       (c) `path:` resolves to an existing absolute directory on disk.
//  2. Walk up from filepath.Dir(c.root) with the same checks —
//     handles the case where startDir was the workspace itself but
//     ST_ROOT had redirected discovery to a peer's workspace.
//  3. Fall back to filepath.Dir(c.root) — the I-407 default.
//
// Result is memoized; the directory layout is immutable for the
// lifetime of a Config so the walk runs at most once per session.
// Use ResetAgentRootCache() in tests that mutate cfg.root / cfg.startDir.
func (c *Config) AgentRoot() string {
	if c.agentRootResolved {
		return c.agentRootCache
	}
	resolved := c.resolveAgentRoot()
	c.agentRootCache = resolved
	c.agentRootResolved = true
	return resolved
}

// RepoParent returns the directory under which the configured source
// repo clones live (joined with the repo's mapped name to get the
// repo dir). Defaults to AgentRoot() so repos sit one level up from
// the workspace per the I-418 layout, but is overridden by
// worktree.parent_dir when set: absolute paths are honored verbatim
// (operator escape hatch for non-standard layouts); relative paths
// are joined with c.root for back-compat with pre-I-778 `parent_dir:
// ..` configs. I-778.
//
// This is the helper the 7+3 worktree call sites that previously
// inlined `cfg.Worktree.ParentDir → cfg.Root() + ParentDir` should
// call. AgentRoot() is for code that needs the per-agent root
// regardless of where repos live (e.g., WorktreeBase, the freshness
// gate's sibling-repo probe, the single-agent drift warning).
func (c *Config) RepoParent() string {
	if c.Worktree != nil && c.Worktree.ParentDir != "" {
		if filepath.IsAbs(c.Worktree.ParentDir) {
			return c.Worktree.ParentDir
		}
		if c.root != "" {
			// Try the walk first: if startDir recovers a per-agent
			// marker, prefer it over joining the leaked c.root with
			// the relative override (the ST_ROOT-leak repro).
			if found := c.AgentRoot(); found != "" && found != filepath.Dir(c.root) {
				return found
			}
			return filepath.Join(c.root, c.Worktree.ParentDir)
		}
		return c.Worktree.ParentDir
	}
	return c.AgentRoot()
}

// ResetAgentRootCache discards the memoized AgentRoot result. Intended
// for tests that mutate cfg.root / cfg.startDir / cfg.Worktree after
// construction. I-778.
func (c *Config) ResetAgentRootCache() {
	c.agentRootCache = ""
	c.agentRootResolved = false
}

func (c *Config) resolveAgentRoot() string {
	wantAgentID := c.trustedAgentID()
	if found := walkForAgentRoot(c.startDir, wantAgentID); found != "" {
		return found
	}
	if c.root != "" {
		if found := walkForAgentRoot(filepath.Dir(c.root), wantAgentID); found != "" {
			return found
		}
		return filepath.Dir(c.root)
	}
	return ""
}

// trustedAgentID returns the agent id only when sourced from a
// channel that doesn't depend on cfg.root (env var or local config
// file). Path-inferred IDs are deliberately omitted because they
// derive from cfg.root and would defeat the I-778 recovery (a leaked
// cfg.root makes path-inference name the peer agent, which would
// then reject the real agent's marker). I-778.
func (c *Config) trustedAgentID() string {
	id := c.Identity()
	switch id.Source {
	case "env", "local-config":
		return id.ID
	}
	return ""
}

// walkForAgentRoot walks up from dir looking for the per-agent marker
// .as/agent-workspace.yaml. The returned path: value must satisfy:
//   - top-level `path:` (no leading indentation — flat YAML only)
//   - absolute and resolvable to an existing directory on disk
//   - matching `agent_id:` (when wantAgentID != "")
//
// No boundary stop is enforced: the per-marker identity sanity check
// (matching agent_id) is what prevents a stray marker upstream of a
// workspace from hijacking the result. The walk terminates naturally
// when it hits filesystem root. I-778.
func walkForAgentRoot(dir, wantAgentID string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil || abs == "" {
		return ""
	}
	dir = abs
	for {
		candidate := filepath.Join(dir, ".as", "agent-workspace.yaml")
		if body, err := os.ReadFile(candidate); err == nil {
			path, gotAgentID := parseAgentWorkspaceMarker(body)
			if path != "" && filepath.IsAbs(path) {
				if fi, err := os.Stat(path); err == nil && fi.IsDir() {
					if wantAgentID == "" || gotAgentID == "" || gotAgentID == wantAgentID {
						return path
					}
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// agentIDFromWorkspaceMarker walks up from dir looking for
// .as/agent-workspace.yaml and returns the first non-empty agent_id: found.
// Unlike walkForAgentRoot it does NOT validate the path: field, which avoids
// a circular dependency with trustedAgentID() → Identity(). I-936.
func agentIDFromWorkspaceMarker(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(abs, ".as", "agent-workspace.yaml")
		if body, err := os.ReadFile(candidate); err == nil {
			_, agentID := parseAgentWorkspaceMarker(body)
			if agentID != "" {
				return agentID
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

// parseAgentWorkspaceMarker extracts the top-level `path:` and `agent_id:`
// fields from a flat agent-workspace.yaml. Lines with any leading
// whitespace are ignored so a nested `path:` key (e.g., under a future
// `repos:` block) can't be mistaken for the top-level field. I-778.
func parseAgentWorkspaceMarker(body []byte) (path string, agentID string) {
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		// Top-level only: reject indented lines so nested mappings
		// (e.g. repos:\n  path: ...) cannot satisfy the parser.
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		switch k {
		case "path":
			if path == "" {
				path = v
			}
		case "agent_id":
			if agentID == "" {
				agentID = v
			}
		}
	}
	return path, agentID
}

// WorktreeBaseLegacy returns the pre-I-407 worktree location (under the
// workspace, shared across agents via the I-418 symlink). Used by
// finish/close as a fallback when an old worktree predates the I-407
// fix and needs to be cleaned up from its original location. Returns
// "" when worktree integration is disabled.
func (c *Config) WorktreeBaseLegacy() string {
	if c.Worktree == nil || !c.Worktree.Enabled || c.Worktree.BaseDir == "" {
		return ""
	}
	return filepath.Join(c.root, c.Worktree.BaseDir)
}

// WorktreeForItem returns the worktree directory for the given item id,
// preferring the new agent-root location but falling back to the legacy
// shared-workspace location when an existing worktree predates the
// I-407 fix. When neither exists (e.g., callers checking for an
// expected-but-not-yet-created worktree), returns the new path so
// downstream "create" logic uses the post-fix layout. Returns "" when
// worktree integration is disabled.
//
// Read sites (run.go, uat.go, prep.go, testrecord.go, gitdiff.go)
// should use this; write sites (st start) should use WorktreeBase()
// directly so newly-created worktrees always land in the new location.
func (c *Config) WorktreeForItem(id string) string {
	base := c.WorktreeBase()
	if base == "" || id == "" {
		return ""
	}
	newDir := filepath.Join(base, id)
	if _, err := os.Stat(newDir); err == nil {
		return newDir
	}
	if legacy := c.WorktreeBaseLegacy(); legacy != "" {
		legacyDir := filepath.Join(legacy, id)
		if _, err := os.Stat(legacyDir); err == nil {
			return legacyDir
		}
	}
	return newDir
}

// StaleClaimTTL returns the stale claim threshold in seconds.
// Defaults to 7200 (2 hours) if not configured.
func (c *Config) StaleClaimTTL() int {
	if c.Sprints != nil && c.Sprints.StaleClaimTTL > 0 {
		return c.Sprints.StaleClaimTTL
	}
	return 7200
}

// StaleActiveHours returns the threshold (in hours) after which a
// status=active item with no recent changelog activity is eligible
// for auto-release by st reconcile. Resolution order:
//  1. sprints.stale_active_hours in .as/config.yaml (when > 0)
//  2. ST_STALE_ACTIVE_HOURS env var
//  3. ST_STALE_ACTIVE_DAYS env var × 24 (backward compat)
//  4. Default: 6 (hours). I-874.
func (c *Config) StaleActiveHours() int {
	if c.Sprints != nil && c.Sprints.StaleActiveHours > 0 {
		return c.Sprints.StaleActiveHours
	}
	if env := os.Getenv("ST_STALE_ACTIVE_HOURS"); env != "" {
		var h int
		if _, err := fmt.Sscanf(env, "%d", &h); err == nil && h > 0 {
			return h
		}
	}
	if env := os.Getenv("ST_STALE_ACTIVE_DAYS"); env != "" {
		var days int
		if _, err := fmt.Sscanf(env, "%d", &days); err == nil && days > 0 {
			return days * 24
		}
	}
	return 6
}

// ValidStatuses returns the allowed statuses for a given item type.
func (c *Config) ValidStatuses(itemType string) []string {
	if tc, ok := c.Types[itemType]; ok {
		return tc.Statuses
	}
	return nil
}

// DirectoryForStatus returns the directory an item should be in for a given type+status.
func (c *Config) DirectoryForStatus(itemType, status string) string {
	if tc, ok := c.Types[itemType]; ok {
		if dir, ok := tc.DirectoryMap[status]; ok {
			return dir
		}
	}
	return ""
}

// IsTerminalStatus returns true if the status is terminal for the given type.
func (c *Config) IsTerminalStatus(itemType, status string) bool {
	tc, ok := c.Types[itemType]
	if !ok {
		return false
	}
	for _, ts := range tc.TerminalStatuses {
		if ts == status {
			return true
		}
	}
	return false
}

// StageReached returns true if current delivery stage is at or past the required stage.
func (c *Config) StageReached(current, required string) bool {
	if c.Delivery == nil || current == "" {
		return false
	}
	currentIdx := -1
	requiredIdx := -1
	for i, s := range c.Delivery.Stages {
		if s == current {
			currentIdx = i
		}
		if s == required {
			requiredIdx = i
		}
	}
	return currentIdx >= 0 && requiredIdx >= 0 && currentIdx >= requiredIdx
}

// Defaults returns a Config with sensible defaults that work out of the box.
func Defaults() *Config {
	return &Config{
		Project: ProjectConfig{
			Name: "project",
		},
		Paths: PathsConfig{
			Root:      ".",
			Templates: "templates",
			Changelog: ".changelog",
			Index:     "index.md",
		},
		Types: map[string]TypeConfig{
			// I-433: tasks and issues share the same status vocabulary —
			// queued / active / done / abandoned / archived. The legacy
			// per-type vocabularies (tasks: completed; issues: open /
			// resolved / wontfix) were renamed and backfilled by
			// cmd/migrate-status-vocab. Items still live in their
			// type-specific directories during start (tasks/, issues/);
			// terminal statuses route to archive/ as before.
			"task": {
				IDPrefix: "T",
				// T-346: `awaiting_decision` is a non-terminal pause state
				// — set by the binary autonomy loop when the classifier
				// returns red, cleared by `st decide`. Stays in tasks/
				// (not archived) so it's visible in the normal scope.
				Statuses:         []string{"queued", "active", "awaiting_decision", "done", "abandoned", "archived"},
				StartStatus:      "queued",
				ActiveStatus:     "active",
				TerminalStatuses: []string{"done", "abandoned", "archived"},
				RequiredFields:   []string{"depends_on", "blocks"},
				DirectoryMap: map[string]string{
					"queued":            "tasks",
					"active":            "tasks",
					"awaiting_decision": "tasks",
					"done":              "archive",
					"abandoned":         "archive",
					"archived":          "archive",
				},
			},
			"issue": {
				IDPrefix:         "I",
				Statuses:         []string{"queued", "active", "awaiting_decision", "done", "abandoned", "archived"},
				StartStatus:      "queued",
				ActiveStatus:     "active",
				TerminalStatuses: []string{"done", "abandoned", "archived"},
				// I-406: severity dropped in favor of priority (p0-p4
				// scale, shared with tasks). Priority isn't required at
				// the schema level — create.go fills a default of 2.
				RequiredFields: []string{"depends_on", "blocks"},
				DirectoryMap: map[string]string{
					"queued":            "issues",
					"active":            "issues",
					"awaiting_decision": "issues",
					"done":              "archive",
					"abandoned":         "archive",
					"archived":          "archive",
				},
			},
			"idea": {
				IDPrefix:         "D",
				Statuses:         []string{"captured", "promoted", "declined"},
				StartStatus:      "captured",
				TerminalStatuses: []string{"promoted", "declined"},
				DirectoryMap: map[string]string{
					"captured": "ideas",
					"promoted": "ideas",
					"declined": "archive",
				},
			},
			"promotion": {
				IDPrefix:         "P",
				Statuses:         []string{"archived"},
				TerminalStatuses: []string{"archived"},
				DirectoryMap: map[string]string{
					"archived": "archive",
				},
			},
			"goal": {
				IDPrefix:         "G",
				Statuses:         []string{"draft", "active", "met", "dropped", "archived"},
				StartStatus:      "draft",
				ActiveStatus:     "active",
				TerminalStatuses: []string{"met", "dropped", "archived"},
				DirectoryMap: map[string]string{
					"draft":    "goals",
					"active":   "goals",
					"met":      "archive",
					"dropped":  "archive",
					"archived": "archive",
				},
			},
		},
		IDPatterns: map[string]string{
			"task":      "T-{seq}",
			"issue":     "I-{seq}",
			"idea":      "D-{seq}",
			"promotion": "P-{seq}",
			"goal":      "G-{seq}",
		},
		Fields: FieldsConfig{
			Required: []string{"id", "type", "status", "title", "created", "last_touched"},
			Computed: []ComputedField{
				{Name: "blocks", Source: "inverse(depends_on)"},
			},
		},
		Git: &GitConfig{
			AutoCommit: true,
			AutoPush:   true,
			LockFile:   ".as.lock",
		},
	}
}

// Load discovers and loads configuration.
// Search order: walk up from startDir → $ST_ROOT env var → defaults rooted at startDir.
func Load(startDir string) (*Config, error) {
	cfg := Defaults()
	cfg.startDir, _ = filepath.Abs(startDir)

	configPath, found, explicitTarget := discover(startDir)
	if !found {
		// Fallback: check ST_ROOT env var. ST_ROOT explicitly names the state
		// root, so — like a `.st-root` redirect or `--config` — a config reached
		// through it is an operator override and must NOT be canonicalized away.
		if root := os.Getenv("ST_ROOT"); root != "" {
			configPath, found, _ = discover(root)
			if found {
				explicitTarget = true
			}
		}
	}

	if found {
		if err := parseConfigFile(cfg, configPath); err != nil {
			return nil, fmt.Errorf("loading %s: %w", configPath, err)
		}
		cfg.root = filepath.Dir(filepath.Dir(configPath)) // .as/config.yaml -> project root
		cfg.Discovered = true
	} else {
		cfg.root = startDir
	}

	// I-1596: identity-anchor the state root for an ordinary discovery only — not
	// an explicit --config/.st-root/ST_ROOT override, and not the no-config
	// startDir fallback. When the discovered root is a worktree snapshot, RE-LOAD
	// from the canonical store's own config.yaml so cfg's config values and
	// cfg.root come from the SAME store: never frozen-snapshot config applied over
	// live canonical state (split-brain).
	if cfg.Discovered && !explicitTarget {
		if canonical := cfg.canonicalStateRoot(); canonical != "" {
			canonicalCfg := Defaults()
			canonicalCfg.startDir = cfg.startDir
			if err := parseConfigFile(canonicalCfg, filepath.Join(canonical, ".as", "config.yaml")); err != nil {
				return nil, fmt.Errorf("loading canonical config under %s: %w", canonical, err)
			}
			canonicalCfg.root = canonical
			canonicalCfg.Discovered = true
			cfg = canonicalCfg
		}
	}
	return cfg, nil
}

// LoadFrom loads config from a specific path. Used with --config flag.
func LoadFrom(configPath string) (*Config, error) {
	cfg := Defaults()
	if err := parseConfigFile(cfg, configPath); err != nil {
		return nil, fmt.Errorf("loading %s: %w", configPath, err)
	}
	cfg.root = filepath.Dir(filepath.Dir(configPath))
	// startDir must be CWD (un-tainted by ST_ROOT), not the config-derived root.
	// agentIDFromWorkspaceMarker walks up from startDir to find the real agent
	// identity — using cfg.root here would re-introduce the ST_ROOT leak. I-936.
	if cwd, err := os.Getwd(); err == nil {
		cfg.startDir = cwd
	} else {
		cfg.startDir = cfg.root
	}
	// I-1596: do NOT canonicalize here. `--config <path>` is an explicit operator
	// override — the whole point is to target that exact store (e.g. to inspect or
	// repair a worktree snapshot). Redirecting it to the canonical workspace would
	// defeat the override and silently read/write the wrong store.
	return cfg, nil
}

// canonicalStateRoot returns the canonical per-agent workspace that cfg.root
// should be redirected to — resolved from the agent-workspace identity
// (.as/agent-workspace.yaml at the agent root) — or "" if no redirect applies.
// It is PURE: the caller (Load) re-loads config from the returned store so that
// cfg.root and cfg's config values always come from the same place. I-1596 / Inv 1/2.
//
// discover() walks up from CWD for .as/config.yaml, so running st from inside a
// worktree (<agent-root>/worktrees/<id>/theraprac-workspace) resolves cfg.root to
// the worktree's FROZEN agent-state snapshot. Because every state accessor
// (ItemDir/ChangelogDir/PlansDir/...) joins cfg.root, all reads/writes would then
// hit the snapshot instead of the one canonical store. Re-anchor cfg.root to the
// workspace directly under the identity-resolved agent root.
//
// AgentRoot() walks .as/agent-workspace.yaml from startDir (ST_ROOT-immune,
// agent_id-validated). The marker lives at the agent root, not inside the
// theraprac-workspace repo, so a worktree checkout never carries it — AgentRoot()
// resolves the real agent root even from a worktree CWD.
//
// The redirect fires ONLY when ALL of the following hold (each guard closes a
// way the redirect could otherwise hijack a store the caller meant to target):
//
//   - The config was discovered by an ordinary walk-up, NOT an explicit override
//     (`--config` via LoadFrom, a `.st-root` redirect file, or an ST_ROOT target)
//     and NOT the no-config startDir fallback. Enforced by the caller (Load gates
//     on Discovered && !explicitTarget; LoadFrom never calls this).
//   - cfg.root is a strict nested descendant of the resolved agent root — i.e. an
//     actual per-agent worktree snapshot of THIS agent. AgentRoot() resolves from
//     startDir/CWD, which (e.g. under `go test`) can be inside a real agent tree
//     while cfg.root points at an unrelated temp workspace; the descendant check
//     stops us hijacking that unrelated root into a bogus <realAgentRoot>/<base>.
//   - cfg.root is not already the canonical workspace (inode-equal → no-op).
//   - The canonical target <agentRoot>/<base> ALREADY EXISTS as a real store
//     (.as/config.yaml present). Without this, a worktree whose basename has no
//     matching sibling under the agent root, or a main clone that isn't checked
//     out yet, would repoint root at a phantom directory — every state accessor
//     would then read an empty store or MkdirAll a stray one (apparent data loss).
//
// All "is X the same dir as / under Y" checks compare by INODE (os.SameFile),
// not by string prefix: the agent-workspace marker's path: and the discovered
// cfg.root routinely differ in string form (case-insensitive macOS — Dev vs dev
// — and the I-418 workspace symlink) while naming the same directory. A
// string HasPrefix check silently fails to fire in exactly those cases (caught
// in live acceptance: a worktree-cwd record still landed in the frozen copy).
func (c *Config) canonicalStateRoot() string {
	if c.root == "" {
		return ""
	}
	agentRoot := c.AgentRoot()
	if agentRoot == "" {
		return ""
	}
	canonical := filepath.Join(agentRoot, filepath.Base(c.root))
	if sameDir(canonical, c.root) {
		return "" // already the one canonical workspace (inode-equal)
	}
	if !dirIsUnder(c.root, agentRoot) {
		return "" // cfg.root lives outside this agent's tree — never touch it
	}
	// Only redirect to a canonical workspace that actually exists and is a real
	// store. Guards the phantom-directory case where <agentRoot>/<base> is absent
	// (missing main clone, or a worktree basename with no top-level sibling):
	// repointing there would lose all existing state behind an empty/stray store.
	if _, err := os.Stat(filepath.Join(canonical, ".as", "config.yaml")); err != nil {
		return ""
	}
	return canonical
}

// sameDir / dirIsUnder compare paths by INODE (os.SameFile) rather than by
// string. This is deliberate and not duplication-by-accident with the
// EvalSymlinks+ToLower helpers in internal/store/git.go (I-835): config is a
// lower-level package that store imports, so it cannot import store back without
// a cycle; and inode comparison is strictly more robust here — it tolerates BOTH
// the macOS case-insensitive (Dev vs dev) and symlink (I-418) path-string
// differences with one mechanism, where a lowercase+EvalSymlinks string compare
// can still diverge on case the resolver does not normalize.

// sameDir reports whether a and b name the same directory by inode, tolerating
// case-insensitive and symlinked path-string differences. False if either is
// unstat-able.
func sameDir(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// dirIsUnder reports whether dir is a strict descendant of ancestor, comparing
// each parent by inode (os.SameFile) rather than string prefix — immune to
// case/symlink path-string differences. I-1596.
//
// The walk is bounded by dir's own depth (it terminates at the filesystem root)
// and runs at most a handful of os.Stat calls per Load — only on the discovered,
// non-override path. We deliberately do NOT add a string-length / separator-depth
// early-exit: under the symlink/case differences this inode walk exists to handle,
// the lexical parent of a true descendant can be longer OR shorter than ancestor's
// string form, so a length bound risks a false negative — silently reinstating the
// frozen-snapshot reads this fix removes. Correctness over saving a few stats.
func dirIsUnder(dir, ancestor string) bool {
	aStat, err := os.Stat(ancestor)
	if err != nil {
		return false
	}
	cur := dir
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return false // reached filesystem root without matching
		}
		if pStat, err := os.Stat(parent); err == nil && os.SameFile(pStat, aStat) {
			return true
		}
		cur = parent
	}
}

// discover walks up from dir looking for .as/config.yaml.
// If a .st-root file is found first, its content is used as a redirect path.
// viaRedirect reports whether the returned config was reached by following a
// .st-root redirect file (an explicit operator override) rather than a direct
// .as/config.yaml hit — I-1596 canonicalization must defer to that override.
func discover(dir string) (path string, found bool, viaRedirect bool) {
	dir, _ = filepath.Abs(dir)
	for {
		candidate := filepath.Join(dir, ".as", "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true, false
		}
		// Check for .st-root redirect file
		rootFile := filepath.Join(dir, ".st-root")
		if data, err := os.ReadFile(rootFile); err == nil {
			target := strings.TrimSpace(string(data))
			if target != "" {
				if !filepath.IsAbs(target) {
					target = filepath.Join(dir, target)
				}
				redirected := filepath.Join(target, ".as", "config.yaml")
				if _, err := os.Stat(redirected); err == nil {
					return redirected, true, true
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, false
		}
		dir = parent
	}
}

// parseConfigFile reads a simple YAML-like config file and applies values to cfg.
// We use a simple line-based parser to maintain zero external dependencies.
// Supports up to 4 levels of nesting via indent tracking.
func parseConfigFile(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var levels [4]string // section hierarchy: [section, subsection, subkey, prop]

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		level := indent / 2
		if level > 3 {
			level = 3
		}

		// Clear deeper levels when moving to a shallower level
		for i := level + 1; i < 4; i++ {
			levels[i] = ""
		}

		// Handle list items (- value)
		if strings.HasPrefix(trimmed, "- ") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			val = strings.Trim(val, `"'`)
			applyListItem(cfg, levels, val)
			continue
		}

		key, val := splitKV(trimmed)
		levels[level] = key

		// Handle inline lists: [a, b, c]
		if strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]") {
			items := parseInlineList(val)
			applyInlineList(cfg, levels, key, items)
			continue
		}

		if val != "" {
			applyValue(cfg, levels, key, val)
		}
	}

	return scanner.Err()
}

// parseInlineList parses [a, b, c] into a string slice.
func parseInlineList(val string) []string {
	inner := val[1 : len(val)-1]
	parts := strings.Split(inner, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"'`)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func splitKV(line string) (string, string) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return line, ""
	}
	key := strings.TrimSpace(line[:idx])
	val := strings.TrimSpace(line[idx+1:])
	// Strip inline comments
	if ci := strings.Index(val, " #"); ci >= 0 {
		val = strings.TrimSpace(val[:ci])
	}
	// Strip quotes
	val = strings.Trim(val, `"'`)
	return key, val
}

// applyValue routes a scalar value to the appropriate config field based on nesting level.
func applyValue(cfg *Config, levels [4]string, key, val string) {
	switch levels[0] {
	// Top-level scalars (backward compat: name: val at indent 0)
	case "name":
		cfg.Project.Name = val
	case "description":
		cfg.Project.Description = val

	case "guidance":
		cfg.Guidance = val

	case "project":
		switch key {
		case "name":
			cfg.Project.Name = val
		case "description":
			cfg.Project.Description = val
		}

	case "paths":
		switch key {
		case "root":
			cfg.Paths.Root = val
		case "templates":
			cfg.Paths.Templates = val
		case "changelog":
			cfg.Paths.Changelog = val
		case "index":
			cfg.Paths.Index = val
		}

	case "git":
		if cfg.Git == nil {
			cfg.Git = &GitConfig{}
		}
		switch key {
		case "auto_commit":
			cfg.Git.AutoCommit = val == "true"
		case "auto_push":
			cfg.Git.AutoPush = val == "true"
		case "lock_file":
			cfg.Git.LockFile = val
		}

	case "testing":
		ensureTesting(cfg)
		switch levels[1] {
		case "required_suites":
			if val != "" && key != "command" && key != "artifacts" {
				// Simple format: suite_name: command_string
				cfg.Testing.RequiredSuites[key] = SuiteConfig{Command: val}
			} else if val != "" {
				// Nested format field: command or artifacts under suite_name
				suiteName := levels[2]
				sc := cfg.Testing.RequiredSuites[suiteName]
				if key == "command" {
					sc.Command = val
				}
				cfg.Testing.RequiredSuites[suiteName] = sc
			}
			// val == "" means this is a section header (suite name), levels tracks it
		case "scope_suites":
			if val != "" && key != "command" && key != "artifacts" && key != "post_deploy" && key != "post_merge" && key != "repo_trigger" && key != "env_from" {
				// Simple format
				cfg.Testing.ScopeSuites[key] = ScopeSuiteConfig{Command: val}
			} else if val != "" {
				// Nested format field
				suiteName := levels[2]
				sc := cfg.Testing.ScopeSuites[suiteName]
				switch key {
				case "command":
					sc.Command = val
				case "post_deploy":
					sc.PostDeployCmd = val
				case "post_merge":
					sc.PostMergeCmd = val
				case "repo_trigger":
					sc.RepoTrigger = val
				case "env_from":
					sc.EnvFrom = val
				}
				cfg.Testing.ScopeSuites[suiteName] = sc
			}
		case "coverage_thresholds":
			if cfg.Testing.CoverageThresholds == nil {
				cfg.Testing.CoverageThresholds = &CoverageThresholds{Lines: 90, Branches: 80, Functions: 100}
			}
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				switch key {
				case "lines":
					cfg.Testing.CoverageThresholds.Lines = v
				case "branches":
					cfg.Testing.CoverageThresholds.Branches = v
				case "functions":
					cfg.Testing.CoverageThresholds.Functions = v
				}
			}
		case "scope_classes":
			// I-776: flat shape only — testing → scope_classes → <class> → <suite>: <cmd>.
			// Three failure modes have to be detected explicitly because the
			// shared line parser would otherwise mis-shape them into a phantom
			// class or phantom suite:
			//
			// 1. Missing class header: `scope_classes:\n  workspace_test: cmd`
			//    levels[2] gets set to the suite key on this iteration, so the
			//    `className == ""` defense doesn't fire on its own. We reject
			//    by requiring at least 4 populated levels[].
			//
			// 2. Nested command form: `<class>:\n  <suite>:\n    command: cmd`
			//    The deepest line is at indent 8 which the parser clamps to
			//    level=3, overwriting levels[3]="<suite>" with "command".
			//    Reject the literal key "command" (and the other nested fields
			//    SuiteConfig supports) so the operator gets a loud error
			//    instead of a phantom suite named "command".
			//
			// 3. Section header lines (val == "") are no-ops — the lazy
			//    map-create below handles class registration on the first
			//    leaf line.
			if val == "" {
				return
			}
			className := levels[2]
			// Detect form (1): if levels[2] is empty, this is a leaf with no
			// class header above it. Also reject if levels[2] is the same as
			// the key — that's the same-iteration mis-assignment shape, e.g.
			// `scope_classes:\n  workspace_test: cmd` where levels[2]="workspace_test"
			// because the parser just wrote it.
			if className == "" || className == key {
				fmt.Fprintf(os.Stderr,
					"warning: malformed scope_classes entry %q at indent under scope_classes — every suite must be nested under a class name (testing.scope_classes.<class>.<suite>: <cmd>); dropping\n",
					key)
				return
			}
			// Detect form (2): nested suite shape would surface SuiteConfig
			// field names (command/artifacts) as the leaf key. Reject loudly
			// — only the flat shape is supported in v1.
			if key == "command" || key == "artifacts" {
				fmt.Fprintf(os.Stderr,
					"warning: scope_classes does not support the nested suite form (%q under %q.%q); use the flat shape (<class>.<suite>: <cmd>) — dropping\n",
					key, className, levels[3])
				return
			}
			// applies_to_goals is only valid as an inline list [a, b]; a scalar
			// form reaches applyValue — guard and drop rather than registering a
			// phantom suite named "applies_to_goals".
			if key == "applies_to_goals" {
				fmt.Fprintf(os.Stderr,
					"warning: applies_to_goals under scope_classes.%s must be an inline list (e.g. [st-tooling]), not a scalar %q; dropping\n",
					className, val)
				return
			}
			class, ok := cfg.Testing.ScopeClasses[className]
			if !ok {
				class = ScopeClassConfig{RequiredSuites: make(map[string]SuiteConfig)}
			} else if class.RequiredSuites == nil {
				class.RequiredSuites = make(map[string]SuiteConfig)
			}
			// applies_to_goals with inline-list value [a, b] routes through
			// applyInlineList (not here) — see the testing case there.
			class.RequiredSuites[key] = SuiteConfig{Command: val}
			cfg.Testing.ScopeClasses[className] = class
		}

	case "pipeline":
		if cfg.Pipeline == nil {
			cfg.Pipeline = &PipelineConfig{}
		}
		var step **PipelineStepConfig
		switch levels[1] {
		case "merge":
			step = &cfg.Pipeline.Merge
		case "deploy_check":
			step = &cfg.Pipeline.DeployCheck
		case "smoke":
			step = &cfg.Pipeline.Smoke
		}
		if step != nil {
			if *step == nil {
				*step = &PipelineStepConfig{}
			}
			switch key {
			case "command":
				(*step).Command = val
			case "post_record":
				(*step).PostRecord = val
			case "health_url":
				(*step).HealthURL = val
			case "timeout":
				if v, err := strconv.Atoi(val); err == nil {
					(*step).Timeout = v
				}
			case "watch_ci":
				(*step).WatchCI = val == "true"
			}
		}

	case "scope_classes":
		// I-776: scope_classes must live UNDER testing:. A bare top-level
		// `scope_classes:` block is a common YAML mistake when adding a new
		// section — warn loudly instead of silently dropping it, otherwise
		// items declaring scope_class get "unknown scope_class" with no
		// diagnostic distinguishing 'malformed config' from 'typo on item'.
		fmt.Fprintf(os.Stderr,
			"warning: scope_classes:%q at top level — scope_classes must live under testing: (testing.scope_classes.<class>.<suite>); dropping\n",
			key)

	case "evidence":
		if cfg.Evidence == nil {
			cfg.Evidence = &EvidenceConfig{}
		}
		switch key {
		case "backend":
			cfg.Evidence.Backend = val
		case "local_dir":
			cfg.Evidence.LocalDir = val
		case "s3_bucket":
			cfg.Evidence.S3Bucket = val
		case "s3_region":
			cfg.Evidence.S3Region = val
		case "s3_prefix":
			cfg.Evidence.S3Prefix = val
		case "s3_profile":
			cfg.Evidence.S3Profile = val
		}

	case "delivery":
		if cfg.Delivery == nil {
			cfg.Delivery = &DeliveryConfig{}
		}
		switch key {
		case "archive_gate":
			cfg.Delivery.ArchiveGate = val
		}

	case "worktree":
		if cfg.Worktree == nil {
			cfg.Worktree = &WorktreeConfig{RepoMap: make(map[string]string)}
		}
		switch key {
		case "enabled":
			cfg.Worktree.Enabled = val == "true"
		case "base_dir":
			cfg.Worktree.BaseDir = val
		case "parent_dir":
			cfg.Worktree.ParentDir = val
		}

	case "sprints":
		if cfg.Sprints == nil {
			cfg.Sprints = &SprintsConfig{}
		}
		switch key {
		case "stale_claim_ttl":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.Sprints.StaleClaimTTL = v
			}
		case "stale_active_hours":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.Sprints.StaleActiveHours = v
			}
		}

	case "run":
		ensureRun(cfg)
		switch levels[1] {
		case "":
			// Top-level run scalars
			switch key {
			case "permission_mode":
				cfg.Run.PermissionMode = val
			case "default_model":
				cfg.Run.DefaultModel = val
			case "max_parallelism":
				if v, err := strconv.Atoi(val); err == nil {
					cfg.Run.MaxParallelism = v
				}
			case "default_budget_usd":
				if v, err := strconv.ParseFloat(val, 64); err == nil {
					cfg.Run.DefaultBudgetUSD = v
				}
			case "auto_parallel":
				cfg.Run.AutoParallel = val == "true"
			}
		case "steps":
			// levels[2] = step name, key = field name
			stepName := levels[2]
			if stepName != "" && val != "" {
				step := cfg.Run.Steps[stepName]
				switch key {
				case "type":
					step.Type = val
				case "command":
					step.Command = val
				case "prompt":
					step.Prompt = val
				case "resolution":
					step.Resolution = val
				case "timeout":
					if v, err := strconv.Atoi(val); err == nil {
						step.Timeout = v
					}
				case "coverage":
					step.Coverage = val == "true"
				case "budget":
					if v, err := strconv.ParseFloat(val, 64); err == nil {
						step.Budget = v
					}
				}
				cfg.Run.Steps[stepName] = step
			}
		}
	}
}

// applyInlineList routes an inline list [a, b, c] to the appropriate config field.
func applyInlineList(cfg *Config, levels [4]string, key string, items []string) {
	switch levels[0] {
	case "delivery":
		if key == "stages" {
			if cfg.Delivery == nil {
				cfg.Delivery = &DeliveryConfig{}
			}
			cfg.Delivery.Stages = items
		}
	case "worktree":
		if key == "repos" {
			if cfg.Worktree == nil {
				cfg.Worktree = &WorktreeConfig{RepoMap: make(map[string]string)}
			}
			cfg.Worktree.Repos = items
		}
	case "pipeline":
		if cfg.Pipeline != nil {
			var step *PipelineStepConfig
			switch levels[1] {
			case "merge":
				step = cfg.Pipeline.Merge
			case "deploy_check":
				step = cfg.Pipeline.DeployCheck
			case "smoke":
				step = cfg.Pipeline.Smoke
			}
			if step != nil {
				switch key {
				case "pre_checks":
					step.PreChecks = items
				case "artifacts":
					step.Artifacts = items
				case "health_urls":
					step.HealthURLs = items
				}
			}
		}
	case "classify":
		ensureClassify(cfg)
		switch key {
		case "deny_path_prefixes":
			cfg.Classify.DenyPathPrefixes = append(cfg.Classify.DenyPathPrefixes, items...)
		case "deny_basename_globs":
			cfg.Classify.DenyBasenameGlobs = append(cfg.Classify.DenyBasenameGlobs, items...)
		}
	case "run":
		ensureRun(cfg)
		switch key {
		case "step_order":
			cfg.Run.StepOrder = items
		case "breakpoints":
			cfg.Run.Breakpoints = items
		}

	case "testing":
		ensureTesting(cfg)
		// I-830: applies_to_goals is an inline list under scope_classes.<class>.
		if key == "applies_to_goals" && levels[1] == "scope_classes" && levels[2] != "" {
			className := levels[2]
			class, ok := cfg.Testing.ScopeClasses[className]
			if !ok {
				class = ScopeClassConfig{RequiredSuites: make(map[string]SuiteConfig)}
			}
			class.AppliesToGoals = items
			cfg.Testing.ScopeClasses[className] = class
			return
		}
		if (key == "artifacts" || key == "triggers" || key == "target_from" || key == "vendor_tiers") && levels[2] != "" {
			suiteName := levels[2]
			switch levels[1] {
			case "required_suites":
				sc := cfg.Testing.RequiredSuites[suiteName]
				if key == "artifacts" {
					sc.Artifacts = items
				}
				cfg.Testing.RequiredSuites[suiteName] = sc
			case "scope_suites":
				sc := cfg.Testing.ScopeSuites[suiteName]
				switch key {
				case "artifacts":
					sc.Artifacts = items
				case "triggers":
					sc.Triggers = items
				case "target_from":
					sc.TargetFrom = items
				case "vendor_tiers":
					sc.VendorTiers = items
				}
				cfg.Testing.ScopeSuites[suiteName] = sc
			}
		}
	}
}

// applyListItem routes a dash-prefixed list item to the appropriate config field.
func applyListItem(cfg *Config, levels [4]string, val string) {
	switch levels[0] {
	case "classify":
		ensureClassify(cfg)
		switch levels[1] {
		case "deny_path_prefixes":
			cfg.Classify.DenyPathPrefixes = append(cfg.Classify.DenyPathPrefixes, val)
		case "deny_basename_globs":
			cfg.Classify.DenyBasenameGlobs = append(cfg.Classify.DenyBasenameGlobs, val)
		}
	}
}

func ensureClassify(cfg *Config) {
	if cfg.Classify == nil {
		cfg.Classify = &ClassifyConfig{}
	}
}

func ensureRun(cfg *Config) {
	if cfg.Run == nil {
		cfg.Run = &RunConfig{
			Steps: make(map[string]RunStepDef),
		}
	}
}

func ensureTesting(cfg *Config) {
	if cfg.Testing == nil {
		cfg.Testing = &TestingConfig{
			RequiredSuites: make(map[string]SuiteConfig),
			ScopeSuites:    make(map[string]ScopeSuiteConfig),
			ScopeClasses:   make(map[string]ScopeClassConfig),
		}
	}
	if cfg.Testing.ScopeClasses == nil {
		cfg.Testing.ScopeClasses = make(map[string]ScopeClassConfig)
	}
}

// RequiredSuiteNames returns required suite names in sorted order.
func (t *TestingConfig) RequiredSuiteNames() []string {
	names := make([]string, 0, len(t.RequiredSuites))
	for name := range t.RequiredSuites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ScopeSuiteNames returns scope suite names in sorted order.
func (t *TestingConfig) ScopeSuiteNames() []string {
	names := make([]string, 0, len(t.ScopeSuites))
	for name := range t.ScopeSuites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
