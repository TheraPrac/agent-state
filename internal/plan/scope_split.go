package plan

import "strings"

// I-180: full-stack scope-split detection.
//
// Classify buckets a Plan by scope into one of:
//   - "infra"      → only theraprac-infra
//   - "backend"    → only theraprac-api
//   - "frontend"   → only theraprac-web
//   - "full-stack" → both api AND web (the cost-heavy bucket)
//   - "unknown"    → neither ScopeRepos nor file paths express scope
//
// Logic uses the same prefix-pattern semantics that
// `cmd/pr.go::matchesTriggers` uses for Tier-2 scope suite resolution
// (file path prefix matching), so the classifier ships with proven
// semantics rather than a new heuristic.
//
// Precedence:
//   1. Explicit ScopeRepos (frontmatter or `## Scope` section).
//   2. Inferred from FilesToCreate + FilesToModify path prefixes.
//
// When both signals disagree (e.g., ScopeRepos says api but a file
// also lives in theraprac-web/), the union wins — full-stack — because
// that's the conservative bucket: a planner who declared api-only but
// touches web files is exactly the case the recommendation should
// fire on.
func Classify(p *Plan) string {
	if p == nil {
		return "unknown"
	}

	// Build the set of repos touched by ScopeRepos + file paths.
	hasAPI := false
	hasWeb := false
	hasInfra := false

	for _, repo := range p.ScopeRepos {
		switch strings.TrimSpace(repo) {
		case "theraprac-api", "api":
			hasAPI = true
		case "theraprac-web", "web":
			hasWeb = true
		case "theraprac-infra", "infra":
			hasInfra = true
		}
	}

	allFiles := append(append([]string{}, p.FilesToCreate...), p.FilesToModify...)
	for _, f := range allFiles {
		if hasFilePrefix(f, "theraprac-api/") {
			hasAPI = true
		}
		if hasFilePrefix(f, "theraprac-web/") {
			hasWeb = true
		}
		if hasFilePrefix(f, "theraprac-infra/") {
			hasInfra = true
		}
	}

	count := 0
	if hasAPI {
		count++
	}
	if hasWeb {
		count++
	}
	if hasInfra {
		count++
	}

	switch {
	case count == 0:
		return "unknown"
	case hasAPI && hasWeb:
		// api + web is the full-stack bucket; presence of infra
		// alongside doesn't change the recommendation since the
		// cost driver is the api-web boundary.
		return "full-stack"
	case hasInfra && count == 1:
		return "infra"
	case hasAPI && count == 1:
		return "backend"
	case hasWeb && count == 1:
		return "frontend"
	default:
		// e.g. api + infra (no web) or web + infra (no api). These
		// don't have the api-web review-finding cascade, so they're
		// not full-stack — fall back to the dominant repo bucket.
		if hasAPI {
			return "backend"
		}
		if hasWeb {
			return "frontend"
		}
		return "infra"
	}
}

// DetectFullStack reports whether the plan is a candidate for the
// SPLIT RECOMMENDATION banner shown during `st prep`. Returns true
// iff Classify(p) == "full-stack" AND len(p.ACs) >= acThreshold.
//
// Default threshold is 5 (per the I-180 issue body: items with 5+
// ACs across api+web are the highest-cost / highest-rework cluster).
// Callers may override for tests or future tuning.
func DetectFullStack(p *Plan, acThreshold int) bool {
	if p == nil {
		return false
	}
	if Classify(p) != "full-stack" {
		return false
	}
	return len(p.ACs) >= acThreshold
}

// hasFilePrefix returns true when path starts with prefix, after
// stripping any leading `./` segment that prep prompts often
// produce.
func hasFilePrefix(path, prefix string) bool {
	path = strings.TrimPrefix(path, "./")
	return strings.HasPrefix(path, prefix)
}

// PartitionACsByLayer splits a flat AC list into (apiACs, webACs).
// Heuristic: an AC is api-shaped if its command targets the api repo
// (paths under theraprac-api/, `make integration-local`, OpenAPI/
// schema/contract keywords). Web-shaped if it targets the web repo
// (theraprac-web/, `npm`, `playwright`, e2e, type-check). ACs that
// don't match either are duplicated to BOTH parts so they aren't
// silently dropped — the operator can hand-edit the children.
//
// Used by Split to construct child item AC lists.
func PartitionACsByLayer(acs []string) (apiACs, webACs []string) {
	for _, ac := range acs {
		l := strings.ToLower(ac)
		isAPI := strings.Contains(l, "theraprac-api") ||
			strings.Contains(l, "make integration-local") ||
			strings.Contains(l, "openapi") ||
			strings.Contains(l, "go test ") ||
			strings.Contains(l, "go build") ||
			strings.Contains(l, "schema") ||
			strings.Contains(l, "liquibase")
		isWeb := strings.Contains(l, "theraprac-web") ||
			strings.Contains(l, "npm ") ||
			strings.Contains(l, "playwright") ||
			strings.Contains(l, "type-check") ||
			strings.Contains(l, "e2e") ||
			strings.Contains(l, "vitest")
		switch {
		case isAPI && !isWeb:
			apiACs = append(apiACs, ac)
		case isWeb && !isAPI:
			webACs = append(webACs, ac)
		default:
			// Ambiguous (matches both or neither): include in both so
			// nothing is silently dropped. Operator can prune.
			apiACs = append(apiACs, ac)
			webACs = append(webACs, ac)
		}
	}
	return apiACs, webACs
}
