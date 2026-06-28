package command

import "testing"

func TestQualifyGhPR(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		// Unchanged cases first (the failure modes the guard must NOT touch).
		{"non-gh command", "echo hi", "echo hi"},
		{"git push untouched", "git push origin main", "git push origin main"},
		{"already -R qualified", "gh pr merge 5 -R o/r --squash", "gh pr merge 5 -R o/r --squash"},
		{"already --repo qualified", "gh pr merge --repo o/r --squash", "gh pr merge --repo o/r --squash"},
		// The actual rewrites.
		{
			"merge command",
			"gh pr merge --squash --delete-branch",
			"gh pr merge 5 -R o/r --squash --delete-branch",
		},
		{
			"checks pre-check",
			"gh pr checks --watch --interval 20",
			"gh pr checks 5 -R o/r --watch --interval 20",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := qualifyGhPR(tc.cmd, "o/r", 5)
			if got != tc.want {
				t.Errorf("qualifyGhPR(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}
