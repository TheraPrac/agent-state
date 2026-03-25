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

	// Multi-agent support (optional)
	Agents *AgentsConfig

	// Root directory (where .as/ lives, or CWD)
	root string
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
	RequiredSuites map[string]SuiteConfig
	ScopeSuites    map[string]ScopeSuiteConfig
}

type SuiteConfig struct {
	Command string
}

type ScopeSuiteConfig struct {
	Command  string
	Triggers []string // file glob patterns
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
	BaseDir string
	Repos   []string
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
				DirectoryMap: map[string]string{
					"open":     "issues",
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
		},
		IDPatterns: map[string]string{
			"task":  "T-{seq}",
			"issue": "I-{seq}",
			"idea":  "D-{seq}",
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

// Load discovers and loads configuration. It walks up from startDir
// looking for .as/config.yaml. If not found, returns defaults rooted at startDir.
func Load(startDir string) (*Config, error) {
	cfg := Defaults()

	configPath, found := discover(startDir)
	if found {
		if err := parseConfigFile(cfg, configPath); err != nil {
			return nil, fmt.Errorf("loading %s: %w", configPath, err)
		}
		cfg.root = filepath.Dir(filepath.Dir(configPath)) // .as/config.yaml -> project root
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
func parseConfigFile(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var section string // tracks current top-level key

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Skip empty lines and comments
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Detect indentation level
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		// Top-level key
		if indent == 0 && strings.Contains(trimmed, ":") {
			key, val := splitKV(trimmed)
			section = key
			if val != "" {
				applyTopLevel(cfg, key, val)
			}
			continue
		}

		// Nested key under a section
		if indent > 0 && section != "" {
			key, val := splitKV(trimmed)
			applyNested(cfg, section, key, val)
		}
	}

	return scanner.Err()
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

func applyTopLevel(cfg *Config, key, val string) {
	// Simple top-level scalar overrides
	switch key {
	case "name":
		cfg.Project.Name = val
	case "description":
		cfg.Project.Description = val
	}
}

func applyNested(cfg *Config, section, key, val string) {
	switch section {
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
	}
}
