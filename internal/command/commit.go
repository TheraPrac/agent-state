package command

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Commit records a commit message in work_tracking.commits for an item.
func Commit(s *store.Store, cfg *config.Config, id, message string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Doc == nil {
		fmt.Fprintf(os.Stderr, "%s has no document\n", id)
		return 1
	}

	now := time.Now().Format(time.RFC3339)

	// Try to get the HEAD SHA from the current directory
	sha := ""
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		sha = strings.TrimSpace(string(out))
	}

	commitLine := fmt.Sprintf("%s %s: %s", now[:10], sha, message)

	// Add to work_tracking.commits in the document
	appendToNestedList(item.Doc, "work_tracking", "commits", commitLine)

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "commit", Field: "work_tracking.commits", NewValue: message,
	})

	fmt.Printf("Recorded commit on %s: %s\n", id, message)
	return 0
}

// appendToNestedList appends a value to a list field nested under a parent key.
// If the parent or list doesn't exist, it creates them.
func appendToNestedList(doc *model.ParsedDocument, parent, key, value string) {
	// Find the parent section
	parentIdx := -1
	keyIdx := -1
	for i, line := range doc.Lines {
		if line.Key == parent && line.Indent == 0 {
			parentIdx = i
		}
		if parentIdx >= 0 && line.Key == key && line.Indent > 0 {
			keyIdx = i
		}
	}

	if parentIdx < 0 {
		// Parent section doesn't exist — create it
		doc.Lines = append(doc.Lines, model.Line{Raw: "", IsEmpty: true})
		doc.Lines = append(doc.Lines, model.Line{Raw: parent + ":", Key: parent})
		doc.Lines = append(doc.Lines, model.Line{Raw: "  " + key + ":", Key: key, Indent: 2, BlockKey: parent})
		doc.Lines = append(doc.Lines, model.Line{Raw: "  - " + value, IsList: true, Indent: 2, BlockKey: parent})
		return
	}

	if keyIdx < 0 {
		// Parent exists but key doesn't — find end of parent section and insert
		insertAt := parentIdx + 1
		for insertAt < len(doc.Lines) {
			line := doc.Lines[insertAt]
			if line.Indent > 0 || line.IsEmpty {
				insertAt++
			} else {
				break
			}
		}
		newLines := []model.Line{
			{Raw: "  " + key + ":", Key: key, Indent: 2, BlockKey: parent},
			{Raw: "  - " + value, IsList: true, Indent: 2, BlockKey: parent},
		}
		doc.Lines = append(doc.Lines[:insertAt], append(newLines, doc.Lines[insertAt:]...)...)
		return
	}

	// Key exists — find the end of its list and check for empty marker
	insertAt := keyIdx + 1
	for insertAt < len(doc.Lines) {
		line := doc.Lines[insertAt]
		if line.IsList && line.Indent >= 2 {
			// Check for empty list marker
			raw := line.Raw
			trimmed := ""
			for _, c := range raw {
				if c != ' ' && c != '\t' {
					trimmed += string(c)
				}
			}
			if trimmed == "-[]" || trimmed == "-[[]]" {
				// Replace empty marker with actual value
				doc.Lines[insertAt] = model.Line{Raw: "  - " + value, IsList: true, Indent: 2, BlockKey: parent}
				return
			}
			insertAt++
		} else {
			break
		}
	}

	// Append new list item
	newLine := model.Line{Raw: "  - " + value, IsList: true, Indent: 2, BlockKey: parent}
	doc.Lines = append(doc.Lines[:insertAt], append([]model.Line{newLine}, doc.Lines[insertAt:]...)...)
}
