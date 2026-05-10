package command

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/testutil"
)

// I-569 step 2: real_tokens is the canonical cumulative-tokens block written
// alongside the legacy reg_input_tokens / cache_*_tokens fields. Field names
// match Anthropic SDK exactly (input, output, cache_read,
// cache_creation_5m, cache_creation_1h) so reconcile-tokens (step 6) can
// compare against transcript JSONL `usage` blocks without translation.
func TestSessionLog_RealTokensCumulative(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	for i := 0; i < 3; i++ {
		SessionLog(env.S, env.Cfg, SessionLogPayload{
			SessionID:        "sess-1",
			Model:            "claude-opus-4-7",
			RegInputTokens:   100,
			RegOutputTokens:  50,
			CacheReadInputTokens:    1000,
			CacheCreation5mInputTokens:   200,
			CacheCreation1hInputTokens: 30,
			ProcessMs:        5000,
		})
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	got := readRealTokens(item)
	want := realTokens{
		Input: 300, Output: 150, CacheRead: 3000,
		CacheCreation5m: 600, CacheCreation1h: 90,
	}
	if got != want {
		t.Errorf("real_tokens = %+v, want %+v", got, want)
	}

	// And the line is present in the file with all five fields named verbatim.
	rendered := item.Doc.String()
	for _, want := range []string{
		"input=300", "output=150", "cache_read=3000",
		"cache_creation_5m=600", "cache_creation_1h=90",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("real_tokens line missing %q. File:\n%s", want, rendered)
		}
	}
}

