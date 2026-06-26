package coordinator

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/theraprac/agent-state/internal/model"
)

var (
	empiricalBaselines    map[string]float64
	empiricalSampleCounts map[string]int
	empiricalOnce         sync.Once
)

// CostBinKey returns the size-class key for item, mirroring the type+priority
// buckets in heuristicCostBaseline so empirical lookups align 1:1 with the
// fallback. Format: "<type>:hi" (priority ≤ 1) or "<type>:lo" (priority > 1).
func CostBinKey(item *model.Item) string {
	pri := 2
	if item.Priority != nil {
		pri = *item.Priority
	}
	tier := "lo"
	if pri <= 1 {
		tier = "hi"
	}
	t := item.Type
	if t == "" {
		t = "task"
	}
	return t + ":" + tier
}

// EmpiricalSamplesForBin returns the number of (item, session) cost samples
// used to derive the empirical baseline for key. Returns 0 when the bin is
// absent (heuristic fallback) or LoadEmpiricalBaselines has not yet run.
func EmpiricalSamplesForBin(key string) int {
	if empiricalSampleCounts == nil {
		return 0
	}
	return empiricalSampleCounts[key]
}

// LoadEmpiricalBaselines populates the package-level empiricalBaselines map
// exactly once (sync.Once). Subsequent calls are no-ops. items should be the
// done items from the archive; b provides the K2 (StuckMultiplier) and K1
// (PerItemUSD) guardrail parameters.
//
// Per-bin guardrails — if either fires the bin stays empty (heuristic fallback):
//   - N < 5: not enough archive samples to trust the median.
//   - median × b.StuckMultiplier ≥ b.PerItemUSD: K1-headroom breach — D2
//     would never fire before the hard budget cap.
//
// One diagnostic line per bin is written to stderr.
func LoadEmpiricalBaselines(items []*model.Item, b *Boundary) {
	empiricalOnce.Do(func() { loadEmpiricalBaselines(items, b) })
}

func loadEmpiricalBaselines(items []*model.Item, b *Boundary) {
	binSamples := make(map[string][]float64)

	for _, item := range items {
		if item == nil || item.Status != "done" || item.Type == "" {
			continue
		}
		key := CostBinKey(item)
		for _, cost := range aiTurnsCostBySession(item) {
			if cost <= 0 {
				continue
			}
			binSamples[key] = append(binSamples[key], cost)
		}
	}

	result := make(map[string]float64)
	counts := make(map[string]int)

	// Sort keys for deterministic log order.
	keys := make([]string, 0, len(binSamples))
	for k := range binSamples {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		samples := binSamples[key]
		n := len(samples)
		if n < 5 {
			fmt.Fprintf(os.Stderr, "empirical cost baseline [%s]: heuristic-fallback (N=%d < 5)\n", key, n)
			continue
		}
		sort.Float64s(samples)
		var median float64
		mid := n / 2
		if n%2 == 0 {
			median = (samples[mid-1] + samples[mid]) / 2
		} else {
			median = samples[mid]
		}
		if b.StuckMultiplier > 0 && b.PerItemUSD > 0 && median*b.StuckMultiplier >= b.PerItemUSD {
			fmt.Fprintf(os.Stderr,
				"empirical cost baseline [%s]: heuristic-fallback (median=$%.4f × K2=%g = $%.4f ≥ K1=$%g)\n",
				key, median, b.StuckMultiplier, median*b.StuckMultiplier, b.PerItemUSD)
			continue
		}
		result[key] = median
		counts[key] = n
		fmt.Fprintf(os.Stderr, "empirical cost baseline [%s]: median=$%.4f (N=%d)\n", key, median, n)
	}

	empiricalBaselines = result
	empiricalSampleCounts = counts
}

// aiTurnsCostBySession walks item.Doc.Lines for time_tracking.ai_turns entries
// and returns a map of session-id → total cost across all turns in that session.
// Entries with absent, malformed, or non-positive cost are skipped individually;
// sessions whose turns are all skipped are absent from the returned map.
func aiTurnsCostBySession(item *model.Item) map[string]float64 {
	if item == nil || item.Doc == nil {
		return nil
	}
	out := make(map[string]float64)
	inTT, inAI := false, false
	for _, line := range item.Doc.Lines {
		if line.Indent == 0 && line.Key != "" {
			inTT = line.Key == "time_tracking"
			inAI = false
			continue
		}
		if !inTT {
			continue
		}
		if line.Indent == 2 && line.Key == "ai_turns" {
			inAI = true
			continue
		}
		if line.Indent <= 2 && line.Key != "" && line.Key != "ai_turns" {
			inAI = false
			continue
		}
		if !inAI {
			continue
		}
		trimmed := strings.TrimSpace(line.Raw)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		entry := strings.TrimPrefix(trimmed, "- ")

		sid := empExtractField(entry, "session:")
		if sid == "" {
			continue
		}
		costRaw := empExtractField(entry, "cost:")
		if costRaw == "" || !strings.HasPrefix(costRaw, "$") {
			continue
		}
		cost, err := strconv.ParseFloat(costRaw[1:], 64)
		if err != nil || cost <= 0 {
			continue
		}
		out[sid] += cost
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// empExtractField pulls the value of a "key:value" token from an ai_turns
// line. Value runs to the next space or end of string. The match must be at
// position 0 or immediately after a space so that a key string appearing
// inside a value (e.g. "session:" inside a response ID) is not extracted.
// Duplicates command.extractField to avoid a circular import (command imports
// coordinator, so coordinator cannot import command).
func empExtractField(line, key string) string {
	idx := strings.Index(line, key)
	if idx < 0 {
		return ""
	}
	if idx > 0 && line[idx-1] != ' ' {
		return ""
	}
	rest := line[idx+len(key):]
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	return rest
}

// ResetEmpiricalForTest resets the sync.Once and empirical maps so unit tests
// can invoke LoadEmpiricalBaselines multiple times with different data.
// Must only be called from test code.
func ResetEmpiricalForTest() {
	empiricalOnce = sync.Once{}
	empiricalBaselines = nil
	empiricalSampleCounts = nil
}
