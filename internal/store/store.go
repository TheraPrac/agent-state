// Package store provides file-based storage for agent-state items.
// It handles scanning directories, loading items, ID allocation,
// and writing changes back with git sync.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/parse"
)

// Store manages items on disk.
type Store struct {
	cfg   *config.Config
	items map[string]*model.Item // keyed by ID
	paths map[string]string      // ID -> file path
}

// New creates a store from config and scans all item directories.
func New(cfg *config.Config) (*Store, error) {
	s := &Store{
		cfg:   cfg,
		items: make(map[string]*model.Item),
		paths: make(map[string]string),
	}

	if err := s.scan(); err != nil {
		return nil, fmt.Errorf("scanning items: %w", err)
	}

	return s, nil
}

// scan walks all directories defined in config and loads items.
func (s *Store) scan() error {
	root := s.cfg.ItemDir()

	// Collect all directories from type configs
	dirs := make(map[string]bool)
	for _, tc := range s.cfg.Types {
		for _, dir := range tc.DirectoryMap {
			dirs[dir] = true
		}
	}

	for dir := range dirs {
		dirPath := filepath.Join(root, dir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("reading %s: %w", dirPath, err)
		}

		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			// Skip known non-item files
			if entry.Name() == "index.md" || entry.Name() == "README.md" {
				continue
			}

			path := filepath.Join(dirPath, entry.Name())
			item, err := parse.File(path)
			if err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}

			if item.ID != "" {
				s.items[item.ID] = item
				s.paths[item.ID] = path
			}
		}
	}

	return nil
}

// Get returns an item by ID.
func (s *Store) Get(id string) (*model.Item, bool) {
	item, ok := s.items[id]
	return item, ok
}

// Path returns the file path for an item ID.
func (s *Store) Path(id string) (string, bool) {
	path, ok := s.paths[id]
	return path, ok
}

// All returns all items.
func (s *Store) All() map[string]*model.Item {
	return s.items
}

// List returns items matching filters, sorted by ID.
func (s *Store) List(filters ...Filter) []*model.Item {
	var result []*model.Item
	for _, item := range s.items {
		if matchesAll(item, filters) {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}

// Filter is a predicate for filtering items.
type Filter func(*model.Item) bool

// TypeFilter returns items of a specific type.
func TypeFilter(t string) Filter {
	return func(item *model.Item) bool { return item.Type == t }
}

// StatusFilter returns items with a specific status.
func StatusFilter(s string) Filter {
	return func(item *model.Item) bool { return item.Status == s }
}

// TagFilter returns items with a specific tag.
func TagFilter(tag string) Filter {
	return func(item *model.Item) bool {
		for _, t := range item.Tags {
			if t == tag {
				return true
			}
		}
		return false
	}
}

// AssignedFilter returns items assigned to a specific agent.
func AssignedFilter(agent string) Filter {
	return func(item *model.Item) bool { return item.AssignedTo == agent }
}

// NonTerminalFilter returns items not in terminal statuses.
func NonTerminalFilter(cfg *config.Config) Filter {
	return func(item *model.Item) bool {
		tc, ok := cfg.Types[item.Type]
		if !ok {
			return true
		}
		for _, ts := range tc.Statuses {
			// Check if this status is terminal by checking directory_map
			// Terminal statuses map to archive
			if ts == item.Status {
				dir := tc.DirectoryMap[ts]
				return dir != "archive"
			}
		}
		return true
	}
}

func matchesAll(item *model.Item, filters []Filter) bool {
	for _, f := range filters {
		if !f(item) {
			return false
		}
	}
	return true
}

// NextID allocates the next available ID for a type.
func (s *Store) NextID(itemType string) (string, error) {
	tc, ok := s.cfg.Types[itemType]
	if !ok {
		return "", fmt.Errorf("unknown type: %s", itemType)
	}

	prefix := tc.IDPrefix
	maxNum := 0

	for id := range s.items {
		if !strings.HasPrefix(id, prefix+"-") {
			continue
		}
		numStr := id[len(prefix)+1:]
		num, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		if num > maxNum {
			maxNum = num
		}
	}

	return fmt.Sprintf("%s-%03d", prefix, maxNum+1), nil
}

// write persists an item to disk using its ParsedDocument for lossless
// output. It is package-private — external callers must go through
// Mutate (for existing items) or Create (for new items) so every
// public write path holds a flock and re-reads under the lock. write
// is retained for the package-internal store_test.go and git_test.go
// fixtures that need the legacy single-shot path.
func (s *Store) write(item *model.Item) error {
	if item.Doc == nil {
		return fmt.Errorf("item %s has no ParsedDocument", item.ID)
	}

	// Auto-update timestamps
	now := time.Now().Format(time.RFC3339)
	item.Doc.SetField("last_touched", now)

	// Auto-update agent ID if set
	agentID := s.cfg.AgentID()
	if agentID != "" {
		item.Doc.SetField("last_touched_by", agentID)
	}

	path, ok := s.paths[item.ID]
	if !ok {
		// New item — compute path from type + status
		dir := s.cfg.DirectoryForStatus(item.Type, item.Status)
		if dir == "" {
			return fmt.Errorf("no directory for type=%s status=%s", item.Type, item.Status)
		}
		filename := s.filenameForItem(item)
		path = filepath.Join(s.cfg.ItemDir(), dir, filename)
		s.paths[item.ID] = path
	}

	content := item.Doc.String()
	// Ensure file ends with newline
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	// Update in-memory state
	s.items[item.ID] = item

	return nil
}

// Move moves an item file to the correct directory for its current status.
func (s *Store) Move(id string) error {
	item, ok := s.items[id]
	if !ok {
		return fmt.Errorf("item %s not found", id)
	}

	oldPath, ok := s.paths[id]
	if !ok {
		return fmt.Errorf("no path for item %s", id)
	}

	dir := s.cfg.DirectoryForStatus(item.Type, item.Status)
	if dir == "" {
		return fmt.Errorf("no directory for type=%s status=%s", item.Type, item.Status)
	}

	filename := filepath.Base(oldPath)
	newPath := filepath.Join(s.cfg.ItemDir(), dir, filename)

	if oldPath == newPath {
		return nil // already in correct location
	}

	// Ensure target directory exists
	if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
		return err
	}

	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("moving %s -> %s: %w", oldPath, newPath, err)
	}

	s.paths[id] = newPath
	return nil
}

func (s *Store) filenameForItem(item *model.Item) string {
	// Generate slug from title
	slug := strings.ToLower(item.Title)
	slug = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r == ' ' || r == '-' || r == '_' {
			return '-'
		}
		return -1
	}, slug)
	// Collapse multiple dashes
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	// Truncate
	if len(slug) > 60 {
		slug = slug[:60]
	}
	return fmt.Sprintf("%s-%s.md", item.ID, slug)
}
