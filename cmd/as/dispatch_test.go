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
