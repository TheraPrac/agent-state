package main

import (
	"strings"
	"testing"
)

func TestAgentGoalCommandsWired(t *testing.T) {
	app := newApp("")
	agentCmd := findCommand(app, "agent")
	if agentCmd == nil {
		t.Fatal("st agent command not found in cobra tree")
	}

	goalCmd := findCommand(agentCmd, "goal")
	if goalCmd == nil {
		t.Fatal("st agent goal command not found in cobra tree")
	}

	want := []string{"set", "clear", "show"}
	for _, name := range want {
		sub := findCommand(goalCmd, name)
		if sub == nil {
			t.Errorf("st agent goal %s subcommand not wired", name)
			continue
		}
		if strings.TrimSpace(sub.Short) == "" {
			t.Errorf("st agent goal %s has no Short description", name)
		}
	}
}
