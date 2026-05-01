package validate

// legacyStatusAliases maps pre-I-433 status values to the unified
// vocabulary. Used by WriteOK to emit a "did you mean" hint when an
// agent (or muscle-memory operator) tries to write a deprecated value.
// Keeping the map small and explicit makes the hint precise — we only
// suggest something when the input is actually a known historical alias.
var legacyStatusAliases = map[string]string{
	"open":      "queued",
	"resolved":  "done",
	"wontfix":   "abandoned",
	"completed": "done",
}

// suggestStatus returns the unified-vocabulary replacement for a legacy
// status alias, or "" when there is no mapping. I-508.
func suggestStatus(input string) string {
	return legacyStatusAliases[input]
}
