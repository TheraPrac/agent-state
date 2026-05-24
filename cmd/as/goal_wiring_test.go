package main

import (
	"strings"
	"testing"
)

func TestGoalSubcommandsWired(t *testing.T) {
	app := newApp("")
	goalCmd := findCommand(app, "goal")
	if goalCmd == nil {
		t.Fatal("st goal command not found in cobra tree")
	}

	want := []string{"create", "activate", "mark-met", "drop", "list"}
	for _, name := range want {
		sub := findCommand(goalCmd, name)
		if sub == nil {
			t.Errorf("st goal %s subcommand not wired", name)
			continue
		}
		if strings.TrimSpace(sub.Short) == "" {
			t.Errorf("st goal %s has no Short description", name)
		}
	}
}
