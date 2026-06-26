package command

import (
	"fmt"
	"os"
	"path/filepath"
)

const defaultConfig = `# st configuration
# See: https://github.com/TheraPrac/agent-state

project:
  name: my-project

paths:
  root: .
  templates: templates
  changelog: .changelog
  index: index.md
`

const taskTemplate = `id: {{ID}}
type: task
status: queued
created: {{CREATED}}
last_touched: {{CREATED}}
title: {{TITLE}}

depends_on:
- []

blocks:
- []

next_actions:
- []
`

const issueTemplate = `id: {{ID}}
type: issue
status: open
created: {{CREATED}}
last_touched: {{CREATED}}
title: {{TITLE}}
severity: medium

depends_on:
- []

blocks:
- []
`

// Init creates a new st project in the given directory.
func Init(dir string) int {
	asDir := filepath.Join(dir, ".as")
	configPath := filepath.Join(asDir, "config.yaml")

	// Check if already initialized
	if _, err := os.Stat(configPath); err == nil {
		fmt.Fprintf(os.Stderr, "Already initialized: %s\n", configPath)
		return 1
	}

	// Create directories
	dirs := []string{
		asDir,
		filepath.Join(dir, "tasks"),
		filepath.Join(dir, "issues"),
		filepath.Join(dir, "archive"),
		filepath.Join(dir, "templates"),
		filepath.Join(dir, ".changelog"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "creating %s: %v\n", d, err)
			return 1
		}
	}

	// Write config
	if err := os.WriteFile(configPath, []byte(defaultConfig), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "writing config: %v\n", err)
		return 1
	}

	// Write templates
	if err := os.WriteFile(filepath.Join(dir, "templates", "task.md"), []byte(taskTemplate), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "writing task template: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "issue.md"), []byte(issueTemplate), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "writing issue template: %v\n", err)
		return 1
	}

	// Write empty index
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte("# Agent State Index\ngenerated: auto\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "writing index: %v\n", err)
		return 1
	}

	fmt.Printf("Initialized st project in %s\n\n", dir)
	fmt.Println("  st create task \"My first task\"")
	fmt.Println("  st create issue \"Bug to fix\"")
	fmt.Println("  st list")
	fmt.Println("  st status")

	return 0
}