// by_step splits cumulative tokens by the producer's step label
// (interactive vs subagent vs anything else). Per-step turn counts must sum
// to the overall turn_count.
func TestSessionLog_ByStepUpsert(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	// 4 interactive turns
	for i := 0; i < 4; i++ {
		SessionLog(env.S, env.Cfg, SessionLogPayload{
			SessionID: "sess-1", Model: "claude-opus-4-7",
			Step:           "interactive",
			RegInputTokens: 100, CacheReadInputTokens: 1000,
			ProcessMs: 3000,
		})
	}
	// 2 subagent turns — distinct SessionIDs and distinct token counts
	// (each subagent does different work). The I-448 dedup still drops
	// any second subagent with byte-identical tokens within 60s, so
	// using different counts is necessary for both turns to accrue.
	for _, sub := range []struct {
		sid string
		in  int
		ms  int64
	}{
		{"agent-aaa", 50, 1000},
		{"agent-bbb", 70, 1200},
	} {
		SessionLog(env.S, env.Cfg, SessionLogPayload{
			SessionID: sub.sid, Model: "claude-haiku-4-5",
			Step:           "subagent",
			Role:           "Explore",
			RegInputTokens: sub.in, CacheReadInputTokens: 500,
			ProcessMs: sub.ms,
		})
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	interactive := readByStep(item, "interactive")
	subagent := readByStep(item, "subagent")

	if interactive.Turns != 4 {
		t.Errorf("interactive turns = %d, want 4", interactive.Turns)
	}
	if interactive.Tokens.Input != 400 {
		t.Errorf("interactive input = %d, want 400", interactive.Tokens.Input)
	}
	if interactive.Tokens.CacheRead != 4000 {
		t.Errorf("interactive cache_read = %d, want 4000", interactive.Tokens.CacheRead)
	}
	if interactive.Ms != 12000 {
		t.Errorf("interactive ms = %d, want 12000", interactive.Ms)
	}

	if subagent.Turns != 2 {
		t.Errorf("subagent turns = %d, want 2", subagent.Turns)
	}
	if subagent.Tokens.Input != 120 {
		t.Errorf("subagent input = %d, want 120 (50+70)", subagent.Tokens.Input)
	}
	if subagent.Ms != 2200 {
		t.Errorf("subagent ms = %d, want 2200 (1000+1200)", subagent.Ms)
	}

	// Invariant: sum of by_step turns equals time_tracking.turn_count.
	totalTurns := readIntField(item, "time_tracking", "turn_count")
	if totalTurns != interactive.Turns+subagent.Turns {
		t.Errorf("turn_count %d != interactive(%d) + subagent(%d)",
			totalTurns, interactive.Turns, subagent.Turns)
	}
}

// by_step defaults the step label to "interactive" when the producer omits
// it — matches the existing per-turn behavior in formatAITurnLine.
func TestSessionLog_ByStepDefaultsInteractive(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	SessionLog(env.S, env.Cfg, SessionLogPayload{
		SessionID: "sess-1", Model: "claude-opus-4-7",
		// Step intentionally empty.
		RegInputTokens: 10,
	})

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	got := readByStep(item, "interactive")
	if got.Turns != 1 {
		t.Errorf("interactive turns = %d, want 1 (empty step should default)", got.Turns)
	}
}

// by_session captures one entry per (sid, project_dir) with sticky started_at
// and advancing ended_at. Reconcile-tokens (step 6) reads these to know which
// `~/.claude/projects/<slug>/<sid>.jsonl` files to walk for ground truth.
func TestSessionLog_BySessionUpsert(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	// Three turns on session A, then one on session B, then one more on A.
	for _, p := range []SessionLogPayload{
		{SessionID: "sess-A", ProjectDir: "/proj/a", Model: "claude-opus-4-7", RegInputTokens: 100},
		{SessionID: "sess-A", ProjectDir: "/proj/a", Model: "claude-opus-4-7", RegInputTokens: 200, CacheReadInputTokens: 5000},
		{SessionID: "sess-A", ProjectDir: "/proj/a", Model: "claude-opus-4-7", RegInputTokens: 300},
		{SessionID: "sess-B", ProjectDir: "/proj/b", Model: "claude-haiku-4-5", RegInputTokens: 10},
		{SessionID: "sess-A", ProjectDir: "/proj/a", Model: "claude-opus-4-7", RegInputTokens: 50},
	} {
		SessionLog(env.S, env.Cfg, p)
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	a := readBySession(item, "sess-A")
	b := readBySession(item, "sess-B")

	if a.Turns != 4 {
		t.Errorf("sess-A turns = %d, want 4", a.Turns)
	}
	if a.Tokens.Input != 650 {
		t.Errorf("sess-A input = %d, want 650", a.Tokens.Input)
	}
	if a.Tokens.CacheRead != 5000 {
		t.Errorf("sess-A cache_read = %d, want 5000", a.Tokens.CacheRead)
	}
	if a.ProjectDir != "/proj/a" {
		t.Errorf("sess-A project_dir = %q, want /proj/a", a.ProjectDir)
	}
	if a.StartedAt == "" || a.EndedAt == "" {
		t.Errorf("sess-A timestamps empty: started=%q ended=%q", a.StartedAt, a.EndedAt)
	}

	if b.Turns != 1 {
		t.Errorf("sess-B turns = %d, want 1", b.Turns)
	}
	if b.ProjectDir != "/proj/b" {
		t.Errorf("sess-B project_dir = %q, want /proj/b", b.ProjectDir)
	}

	// Invariant: sum of by_session turns equals overall turn_count.
	totalTurns := readIntField(item, "time_tracking", "turn_count")
	if totalTurns != a.Turns+b.Turns {
		t.Errorf("turn_count %d != A(%d) + B(%d)", totalTurns, a.Turns, b.Turns)
	}
}

// by_session must NOT create an entry for orphan turns (empty SessionID).
// Those land in orphan.log via the resolver path; including them in
// by_session under a synthetic "unknown" key would muddle reconcile's
// per-session JSONL lookup.
func TestSessionLog_BySessionSkipsEmptySID(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	SessionLog(env.S, env.Cfg, SessionLogPayload{
		// No SessionID — but ItemID forces routing to T-003 anyway.
		ItemID:         "T-003",
		Model:          "claude-opus-4-7",
		RegInputTokens: 10,
	})

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	rendered := item.Doc.String()
	if strings.Contains(rendered, "by_session:") {
		t.Errorf("by_session block should not exist for empty-sid turn. File:\n%s", rendered)
	}
}

// by_session.started_at is sticky — set on the first turn, never overwritten.
// ended_at advances on every turn so the entry's wall span is queryable.
func TestSessionLog_BySessionStartedAtIsSticky(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{SessionID: "sess-X", Model: "claude-opus-4-7", RegInputTokens: 1}
	SessionLog(env.S, env.Cfg, p)

	env.Reload(t)
	item, _ := env.S.Get("T-003")
	first := readBySession(item, "sess-X")

	if first.StartedAt == "" {
		t.Fatal("sess-X started_at empty after first turn")
	}
	firstStart := first.StartedAt

	// Time-of-call is captured fresh inside SessionLog. The next call
	// should leave started_at unchanged but advance ended_at — we can't
	// reliably assert ended_at moves forward in a unit test (subsecond
	// resolution) so we just check started_at didn't drift.
	SessionLog(env.S, env.Cfg, p)

	env.Reload(t)
	item, _ = env.S.Get("T-003")
	second := readBySession(item, "sess-X")

	if second.StartedAt != firstStart {
		t.Errorf("started_at changed from %q to %q across turns", firstStart, second.StartedAt)
	}
	if second.Turns != 2 {
		t.Errorf("sess-X turns after second call = %d, want 2", second.Turns)
	}
}

// formatRealTokensBlob round-trips through parseRealTokensBlob bit-exact —
// the I-569 invariant that every accumulator write can be re-read by the
// next turn without precision loss.
func TestRealTokensBlobRoundTrip(t *testing.T) {
	cases := []realTokens{
		{},
		{Input: 1, Output: 2, CacheRead: 3, CacheCreation5m: 4, CacheCreation1h: 5},
		{Input: 999_999_999, CacheRead: 1_234_567_890},
	}
	for _, want := range cases {
		got := parseRealTokensBlob(formatRealTokensBlob(want))
		if got != want {
			t.Errorf("round-trip failed: %+v -> %+v", want, got)
		}
	}
}
