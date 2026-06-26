// Package validate provides schema validation for agent-state items.
// Rules are driven by config — no hardcoded enums.
package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
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
		r.add(item.ID, "id", fmt.Sprintf("invalid format %q — expected X-NNN (uppercase letter, dash, 3+ digits)", item.ID))
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

	// Type-specific required fields (check field key exists in document).
	// Only checked for non-terminal items (archived items may predate field requirements).
	if item.Type != "" && item.Doc != nil && !cfg.IsTerminalStatus(item.Type, item.Status) {
		if tc, ok := cfg.Types[item.Type]; ok {
			for _, field := range tc.RequiredFields {
				if !HasField(item.Doc, field) {
					r.add(item.ID, field, fmt.Sprintf("required for type %q", item.Type))
				}
			}
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

// IndexCoverage checks that all non-archived items appear in the index content.
func IndexCoverage(items map[string]*model.Item, indexContent string, cfg *config.Config) []Error {
	var errs []Error
	for id, item := range items {
		// Only check items in non-terminal (active) statuses
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}
		if !strings.Contains(indexContent, id) {
			errs = append(errs, Error{
				ItemID:  id,
				Field:   "index",
				Message: "not listed in index.md",
			})
		}
	}
	return errs
}

// DeliveryGate checks that items in terminal status have reached the required delivery stage.
func DeliveryGate(item *model.Item, cfg *config.Config) *Result {
	r := &Result{}

	if cfg.Delivery == nil || cfg.Delivery.ArchiveGate == "" {
		return r
	}

	// Only check items in terminal status
	if !cfg.IsTerminalStatus(item.Type, item.Status) {
		return r
	}

	// Skip items without delivery data (predates delivery tracking)
	if item.Delivery == nil || len(item.Delivery) == 0 {
		return r
	}

	// Skip non-task/issue types (e.g., deprecated promotions)
	if item.Type != "task" && item.Type != "issue" {
		return r
	}

	stage, _ := item.Delivery["stage"].(string)
	if !cfg.StageReached(stage, cfg.Delivery.ArchiveGate) {
		if stage == "" {
			stage = "null"
		}
		r.add(item.ID, "delivery", fmt.Sprintf("archived without reaching %s (delivery_stage: %s)",
			cfg.Delivery.ArchiveGate, stage))
	}

	return r
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

// HasField checks if a field key exists in the parsed document (top-level only).
func HasField(doc *model.ParsedDocument, key string) bool {
	for _, line := range doc.Lines {
		if line.Key == key && line.Indent == 0 {
			return true
		}
	}
	return false
}

// DuplicateID is one drift report from DuplicateIDs: a single item file
// (identified by basename) that exists in more than one type-directory
// at the same time. The ID field is the parsed ID prefix (e.g.,
// "I-443"); Paths lists every location of the file.
type DuplicateID struct {
	ID    string
	Paths []string
}

// DuplicateIDs walks every type-directory under itemDir and returns one
// DuplicateID entry per filename that appears in two or more dirs. This
// is the I-472 drift detector — the symptom of a peer-merge event that
// re-introduced a stale pre-archive copy alongside the canonical one.
// The fix-path in `st check` calls Store.RemoveStaleDuplicates(id).
//
// IMPORTANT: this matches by full filename, not just the ID prefix. Two
// files with the same ID prefix but different slugs (e.g.
// `T-015-billing.md` and `T-015-openapi.md`) represent a historical
// ID-collision — different items that happen to share an ID — and are
// NOT reported here. Removing either would destroy data; that drift
// class needs separate human triage.
func DuplicateIDs(itemDir string, cfg *config.Config) []DuplicateID {
	dirs := make(map[string]bool)
	for _, tc := range cfg.Types {
		for _, dir := range tc.DirectoryMap {
			dirs[dir] = true
		}
	}

	type fileLoc struct {
		basename string
		path     string
	}
	byBase := make(map[string][]fileLoc)
	for dir := range dirs {
		dirPath := filepath.Join(itemDir, dir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			if !os.IsNotExist(err) {
				// IsNotExist is the expected case (a configured dir
				// that doesn't exist yet on this machine). Anything
				// else (permission, transient I/O) is silent
				// false-negative-prone — surface it on stderr so a
				// CI/hook run sees the partial-scan condition.
				fmt.Fprintf(os.Stderr, "warning: DuplicateIDs cannot read %s: %v\n", dirPath, err)
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			if entry.Name() == "index.md" || entry.Name() == "README.md" {
				continue
			}
			byBase[entry.Name()] = append(byBase[entry.Name()],
				fileLoc{basename: entry.Name(), path: filepath.Join(dirPath, entry.Name())})
		}
	}

	var dups []DuplicateID
	for basename, locs := range byBase {
		if len(locs) < 2 {
			continue
		}
		id := extractIDFromFilename(basename)
		if id == "" {
			continue
		}
		paths := make([]string, 0, len(locs))
		for _, l := range locs {
			paths = append(paths, l.path)
		}
		sort.Strings(paths)
		dups = append(dups, DuplicateID{ID: id, Paths: paths})
	}
	sort.Slice(dups, func(i, j int) bool { return dups[i].ID < dups[j].ID })
	return dups
}

// extractIDFromFilename pulls the leading ID prefix off an item file
// name. Filenames have the form "<PREFIX>-<NUM>-<slug>.md" or
// "<PREFIX>-<NUM>.md"; the ID is the first two hyphen-delimited
// segments. Returns "" when the filename doesn't match the pattern so
// strays in the directory don't false-trigger duplicate reports.
func extractIDFromFilename(name string) string {
	base := strings.TrimSuffix(name, ".md")
	parts := strings.SplitN(base, "-", 3)
	if len(parts) < 2 {
		return ""
	}
	id := parts[0] + "-" + parts[1]
	if !idPattern.MatchString(id) {
		return ""
	}
	return id
}
