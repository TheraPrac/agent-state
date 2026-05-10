package plan

import "testing"

// TestClassifyScope exercises the I-180 scope-bucket classifier
// across all five buckets (infra, backend, frontend, full-stack,
// unknown) and the precedence rules between ScopeRepos and inferred
// file-prefix matching.
func TestClassifyScope(t *testing.T) {
	tests := []struct {
		name string
		plan *Plan
		want string
	}{
		{
			name: "nil plan is unknown",
			plan: nil,
			want: "unknown",
		},
		{
			name: "empty plan is unknown",
			plan: &Plan{},
			want: "unknown",
		},
		{
			name: "infra-only via ScopeRepos",
			plan: &Plan{ScopeRepos: []string{"theraprac-infra"}},
			want: "infra",
		},
		{
			name: "backend-only via ScopeRepos",
			plan: &Plan{ScopeRepos: []string{"theraprac-api"}},
			want: "backend",
		},
		{
			name: "frontend-only via ScopeRepos",
			plan: &Plan{ScopeRepos: []string{"theraprac-web"}},
			want: "frontend",
		},
		{
			name: "full-stack via explicit ScopeRepos",
			plan: &Plan{ScopeRepos: []string{"theraprac-api", "theraprac-web"}},
			want: "full-stack",
		},
		{
			name: "backend via FilesToModify only (no ScopeRepos)",
			plan: &Plan{FilesToModify: []string{"theraprac-api/internal/handlers/foo.go"}},
			want: "backend",
		},
		{
			name: "frontend via FilesToCreate only",
			plan: &Plan{FilesToCreate: []string{"theraprac-web/src/app/page.tsx"}},
			want: "frontend",
		},
		{
			name: "full-stack inferred from mixed file paths",
			plan: &Plan{
				FilesToModify: []string{
					"theraprac-api/internal/handlers/foo.go",
					"theraprac-web/src/components/Bar.tsx",
				},
			},
			want: "full-stack",
		},
		{
			name: "ScopeRepos says api but files touch web → full-stack (union wins)",
			plan: &Plan{
				ScopeRepos:    []string{"theraprac-api"},
				FilesToModify: []string{"theraprac-web/src/page.tsx"},
			},
			want: "full-stack",
		},
		{
			name: "api + infra (no web) → backend",
			plan: &Plan{
				ScopeRepos: []string{"theraprac-api", "theraprac-infra"},
			},
			want: "backend",
		},
		{
			name: "web + infra (no api) → frontend",
			plan: &Plan{
				ScopeRepos: []string{"theraprac-web", "theraprac-infra"},
			},
			want: "frontend",
		},
		{
			name: "api + web + infra → full-stack",
			plan: &Plan{
				ScopeRepos: []string{"theraprac-api", "theraprac-web", "theraprac-infra"},
			},
			want: "full-stack",
		},
		{
			name: "leading ./ in file paths is tolerated",
			plan: &Plan{
				FilesToModify: []string{"./theraprac-api/foo.go", "./theraprac-web/page.tsx"},
			},
			want: "full-stack",
		},
		{
			name: "short alias names work too",
			plan: &Plan{ScopeRepos: []string{"api", "web"}},
			want: "full-stack",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.plan)
			if got != tt.want {
				t.Errorf("Classify = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDetectFullStack covers the threshold gating: under, at, and
// over the AC-count threshold, plus per-bucket short-circuits.
func TestDetectFullStack(t *testing.T) {
	tenACs := make([]string, 10)
	for i := range tenACs {
		tenACs[i] = "cmd: test"
	}

	tests := []struct {
		name      string
		plan      *Plan
		threshold int
		want      bool
	}{
		{
			name: "under_threshold_no_recommendation",
			plan: &Plan{
				ScopeRepos: []string{"theraprac-api", "theraprac-web"},
				ACs:        []string{"cmd: a", "cmd: b"},
			},
			threshold: 5,
			want:      false,
		},
		{
			name: "at_threshold_fires",
			plan: &Plan{
				ScopeRepos: []string{"theraprac-api", "theraprac-web"},
				ACs:        []string{"cmd: a", "cmd: b", "cmd: c", "cmd: d", "cmd: e"},
			},
			threshold: 5,
			want:      true,
		},
		{
			name: "over_threshold_fires",
			plan: &Plan{
				ScopeRepos: []string{"theraprac-api", "theraprac-web"},
				ACs:        tenACs,
			},
			threshold: 5,
			want:      true,
		},
		{
			name: "backend_only_never_fires",
			plan: &Plan{
				ScopeRepos: []string{"theraprac-api"},
				ACs:        tenACs,
			},
			threshold: 5,
			want:      false,
		},
		{
			name: "frontend_only_never_fires",
			plan: &Plan{
				ScopeRepos: []string{"theraprac-web"},
				ACs:        tenACs,
			},
			threshold: 5,
			want:      false,
		},
		{
			name: "infra_only_never_fires",
			plan: &Plan{
				ScopeRepos: []string{"theraprac-infra"},
				ACs:        tenACs,
			},
			threshold: 5,
			want:      false,
		},
		{
			name: "nil_plan_safe",
			plan: nil,
			threshold: 5,
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectFullStack(tt.plan, tt.threshold)
			if got != tt.want {
				t.Errorf("DetectFullStack = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPartitionACsByLayer covers the heuristic AC partitioning that
// Split uses to populate child items. ACs that match neither layer
// (or both) are duplicated to BOTH children so they aren't silently
// dropped — operator can prune.
func TestPartitionACsByLayer(t *testing.T) {
	api, web := PartitionACsByLayer([]string{
		"cmd: cd ../theraprac-api && make integration-local",  // api-shaped
		"cmd: cd ../theraprac-web && npm run type-check",      // web-shaped
		"cmd: go test ./internal/foo",                         // api-shaped (go test)
		"cmd: cd ../theraprac-web && npx playwright test",     // web-shaped (playwright)
		"cmd: echo ambiguous",                                 // matches neither → both
	})
	if len(api) != 3 {
		t.Errorf("api ACs = %d, want 3 (got %v)", len(api), api)
	}
	if len(web) != 3 {
		t.Errorf("web ACs = %d, want 3 (got %v)", len(web), web)
	}
}
