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

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/parse"
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

// scan walks all directories defined in config and loads items. When
// the same ID appears in multiple type-directories, scan picks the file
// in the directory matching the item's current status. Two drift
// classes can produce that situation:
//
//   - I-472 peer-merge resurrection — a peer agent's feature branch
//     created when the item was still active brings back the
//     pre-archive copy after merge, so issues/<id>-foo.md and
//     archive/<id>-foo.md (same basename) coexist. Safe to auto-clean
//     via Store.RemoveStaleDuplicates.
//   - ID collision — two different items with the same ID prefix but
//     different filename slugs (e.g. T-015-billing.md +
//     T-015-openapi.md). Auto-cleaning would destroy data; this case
//     warns to stderr and requires human triage.
//
// On-disk state is unchanged here; cleanup happens in Close and
// `st check --fix`.
func (s *Store) scan() error {
	root := s.cfg.ItemDir()

	// Collect all directories from type configs
	dirs := make(map[string]bool)
	for _, tc := range s.cfg.Types {
		for _, dir := range tc.DirectoryMap {
			dirs[dir] = true
		}
	}

	candidates := make(map[string][]scanCandidate)

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
				candidates[item.ID] = append(candidates[item.ID], scanCandidate{path: path, item: item})
			}
		}
	}

	for id, cands := range candidates {
		if len(cands) == 1 {
			s.items[id] = cands[0].item
			s.paths[id] = cands[0].path
			continue
		}

		chosen := pickCanonicalCandidate(cands, s.cfg)
		paths := make([]string, 0, len(cands))
		for _, c := range cands {
			paths = append(paths, c.path)
		}
		if sameBasename(cands) {
			fmt.Fprintf(os.Stderr,
				"warning: duplicate id %s in %v — using %s. Run `st check` to clean up.\n",
				id, paths, chosen.path)
		} else {
			fmt.Fprintf(os.Stderr,
				"warning: id collision %s — different items share this ID: %v. Using %s; rename the others manually.\n",
				id, paths, chosen.path)
		}
		s.items[id] = chosen.item
		s.paths[id] = chosen.path
	}

	return nil
}

// scanCandidate is one parsed (path, item) pair seen during a scan.
type scanCandidate struct {
	path string
	item *model.Item
}

// sameBasename reports whether every candidate has the same filename
// basename. True ⇒ peer-merge resurrection (I-472, safe to auto-clean).
// False ⇒ ID-collision (different items, needs human triage).
func sameBasename(cands []scanCandidate) bool {
	if len(cands) < 2 {
		return true
	}
	first := filepath.Base(cands[0].path)
	for _, c := range cands[1:] {
		if filepath.Base(c.path) != first {
			return false
		}
	}
	return true
}

// effectiveTouch returns the most recent intentional-write timestamp for
// an item: the later of LastTouched and Completed. A close stamps both;
// a stale resurrected copy (carried in by a peer git-merge from before the
// close) retains its older timestamps. Used as the primary recency signal
// in pickCanonicalCandidate so the last real state transition wins.
func effectiveTouch(item *model.Item) time.Time {
	t := item.LastTouched
	if item.Completed != nil && item.Completed.After(t) {
		t = *item.Completed
	}
	return t
}

// pickCanonicalCandidate chooses which of several files sharing an ID is the
// authoritative one. Selection is a deterministic, layered comparison so the
// result never depends on directory-scan (Go map) iteration order:
//
//  1. Self-consistent first — a copy whose directory matches its own status
//     (e.g. status=done living in archive/) beats a copy that is mid-move or
//     corrupt. When exactly one copy is self-consistent it always wins.
//  2. More-recent effectiveTouch — when several copies are each self-consistent
//     (the I-1241 case: a done/archive copy plus a pre-close active/issues copy
//     resurrected by a peer merge), the one written most recently wins. This is
//     correct in BOTH directions: on close the archive copy is newer; on a
//     genuine reopen the issues copy is newer — so neither silently reverts.
//  3. Terminal status — tie-break when timestamps are equal (e.g. an
//     identical-body resurrection): a terminal copy (done/abandoned) is the
//     intended end state and beats a non-terminal one.
//  4. Lexicographically-smallest path — final tie-break for full determinism.
func pickCanonicalCandidate(cands []scanCandidate, cfg *config.Config) scanCandidate {
	chosen := cands[0]
	for _, c := range cands[1:] {
		if betterCandidate(c, chosen, cfg) {
			chosen = c
		}
	}
	return chosen
}

// selfConsistent reports whether a candidate file sits in the directory that
// its own status maps to (e.g. status=done living in archive/). A copy that
// is mid-move or corrupt fails this check.
func selfConsistent(c scanCandidate, cfg *config.Config) bool {
	dir := cfg.DirectoryForStatus(c.item.Type, c.item.Status)
	return dir != "" && filepath.Base(filepath.Dir(c.path)) == dir
}

