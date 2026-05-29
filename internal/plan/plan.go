// Package plan provides sidecar storage for implementation plans.
// Each item gets a .plans/<id>.md file with structured sections
// and YAML frontmatter for machine-readable metadata.
package plan

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Plan represents a structured implementation plan.
type Plan struct {
	ScopeRepos  []string   // which repos this item touches
	Approved    bool       // user accepted the plan
	ApprovedAt  string     // timestamp of approval
	Rejected    bool       // user explicitly rejected the plan
	RejectedAt  string     // timestamp of rejection
	Approach    string     // high-level approach description
	Steps       []string   // ordered implementation steps
	FilesToCreate []string // new files to create
	FilesToModify []string // existing files to change
	ACs         []string   // acceptance criteria (cmd: prefixed)
	Revisions   []Revision // revision history
	RawText     string     // full plan text (fallback if parsing fails)
}

// Revision records a plan revision event.
type Revision struct {
	Timestamp string
	Summary   string
}

// Exists checks if a plan sidecar exists for the given item.
func Exists(dir, id string) bool {
	_, err := os.Stat(filepath.Join(dir, id+".md"))
	return err == nil
}

// ReportPath returns the plan-review report sidecar path for itemID.
// I-565: when prep runs in --write-only mode, the verbose plan-review
// narrative is saved alongside the plan as <id>.report.md.
func ReportPath(dir, id string) string {
	return filepath.Join(dir, id+".report.md")
}

// ReportExists checks if a plan-review report sidecar exists.
func ReportExists(dir, id string) bool {
	_, err := os.Stat(ReportPath(dir, id))
	return err == nil
}

