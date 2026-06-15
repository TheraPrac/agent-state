// Package classify implements the binary autonomy classifier — given an
// item plus a list of touched files, it returns a green/red verdict
// plus a reason. Green means "auto-run the full delivery loop without
// operator intervention"; red means "stop and surface to the operator
// for a decision".
//
// The static deny-list in this file is the hard floor: any match here
// forces a red verdict regardless of what the model says. The model
// can promote a green to red, but never demote a deny-list red to
// green. Defense in depth for security-critical, destructive, or
// irreversible changes.
package classify

import (
	"path/filepath"
	"strings"
)

// DenyPattern identifies a path family whose mere appearance in a
// change set forces a red verdict. Each pattern carries a Reason that
// becomes part of the verdict's audit trail.
//
// A pattern matches if EITHER PathPrefix (file starts with the given
// prefix) OR BasenameGlob (filepath.Match against the file's basename)
// matches. Set at least one — patterns with neither field set never
// match. Setting both is allowed and behaves as an OR (matches if
// either condition is satisfied); current HardRedPatterns entries set
// exactly one for clarity.
type DenyPattern struct {
	PathPrefix   string
	BasenameGlob string
	Reason       string
}

// Label returns a short identifier for the pattern, suitable for
// audit messages ("static deny-list match: <label>").
func (p DenyPattern) Label() string {
	if p.PathPrefix != "" {
		return p.PathPrefix
	}
	return p.BasenameGlob
}

// Matches reports whether the given path is covered by the pattern.
func (p DenyPattern) Matches(path string) bool {
	if p.PathPrefix != "" && strings.HasPrefix(path, p.PathPrefix) {
		return true
	}
	if p.BasenameGlob != "" {
		ok, _ := filepath.Match(p.BasenameGlob, filepath.Base(path))
		if ok {
			return true
		}
	}
	return false
}

// HardRedPatterns is the generic deny-list applied to every project.
// Project-specific path prefixes belong in .as/config.yaml under
// classify.deny_path_prefixes — they are merged at classify time by
// command/classify.go so the overall deny-list = HardRedPatterns + cfg.Classify.
// Keep narrow: only patterns where the cost of a wrong "green" verdict is
// materially worse than the cost of a wrong "red" verdict.
var HardRedPatterns = []DenyPattern{
	{BasenameGlob: "iam_*.tf", Reason: "IAM terraform — credentials and permissions"},
	{BasenameGlob: "secrets_*.tf", Reason: "secrets terraform — production credentials"},
	{BasenameGlob: "secrets-manifest.yaml", Reason: "secrets manifest"},
	{BasenameGlob: "*.pem", Reason: "private key material"},
	{BasenameGlob: "*.key", Reason: "private key material"},
}

// Match returns the first DenyPattern that matches any of the touched
// files, or nil if none match. Pattern order in patterns is the
// evaluation order; callers using HardRedPatterns get the order
// declared above.
func Match(touchedFiles []string, patterns []DenyPattern) *DenyPattern {
	for _, f := range touchedFiles {
		for i := range patterns {
			if patterns[i].Matches(f) {
				return &patterns[i]
			}
		}
	}
	return nil
}
