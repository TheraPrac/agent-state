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

func TestGoalMustDoSubcommandsWired(t *testing.T) {
	app := newApp("")
	goalCmd := findCommand(app, "goal")
	if goalCmd == nil {
		t.Fatal("st goal command not found in cobra tree")
	}
	mustDoCmd := findCommand(goalCmd, "must-do")
	if mustDoCmd == nil {
		t.Fatal("st goal must-do subcommand not wired")
	}
	for _, name := range []string{"add", "remove", "list"} {
		sub := findCommand(mustDoCmd, name)
		if sub == nil {
			t.Errorf("st goal must-do %s subcommand not wired", name)
			continue
		}
		if strings.TrimSpace(sub.Short) == "" {
			t.Errorf("st goal must-do %s has no Short description", name)
		}
	}
	// --bucket flag must be present on add.
	addCmd := findCommand(mustDoCmd, "add")
	if addCmd != nil {
		if addCmd.Flags().Lookup("bucket") == nil {
			t.Error("st goal must-do add missing --bucket flag")
		}
	}
}