// Delete removes the plan sidecar .plans/<id>.md. A missing file is
// not an error (idempotent), mirroring Load's IsNotExist tolerance.
// I-767: used by `st plan invalidate` to discard a stale plan body
// so `st plan prep` re-authors from scratch.
func Delete(dir, id string) error {
	err := os.Remove(filepath.Join(dir, id+".md"))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// DeleteReport removes the plan-review report sidecar
// .plans/<id>.report.md. A missing file is not an error. I-767.
func DeleteReport(dir, id string) error {
	err := os.Remove(ReportPath(dir, id))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// LoadReport reads a plan-review report sidecar. Returns ("", nil) if
// the file does not exist, mirroring Load's missing-file semantics.
func LoadReport(dir, id string) (string, error) {
	data, err := os.ReadFile(ReportPath(dir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// SaveReport writes a plan-review report sidecar verbatim, creating the
// directory if needed.
func SaveReport(dir, id, body string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(ReportPath(dir, id), []byte(body), 0644)
}

// Load reads a plan sidecar from .plans/<id>.md.
// Returns nil, nil if the file does not exist.
func Load(dir, id string) (*Plan, error) {
	path := filepath.Join(dir, id+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return Parse(string(data))
}

// SaveOpts configures Save behavior. The default zero value preserves
// the historical lenient mode (warn-only AC quality); Strict makes AC
// quality a hard rejection so callers like `st plan approve --strict`
// can refuse to greenlight a vague plan. I-511.
type SaveOpts struct {
	Strict bool
	// Stderr captures warning output. nil ⇒ os.Stderr. Tests inject a
	// buffer to assert on warning text without rerouting fd 2.
	Stderr io.Writer
}

// Save writes a plan sidecar to .plans/<id>.md with default options
// (Strict=false). Same back-compat shape as before — existing callers
// don't need to change. I-511 added the SaveWithOpts variant.
func Save(dir, id string, p *Plan) error {
	return SaveWithOpts(dir, id, p, SaveOpts{})
}

// SaveWithOpts is the configurable variant of Save. Returns an error
// when the plan is structurally incomplete (missing scope_repos /
// approach / acceptance_criteria) — same behavior as the old Save.
//
// I-511: when opts.Strict is true, also returns an error if any
// acceptance criterion fails ValidateACs. When Strict is false (the
// default), un-verifiable ACs emit warnings to opts.Stderr but the
// save still succeeds. Rejected plans skip both validation paths
// (they're partial drafts).
func SaveWithOpts(dir, id string, p *Plan, opts SaveOpts) error {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Skip validation for rejected plans — they may be incomplete drafts
	if !p.Rejected {
		var missing []string
		if len(p.ScopeRepos) == 0 {
			missing = append(missing, "scope_repos")
		}
		if p.Approach == "" {
			missing = append(missing, "approach")
		}
		if len(p.ACs) == 0 {
			missing = append(missing, "acceptance_criteria")
		}
		if len(missing) > 0 {
			return fmt.Errorf("plan %s incomplete — missing: %s", id, strings.Join(missing, ", "))
		}

		// I-511: AC quality validation. Findings are advisory by default
		// (warn on stderr) and blocking under --strict.
		if findings := ValidateACs(p.ACs); len(findings) > 0 {
			if opts.Strict {
				var lines []string
				for _, f := range findings {
					lines = append(lines, "  "+f.String())
				}
				return fmt.Errorf("plan %s has un-verifiable acceptance criteria (--strict refused):\n%s",
					id, strings.Join(lines, "\n"))
			}
			fmt.Fprintf(stderr, "plan %s save warning: %d acceptance criterion/criteria not obviously verifiable:\n", id, len(findings))
			for _, f := range findings {
				fmt.Fprintf(stderr, "  %s\n", f.String())
			}
		}
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, id+".md"), []byte(Render(p)), 0644)
}

// Parse extracts a Plan from markdown text with YAML frontmatter.
func Parse(text string) (*Plan, error) {
	p := &Plan{RawText: text}

	// Extract YAML frontmatter (between --- delimiters)
	body := text
	if strings.HasPrefix(text, "---\n") {
		end := strings.Index(text[4:], "\n---")
		if end >= 0 {
			frontmatter := text[4 : 4+end]
			body = strings.TrimSpace(text[4+end+4:])
			parseFrontmatter(p, frontmatter)
		}
	}

	// Parse markdown sections
	sections := parseSections(body)

	if v, ok := sections["Approach"]; ok {
		p.Approach = strings.TrimSpace(v)
	}
	if v, ok := sections["Scope"]; ok && len(p.ScopeRepos) == 0 {
		p.ScopeRepos = parseScopeRepos(v)
	}
	if v, ok := sections["Implementation Steps"]; ok {
		p.Steps = parseList(v)
	}
	if v, ok := sections["Files to Create"]; ok {
		p.FilesToCreate = parseList(v)
	}
	if v, ok := sections["Files to Modify"]; ok {
		p.FilesToModify = parseList(v)
	}
	if v, ok := sections["Acceptance Criteria"]; ok {
		p.ACs = parseList(v)
	}
	if v, ok := sections["Revision History"]; ok {
		for _, line := range parseList(v) {
			parts := strings.SplitN(line, ": ", 2)
			rev := Revision{Timestamp: parts[0]}
			if len(parts) > 1 {
				rev.Summary = parts[1]
			}
			p.Revisions = append(p.Revisions, rev)
		}
	}

	return p, nil
}

// Render produces the markdown representation of a plan.
func Render(p *Plan) string {
	var b strings.Builder

	// YAML frontmatter
	b.WriteString("---\n")
	if len(p.ScopeRepos) > 0 {
		b.WriteString(fmt.Sprintf("scope_repos: [%s]\n", strings.Join(p.ScopeRepos, ", ")))
	}
	b.WriteString(fmt.Sprintf("plan_approved: %v\n", p.Approved))
	if p.ApprovedAt != "" {
		b.WriteString(fmt.Sprintf("approved_at: %s\n", p.ApprovedAt))
	}
	if p.Rejected {
		b.WriteString(fmt.Sprintf("rejected: %v\n", p.Rejected))
	}
	if p.RejectedAt != "" {
		b.WriteString(fmt.Sprintf("rejected_at: %s\n", p.RejectedAt))
	}
	b.WriteString("---\n\n")

	// Approach
	if p.Approach != "" {
		b.WriteString("## Approach\n")
		b.WriteString(p.Approach)
		b.WriteString("\n\n")
	}

	// Scope
	if len(p.ScopeRepos) > 0 {
		b.WriteString("## Scope\n")
		b.WriteString(fmt.Sprintf("Repos: %s\n\n", strings.Join(p.ScopeRepos, ", ")))
	}

	// Implementation Steps
	if len(p.Steps) > 0 {
		b.WriteString("## Implementation Steps\n")
		for i, step := range p.Steps {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, step))
		}
		b.WriteString("\n")
	}

	// Files to Create
	if len(p.FilesToCreate) > 0 {
		b.WriteString("## Files to Create\n")
		for _, f := range p.FilesToCreate {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
		b.WriteString("\n")
	}

	// Files to Modify
	if len(p.FilesToModify) > 0 {
		b.WriteString("## Files to Modify\n")
		for _, f := range p.FilesToModify {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
		b.WriteString("\n")
	}

	// Acceptance Criteria
	if len(p.ACs) > 0 {
		b.WriteString("## Acceptance Criteria\n")
		for _, ac := range p.ACs {
			b.WriteString(fmt.Sprintf("- %s\n", ac))
		}
		b.WriteString("\n")
	}

	// Revision History
	if len(p.Revisions) > 0 {
		b.WriteString("## Revision History\n")
		for _, rev := range p.Revisions {
			b.WriteString(fmt.Sprintf("- %s: %s\n", rev.Timestamp, rev.Summary))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// PlainText returns the plan body suitable for prompt injection.
// Strips frontmatter, keeps all sections.
func PlainText(p *Plan) string {
	if p.Approach == "" && len(p.Steps) == 0 && p.RawText != "" {
		// Couldn't parse sections — return raw text without frontmatter
		text := p.RawText
		if strings.HasPrefix(text, "---\n") {
			if end := strings.Index(text[4:], "\n---"); end >= 0 {
				text = strings.TrimSpace(text[4+end+4:])
			}
		}
		return text
	}

	var b strings.Builder
	if p.Approach != "" {
		b.WriteString("Approach: ")
		b.WriteString(p.Approach)
		b.WriteString("\n\n")
	}
	if len(p.ScopeRepos) > 0 {
		b.WriteString(fmt.Sprintf("Repos: %s\n\n", strings.Join(p.ScopeRepos, ", ")))
	}
	if len(p.Steps) > 0 {
		b.WriteString("Implementation Steps:\n")
		for i, step := range p.Steps {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, step))
		}
		b.WriteString("\n")
	}
	if len(p.FilesToCreate) > 0 {
		b.WriteString("Files to create:\n")
		for _, f := range p.FilesToCreate {
			b.WriteString(fmt.Sprintf("  - %s\n", f))
		}
		b.WriteString("\n")
	}
	if len(p.FilesToModify) > 0 {
		b.WriteString("Files to modify:\n")
		for _, f := range p.FilesToModify {
			b.WriteString(fmt.Sprintf("  - %s\n", f))
		}
	}
	return b.String()
}

// --- Internal helpers ---

func parseFrontmatter(p *Plan, text string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "scope_repos:") {
			val := strings.TrimPrefix(line, "scope_repos:")
			val = strings.TrimSpace(val)
			val = strings.Trim(val, "[]")
			for _, repo := range strings.Split(val, ",") {
				repo = strings.TrimSpace(repo)
				if repo != "" {
					p.ScopeRepos = append(p.ScopeRepos, repo)
				}
			}
		}
		if strings.HasPrefix(line, "plan_approved:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "plan_approved:"))
			p.Approved = val == "true"
		}
		if strings.HasPrefix(line, "approved_at:") {
			p.ApprovedAt = strings.TrimSpace(strings.TrimPrefix(line, "approved_at:"))
		}
		if strings.HasPrefix(line, "rejected:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "rejected:"))
			p.Rejected = val == "true"
		}
		if strings.HasPrefix(line, "rejected_at:") {
			p.RejectedAt = strings.TrimSpace(strings.TrimPrefix(line, "rejected_at:"))
		}
	}
}

// parseScopeRepos extracts repo names from a "## Scope" section.
// Expects format like "Repos: theraprac-api, theraprac-web, theraprac-infra"
func parseScopeRepos(text string) []string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Repos:") {
			val := strings.TrimPrefix(line, "Repos:")
			val = strings.TrimSpace(val)
			var repos []string
			for _, repo := range strings.Split(val, ",") {
				repo = strings.TrimSpace(repo)
				if repo != "" {
					repos = append(repos, repo)
				}
			}
			return repos
		}
	}
	return nil
}

func parseSections(text string) map[string]string {
	sections := make(map[string]string)
	lines := strings.Split(text, "\n")
	currentSection := ""
	var currentContent []string

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			// Save previous section
			if currentSection != "" {
				sections[currentSection] = strings.TrimSpace(strings.Join(currentContent, "\n"))
			}
			currentSection = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			currentContent = nil
		} else if currentSection != "" {
			currentContent = append(currentContent, line)
		}
	}
	// Save last section
	if currentSection != "" {
		sections[currentSection] = strings.TrimSpace(strings.Join(currentContent, "\n"))
	}

	return sections
}

func parseList(text string) []string {
	var items []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		// Strip list markers: "- ", "1. ", "2. ", etc.
		if strings.HasPrefix(line, "- ") {
			line = strings.TrimPrefix(line, "- ")
		} else if len(line) > 2 && line[0] >= '0' && line[0] <= '9' {
			if idx := strings.Index(line, ". "); idx >= 0 && idx < 4 {
				line = line[idx+2:]
			}
		}
		line = strings.TrimSpace(line)
		// Strip balanced outer backtick wrapping that Claude sometimes adds
		// to cmd: ACs (e.g. `cmd: foo` → cmd: foo). Only strip when the
		// entire item is wrapped — inner backticks (cmd: grep -q `x` f) are
		// preserved. I-990.
		if len(line) > 2 && line[0] == '`' && line[len(line)-1] == '`' {
			line = line[1 : len(line)-1]
		}
		if line != "" {
			items = append(items, line)
		}
	}
	return items
}

// Now returns a formatted timestamp for use in revision history.
func Now() string {
	return time.Now().Format(time.RFC3339)
}
