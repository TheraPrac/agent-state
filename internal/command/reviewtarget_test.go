package command

import "testing"

func TestParseGitHubSlug(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"ssh form", "git@github.com:TheraPrac/agent-state.git", "TheraPrac/agent-state", false},
		{"https form", "https://github.com/TheraPrac/agent-state.git", "TheraPrac/agent-state", false},
		{"https no suffix", "https://github.com/TheraPrac/theraprac-workspace", "TheraPrac/theraprac-workspace", false},
		{"trailing newline", "git@github.com:TheraPrac/theraprac-api.git\n", "TheraPrac/theraprac-api", false},
		{"non-github", "https://gitlab.com/x/y.git", "", true},
		{"garbage", "not-a-url", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGitHubSlug(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.url, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.url, err)
			}
			if got != tc.want {
				t.Errorf("parseGitHubSlug(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestPickReviewTarget(t *testing.T) {
	cases := []struct {
		name    string
		num     int
		matches []string
		scope   []string
		want    string
		wantErr bool
	}{
		// Failure modes first (I-1623): prove the resolver refuses to guess.
		{
			name:    "no match errors",
			num:     999,
			matches: nil,
			wantErr: true,
		},
		{
			name:    "ambiguous with no scope hint errors",
			num:     131,
			matches: []string{"TheraPrac/agent-state", "TheraPrac/theraprac-workspace"},
			scope:   nil,
			wantErr: true,
		},
		{
			name:    "ambiguous with scope hint matching both still errors",
			num:     131,
			matches: []string{"TheraPrac/agent-state", "TheraPrac/theraprac-workspace"},
			scope:   []string{"TheraPrac/agent-state", "TheraPrac/theraprac-workspace"},
			wantErr: true,
		},
		// Happy paths.
		{
			name:    "single match resolves",
			num:     131,
			matches: []string{"TheraPrac/theraprac-workspace"},
			want:    "TheraPrac/theraprac-workspace#131",
		},
		{
			name:    "multi-match disambiguated by scope_repos (the I-1616 case)",
			num:     131,
			matches: []string{"TheraPrac/agent-state", "TheraPrac/theraprac-workspace"},
			scope:   []string{"TheraPrac/theraprac-workspace"},
			want:    "TheraPrac/theraprac-workspace#131",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickReviewTarget(tc.num, tc.matches, tc.scope)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("pickReviewTarget(%d, %v, %v) = %q, want %q", tc.num, tc.matches, tc.scope, got, tc.want)
			}
		})
	}
}

// TestSelectTarget exercises match-building + disambiguation against stubbed gh, the
// orchestration seam of ReviewTarget. The core I-1616 case: two repos carry #131 and
// scope_repos picks the right one; without the hint it must refuse to guess.
func TestSelectTarget(t *testing.T) {
	slugs := []string{"TheraPrac/agent-state", "TheraPrac/theraprac-workspace", "TheraPrac/theraprac-api"}
	gh := func(slug string, n int) bool {
		return n == 131 && (slug == "TheraPrac/agent-state" || slug == "TheraPrac/theraprac-workspace")
	}

	got, err := selectTarget(131, slugs, []string{"TheraPrac/theraprac-workspace"}, gh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "TheraPrac/theraprac-workspace#131" {
		t.Errorf("got %q, want TheraPrac/theraprac-workspace#131", got)
	}

	if _, err := selectTarget(131, slugs, nil, gh); err == nil {
		t.Error("expected ambiguity error with no scope hint, got none")
	}

	if _, err := selectTarget(777, slugs, nil, gh); err == nil {
		t.Error("expected no-match error for unknown PR, got none")
	}
}
