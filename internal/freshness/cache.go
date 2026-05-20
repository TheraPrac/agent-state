package freshness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// cacheKey computes the stable key for a freshness result:
// sha256(plan body) + workspace HEAD short sha. Identical state
// returns an instant cache hit on a same-session re-start.
func cacheKey(planBody, head string) string {
	sum := sha256.Sum256([]byte(planBody))
	return hex.EncodeToString(sum[:])[:16] + "-" + head
}

func cachePath(workspaceRoot, id, planBody, head string) string {
	return filepath.Join(workspaceRoot, ".as", "cache", "freshness", id+"-"+cacheKey(planBody, head)+".json")
}

// loadCache reads a previously-evaluated Result for (id, plan,
// head). Returns (nil, false) on miss or parse error.
func loadCache(workspaceRoot, id, planBody, head string) (*Result, bool) {
	if workspaceRoot == "" || id == "" || planBody == "" || head == "" {
		return nil, false
	}
	data, err := os.ReadFile(cachePath(workspaceRoot, id, planBody, head))
	if err != nil {
		return nil, false
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, false
	}
	return &r, true
}

// storeCache writes a Result to disk so the next invocation can
// short-circuit. Stale verdicts are NOT cached — the operator may
// have re-prepped between calls and an instant Stale verdict
// would surprise them.
func storeCache(workspaceRoot, id, planBody, head string, r *Result) error {
	if r == nil || r.Verdict == VerdictStale {
		return nil
	}
	path := cachePath(workspaceRoot, id, planBody, head)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// PruneCache removes freshness cache entries older than maxAge.
// Returns the number of entries pruned. Used by `st cache prune`
// to keep the cache bounded.
func PruneCache(workspaceRoot string, maxAge time.Duration, now time.Time) (int, error) {
	dir := filepath.Join(workspaceRoot, ".as", "cache", "freshness")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	pruned := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				pruned++
			}
		}
	}
	return pruned, nil
}

// readableCachePath returns the on-disk path Check would use, so
// `st cache prune` and tests can target it explicitly.
func readableCachePath(workspaceRoot, id, planBody, head string) string {
	return cachePath(workspaceRoot, id, planBody, head)
}

// workspaceHead resolves `git rev-parse HEAD` for the workspace
// clone, used as the second half of the cache key. Returns a
// fallback string on failure so tests don't have to set up a real
// git repo — the cache still works, just less precisely.
func workspaceHead(workspaceRoot string, runner func(root string, args []string) ([]byte, error)) string {
	if runner == nil {
		runner = defaultGitRunner
	}
	out, err := runner(workspaceRoot, []string{"rev-parse", "HEAD"})
	if err != nil {
		return "unknown"
	}
	head := string(out)
	for i := 0; i < len(head); i++ {
		if head[i] == '\n' || head[i] == ' ' {
			head = head[:i]
			break
		}
	}
	if head == "" {
		return "unknown"
	}
	if len(head) >= 12 {
		head = head[:12]
	}
	return head
}

// hashPlanBody is exposed for tests that want to assert on the
// cache key without hard-coding sha256 hex strings.
func hashPlanBody(planBody string) string {
	sum := sha256.Sum256([]byte(planBody))
	return hex.EncodeToString(sum[:])[:16]
}

// ensure unused-import quiet — fmt referenced inside future
// formatter changes; the file currently has no direct fmt usage.
var _ = fmt.Sprintf
