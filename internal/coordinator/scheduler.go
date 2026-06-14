package coordinator

import (
	"strings"

	"github.com/jfinlinson/agent-state/internal/plan"
)

// scheduler.go is the PURE scheduling layer for T-364 multi-worker fan-out.
// It answers one question with no I/O and no shared state: "would dispatching
// these two items concurrently create a C1 semantic conflict?" The imperative
// coordinator loop (internal/command/coordinate.go) uses it to SERIALIZE
// conflicting items — run them one-after-another — instead of letting two
// workers stomp the same OpenAPI contract or migration changelog in parallel.
//
// Conflict is detected at the CLASS level, not the exact-file level, because
// that is where the real collision lives (contract §C1):
//
//   - openapi:   the OpenAPI spec is the single source of truth and is
//     regenerated as a unit (`make generate` derives the models from it).
//     Two workers touching ANY spec file collide on the generated output and
//     at merge, even when they edit different spec files.
//   - changelog: the Liquibase migration changelog is ordered and append-only.
//     Two workers both adding migrations collide on changelog ordering.
//
// So an item's "conflict signature" is the SET OF CLASSES it touches, and two
// items conflict iff their class sets intersect. A path-prefix match is
// exactly as precise as the conflict is real and needs no content parsing.

// c1Class maps a conflict-class token to the path fragments that identify it.
// Matched as substrings so both repo-relative and nested paths are caught.
var c1Classes = map[string][]string{
	"openapi":   {"api/openapi", "api.yaml"},
	"changelog": {"db/changelog"},
}

// ConflictSensitivePaths returns the SET OF C1 CONFLICT CLASSES a plan touches
// (e.g. ["openapi"], ["changelog"], or both), derived from its declared files
// (FilesToCreate ∪ FilesToModify). The result is deduped and is the signature
// the dispatcher compares across in-flight workers. A nil plan yields nil (an
// unplanned item declares no files and so conflicts with nothing on this axis;
// the I-491 no-plan-no-dispatch guard handles unplanned items separately).
//
// The name is retained from the original design for continuity; the returned
// strings are class tokens, not raw paths, because class-level is the correct
// granularity for C1 serialization (see file header).
func ConflictSensitivePaths(p *plan.Plan) []string {
	if p == nil {
		return nil
	}
	files := append(append([]string{}, p.FilesToCreate...), p.FilesToModify...)
	classSet := map[string]bool{}
	for _, f := range files {
		for class, markers := range c1Classes {
			for _, m := range markers {
				if strings.Contains(f, m) {
					classSet[class] = true
				}
			}
		}
	}
	if len(classSet) == 0 {
		return nil
	}
	out := make([]string, 0, len(classSet))
	for class := range classSet {
		out = append(out, class)
	}
	return out
}

// C1Conflicts reports whether two conflict-class signatures intersect — i.e.
// whether dispatching the second item while the first is in flight would put
// two workers on the same OpenAPI/migration surface. Empty on either side is
// no conflict (an item touching no sensitive class can always run in parallel).
func C1Conflicts(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, c := range a {
		seen[c] = true
	}
	for _, c := range b {
		if seen[c] {
			return true
		}
	}
	return false
}
