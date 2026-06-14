package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
)

// TestMutateUpdatesItemAndDoc verifies the basic happy path: a Mutate
// closure that mutates the item is reflected on disk and in the cache.
func TestMutateUpdatesItemAndDoc(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	if err := s.Mutate("T-001", func(item *model.Item) error {
		item.Doc.SetField("status", "active")
		item.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	// Cache reflects new state
	got, _ := s.Get("T-001")
	if got.Status != "active" {
		t.Errorf("cached status = %q, want active", got.Status)
	}

	// Disk reflects new state
	data, err := os.ReadFile(filepath.Join(root, "tasks", "T-001-first-task.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "status: active") {
		t.Errorf("disk content missing status update:\n%s", data)
	}
}

// TestMutateClosureErrorAborts verifies that a non-nil error from fn
// leaves the on-disk file unchanged and propagates the error.
func TestMutateClosureErrorAborts(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	original, err := os.ReadFile(filepath.Join(root, "tasks", "T-001-first-task.md"))
	if err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("nope")
	err = s.Mutate("T-001", func(item *model.Item) error {
		item.Doc.SetField("status", "active")
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Mutate err = %v, want %v", err, sentinel)
	}

	after, _ := os.ReadFile(filepath.Join(root, "tasks", "T-001-first-task.md"))
	if string(after) != string(original) {
		t.Errorf("file changed despite fn error:\nbefore=%s\nafter=%s", original, after)
	}
}

// TestMutateMissingItem verifies ErrItemMissing on unknown IDs.
func TestMutateMissingItem(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	err := s.Mutate("T-999", func(item *model.Item) error { return nil })
	if !errors.Is(err, ErrItemMissing) {
		t.Errorf("err = %v, want ErrItemMissing", err)
	}
}

// TestMutateMissingFileAtLockTime verifies ErrItemMissing when the
// in-memory cache knows about an item but its file has been deleted
// between scan and Mutate.
func TestMutateMissingFileAtLockTime(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	// Delete the file directly behind the store's back.
	if err := os.Remove(filepath.Join(root, "tasks", "T-001-first-task.md")); err != nil {
		t.Fatal(err)
	}

	err := s.Mutate("T-001", func(item *model.Item) error { return nil })
	if !errors.Is(err, ErrItemMissing) {
		t.Errorf("err = %v, want ErrItemMissing", err)
	}
}

// TestMutateConcurrent is the I-185-class regression at the lock
// layer: many goroutines mutate the same item, and after all complete
// the on-disk state reflects every increment with no lost updates.
func TestMutateConcurrent(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			err := s.Mutate("T-001", func(item *model.Item) error {
				cur, _ := item.Doc.GetField("counter")
				next := 0
				fmt.Sscanf(cur, "%d", &next)
				item.Doc.SetField("counter", fmt.Sprintf("%d", next+1))
				return nil
			})
			if err != nil {
				t.Errorf("Mutate: %v", err)
			}
		}()
	}
	wg.Wait()

	got, _ := s.Get("T-001")
	final, _ := got.Doc.GetField("counter")
	if final != fmt.Sprintf("%d", N) {
		t.Errorf("counter = %q after %d concurrent mutates, want %d (lost updates)", final, N, N)
	}
}

// TestMutateLockTimeout verifies ErrLockTimeout when the lock cannot
// be acquired in time. We hold the lock from another goroutine while
// the test sets an aggressive timeout.
func TestMutateLockTimeout(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	hold := make(chan struct{})
	released := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Mutate("T-001", func(item *model.Item) error {
			close(hold)
			<-released
			return nil
		})
	}()
	<-hold

	old := LockTimeout
	LockTimeout = 100 * time.Millisecond
	defer func() { LockTimeout = old }()

	err := s.Mutate("T-001", func(item *model.Item) error { return nil })
	close(released)
	<-done // wait for goroutine to complete its Mutate before TempDir cleanup
	if !errors.Is(err, ErrLockTimeout) {
		t.Errorf("err = %v, want ErrLockTimeout", err)
	}
}

// TestMutateAtomicWrite verifies that the on-disk file is published via
// rename (no partial writes visible) — the test checks that a reader
// reading concurrently with a writer either sees the pre-state or the
// post-state, never an in-between truncated form.
func TestMutateAtomicWrite(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	dataPath := filepath.Join(root, "tasks", "T-001-first-task.md")

	const writers = 10
	const readers = 40
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers append a long marker that would be visible mid-write
	// if writes weren't atomic.
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := strings.Repeat("X", 5000)
			_ = s.Mutate("T-001", func(item *model.Item) error {
				item.Doc.SetField("payload", marker)
				return nil
			})
		}(i)
	}

	// Readers parse the file via raw read; any partial-write would
	// show up as a file too short or with truncated content. We
	// detect by checking the file always ends with "\n".
	var partials int64
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(dataPath)
				if err != nil {
					continue
				}
				if len(data) == 0 || data[len(data)-1] != '\n' {
					atomic.AddInt64(&partials, 1)
				}
			}
		}()
	}

	// Let writers finish, then stop readers.
	go func() {
		// All writers complete in their own time; once they're done,
		// give readers a moment, then stop.
		time.Sleep(200 * time.Millisecond)
		close(stop)
	}()

	wg.Wait()
	if atomic.LoadInt64(&partials) > 0 {
		t.Errorf("observed %d partial reads — atomic write violated", partials)
	}
}

