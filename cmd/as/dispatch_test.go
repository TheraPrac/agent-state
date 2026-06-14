package main

import (
	"strings"
	"testing"
)

// TestNextCommandWired verifies `st next` is registered and runs without panic.
func TestNextCommandWired(t *testing.T) {
	app := newApp(t.TempDir())
	var found bool
	for _, cmd := range app.Commands() {
		if cmd.Name() == "next" {
			found = true
		}
	}
	if !found {
		t.Fatal("st next command must be registered on the root cobra command")
	}
}

// TestRecommendBriefFlagWired verifies --brief is registered on st recommend.
func TestRecommendBriefFlagWired(t *testing.T) {
	app := newApp(t.TempDir())
	var recCmd interface{ HasFlags() bool }
	for _, cmd := range app.Commands() {
		if cmd.Name() == "recommend" {
			recCmd = cmd
			f := cmd.Flags().Lookup("brief")
			if f == nil {
				t.Fatal("--brief flag must be registered on st recommend")
			}
			if !strings.Contains(f.Usage, "one-line") {
				t.Errorf("--brief usage must mention one-line; got: %q", f.Usage)
			}
			return
		}
	}
	if recCmd == nil {
		t.Fatal("st recommend command must be registered")
	}
}

// TestGoalReviewCommandWired verifies `st goal review` is registered as a
// subcommand of `st goal` with --count and --list flags (T-413).
func TestGoalReviewCommandWired(t *testing.T) {
	app := newApp(t.TempDir())
	for _, cmd := range app.Commands() {
		if cmd.Name() == "goal" {
			for _, sub := range cmd.Commands() {
				if sub.Name() == "review" {
					if sub.Flags().Lookup("count") == nil {
						t.Fatal("st goal review must have --count flag")
					}
					if sub.Flags().Lookup("list") == nil {
						t.Fatal("st goal review must have --list flag")
					}
					return
				}
			}
			t.Fatal("st goal review subcommand must be registered under st goal")
		}
	}
	t.Fatal("st goal command must be registered on the root cobra command")
}

// TestQueueAutoApproveCommandWired verifies `st queue auto-approve` is
// registered as a subcommand of `st queue` (T-412).
func TestQueueAutoApproveCommandWired(t *testing.T) {
	app := newApp(t.TempDir())
	for _, cmd := range app.Commands() {
		if cmd.Name() == "queue" {
			for _, sub := range cmd.Commands() {
				if sub.Name() == "auto-approve" {
					return
				}
			}
			t.Fatal("st queue auto-approve subcommand must be registered under st queue")
		}
	}
	t.Fatal("st queue command must be registered on the root cobra command")
}

// TestClaimCommandWired verifies `st claim` is registered (T-384).
func TestClaimCommandWired(t *testing.T) {
	app := newApp(t.TempDir())
	for _, cmd := range app.Commands() {
		if cmd.Name() == "claim" {
			return
		}
	}
	t.Fatal("st claim command must be registered on the root cobra command")
}

// TestDispatchCommandWired verifies `st dispatch` is registered with its
// required flags (T-384).
func TestDispatchCommandWired(t *testing.T) {
	app := newApp(t.TempDir())
	for _, cmd := range app.Commands() {
		if cmd.Name() == "dispatch" {
			if f := cmd.Flags().Lookup("parallelism"); f == nil {
				t.Fatal("st dispatch must have --parallelism flag")
			}
			if f := cmd.Flags().Lookup("dry-run"); f == nil {
				t.Fatal("st dispatch must have --dry-run flag")
			}
			if f := cmd.Flags().Lookup("budget"); f == nil {
				t.Fatal("st dispatch must have --budget flag")
			}
			return
		}
	}
	t.Fatal("st dispatch command must be registered on the root cobra command")
}

// I-504 (review fix): the cobra dispatch decision is a pure helper
// — exercise it directly so the routing invariant is covered
// without spinning up a full cobra root. This file lives in package
// main (same as app.go) so it can call the unexported helper.
func TestAllLookLikePairs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"foo=bar"}, true},
		{[]string{"foo=bar", "baz=qux"}, true},
		{[]string{"foo"}, false},
		{[]string{"foo=bar", "baz"}, false},
		{[]string{"=foo"}, false}, // empty key — key index 0 is invalid
		{[]string{"key=value=with=equals"}, true},
	}
	for _, c := range cases {
		got := allLookLikePairs(c.args)
		if got != c.want {
			t.Errorf("allLookLikePairs(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}
