package config

import "testing"

func TestGoalTypeRegistered(t *testing.T) {
	cfg := Defaults()

	gt, ok := cfg.Types["goal"]
	if !ok {
		t.Fatal("goal type not registered in Defaults()")
	}

	if gt.IDPrefix != "G" {
		t.Errorf("goal IDPrefix = %q, want G", gt.IDPrefix)
	}

	wantStatuses := []string{"draft", "active", "met", "dropped", "archived"}
	if len(gt.Statuses) != len(wantStatuses) {
		t.Fatalf("goal Statuses = %v, want %v", gt.Statuses, wantStatuses)
	}
	for i, s := range wantStatuses {
		if gt.Statuses[i] != s {
			t.Errorf("goal status[%d] = %q, want %q", i, gt.Statuses[i], s)
		}
	}

	if gt.StartStatus != "draft" {
		t.Errorf("goal StartStatus = %q, want draft", gt.StartStatus)
	}
	if gt.ActiveStatus != "active" {
		t.Errorf("goal ActiveStatus = %q, want active", gt.ActiveStatus)
	}

	termSet := map[string]bool{}
	for _, s := range gt.TerminalStatuses {
		termSet[s] = true
	}
	for _, s := range []string{"met", "dropped", "archived"} {
		if !termSet[s] {
			t.Errorf("goal TerminalStatuses missing %q", s)
		}
	}

	for _, status := range []string{"draft", "active"} {
		if dir := gt.DirectoryMap[status]; dir != "goals" {
			t.Errorf("goal DirectoryMap[%q] = %q, want goals", status, dir)
		}
	}
	for _, status := range []string{"met", "dropped", "archived"} {
		if dir := gt.DirectoryMap[status]; dir != "archive" {
			t.Errorf("goal DirectoryMap[%q] = %q, want archive", status, dir)
		}
	}

	pat, ok := cfg.IDPatterns["goal"]
	if !ok {
		t.Fatal("goal missing from IDPatterns")
	}
	if pat != "G-{seq}" {
		t.Errorf("goal IDPattern = %q, want G-{seq}", pat)
	}
}
