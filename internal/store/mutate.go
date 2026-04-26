package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/parse"
)

// ErrLockTimeout is returned by Mutate / MutateMany when an exclusive
// flock could not be acquired within LockTimeout.
var ErrLockTimeout = errors.New("flock acquire timed out")

// ErrItemMissing is returned when the on-disk file for an item does
// not exist at the moment Mutate / MutateMany tries to lock it.
var ErrItemMissing = errors.New("item file missing")

// LockTimeout is the maximum time Mutate will wait for an exclusive
// flock on an item's lock file. The default of 30 seconds covers a slow
// concurrent writer; tests override it for fast failure.
var LockTimeout = 30 * time.Second

// lockPollInterval controls how often Mutate retries a non-blocking
// flock acquire while waiting up to LockTimeout. Small enough that
// release-to-acquire latency is bounded; large enough that pure
// contention loops don't spin a CPU.
const lockPollInterval = 50 * time.Millisecond

// Mutate atomically applies fn to the item identified by id. It
// acquires an exclusive flock on the item's dedicated lock file,
// re-reads the item from disk (so fn never operates on a stale
// in-memory copy), invokes fn, and on a nil error from fn writes the
// result with a tmp + rename atomic publish — all while holding the
// lock.
//
// fn must be a pure transformation: no network calls, no exec, no
// other store operations. External data (PR numbers, test output, git
// state) must be gathered before entering Mutate and captured in the
// closure. This is the only way to guarantee that a retry under
// contention behaves predictably.
//
// On any error (lock timeout, missing file, parse failure, fn error,
// write failure), the on-disk state is unchanged. fn errors propagate
// verbatim; callers can use errors.Is to test for sentinel values.
func (s *Store) Mutate(id string, fn func(*model.Item) error) error {
	path, ok := s.paths[id]
	if !ok {
		return ErrItemMissing
	}

	release, err := acquireExclusive(s.cfg.ItemDir(), id, path)
	if err != nil {
		return err
	}
	defer release()

	// Re-parse from disk under the lock — the in-memory copy may be
	// stale relative to a write that just happened in another process.
	item, err := parse.File(path)
	if err != nil {
		return fmt.Errorf("re-parse %s: %w", path, err)
	}

	if err := fn(item); err != nil {
		return err
	}

	if err := s.writeAtomic(item, path); err != nil {
		return err
	}

	s.items[id] = item
	return nil
}

// Create persists a brand-new item to disk for the first time. Unlike
// Mutate, which requires an existing file to lock and re-read, Create
// computes the destination path from item.Type + item.Status, writes
// the file atomically (tmp + rename), and registers it in the cache.
//
// Returns an error if the item already exists in the cache (use
// Mutate to update existing items).
func (s *Store) Create(item *model.Item) error {
	if item.Doc == nil {
		return fmt.Errorf("item %s has no ParsedDocument", item.ID)
	}
	if _, exists := s.items[item.ID]; exists {
		return fmt.Errorf("item %s already exists — use Mutate to update", item.ID)
	}

	dir := s.cfg.DirectoryForStatus(item.Type, item.Status)
	if dir == "" {
		return fmt.Errorf("no directory for type=%s status=%s", item.Type, item.Status)
	}
	path := filepath.Join(s.cfg.ItemDir(), dir, s.filenameForItem(item))

	if err := s.writeAtomic(item, path); err != nil {
		return err
	}
	s.items[item.ID] = item
	s.paths[item.ID] = path
	return nil
}

// MutateMany atomically applies fn to multiple items. Locks are
// acquired in lexicographic order of the requested IDs to prevent
// deadlock with concurrent MutateMany callers. fn receives a map of
// id → item; on a non-nil error from fn, no items are written.
//
// Same purity rule as Mutate: fn must be a pure transformation. All
// external data should be captured in the closure before MutateMany
// is called.
func (s *Store) MutateMany(ids []string, fn func(map[string]*model.Item) error) error {
	if len(ids) == 0 {
		return nil
	}
	sorted := make([]string, len(ids))
	copy(sorted, ids)
	sort.Strings(sorted)

	releases := make([]func(), 0, len(sorted))
	defer func() {
		// Release in reverse acquisition order — symmetric stack
		// unwind, even though flock release order doesn't matter.
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()

	items := make(map[string]*model.Item, len(sorted))
	for _, id := range sorted {
		path, ok := s.paths[id]
		if !ok {
			return ErrItemMissing
		}
		release, err := acquireExclusive(s.cfg.ItemDir(), id, path)
		if err != nil {
			return err
		}
		releases = append(releases, release)

		item, err := parse.File(path)
		if err != nil {
			return fmt.Errorf("re-parse %s: %w", path, err)
		}
		items[id] = item
	}

	if err := fn(items); err != nil {
		return err
	}

	for _, id := range sorted {
		if err := s.writeAtomic(items[id], s.paths[id]); err != nil {
			return err
		}
		s.items[id] = items[id]
	}
	return nil
}

// acquireExclusive obtains an exclusive flock on the lock file for an
// item. The lock file lives at <itemDir>/.locks/<id>.lock — a
// separate file from the data file so the data file can be atomically
// replaced via tmp + rename without ever invalidating an outstanding
// lock. Returns a release function that closes the lock file (which
// releases the flock as a side effect).
func acquireExclusive(itemDir, id, dataPath string) (func(), error) {
	if _, err := os.Stat(dataPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrItemMissing
		}
		return nil, fmt.Errorf("stat %s: %w", dataPath, err)
	}

	lockDir := filepath.Join(itemDir, ".locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", lockDir, err)
	}
	lockPath := filepath.Join(lockDir, id+".lock")

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", lockPath, err)
	}

	deadline := time.Now().Add(LockTimeout)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() { _ = f.Close() }, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(lockPollInterval)
	}
}

// writeAtomic serializes the item via Doc.String() and publishes it
// to disk with a tmp + rename so a crash never leaves a partial file.
// It also stamps last_touched and last_touched_by — the same auto-
// fields the legacy Write path applied.
func (s *Store) writeAtomic(item *model.Item, path string) error {
	if item.Doc == nil {
		return fmt.Errorf("item %s has no ParsedDocument", item.ID)
	}

	now := time.Now().Format(time.RFC3339)
	item.Doc.SetField("last_touched", now)
	if agentID := s.cfg.AgentID(); agentID != "" {
		item.Doc.SetField("last_touched_by", agentID)
	}

	content := item.Doc.String()
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".st-mutate-*")
	if err != nil {
		return fmt.Errorf("create tmp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close tmp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}
