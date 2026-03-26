package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNewEnv(t *testing.T) {
	env := NewEnv(t)

	// Verify directory structure
	for _, dir := range []string{"tasks", "issues", "archive", ".as", ".changelog"} {
		path := filepath.Join(env.Root, dir)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing directory %s: %v", dir, err)
		}
	}

	// Verify store loaded items
	if _, ok := env.S.Get("T-001"); !ok {
		t.Error("T-001 not found")
	}
	if _, ok := env.S.Get("I-001"); !ok {
		t.Error("I-001 not found")
	}

	all := env.S.All()
	if len(all) != 5 {
		t.Errorf("store has %d items, want 5", len(all))
	}
}

func TestNewGitEnv(t *testing.T) {
	env := NewGitEnv(t)

	// Verify it's a git repo with at least one commit
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = env.Root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("not a git repo: %v", err)
	}
	if len(out) < 7 {
		t.Error("expected a valid commit hash")
	}

	// Verify clean working tree
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = env.Root
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(out) > 0 {
		t.Errorf("working tree not clean: %s", out)
	}
}

func TestReload(t *testing.T) {
	env := NewEnv(t)

	// Write a new item directly to disk
	WriteItem(t, filepath.Join(env.Root, "tasks", "T-099-new.md"), `id: T-099
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: New item
`)

	// Before reload, store doesn't know about it
	if _, ok := env.S.Get("T-099"); ok {
		t.Error("T-099 should not exist before reload")
	}

	env.Reload(t)

	if _, ok := env.S.Get("T-099"); !ok {
		t.Error("T-099 should exist after reload")
	}
}
