package command

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRelativePlanPath verifies the I-512 helper that records the plan
// sidecar path on item.LinkedPlans. Round-tripability requires the
// relative form so the value doesn't drift across machines / agents.
func TestRelativePlanPath(t *testing.T) {
	tests := []struct {
		name      string
		plansDir  string
		root      string
		itemID    string
		want      string
		wantAbs   bool // expect absolute (helper falls back when Rel fails)
	}{
		{
			name:     "simple subdir",
			plansDir: "/repo/.plans",
			root:     "/repo",
			itemID:   "I-509",
			want:     filepath.Join(".plans", "I-509.md"),
		},
		{
			name:     "nested plans dir",
			plansDir: "/repo/agent-state/.plans",
			root:     "/repo",
			itemID:   "T-100",
			want:     filepath.Join("agent-state", ".plans", "T-100.md"),
		},
		{
			name:     "empty root falls back to abs",
			plansDir: "/some/abs/.plans",
			root:     "",
			itemID:   "I-200",
			want:     filepath.Join("/some/abs/.plans", "I-200.md"),
			wantAbs:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relativePlanPath(tt.plansDir, tt.root, tt.itemID)
			if got != tt.want {
				t.Errorf("relativePlanPath(%q,%q,%q) = %q, want %q",
					tt.plansDir, tt.root, tt.itemID, got, tt.want)
			}
			// Sanity: relative cases shouldn't start with / on POSIX.
			if !tt.wantAbs && strings.HasPrefix(got, "/") {
				t.Errorf("expected relative path; got absolute %q", got)
			}
		})
	}
}
