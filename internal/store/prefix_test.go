package store

import "testing"

// TestItemsPrefixFromToplevel_I1624 pins the shared items-prefix derivation,
// failure modes first. The case-divergence case is the regression guard for the
// latent bug fixed here: the prior raw filepath.Rel (no ToLower) emitted a `../`
// traversal when toplevel and root differed only in case, mis-classifying paths.
// Non-existent paths make EvalSymlinks fall back to the raw inputs, so these
// cases deterministically exercise the pure Rel+ToLower logic on any filesystem.
func TestItemsPrefixFromToplevel_I1624(t *testing.T) {
	cases := []struct {
		name       string
		root       string
		toplevel   string
		wantPrefix string
		wantOK     bool
	}{
		{
			// THE BUG: case divergence between toplevel and root. Must resolve to
			// the real nested prefix, never a `../...` traversal.
			name: "case-divergent root and toplevel", root: "/nope/ws/agent-state",
			toplevel: "/NOPE/ws", wantPrefix: "agent-state/", wantOK: true,
		},
		{
			name: "nested layout", root: "/ws/agent-state",
			toplevel: "/ws", wantPrefix: "agent-state/", wantOK: true,
		},
		{
			name: "deeper nesting", root: "/ws/sub/agent-state",
			toplevel: "/ws", wantPrefix: "sub/agent-state/", wantOK: true,
		},
		{
			name: "flat layout (root == toplevel)", root: "/ws",
			toplevel: "/ws", wantPrefix: "", wantOK: true,
		},
		{
			// Rel cannot relate an absolute toplevel to a relative root → fail-open.
			name: "Rel failure (abs toplevel, relative root)", root: "agent-state",
			toplevel: "/abs/ws", wantPrefix: "", wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPrefix, gotOK := itemsPrefixFromToplevel(c.root, c.toplevel)
			if gotOK != c.wantOK || gotPrefix != c.wantPrefix {
				t.Errorf("itemsPrefixFromToplevel(%q, %q) = (%q, %v), want (%q, %v)",
					c.root, c.toplevel, gotPrefix, gotOK, c.wantPrefix, c.wantOK)
			}
		})
	}
}
