package freshness

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCacheKey_StableAcrossSameInputs(t *testing.T) {
	a := cacheKey("plan body", "abcd1234")
	b := cacheKey("plan body", "abcd1234")
	if a != b {
		t.Errorf("expected stable cache key; got %q vs %q", a, b)
	}
}

func TestCacheKey_ChangesOnPlanBodyChange(t *testing.T) {
	a := cacheKey("plan body v1", "abcd")
	b := cacheKey("plan body v2", "abcd")
	if a == b {
		t.Errorf("cache key should differ when plan body differs")
	}
}

func TestCacheKey_ChangesOnHeadChange(t *testing.T) {
	a := cacheKey("plan", "head-1")
	b := cacheKey("plan", "head-2")
	if a == b {
		t.Errorf("cache key should differ when HEAD differs")
	}
}

func TestStoreCacheAndLoadCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := &Result{
		Verdict:     VerdictFresh,
		Findings:    nil,
		PlanHash:    "abc",
		Head:        "def",
		EvaluatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := storeCache(dir, "I-001", "body", "head", r); err != nil {
		t.Fatalf("storeCache: %v", err)
	}
	loaded, ok := loadCache(dir, "I-001", "body", "head")
	if !ok {
		t.Fatal("expected cache hit on round-trip")
	}
	if loaded.Verdict != VerdictFresh {
		t.Errorf("verdict round-trip mismatch: got %s", loaded.Verdict)
	}
}

func TestStoreCache_DoesNotPersistStale(t *testing.T) {
	dir := t.TempDir()
	r := &Result{Verdict: VerdictStale}
	if err := storeCache(dir, "I-001", "body", "head", r); err != nil {
		t.Fatalf("storeCache: %v", err)
	}
	if _, ok := loadCache(dir, "I-001", "body", "head"); ok {
		t.Errorf("Stale verdict should NOT be cached; got hit")
	}
}

func TestLoadCache_MissReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	if _, ok := loadCache(dir, "I-999", "body", "head"); ok {
		t.Errorf("expected miss for non-existent cache entry")
	}
}

func TestPruneCache_RemovesOldEntries(t *testing.T) {
	dir := t.TempDir()
	// Seed two entries; one fresh, one old (force ModTime).
	freshR := &Result{Verdict: VerdictFresh, PlanHash: "a", Head: "h1"}
	staleR := &Result{Verdict: VerdictFresh, PlanHash: "b", Head: "h2"}
	if err := storeCache(dir, "I-001", "body-a", "h1", freshR); err != nil {
		t.Fatal(err)
	}
	if err := storeCache(dir, "I-002", "body-b", "h2", staleR); err != nil {
		t.Fatal(err)
	}
	// Force the I-002 entry's mtime back 40 days.
	oldPath := readableCachePath(dir, "I-002", "body-b", "h2")
	pastTime := time.Now().Add(-40 * 24 * time.Hour)
	if err := chtime(oldPath, pastTime); err != nil {
		t.Fatalf("chtime: %v", err)
	}

	pruned, err := PruneCache(dir, 30*24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if pruned != 1 {
		t.Errorf("expected 1 pruned; got %d", pruned)
	}
	// Fresh entry survives.
	if _, ok := loadCache(dir, "I-001", "body-a", "h1"); !ok {
		t.Errorf("fresh entry was pruned")
	}
}

func TestPruneCache_NoDirReturnsZero(t *testing.T) {
	dir := t.TempDir() // empty
	pruned, err := PruneCache(dir, 30*24*time.Hour, time.Now())
	if err != nil {
		t.Errorf("expected nil err on missing cache dir; got %v", err)
	}
	if pruned != 0 {
		t.Errorf("expected 0 pruned on empty dir; got %d", pruned)
	}
}

func TestReadableCachePath_StructureIsStable(t *testing.T) {
	got := readableCachePath("/wsroot", "I-001", "body", "head")
	want := filepath.Join("/wsroot", ".as", "cache", "freshness", "I-001-"+cacheKey("body", "head")+".json")
	if got != want {
		t.Errorf("readableCachePath:\n got  %s\n want %s", got, want)
	}
}
