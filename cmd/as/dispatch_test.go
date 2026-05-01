package main

import "testing"

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
