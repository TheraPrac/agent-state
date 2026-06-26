package coordinator

import (
	"testing"

	"github.com/theraprac/agent-state/internal/plan"
)

func TestConflictSensitivePaths(t *testing.T) {
	cases := []struct {
		name string
		p    *plan.Plan
		want []string // expected class set (order-independent)
	}{
		{
			name: "nil plan → nil",
			p:    nil,
			want: nil,
		},
		{
			name: "neutral plan touches no conflict class",
			p:    &plan.Plan{FilesToModify: []string{"internal/command/coordinate.go", "README.md"}},
			want: nil,
		},
		{
			name: "openapi spec dir → openapi class",
			p:    &plan.Plan{FilesToModify: []string{"api/openapi/api.yaml"}},
			want: []string{"openapi"},
		},
		{
			name: "bare api.yaml anywhere → openapi class",
			p:    &plan.Plan{FilesToCreate: []string{"some/nested/api.yaml"}},
			want: []string{"openapi"},
		},
		{
			name: "migration changelog → changelog class",
			p:    &plan.Plan{FilesToCreate: []string{"db/changelog/0042-add-table.xml"}},
			want: []string{"changelog"},
		},
		{
			name: "both classes touched → both returned",
			p: &plan.Plan{
				FilesToModify: []string{"api/openapi/paths/billing.yaml"},
				FilesToCreate: []string{"db/changelog/0043-x.sql"},
			},
			want: []string{"openapi", "changelog"},
		},
		{
			name: "two different openapi files collapse to one class",
			p:    &plan.Plan{FilesToModify: []string{"api/openapi/api.yaml", "api/openapi/paths/x.yaml"}},
			want: []string{"openapi"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ConflictSensitivePaths(tc.p)
			if !sameSet(got, tc.want) {
				t.Errorf("ConflictSensitivePaths = %v, want set %v", got, tc.want)
			}
		})
	}
}

func TestC1Conflicts(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty → no conflict", nil, nil, false},
		{"one empty → no conflict", []string{"openapi"}, nil, false},
		{"disjoint classes → no conflict", []string{"openapi"}, []string{"changelog"}, false},
		{"shared openapi → conflict", []string{"openapi"}, []string{"openapi"}, true},
		{"overlap on one of several → conflict", []string{"openapi", "changelog"}, []string{"changelog"}, true},
		{"both classes shared → conflict", []string{"openapi", "changelog"}, []string{"changelog", "openapi"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := C1Conflicts(tc.a, tc.b); got != tc.want {
				t.Errorf("C1Conflicts(%v,%v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestC1Conflicts_DifferentOpenAPIFilesStillConflict pins the class-level
// (not exact-file) semantics: two workers editing DIFFERENT files under the
// OpenAPI surface must still be serialized, because the spec regenerates as a
// unit. This is the case exact-path-equality would have missed.
func TestC1Conflicts_DifferentOpenAPIFilesStillConflict(t *testing.T) {
	a := ConflictSensitivePaths(&plan.Plan{FilesToModify: []string{"api/openapi/api.yaml"}})
	b := ConflictSensitivePaths(&plan.Plan{FilesToModify: []string{"api/openapi/paths/billing.yaml"}})
	if !C1Conflicts(a, b) {
		t.Errorf("two different OpenAPI files must conflict at the class level: a=%v b=%v", a, b)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
