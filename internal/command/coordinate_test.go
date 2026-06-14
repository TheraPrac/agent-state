package command

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/coordinator"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// boundaryFixture is a minimal-but-real coordinator.yaml written into the
// test env's .as/ so Coordinate's LoadBoundary succeeds.
const boundaryFixture = `escalation:
  respawn_limit: 3
  budget_cap_usd:
    per_item: 40
    per_objective: 150
  stuck_multiplier: 3
  parallelism_cap: 4
  tripwire_list:
    - prod_infra_apply
dedupe:
  window_minutes: 30
escalation_channel:
  default: alerts_band
  active_ping:
    - category_E
    - budget_cap
`

func writeBoundary(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".as", "coordinator.yaml"),
		[]byte(boundaryFixture), 0644); err != nil {
		t.Fatalf("write boundary fixture: %v", err)
	}
}

func TestSelectNext(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// T-461: selectNext derives from item properties, not queue.yaml.
	// The I-491 dispatch-safety guard skips items without an approved plan.
	// Set PlanApproved=true on T-001 only; I-001 (p1) is left unplanned so
	// selectNext must skip it and return T-001 — the only planned candidate.
	if err := s.Mutate("T-001", func(m *model.Item) error {
		m.PlanApproved = true
		return nil
	}); err != nil {
		t.Fatalf("set PlanApproved on T-001: %v", err)
	}
	QueueAdd(s, cfg, "T-001", QueueOpts{}) // pin (score boost, not eligibility gate)

	it, why := selectNext(s, cfg)
	if it == nil || it.ID != "T-001" {
		t.Fatalf("selectNext = %v (%s), want T-001 (only planned candidate)", it, why)
	}
	// Contract §4.2: the hit carries an inspectable scoring rationale.
	if !strings.Contains(why, "priority p") {
		t.Errorf("hit must carry a decomposed rationale, got %q", why)
	}

	// Claim it → no longer eligible (single in-flight invariant).
	if err := s.Mutate("T-001", func(m *model.Item) error {
		m.ClaimedBy = "some-session"
		return nil
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if it, why := selectNext(s, cfg); it != nil {
		t.Errorf("claimed item must be skipped, got %s", it.ID)
	} else if why == "" {
		t.Error("a skip must come with a reason (no opaque empty)")
	}
}

// approvePlanWithFiles seeds an approved plan sidecar whose FilesToModify
// declare the given paths, and flips the item's PlanApproved flag — so the
// item is dispatch-eligible and carries a known C1 conflict signature.
func approvePlanWithFiles(t *testing.T, s *store.Store, cfg *config.Config, id string, files ...string) {
	t.Helper()
	if err := plan.Save(cfg.PlansDir(), id, &plan.Plan{
		Approach:      "test plan",
		ScopeRepos:    []string{"as"},
		ACs:           []string{"cmd: go test ./..."},
		FilesToModify: files,
	}); err != nil {
		t.Fatalf("save plan %s: %v", id, err)
	}
	// Persist plan_approved to the underlying doc (not just the struct field):
	// the store serializes item.Doc, and selectNextExcluding reads a FRESH
	// store from disk, so an in-memory-only struct mutation would not be seen.
	if err := s.Mutate(id, func(m *model.Item) error {
		m.PlanApproved = true
		if m.Doc != nil {
			m.Doc.SetField("plan_approved", "true")
		}
		return nil
	}); err != nil {
		t.Fatalf("approve plan %s: %v", id, err)
	}
}

func TestSelectNextExcluding(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Two ready, unblocked, unassigned candidates. T-001 touches the OpenAPI
	// surface (C1 class "openapi"); I-001 is neutral. Assertions are
	// ranking-agnostic — only the exclusion behaviour is under test.
	approvePlanWithFiles(t, s, cfg, "T-001", "api/openapi/api.yaml")
	approvePlanWithFiles(t, s, cfg, "I-001", "internal/foo/bar.go")

	// No exclusions → some planned candidate is dispatchable.
	top, why := selectNextExcluding(cfg, nil, nil)
	if top == nil {
		t.Fatalf("no-exclusion pick = nil (%s), want a dispatchable item", why)
	}

	// Occupy the top pick → the OTHER planned candidate is returned.
	other, why := selectNextExcluding(cfg, map[string]bool{top.ID: true}, nil)
	if other == nil || other.ID == top.ID {
		t.Fatalf("occupied pick = %v (%s), want the other candidate (not %s)", other, why, top.ID)
	}

	// An in-flight worker already holds the OpenAPI surface → T-001 (also
	// OpenAPI) must be deferred; I-001 (neutral) is the only dispatchable pick.
	it, why := selectNextExcluding(cfg, nil, []string{"openapi"})
	if it == nil || it.ID != "I-001" {
		t.Fatalf("C1-conflict pick = %v (%s), want I-001 (T-001 deferred on openapi)", it, why)
	}

	// Both occupied → nil + a non-empty reason (no opaque blank).
	it, why = selectNextExcluding(cfg, map[string]bool{"T-001": true, "I-001": true}, nil)
	if it != nil {
		t.Fatalf("all-occupied pick = %s, want nil", it.ID)
	}
	if why == "" {
		t.Error("a no-dispatch result must carry a reason (no opaque blank)")
	}
}

// TestCoordinateMultiWorker_D1_ObjectiveBudget drives the concurrent fan-out
// loop with a stubbed worker that burns a fixed cost per item, and asserts the
// loop stops via a single D1 escalation once cumulative per-objective spend
// crosses the cap. The stubbed superviseFn lets the real dispatch /
// accounting / escalation path run under -race without forking workers.
func TestCoordinateMultiWorker_D1_ObjectiveBudget(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root()) // per_objective cap = $150, parallelism_cap = 4
	t.Setenv("AS_AGENT_ID", "")
	coordinator.ResetEmpiricalForTest()
	t.Cleanup(coordinator.ResetEmpiricalForTest)

	// Two neutral, non-conflicting, ready items → both dispatch concurrently
	// (parallelism_cap=4 in the boundary fixture).
	approvePlanWithFiles(t, s, cfg, "T-001", "internal/a/a.go")
	approvePlanWithFiles(t, s, cfg, "I-001", "internal/b/b.go")

	// Scripted per-item spend (cost seam): $80 each. Two workers → $160
	// cumulative ≥ the $150 per_objective cap → D1.
	var costMu sync.Mutex
	costs := map[string]float64{}
	origCost := costUSDOf
	t.Cleanup(func() { costUSDOf = origCost })
	costUSDOf = func(_ *config.Config, id string) float64 {
		costMu.Lock()
		defer costMu.Unlock()
		return costs[id]
	}

	// A 2-worker barrier makes the test deterministic regardless of goroutine
	// scheduling: neither stub returns until BOTH have recorded their spend, so
	// when the loop processes the first completion both costs are visible and
	// the D1 threshold is unambiguously crossed.
	var ready sync.WaitGroup
	ready.Add(2)
	origSupervise := superviseFn
	t.Cleanup(func() { superviseFn = origSupervise })
	superviseFn = func(cfg *config.Config, b *coordinator.Boundary,
		dd *coordinator.Deduper, ex coordinator.Escalator, st *coordinator.WorkerState,
		itemID string, opts CoordinateOpts, base, idleCap time.Duration,
		mu *sync.Mutex, cancel <-chan struct{}) int {
		costMu.Lock()
		costs[itemID] = 80
		costMu.Unlock()
		// Mark the item terminal so it is not re-picked if the loop fills again.
		if fs, err := store.New(cfg); err == nil {
			_ = fs.Mutate(itemID, func(m *model.Item) error {
				m.Status = "done"
				if m.Doc != nil {
					m.Doc.SetField("status", "done")
				}
				return nil
			})
		}
		ready.Done()
		ready.Wait() // barrier: both workers have recorded spend before either returns
		return 0
	}

	// Inject a fake escalator so Fire records the D1 escalation in-memory
	// instead of execing `st create`.
	fe := &fakeEscalator{}
	origEsc := escalatorFor
	t.Cleanup(func() { escalatorFor = origEsc })
	escalatorFor = func(cfg *config.Config, b *coordinator.Boundary) coordinator.Escalator {
		return fe
	}

	var rc int
	out := captureStdout(t, func() {
		rc = Coordinate(s, cfg, CoordinateOpts{MaxItems: 0, PollInterval: time.Millisecond})
	})
	if rc != 0 {
		t.Fatalf("D1 stop must return 0 (escalation is a clean stop), got %d\n%s", rc, out)
	}
	if fe.fileBlockerCalls != 1 {
		t.Errorf("want exactly one D1 FileBlocker call, got %d\n%s", fe.fileBlockerCalls, out)
	}
	if fe.lastPredicate != coordinator.PredicateD1 {
		t.Errorf("escalation predicate = %q, want D1\n%s", fe.lastPredicate, out)
	}
	if !strings.Contains(out, "per-objective spend") || !strings.Contains(out, "D1 escalate") {
		t.Errorf("output must announce the D1 stop, got:\n%s", out)
	}
}

// TestCoordinateMultiWorker_OperationalFailureStops asserts the operational-
// failure stop path: when ONE worker returns a non-zero (spawn could not launch)
// code, the dispatch loop stops the whole fan-out, drains the remaining in-flight
// workers (no goroutine left writing to the results channel), and propagates that
// exact code — rather than dispatching more work into a broken substrate. The
// outcome is deterministic regardless of which worker the scheduler reaps first.
func TestCoordinateMultiWorker_OperationalFailureStops(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	t.Setenv("AS_AGENT_ID", "")
	coordinator.ResetEmpiricalForTest()
	t.Cleanup(coordinator.ResetEmpiricalForTest)

	// Two neutral, non-conflicting, ready items → both dispatch concurrently
	// (parallelism_cap=4 in the boundary fixture).
	approvePlanWithFiles(t, s, cfg, "T-001", "internal/a/a.go")
	approvePlanWithFiles(t, s, cfg, "I-001", "internal/b/b.go")

	// Stub supervise: T-001 reports an operational failure (rc=2); I-001 returns
	// clean (0). Every worker marks its item terminal so the fill loop cannot
	// re-dispatch it. Either reap order converges on rc=2: if T-001 lands first
	// the loop stops + drains I-001; if I-001 lands first the loop re-fills (no
	// dispatchable items remain), blocks, then reaps T-001's failure.
	origSupervise := superviseFn
	t.Cleanup(func() { superviseFn = origSupervise })
	superviseFn = func(cfg *config.Config, b *coordinator.Boundary,
		dd *coordinator.Deduper, ex coordinator.Escalator, st *coordinator.WorkerState,
		itemID string, opts CoordinateOpts, base, idleCap time.Duration,
		mu *sync.Mutex, cancel <-chan struct{}) int {
		if fs, err := store.New(cfg); err == nil {
			_ = fs.Mutate(itemID, func(m *model.Item) error {
				m.Status = "done"
				if m.Doc != nil {
					m.Doc.SetField("status", "done")
				}
				return nil
			})
		}
		if itemID == "T-001" {
			return 2 // operational failure (spawn could not launch)
		}
		return 0
	}

	// rc==2 is the contract: the operational failure is propagated and the loop
	// stopped rather than dispatching into a broken substrate. (The "stopping
	// fan-out" advisory is written to stderr, which captureStdout doesn't see;
	// the propagated exit code is the load-bearing assertion.)
	var rc int
	_ = captureStdout(t, func() {
		rc = Coordinate(s, cfg, CoordinateOpts{MaxItems: 0, PollInterval: time.Millisecond})
	})
	if rc != 2 {
		t.Fatalf("operational failure must propagate rc=2 (stop fan-out), got %d", rc)
	}
}

// fakeEscalator records escalation side-effects in memory (concurrency-safe)
// so tests can assert the D1 path fired without subprocesses.
type fakeEscalator struct {
	mu               sync.Mutex
	fileBlockerCalls int
	lastPredicate    coordinator.Predicate
}

func (f *fakeEscalator) FileBlocker(e coordinator.Escalation) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileBlockerCalls++
	f.lastPredicate = e.Predicate
	return "I-999", nil
}
func (f *fakeEscalator) Log(e coordinator.Escalation, issueID string) error  { return nil }
func (f *fakeEscalator) Mail(e coordinator.Escalation, issueID string) error { return nil }
func (f *fakeEscalator) Notify(e coordinator.Escalation) error               { return nil }

