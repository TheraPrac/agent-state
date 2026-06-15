package quality

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ClaudeMdFinding describes one bloat-pattern hit in CLAUDE.md.
//
// I-736: the same pattern set as scripts/claude-md-audit.sh
// and claude-config/hooks/claude-md-bloat-guard.sh, ported
// to Go so st check can surface drift at session-start (warnings, not failures).
type ClaudeMdFinding struct {
	Line    int    // 1-indexed line number, or 0 for whole-file findings (size)
	Pattern string // one of: operator-quote, lessons-learned, emphatic-quote, item-narrative, block-quote-run, size-cap, size-warn
	Snippet string // ~80 chars of the offending line, or a size message
}

var (
	patOperatorQuote    = regexp.MustCompile(`(?i)(^|\s)Operator\s+\d{4}-\d{2}-\d{2}\s*:`)
	patLessonsLearned   = regexp.MustCompile(`(?i)(we\s+got\s+burned|got\s+bit\s+by|lesson\s+learned|incident\s+report)`)
	patEmphaticQuote    = regexp.MustCompile(`STOP\s+FUCKING|FUCKING\s+(WANT|DOING|SHOULD)`)
	patItemNarrative    = regexp.MustCompile(`^\s*[IT]-\d+\s*:\s`)
	patBlockQuoteLine   = regexp.MustCompile(`^\s*>\s`)
)

// ScanCLAUDEMd scans the file at path and returns any bloat findings.
// Returns nil (no error) when the file is absent — caller should treat
// "no findings" and "no file" the same for warning purposes.
//
// targetLines is the soft cap (size-warn finding above it).
// capLines is the hard cap (size-cap finding above it).
// Both can be overridden via CLAUDE_MD_AUDIT_TARGET / CLAUDE_MD_AUDIT_CAP
// env vars (matching the shell script's contract).
func ScanCLAUDEMd(path string, targetLines, capLines int) []ClaudeMdFinding {
	if v := os.Getenv("CLAUDE_MD_AUDIT_TARGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			targetLines = n
		}
	}
	if v := os.Getenv("CLAUDE_MD_AUDIT_CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			capLines = n
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var findings []ClaudeMdFinding
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	blockQuoteRun := 0
	blockQuoteRunStart := 0
	blockQuoteRunReported := false

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()

		if patOperatorQuote.MatchString(line) {
			findings = append(findings, ClaudeMdFinding{
				Line: lineNo, Pattern: "operator-quote", Snippet: snippet(line),
			})
		}
		if patLessonsLearned.MatchString(line) {
			findings = append(findings, ClaudeMdFinding{
				Line: lineNo, Pattern: "lessons-learned", Snippet: snippet(line),
			})
		}
		if patEmphaticQuote.MatchString(line) {
			findings = append(findings, ClaudeMdFinding{
				Line: lineNo, Pattern: "emphatic-quote", Snippet: snippet(line),
			})
		}
		if patItemNarrative.MatchString(line) {
			findings = append(findings, ClaudeMdFinding{
				Line: lineNo, Pattern: "item-narrative", Snippet: snippet(line),
			})
		}

		// Block-quote run state machine. Report at the first line of a
		// run that grows to >2 consecutive quoted lines.
		if patBlockQuoteLine.MatchString(line) {
			if blockQuoteRun == 0 {
				blockQuoteRunStart = lineNo
				blockQuoteRunReported = false
			}
			blockQuoteRun++
			if blockQuoteRun > 2 && !blockQuoteRunReported {
				findings = append(findings, ClaudeMdFinding{
					Line: blockQuoteRunStart, Pattern: "block-quote-run",
					Snippet: snippet(line),
				})
				blockQuoteRunReported = true
			}
		} else {
			blockQuoteRun = 0
			blockQuoteRunReported = false
		}
	}

	// Size findings.
	if lineNo > capLines {
		findings = append(findings, ClaudeMdFinding{
			Line: lineNo, Pattern: "size-cap",
			Snippet: "file is " + strconv.Itoa(lineNo) + " lines, exceeds hard cap " + strconv.Itoa(capLines),
		})
	} else if lineNo > targetLines {
		findings = append(findings, ClaudeMdFinding{
			Line: lineNo, Pattern: "size-warn",
			Snippet: "file is " + strconv.Itoa(lineNo) + " lines, exceeds target " + strconv.Itoa(targetLines) + " (under cap " + strconv.Itoa(capLines) + ")",
		})
	}

	return findings
}

func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
