package model

import "strings"

// ValidDropReasons is the closed vocabulary for abandoning items or dropping
// goals. "aged" is deliberately excluded — items and goals must be dropped by
// deliberate strategic decision, never by passive time pressure.
var ValidDropReasons = []string{
	"superseded",
	"premise-invalid",
	"out-of-strategy",
	"duplicate",
	"unactionable",
}

// IsValidDropReason reports whether s is in ValidDropReasons.
func IsValidDropReason(s string) bool {
	for _, r := range ValidDropReasons {
		if r == s {
			return true
		}
	}
	return false
}

// ValidDropReasonsJoined returns the reasons as a pipe-separated string for
// use in error messages.
func ValidDropReasonsJoined() string {
	return strings.Join(ValidDropReasons, "|")
}
