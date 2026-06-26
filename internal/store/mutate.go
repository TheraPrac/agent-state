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

	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/parse"
	"github.com/theraprac/agent-state/internal/validate"
)

// ErrLockTimeout is returned by Mutate / MutateMany when an exclusive
// flock could not be acquired within LockTimeout.
var ErrLockTimeout = errors.New("flock acquire timed out")

// ErrItemMissing is returned when the on-disk file for an item does
// not exist at the moment Mutate / MutateMany tries to lock it.
var ErrItemMissing = errors.New("item file missing")

// ErrAlreadyClaimed is the sentinel callers return from a Mutate
// closure when an item's claimed_by field already names a different,
// live session — the caller decided it would be unsafe to overwrite.
// T-310: lets `st start` and friends do an atomic compare-and-claim
// inside Mutate without inventing a per-call error type.
var ErrAlreadyClaimed = errors.New("already claimed")

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
	// I-501: refuse to mutate when the canonical clone is mid-rebase /
	// mid-merge / holding a stale index.lock. Any write in those states
	// would compound corruption; surface the recovery hint instead.
	if err := PreFlightGitState(s.cfg.ItemDir()); err != nil {
		return err
	}

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

	// I-508: snapshot the parsed pre-state for the delta check. We
	// re-parse so the snapshot is independent of the in-memory item
	// the closure mutates — fn may share pointers with `item`.
	before, beforeErr := parse.File(path)
	if beforeErr != nil {
		// Pre-state read failure is non-fatal — fall back to vocab-only
		// gate. The error path is already covered by the re-parse above.
		before = nil
	}

	if err := fn(item); err != nil {
		return err
	}

	// I-508: write-time gate. Vocab violations always reject. Required-
	// field regressions reject only when the field was present before
	// AND missing after, so legacy items that already lack a required
	// field can still receive unrelated updates.
	if err := validate.WriteOKDelta(before, item, s.cfg); err != nil {
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
	// I-501: same pre-flight as Mutate. A mid-rebase / mid-merge state
	// must block new-item creation too — otherwise the new file lands
	// on disk and the subsequent GitSync inherits the corrupt state.
	if err := PreFlightGitState(s.cfg.ItemDir()); err != nil {
		return err
	}

	if item.Doc == nil {
		return fmt.Errorf("item %s has no ParsedDocument", item.ID)
	}
	if _, exists := s.items[item.ID]; exists {
		return fmt.Errorf("item %s already exists — use Mutate to update", item.ID)
	}

	// I-508: same write-time vocab gate as Mutate. Reject before we
	// route the file into a directory — an invalid status would
	// otherwise produce a "no directory for type=X status=Y" message
	// that hides the underlying bug.
	if err := validate.WriteOK(item, s.cfg); err != nil {
		return err
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
	// I-501: same pre-flight gate as the single-item path. Catching it
	// here means a multi-item commit-tree style write also refuses to
	// run when the workspace is corrupt.
	if err := PreFlightGitState(s.cfg.ItemDir()); err != nil {
		return err
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
	befores := make(map[string]*model.Item, len(sorted))
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

		// I-508: independent pre-state snapshot for the delta check.
		// Best-effort; a parse miss falls back to vocab-only gating.
		if before, err := parse.File(path); err == nil {
			befores[id] = before
		}
	}

	if err := fn(items); err != nil {
		return err
	}

	// I-508: validate every item post-fn against its pre-state. If any
	// item would land invalid, abort the whole batch — two-phase
	// commit semantics require all-or-nothing.
	for _, id := range sorted {
		if err := validate.WriteOKDelta(befores[id], items[id], s.cfg); err != nil {
			return err
		}
	}

	// Two-phase commit: write every item to a tmp file first. If any
	// serialization or write fails, clean up tmps and abort BEFORE any
	// rename — so disk state is still all-or-nothing pre-mutation.
	type pending struct {
		dataPath string
		tmpPath  string
	}
	prepared := make([]pending, 0, len(sorted))
	cleanupTmps := func() {
		for _, p := range prepared {
			_ = os.Remove(p.tmpPath)
		}
	}
	for _, id := range sorted {
		dataPath := s.paths[id]
		tmpPath, err := s.stageTmp(items[id], dataPath)
		if err != nil {
			cleanupTmps()
			return err
		}
		prepared = append(prepared, pending{dataPath: dataPath, tmpPath: tmpPath})
	}

	// Commit phase: rename each staged tmp into place. A crash partway
	// through this loop leaves an inconsistent set of files; the window
	// is microseconds in practice and far smaller than the original
	// "fail-after-first-Write" risk this method replaces. True atomic
	// multi-file commit would need a write-ahead log — out of scope.
	for i, p := range prepared {
		if err := os.Rename(p.tmpPath, p.dataPath); err != nil {
			// Best-effort: clean up any remaining staged tmps.
			for j := i + 1; j < len(prepared); j++ {
				_ = os.Remove(prepared[j].tmpPath)
			}
			return fmt.Errorf("rename %s -> %s: %w", p.tmpPath, p.dataPath, err)
		}
		s.items[sorted[i]] = items[sorted[i]]
	}
	return nil
}

// stageTmp serializes the item via Doc.String(), stamps last_touched /
// last_touched_by, writes to a tmp file in the data file's directory,
// fsyncs it, and returns the tmp path. The caller is responsible for
// renaming the tmp into the final dataPath (or removing it on abort).
//
// Used by MutateMany's two-phase commit; writeAtomic is the single-
// item analog that does both stage and rename.
func (s *Store) stageTmp(item *model.Item, dataPath string) (string, error) {
	if item.Doc == nil {
		return "", fmt.Errorf("item %s has no ParsedDocument", item.ID)
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

	dir := filepath.Dir(dataPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".st-mutate-*")
	if err != nil {
		return "", fmt.Errorf("create tmp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("fsync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close tmp: %w", err)
	}
	return tmpPath, nil
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
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		// Only contention (EWOULDBLOCK / EAGAIN) and EINTR are
		// retryable. Anything else (EBADF, ENOLCK, ...) is a real
		// failure that the loop would otherwise mask as ErrLockTimeout.
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("flock %s: %w", lockPath, err)
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
	tmpPath, err := s.stageTmp(item, path)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}