func TestCoordinateBoundaryMissingHardFails(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// No coordinator.yaml written → must refuse to run (never unbounded).
	if rc := Coordinate(s, cfg, CoordinateOpts{DryRun: true}); rc == 0 {
		t.Fatal("missing boundary must hard-fail (contract §11), got rc=0")
	}
}

func TestCoordinateDryRun(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	t.Setenv("AS_AGENT_ID", "")
	// I-491: selectNext requires PlanApproved=true on the dispatched item.
	if err := s.Mutate("T-001", func(m *model.Item) error {
		m.PlanApproved = true
		return nil
	}); err != nil {
		t.Fatalf("set PlanApproved: %v", err)
	}
	QueueAdd(s, cfg, "T-001", QueueOpts{})

	var rc int
	out := captureStdout(t, func() {
		rc = Coordinate(s, cfg, CoordinateOpts{DryRun: true})
	})
	if rc != 0 {
		t.Fatalf("dry-run rc = %d, want 0\n%s", rc, out)
	}
	for _, want := range []string{
		"DRY RUN", "respawn_limit=3", "per_item=$40", "parallelism=4",
		"picked:      T-001", "why:", "size-class:", "st spawn T-001",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q\n--- output ---\n%s", want, out)
		}
	}
	// Side-effect-free: no worker registration may have been written.
	if entries, _ := os.ReadDir(filepath.Join(cfg.Root(), ".as", "agents")); len(entries) > 0 {
		t.Errorf("dry-run must register nothing, found %d agent file(s)", len(entries))
	}
}