// betterCandidate reports whether candidate a should be preferred over b under
// the layered precedence documented on pickCanonicalCandidate.
func betterCandidate(a, b scanCandidate, cfg *config.Config) bool {
	// 1. self-consistency
	if ac, bc := selfConsistent(a, cfg), selfConsistent(b, cfg); ac != bc {
		return ac
	}
	// 2. recency
	if at, bt := effectiveTouch(a.item), effectiveTouch(b.item); !at.Equal(bt) {
		return at.After(bt)
	}
	// 3. terminal status
	at := cfg.IsTerminalStatus(a.item.Type, a.item.Status)
	bt := cfg.IsTerminalStatus(b.item.Type, b.item.Status)
	if at != bt {
		return at
	}
	// 4. deterministic path fallback
	return a.path < b.path
}

// RemoveStaleDuplicates scans every type directory for files whose
// basename matches the canonical file's basename and removes any that
// are not at s.paths[id]. Returns the slice of removed paths.
// Idempotent — when there are no duplicates, returns an empty slice
// with a nil error.
//
// Basename match is the safety boundary. The I-472 case (peer-merge
// resurrection) creates a file with an IDENTICAL filename in a second
// type-dir, and that's what we sweep. Different-slug files that share
// the same ID prefix are historical ID-collisions — a different drift
// class — and removing either of them would destroy data, so this
// helper never touches them.
//
// Used by `st close` (after Move) and `st check --fix`.
func (s *Store) RemoveStaleDuplicates(id string) ([]string, error) {
	canonical, ok := s.paths[id]
	if !ok {
		return nil, fmt.Errorf("no path for item %s", id)
	}
	canonicalBase := filepath.Base(canonical)

	root := s.cfg.ItemDir()
	dirs := make(map[string]bool)
	for _, tc := range s.cfg.Types {
		for _, dir := range tc.DirectoryMap {
			dirs[dir] = true
		}
	}

	var removed []string
	for dir := range dirs {
		dirPath := filepath.Join(root, dir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("reading %s: %w", dirPath, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() != canonicalBase {
				continue
			}
			path := filepath.Join(dirPath, entry.Name())
			if path == canonical {
				continue
			}
			if err := os.Remove(path); err != nil {
				if os.IsNotExist(err) {
					// A concurrent st invocation (peer agent or
					// st check --fix racing st close) already
					// removed it. Treat as success — the goal is
					// "no duplicate at this path" and that goal is
					// met.
					continue
				}
				return removed, fmt.Errorf("removing duplicate %s: %w", path, err)
			}
			removed = append(removed, path)
		}
	}
	return removed, nil
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

// GoalFilter returns items whose Goals slice contains the given goalID.
func GoalFilter(goalID string) Filter {
	return func(item *model.Item) bool {
		for _, g := range item.Goals {
			if g == goalID {
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

// PriorityFilter returns items matching a priority value or comma-separated list (e.g. "0", "0,1").
func PriorityFilter(priority string) Filter {
	return func(item *model.Item) bool {
		if item.Priority == nil {
			return false
		}
		p := strconv.Itoa(*item.Priority)
		for _, v := range strings.Split(priority, ",") {
			if strings.TrimSpace(v) == p {
				return true
			}
		}
		return false
	}
}

// SprintFilter returns items belonging to a specific sprint.
func SprintFilter(sprintID string) Filter {
	return func(item *model.Item) bool { return item.Sprint == sprintID }
}

// EpicFilter returns items belonging to a specific epic.
func EpicFilter(epicID string) Filter {
	return func(item *model.Item) bool { return item.Epic == epicID }
}

// ArcFilter returns items tagged with a specific arc.
func ArcFilter(arc string) Filter {
	return func(item *model.Item) bool { return item.Arc == arc }
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

// Move moves an item file to the correct directory for its current status,
// returning the item's resulting path (whether or not a rename happened).
// I-1721: returning the path lets callers that need it after a Move use
// this return value directly instead of a follow-up s.Path(id) lookup that
// re-derives the same string Move already had in hand.
func (s *Store) Move(id string) (string, error) {
	item, ok := s.items[id]
	if !ok {
		return "", fmt.Errorf("item %s not found", id)
	}

	oldPath, ok := s.paths[id]
	if !ok {
		return "", fmt.Errorf("no path for item %s", id)
	}

	dir := s.cfg.DirectoryForStatus(item.Type, item.Status)
	if dir == "" {
		return "", fmt.Errorf("no directory for type=%s status=%s", item.Type, item.Status)
	}

	filename := filepath.Base(oldPath)
	newPath := filepath.Join(s.cfg.ItemDir(), dir, filename)

	if oldPath == newPath {
		return oldPath, nil // already in correct location
	}

	// Ensure target directory exists
	if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
		return "", err
	}

	if err := os.Rename(oldPath, newPath); err != nil {
		return "", fmt.Errorf("moving %s -> %s: %w", oldPath, newPath, err)
	}

	s.paths[id] = newPath
	return newPath, nil
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
