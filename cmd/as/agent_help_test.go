package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestAgentCommandHelpComplete walks the cobra tree rooted at
// `st agent` and asserts every command (including the root) has both
// a Long description and an Example block.
//
// New `st agent <foo>` subcommands added in the future will fail this
// test until they ship help text — keeps the agent CLI from rotting
// back to one-line Short descriptions. I-402.
func TestAgentCommandHelpComplete(t *testing.T) {
	app := newApp("")
	agentCmd := findCommand(app, "agent")
	if agentCmd == nil {
		t.Fatal("could not locate `st agent` command in the cobra tree")
	}

	visit := func(t *testing.T, c *cobra.Command, path string) {
		t.Helper()
		if strings.TrimSpace(c.Long) == "" {
			t.Errorf("%s: Long description is empty — every command under `st agent` needs a Long block (purpose, when to use, prerequisites, side effects)", path)
		}
		if strings.TrimSpace(c.Example) == "" {
			t.Errorf("%s: Example block is empty — every command under `st agent` needs at least one example invocation", path)
		}
	}

	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		visit(t, c, path)
		for _, child := range c.Commands() {
			if child.Hidden {
				continue
			}
			walk(child, path+" "+child.Name())
		}
	}
	walk(agentCmd, "st agent")
}

// findCommand locates a top-level subcommand by name on the cobra
// root. Returns nil when not found.
func findCommand(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}
