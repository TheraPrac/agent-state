package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
)

// TestAllocateAndCreate_ConcurrentUniqueIDs spawns N goroutines, each
// with its own Store (separate file descriptors → flock serializes even
// in-process), and asserts that all N allocated IDs are distinct and
// contiguous. This is the core I-1552 regression: the old NextID+Create
// sequence read the same max under concurrent load and produced
// duplicate IDs.
func TestAllocateAndCreate_ConcurrentUniqueIDs(t *testing.T) {
	root, _ := setupTestDir(t)

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)

	ids := make([]string, N)
	errs := make([]error, N)

	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			s := newTestStore(t, root)
			item, err := s.AllocateAndCreate("task", func(id string) (*model.Item, error) {
				return minimalItem(id, "task"), nil
			})
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = item.ID
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	seen := map[string]int{}
	for i, id := range ids {
		if id == "" {
			continue
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("duplicate ID %s: allocated by goroutine %d and %d", id, prev, i)
		}
		seen[id] = i
	}
}

// TestAllocateAndCreate_RescansDiskTruth writes an item file directly to
// disk (bypassing the in-memory cache) then calls AllocateAndCreate on a
// store that doesn't know about it. The rescan under the alloc lock must
// discover the on-disk file so NextID skips its ID rather than colliding.
func TestAllocateAndCreate_RescansDiskTruth(t *testing.T) {
	root, _ := setupTestDir(t)

	// Create the store first so T-099 is absent from the initial scan.
	s := newTestStore(t, root)

	// Now write T-099 directly to disk — simulates a concurrent peer
	// that created an item after this process started its scan.
	ghostPath := filepath.Join(root, "tasks", "T-099-ghost.md")
	ghostContent := `id: T-099
type: task
status: queued
created: 2026-01-01T00:00:00-06:00
last_touched: 2026-01-01T00:00:00-06:00

title: Ghost task

depends_on:
- []
`
	if err := os.WriteFile(ghostPath, []byte(ghostContent), 0644); err != nil {
		t.Fatalf("write ghost: %v", err)
	}

	if _, ok := s.Get("T-099"); ok {
		t.Fatal("T-099 should not be in the store yet — test setup error")
	}

	item, err := s.AllocateAndCreate("task", func(id string) (*model.Item, error) {
		return minimalItem(id, "task"), nil
	})
	if err != nil {
		t.Fatalf("AllocateAndCreate: %v", err)
	}

	if item.ID == "T-099" {
		t.Errorf("AllocateAndCreate returned T-099 — rescan did not detect on-disk ghost item")
	}
	suffix := strings.TrimPrefix(item.ID, "T-")
	var n int
	fmt.Sscanf(suffix, "%d", &n)
	if n <= 99 {
		t.Errorf("allocated ID %s (suffix %d) is not greater than ghost T-099", item.ID, n)
	}
}

// TestAllocateAndCreate_BuildErrorWritesNothing verifies that when the
// build closure returns an error, no file is written and the lock is
// cleanly released (a subsequent AllocateAndCreate must succeed).
func TestAllocateAndCreate_BuildErrorWritesNothing(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	buildErr := errors.New("simulated build failure")
	_, err := s.AllocateAndCreate("task", func(id string) (*model.Item, error) {
		return nil, buildErr
	})
	if !errors.Is(err, buildErr) {
		t.Fatalf("AllocateAndCreate err = %v, want %v", err, buildErr)
	}

	// No file should have been created for the attempted ID.
	entries, _ := os.ReadDir(filepath.Join(root, "tasks"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".st-mutate") {
			t.Errorf("tmp file left on disk: %s", e.Name())
		}
	}

	// Lock must be released: a second call with a valid build must succeed.
	item, err := s.AllocateAndCreate("task", func(id string) (*model.Item, error) {
		return minimalItem(id, "task"), nil
	})
	if err != nil {
		t.Fatalf("second AllocateAndCreate after build error: %v", err)
	}
	if item == nil || item.ID == "" {
		t.Error("second AllocateAndCreate returned nil/empty item")
	}
}

// minimalItem builds the smallest valid task/issue item for alloc tests.
func minimalItem(id, itemType string) *model.Item {
	doc := &model.ParsedDocument{}
	doc.Lines = []model.Line{
		{Raw: "id: " + id, Key: "id", Value: id},
		{Raw: "type: " + itemType, Key: "type", Value: itemType},
		{Raw: "status: queued", Key: "status", Value: "queued"},
		{Raw: "created: 2026-01-01T00:00:00-06:00", Key: "created"},
		{Raw: "last_touched: 2026-01-01T00:00:00-06:00", Key: "last_touched"},
		{Raw: ""},
		{Raw: "completed: null", Key: "completed", Value: "null"},
		{Raw: ""},
		{Raw: "title: alloc test item", Key: "title", Value: "alloc test item"},
		{Raw: ""},
		{Raw: "depends_on:", Key: "depends_on"},
		{Raw: "- []", IsList: true},
		{Raw: ""},
		{Raw: "blocks:", Key: "blocks"},
		{Raw: "- []", IsList: true},
	}
	return &model.Item{
		ID:     id,
		Type:   itemType,
		Status: "queued",
		Title:  "alloc test item",
		Doc:    doc,
	}
}