// TestReapDeadChildrenNoChildrenNoop pins the safety invariant: with no
// child processes, reapDeadChildren must return promptly (ECHILD), never
// hang or panic. The actual zombie-reap behaviour is an OS interaction
// proven by T-363 live-verify (a unit test cannot faithfully fork+Release
// a child the way command.Spawn does without being flaky).
func TestReapDeadChildrenNoChildrenNoop(t *testing.T) {
	done := make(chan struct{})
	go func() { reapDeadChildren(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reapDeadChildren hung with no children (must ECHILD-return immediately)")
	}
}

func TestCoordinateDryRunNoEligibleItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	// Nothing queued → reports the reason and exits 0 (no opaque blank).
	var rc int
	out := captureStdout(t, func() {
		rc = Coordinate(s, cfg, CoordinateOpts{DryRun: true})
	})
	if rc != 0 {
		t.Fatalf("no-eligible rc = %d, want 0", rc)
	}
	if !strings.Contains(out, "no eligible item") {
		t.Errorf("must surface WHY nothing ran, got %q", out)
	}
}

// TestCoordinateDryRun_ReflectsEmpiricalBaselines verifies that the DryRun
// size-class line names the cost source ("heuristic" when no done items are
// loaded, as is the case with the standard test environment).
func TestCoordinateDryRun_ReflectsEmpiricalBaselines(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	coordinator.ResetEmpiricalForTest()
	t.Cleanup(coordinator.ResetEmpiricalForTest)
	t.Setenv("AS_AGENT_ID", "")
	// I-491: selectNext requires PlanApproved=true.
	if err := s.Mutate("T-001", func(m *model.Item) error {
		m.PlanApproved = true
		return nil
	}); err != nil {
		t.Fatalf("set PlanApproved: %v", err)
	}
	QueueAdd(s, cfg, "T-001", QueueOpts{})

	var rc int
	out := captureStdout(t, func() {
		rc = Coordinate(s, cfg, CoordinateOpts{DryRun: true})
	})
	if rc != 0 {
		t.Fatalf("dry-run rc=%d, want 0\n%s", rc, out)
	}
	// With no done items the cost source must be "heuristic".
	if !strings.Contains(out, "heuristic") {
		t.Errorf("no done items → want 'heuristic' in size-class line, got:\n%s", out)
	}
	if strings.Contains(out, "empirical") {
		t.Errorf("no done items → must not say 'empirical', got:\n%s", out)
	}
}

