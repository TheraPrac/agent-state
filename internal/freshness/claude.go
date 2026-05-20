package freshness

import (
	"regexp"
	"strings"
)

// freshnessVerdictHeaderRE matches the verdict header line the
// Claude prompt instructs the sub-agent to emit. Anchored to the
// start-of-line "RECOMMENDATION" header so narrative mentions of
// the word don't trigger (mirrors the I-718 fix in
// command.extractRecommendation, intentionally duplicated here to
// avoid the import cycle the I-717 design avoids).
var freshnessVerdictHeaderRE = regexp.MustCompile(`(?i)^[\s*#]*recommendation\s*[:—]`)

// freshnessVerdictTokens are the only verdicts the sub-agent may
// emit; anything else falls into the fail-closed path.
var freshnessVerdictTokens = []string{"fresh", "drift", "stale"}

// runFreshnessClaudePass invokes the Claude sub-agent to
// adjudicate a heuristic-Drift verdict. Returns the final verdict
// and a one-line rationale. Fail-closed posture: any of
//   - engine exec error
//   - non-zero exit
//   - empty stdout
//   - missing/unparseable RECOMMENDATION
//
// returns (heuristicVerdict, "") so the caller keeps the heuristic
// verdict and does NOT append a CategoryClaude finding.
//
// runClaude is the function-value injection that lets the
// freshness package call into command.RunEngine.RunClaude without
// importing the command package (which would create a cycle the
// I-717 design avoids).
//
// I-717.
func runFreshnessClaudePass(runClaude func(cwd string, args []string, env []string) ([]byte, int, error), workspaceRoot string, heuristicVerdict Verdict, prompt string) (Verdict, string) {
	if runClaude == nil {
		return heuristicVerdict, ""
	}

	// Single Claude invocation. Args mirror the existing
	// `claude -p` shape that command.executeClaude uses; using
	// stdin for the prompt + JSON output for the result.
	stdout, code, err := runClaude(workspaceRoot, []string{"-p", prompt, "--output-format", "json"}, nil)
	if err != nil || code != 0 || len(stdout) == 0 {
		return heuristicVerdict, ""
	}

	verdict, rationale, ok := parseFreshnessRecommendation(string(stdout))
	if !ok {
		return heuristicVerdict, ""
	}
	return verdict, rationale
}

// parseFreshnessRecommendation scans `output` for the
// freshnessVerdictHeaderRE-matching lines, picks the LAST one whose
// extracted leading token is a recognized verdict, and returns it.
// (Last-match-wins for the verdict-token-bearing candidates;
// narrative-only "RECOMMENDATION" mentions don't match the header
// shape, so they can't be selected.)
//
// Returns ok=false on any of:
//   - no header-shape line found
//   - header found but the extracted text has no recognized
//     verdict as its leading token
//
// In both cases the caller keeps the heuristic verdict (fail
// closed).
func parseFreshnessRecommendation(output string) (Verdict, string, bool) {
	if output == "" {
		return VerdictFresh, "", false
	}
	lines := strings.Split(output, "\n")
	var lastVerdict Verdict
	var lastRationale string
	found := false

	for _, line := range lines {
		if !freshnessVerdictHeaderRE.MatchString(line) {
			continue
		}
		rest := extractAfterSeparator(line)
		rest = strings.ReplaceAll(rest, "**", "")
		rest = strings.ReplaceAll(rest, "*", "")
		rest = strings.TrimSpace(rest)
		if rest == "" {
			continue
		}
		token := leadingToken(rest)
		v, ok := freshnessTokenToVerdict(token)
		if !ok {
			continue
		}
		lastVerdict = v
		lastRationale = rest
		found = true
	}
	if !found {
		return VerdictFresh, "", false
	}
	return lastVerdict, lastRationale, true
}

// extractAfterSeparator returns the text after the FIRST `:` or
// `—` separator on the line (using Index, not LastIndex, so a
// rationale containing a second separator like
// "Fresh — keyword was incidental — second clause" doesn't drop
// the verdict).
func extractAfterSeparator(line string) string {
	for _, sep := range []string{":", "—"} {
		if idx := strings.Index(line, sep); idx >= 0 {
			return line[idx+len(sep):]
		}
	}
	return ""
}

// leadingToken returns the first whitespace/punctuation-delimited
// token from `s` (lowercased), so "Fresh — keyword 'stale' was
// incidental" → "fresh" (not "stale").
func leadingToken(s string) string {
	ls := strings.ToLower(s)
	first := strings.TrimLeft(ls, " \t-—:*")
	for _, sep := range []string{" ", "\t", "—", "-", ":", ","} {
		if idx := strings.Index(first, sep); idx >= 0 {
			first = first[:idx]
			break
		}
	}
	return first
}

func freshnessTokenToVerdict(token string) (Verdict, bool) {
	for _, t := range freshnessVerdictTokens {
		if token == t {
			switch t {
			case "fresh":
				return VerdictFresh, true
			case "drift":
				return VerdictDrift, true
			case "stale":
				return VerdictStale, true
			}
		}
	}
	return VerdictFresh, false
}