// TestMutateManyLockOrder verifies that two MutateMany goroutines
// requesting overlapping items in different argument orders cannot
// deadlock — locks are always acquired in lex order.
func TestMutateManyLockOrder(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.MutateMany([]string{"T-001", "T-002"}, func(items map[string]*model.Item) error {
				items["T-001"].Doc.SetField("note", "a")
				items["T-002"].Doc.SetField("note", "b")
				return nil
			})
		}()
		go func() {
			defer wg.Done()
			// Reverse order at the call site — but Mutate sorts
			// internally so deadlock cannot happen.
			_ = s.MutateMany([]string{"T-002", "T-001"}, func(items map[string]*model.Item) error {
				items["T-002"].Doc.SetField("note2", "x")
				items["T-001"].Doc.SetField("note2", "y")
				return nil
			})
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock — MutateMany did not return")
	}
}

// TestMutateManyClosureErrorAborts verifies that a non-nil error from
// fn aborts ALL writes (atomicity across the multi-item set).
func TestMutateManyClosureErrorAborts(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	before1, _ := os.ReadFile(filepath.Join(root, "tasks", "T-001-first-task.md"))
	before2, _ := os.ReadFile(filepath.Join(root, "tasks", "T-002-second-task.md"))

	sentinel := errors.New("abort")
	err := s.MutateMany([]string{"T-001", "T-002"}, func(items map[string]*model.Item) error {
		items["T-001"].Doc.SetField("status", "active")
		items["T-002"].Doc.SetField("status", "queued")
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}

	after1, _ := os.ReadFile(filepath.Join(root, "tasks", "T-001-first-task.md"))
	after2, _ := os.ReadFile(filepath.Join(root, "tasks", "T-002-second-task.md"))
	if string(before1) != string(after1) || string(before2) != string(after2) {
		t.Error("MutateMany wrote despite fn error")
	}
}

// TestMutateLockFileLocation verifies the lock files land under
// <ItemDir>/.locks/, not in agent-state/tasks (which would pollute
// the data tree).
func TestMutateLockFileLocation(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	if err := s.Mutate("T-001", func(item *model.Item) error { return nil }); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, ".locks", "T-001.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lock file not at %s: %v", lockPath, err)
	}
	// Lock file must NOT exist in the tasks directory.
	stray := filepath.Join(root, "tasks", "T-001.lock")
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("stray lock file at %s — should be in .locks/", stray)
	}
}
