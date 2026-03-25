// Package validate provides schema validation for agent-state items.
// Rules are driven by config — no hardcoded enums.
package validate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// Error represents a single validation failure.
type Error struct {
	ItemID  string
	Field   string
	Message string
}

func (e Error) String() string {
	if e.ItemID != "" {
		return fmt.Sprintf("%s: %s — %s", e.ItemID, e.Field, e.Message)
	}
	return fmt.Sprintf("%s — %s", e.Field, e.Message)
}

// Result holds all validation errors for an item.
type Result struct {
	Errors []Error
}

// OK returns true if there are no validation errors.
func (r *Result) OK() bool {
	return len(r.Errors) == 0
}

func (r *Result) add(itemID, field, msg string) {
	r.Errors = append(r.Errors, Error{ItemID: itemID, Field: field, Message: msg})
}

// Item validates a single item against the config schema.
func Item(item *model.Item, cfg *config.Config) *Result {
	r := &Result{}

	// Required fields
	if item.ID == "" {
		r.add(item.ID, "id", "required")
	}
	if item.Type == "" {
		r.add(item.ID, "type", "required")
	}
	if item.Status == "" {
		r.add(item.ID, "status", "required")
	}
	if item.Title == "" {
		r.add(item.ID, "title", "required")
	}
	if item.Created.IsZero() {
		r.add(item.ID, "created", "required")
	}
	if item.LastTouched.IsZero() {
		r.add(item.ID, "last_touched", "required")
	}

	// ID format
	if item.ID != "" && !isValidID(item.ID) {
		r.add(item.ID, "id", fmt.Sprintf("invalid format %q — expected T-NNN, I-NNN, or D-NNN", item.ID))
	}

	// Type must be known
	if item.Type != "" {
		if _, ok := cfg.Types[item.Type]; !ok {
			validTypes := make([]string, 0, len(cfg.Types))
			for k := range cfg.Types {
				validTypes = append(validTypes, k)
			}
			r.add(item.ID, "type", fmt.Sprintf("unknown type %q — valid: %s", item.Type, strings.Join(validTypes, ", ")))
		}
	}

	// Status must be valid for the type
	if item.Type != "" && item.Status != "" {
		validStatuses := cfg.ValidStatuses(item.Type)
		if len(validStatuses) > 0 && !contains(validStatuses, item.Status) {
			r.add(item.ID, "status", fmt.Sprintf("invalid status %q for type %q — valid: %s",
				item.Status, item.Type, strings.Join(validStatuses, ", ")))
		}
	}

	return r
}

// DirectoryConsistency checks that an item's file is in the correct directory for its status.
func DirectoryConsistency(item *model.Item, actualDir string, cfg *config.Config) *Result {
	r := &Result{}

	expectedDir := cfg.DirectoryForStatus(item.Type, item.Status)
	if expectedDir == "" {
		return r // unknown type/status — already caught by Item validation
	}

	// Normalize: strip trailing slash, compare base directory name
	actualBase := strings.TrimSuffix(actualDir, "/")
	parts := strings.Split(actualBase, "/")
	if len(parts) > 0 {
		actualBase = parts[len(parts)-1]
	}

	expectedBase := strings.TrimSuffix(expectedDir, "/")

	if actualBase != expectedBase {
		r.add(item.ID, "directory", fmt.Sprintf("item is in %q but status %q requires %q",
			actualBase, item.Status, expectedBase))
	}

	return r
}

// ReciprocalDeps checks that all depends_on/blocks relationships are reciprocal.
// items is a map of ID -> Item for all items in the system.
func ReciprocalDeps(items map[string]*model.Item) []Error {
	var errs []Error

	for id, item := range items {
		for _, depID := range item.DependsOn {
			dep, ok := items[depID]
			if !ok {
				errs = append(errs, Error{
					ItemID:  id,
					Field:   "depends_on",
					Message: fmt.Sprintf("references %s which does not exist", depID),
				})
				continue
			}
			if !containsStr(dep.Blocks, id) {
				errs = append(errs, Error{
					ItemID:  id,
					Field:   "depends_on",
					Message: fmt.Sprintf("depends on %s, but %s does not list %s in blocks", depID, depID, id),
				})
			}
		}

		for _, blockID := range item.Blocks {
			blocked, ok := items[blockID]
			if !ok {
				// Don't error on missing — item might be archived or future
				continue
			}
			if !containsStr(blocked.DependsOn, id) {
				errs = append(errs, Error{
					ItemID:  id,
					Field:   "blocks",
					Message: fmt.Sprintf("blocks %s, but %s does not list %s in depends_on", blockID, blockID, id),
				})
			}
		}
	}

	return errs
}

var idPattern = regexp.MustCompile(`^[A-Z]-\d{3,}$`)

func isValidID(id string) bool {
	return idPattern.MatchString(id)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
