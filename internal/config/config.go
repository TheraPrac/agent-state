// Package config provides configuration for the as CLI.
//
// The tool works with sensible defaults out of the box. An optional
// .as/config.yaml in the project root can override any default.
// Config discovery: walk up from CWD looking for .as/config.yaml,
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
	Command   string
	Triggers  []string // file glob patterns that activate this suite
	Artifacts []string // glob patterns for artifacts to upload after execution
}

type EvidenceConfig struct {
	Backend  string // "local" (default) or "s3"
	LocalDir string // for local: directory path (default: .evidence)
	S3Bucket string // for s3: bucket name
	S3Region string // for s3: AWS region
	S3Prefix string // for s3: key prefix
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
	BaseDir   string              // worktree root relative to config root (e.g. "worktrees")
	ParentDir string              // parent of all repos (e.g. "/Users/x/Dev/project")
	Repos     []string            // short repo names in default order (e.g. ["api", "web"])
	RepoMap   map[string]string   // short name → directory name (e.g. "api" → "theraprac-api")
}

type GateConfig struct {
	Type   string   // deps_resolved, testing_complete, field_nonempty, stage_reached, agent_assigned, manifest_exists
	Fields []string // for field_nonempty
	Stage  string   // for stage_reached
}

type AgentsConfig struct {
	// Agent identity comes from $AS_AGENT_ID env var
}

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

// AgentID returns the current agent identity from $AS_AGENT_ID.
func (c *Config) AgentID() string {
	return os.Getenv("AS_AGENT_ID")
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

// SessionID returns the current Claude Code session ID from $AS_SESSION_ID.
func (c *Config) SessionID() string {
	return os.Getenv("AS_SESSION_ID")
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
				RequiredFields:   []string{"severity", "depends_on", "blocks"},
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
func discover(dir string) (string, bool) {
	dir, _ = filepath.Abs(dir)
	for {
		candidate := filepath.Join(dir, ".as", "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
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
			if val != "" && key != "command" && key != "artifacts" {
				// Simple format
				cfg.Testing.ScopeSuites[key] = ScopeSuiteConfig{Command: val}
			} else if val != "" {
				// Nested format field
				suiteName := levels[2]
				sc := cfg.Testing.ScopeSuites[suiteName]
				if key == "command" {
					sc.Command = val
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
	case "testing":
		ensureTesting(cfg)
		if key == "artifacts" && levels[2] != "" {
			suiteName := levels[2]
			switch levels[1] {
			case "required_suites":
				sc := cfg.Testing.RequiredSuites[suiteName]
				sc.Artifacts = items
				cfg.Testing.RequiredSuites[suiteName] = sc
			case "scope_suites":
				sc := cfg.Testing.ScopeSuites[suiteName]
				sc.Artifacts = items
				cfg.Testing.ScopeSuites[suiteName] = sc
			}
		}
	}
}

// applyListItem routes a dash-prefixed list item to the appropriate config field.
func applyListItem(cfg *Config, levels [4]string, val string) {
	// Currently unused — gates list items would be handled here in the future.
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
