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

	// Session guidance text (optional, shown in prime output)
	Guidance string

	// Root directory (where .as/ lives, or CWD)
	root string

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
	Artifacts     []string // glob patterns for artifacts to upload after execution
	PostDeployCmd string   // command to run post-deploy verification (e.g., E2E against dev)
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

type SprintsConfig struct {
	StaleClaimTTL int // seconds before a claim is stale (default 7200)
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
	Type       string  // claude, test, pr, merge, merge_precheck, deploy, smoke, uat, gate, close, command
	Command    string  // for command type
	Prompt     string  // for claude type (optional, uses default)
	Resolution string  // for close type (e.g. "completed")
	Timeout    int     // for watch/deploy (seconds, default 600)
	Coverage   bool    // for test type
	Budget     float64 // per-step budget override (USD, 0 = use default)
	name       string  // set by RunPipeline(), not from config
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
//  2. <root>/.as/local-agent.yaml (gitignored, per-workspace)
//  3. parent directory named theraprac-agent-<suffix> (I-383 path derivation)
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

// PlansDir returns the path to the plans sidecar directory.
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
// Checks in order: $AS_SESSION_ID env var, then .as/session file in CWD or project root.
func (c *Config) SessionID() string {
	if id := os.Getenv("AS_SESSION_ID"); id != "" {
		return id
	}
	// Fallback: read from session file (written by startup hook)
	// Check project root first (st workspace), then CWD's .as/ (agent project dir)
	for _, dir := range []string{c.root, "."} {
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
func (c *Config) WorktreeBase() string {
	if c.Worktree == nil || !c.Worktree.Enabled || c.Worktree.BaseDir == "" {
		return ""
	}
	agentRoot := filepath.Dir(c.root)
	return filepath.Join(agentRoot, c.Worktree.BaseDir)
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
			"task": {
				IDPrefix:         "T",
				Statuses:         []string{"queued", "active", "completed", "abandoned", "archived"},
				StartStatus:      "queued",
				ActiveStatus:     "active",
				TerminalStatuses: []string{"completed", "abandoned", "archived"},
				RequiredFields:   []string{"depends_on", "blocks"},
				DirectoryMap: map[string]string{
					"queued":    "tasks",
					"active":    "tasks",
					"completed": "archive",
					"abandoned": "archive",
				},
			},
			"issue": {
				IDPrefix:         "I",
				Statuses:         []string{"open", "active", "resolved", "wontfix", "archived"},
				StartStatus:      "open",
				ActiveStatus:     "active",
				TerminalStatuses: []string{"resolved", "wontfix", "archived"},
				// I-406: severity dropped in favor of priority (p0-p4
				// scale, shared with tasks). Priority isn't required at
				// the schema level — create.go fills a default of 2.
				RequiredFields: []string{"depends_on", "blocks"},
				DirectoryMap: map[string]string{
					"open":     "issues",
					"active":   "issues",
					"resolved": "archive",
					"wontfix":  "archive",
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
		},
		IDPatterns: map[string]string{
			"task":      "T-{seq}",
			"issue":     "I-{seq}",
			"idea":      "D-{seq}",
			"promotion": "P-{seq}",
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

	configPath, found := discover(startDir)
	if !found {
		// Fallback: check ST_ROOT env var
		if root := os.Getenv("ST_ROOT"); root != "" {
			configPath, found = discover(root)
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

	return cfg, nil
}

// LoadFrom loads config from a specific path. Used with --config flag.
func LoadFrom(configPath string) (*Config, error) {
	cfg := Defaults()
	if err := parseConfigFile(cfg, configPath); err != nil {
		return nil, fmt.Errorf("loading %s: %w", configPath, err)
	}
	cfg.root = filepath.Dir(filepath.Dir(configPath))
	return cfg, nil
}

// discover walks up from dir looking for .as/config.yaml.
// If a .st-root file is found first, its content is used as a redirect path.
func discover(dir string) (string, bool) {
	dir, _ = filepath.Abs(dir)
	for {
		candidate := filepath.Join(dir, ".as", "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
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
					return redirected, true
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
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
			if val != "" && key != "command" && key != "artifacts" && key != "post_deploy" {
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
		if (key == "artifacts" || key == "triggers") && levels[2] != "" {
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
				}
				cfg.Testing.ScopeSuites[suiteName] = sc
			}
		}
	}
}

// applyListItem routes a dash-prefixed list item to the appropriate config field.
func applyListItem(cfg *Config, levels [4]string, val string) {
	// Currently unused — gates list items would be handled here in the future.
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
		}
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
