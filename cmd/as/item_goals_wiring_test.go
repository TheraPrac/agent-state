package main

import (
	"strings"
	"testing"
)

func TestItemGoalsSubcommandsWired(t *testing.T) {
	app := newApp("")
	itemCmd := findCommand(app, "item")
	if itemCmd == nil {
		t.Fatal("st item command not found in cobra tree")
	}
	goalsCmd := findCommand(itemCmd, "goals")
	if goalsCmd == nil {
		t.Fatal("st item goals subcommand not wired")
	}
	for _, name := range []string{"add", "remove"} {
		sub := findCommand(goalsCmd, name)
		if sub == nil {
			t.Errorf("st item goals %s subcommand not wired", name)
			continue
		}
		if strings.TrimSpace(sub.Short) == "" {
			t.Errorf("st item goals %s has no Short description", name)
		}
	}
}

func TestGoalValidateConsistencyWired(t *testing.T) {
	app := newApp("")
	goalCmd := findCommand(app, "goal")
	if goalCmd == nil {
		t.Fatal("st goal command not found in cobra tree")
	}
	sub := findCommand(goalCmd, "validate-consistency")
	if sub == nil {
		t.Fatal("st goal validate-consistency not wired")
	}
	if strings.TrimSpace(sub.Short) == "" {
		t.Error("st goal validate-consistency has no Short description")
	}
}