// TestSpawnDoesNotMutateAICostUSD guards the T-380 degrade-path invariant
// documented in superviseItem: when freshItem returns nil and we fall back
// to baseItem, we rely on baseItem.ai_cost_usd == postItem.ai_cost_usd —
// which only holds because Spawn forks a worker and never invokes a
// subagent itself. The only writer of ai_cost_usd is session_log.go's
// SubagentStop handler. If Spawn ever grows a path that calls into the
// subagent-stop rollup (e.g. record-run-metrics on a synchronous probe),
// the degrade path would silently revert D2 to item-lifetime semantics —
// exactly the bug T-380 fixes. This source-scan test fails build the
// moment that contract is violated.
func TestSpawnDoesNotMutateAICostUSD(t *testing.T) {
	src, err := os.ReadFile("spawn.go")
	if err != nil {
		t.Fatalf("read spawn.go: %v", err)
	}
	body := string(src)
	for _, forbidden := range []string{
		`"ai_cost_usd"`,            // direct write to the field
		"RollupItemID",             // routes a SubagentStop to an item
		"recordSubagentStopMetric", // hypothetical hook entry point
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("command/spawn.go contains %q — the T-380 degrade-path "+
				"invariant (baseItem.ai_cost_usd == postItem.ai_cost_usd "+
				"after Spawn) assumes Spawn does NOT mutate cost. See the "+
				"comment in superviseItem's postItem-nil branch.", forbidden)
		}
	}
}
