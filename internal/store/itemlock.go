// Package store — item-level locks prevent GitPull from overwriting
// active items during concurrent git operations.
//
// When an item is started (st start), a lock file is created in
// .locks/<item-id>. GitPull snapshots locked item files before pulling
// and restores them after, so concurrent pulls can't revert status.
// Locks are released on close, release, or finish.
package store

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
)

// LocksDir returns the path to the locks directory.
func LocksDir(cfg *config.Config) string {
	return filepath.Join(cfg.ItemDir(), ".locks")
}

// LockItem creates a lock file for the given item ID.
// The lock file contains the session ID and timestamp.
func LockItem(cfg *config.Config, itemID, sessionID string) error {
	dir := LocksDir(cfg)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	content := sessionID + "\n" + time.Now().Format(time.RFC3339) + "\n"
	return os.WriteFile(filepath.Join(dir, itemID), []byte(content), 0644)
}

// UnlockItem removes the lock file for the given item ID.
func UnlockItem(cfg *config.Config, itemID string) {
	os.Remove(filepath.Join(LocksDir(cfg), itemID))
}

// LockedItems returns all currently locked item IDs.
func LockedItems(cfg *config.Config) []string {
	dir := LocksDir(cfg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// IsLocked returns true if the given item ID has an active lock.
func IsLocked(cfg *config.Config, itemID string) bool {
	_, err := os.Stat(filepath.Join(LocksDir(cfg), itemID))
	return err == nil
}

// CleanStaleLocks removes locks for items that are no longer active
// (e.g., already closed/completed, or reverted to queued without a pipeline).
// Called at st run startup to prevent stale locks from accumulating.
func CleanStaleLocks(cfg *config.Config, isActive func(itemID string) bool) int {
	cleaned := 0
	for _, id := range LockedItems(cfg) {
		if !isActive(id) {
			UnlockItem(cfg, id)
			cleaned++
		}
	}
	return cleaned
}
