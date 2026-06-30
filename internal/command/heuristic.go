package command

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
)

// Heuristic_Add records a single operational heuristic for the current agent.
func Heuristic_Add(cfg *config.Config, text, tags string) int {
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "st heuristic add: --text is required")
		return 1
	}
	var relevanceTags []string
	for _, t := range strings.Split(tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			relevanceTags = append(relevanceTags, t)
		}
	}
	entry := changelog.Entry{
		Op:            "heuristic_add",
		Reason:        text,
		RelevanceTags: relevanceTags,
	}
	if err := changelog.HeuristicAppend(cfg, entry); err != nil {
		fmt.Fprintf(os.Stderr, "st heuristic add: %v\n", err)
		return 1
	}
	fmt.Printf("recorded heuristic for %s\n", cfg.AgentID())
	return 0
}

// Heuristic_List prints all recorded heuristics for a given agent.
func Heuristic_List(cfg *config.Config, agentID string) int {
	if agentID == "" {
		agentID = cfg.AgentID()
	}
	entries, err := changelog.HeuristicActiveList(cfg, agentID, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "st heuristic list: %v\n", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Printf("no heuristics recorded for %s\n", agentID)
		return 0
	}
	for _, e := range entries {
		ts := e.Timestamp
		if len(ts) > 19 {
			ts = ts[:19]
		}
		fmt.Printf("[%s] %s\n", ts, e.Reason)
		if len(e.RelevanceTags) > 0 {
			fmt.Printf("  tags: %s\n", strings.Join(e.RelevanceTags, ","))
		}
	}
	return 0
}

// Heuristic_Migrate imports agent-memory/feedback_*.md files as KindHeuristic
// entries. Idempotent: skips files whose basename already has a matching entry.
func Heuristic_Migrate(cfg *config.Config) int {
	agentMemoryDir := filepath.Join(filepath.Dir(cfg.Root()), "theraprac-workspace", "agent-memory")
	pattern := filepath.Join(agentMemoryDir, "feedback_*.md")
	files, err := filepath.Glob(pattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "st heuristic migrate: glob %s: %v\n", pattern, err)
		return 1
	}
	if len(files) == 0 {
		fmt.Println("no agent-memory/feedback_*.md files found")
		return 0
	}

	existing, err := changelog.HeuristicList(cfg, cfg.AgentID(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "st heuristic migrate: reading existing: %v\n", err)
		return 1
	}
	migratedSet := make(map[string]bool, len(existing))
	for _, e := range existing {
		if e.Field != "" {
			migratedSet[e.Field] = true
		}
	}

	n := 0
	for _, path := range files {
		base := filepath.Base(path)
		if migratedSet[base] {
			continue
		}
		reason, err := readAgentMemoryBody(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "st heuristic migrate: reading %s: %v\n", base, err)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			continue
		}
		entry := changelog.Entry{
			Op:     "heuristic_migrate",
			Field:  base,
			Reason: reason,
			Source: changelog.SourceExtracted,
			Scope:  "per-agent",
		}
		if err := changelog.HeuristicAppend(cfg, entry); err != nil {
			fmt.Fprintf(os.Stderr, "st heuristic migrate: writing %s: %v\n", base, err)
			return 1
		}
		n++
	}
	fmt.Printf("migrated %d heuristic(s) from agent-memory/\n", n)
	return 0
}

// Heuristic_Retire marks a recorded heuristic as superseded. idOrIndex is
// either a 1-based integer index into the active list or a timestamp prefix.
// The retire is recorded as a changelog entry so HeuristicActiveList and
// st resume can filter it out.
func Heuristic_Retire(cfg *config.Config, idOrIndex, reason string) int {
	if strings.TrimSpace(reason) == "" {
		fmt.Fprintln(os.Stderr, "st heuristic retire: --reason is required")
		return 1
	}
	if strings.TrimSpace(idOrIndex) == "" {
		fmt.Fprintln(os.Stderr, "st heuristic retire: id or index is required")
		return 1
	}
	// Select from active entries only: excludes retire tombstones and already-retired entries.
	active, err := changelog.HeuristicActiveList(cfg, cfg.AgentID(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "st heuristic retire: %v\n", err)
		return 1
	}

	var target *changelog.Entry
	if idx, err := strconv.Atoi(idOrIndex); err == nil {
		// 1-based index into active list.
		if idx < 1 || idx > len(active) {
			fmt.Fprintf(os.Stderr, "st heuristic retire: index %d out of range (1-%d)\n", idx, len(active))
			return 1
		}
		e := active[idx-1]
		target = &e
	} else {
		// Timestamp prefix match — collect all matches to detect ambiguity.
		var matches []changelog.Entry
		for _, e := range active {
			if strings.HasPrefix(e.Timestamp, idOrIndex) {
				matches = append(matches, e)
			}
		}
		switch len(matches) {
		case 0:
			fmt.Fprintf(os.Stderr, "st heuristic retire: no active heuristic found matching %q\n", idOrIndex)
			return 1
		case 1:
			e := matches[0]
			target = &e
		default:
			fmt.Fprintf(os.Stderr, "st heuristic retire: ambiguous prefix %q matches %d entries — use a more specific prefix or an index\n", idOrIndex, len(matches))
			return 1
		}
	}

	retire := changelog.Entry{
		Op:     "heuristic_retire",
		Field:  target.Timestamp,
		Reason: reason,
	}
	if err := changelog.HeuristicAppend(cfg, retire); err != nil {
		fmt.Fprintf(os.Stderr, "st heuristic retire: %v\n", err)
		return 1
	}
	ts := target.Timestamp
	if len(ts) > 19 {
		ts = ts[:19]
	}
	fmt.Printf("retired heuristic [%s]: %s\n", ts, target.Reason)
	return 0
}

// readAgentMemoryBody reads the body (below frontmatter) of a memory file.
func readAgentMemoryBody(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inFrontmatter := false
	frontmatterDone := false
	dashCount := 0
	var bodyLines []string

	for scanner.Scan() {
		line := scanner.Text()
		if !frontmatterDone {
			if line == "---" {
				dashCount++
				if dashCount == 1 {
					inFrontmatter = true
					continue
				}
				if dashCount == 2 && inFrontmatter {
					frontmatterDone = true
					continue
				}
			}
			if inFrontmatter {
				continue
			}
		}
		bodyLines = append(bodyLines, line)
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.Join(bodyLines, "\n")), nil
}
