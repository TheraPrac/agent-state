package pricing

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// AnthropicPricingURL is the canonical source for Claude model rates.
const AnthropicPricingURL = "https://docs.anthropic.com/en/docs/about-claude/pricing"

// RateDiff describes a single per-field change between old and new rate tables.
type RateDiff struct {
	Model     string
	Field     string
	Old       float64
	New       float64
	PctChange float64 // +n% = increase, -n% = decrease; for new models (Old==0) = +100 by convention
}

// FetchAnthropicRates fetches and parses Anthropic's pricing page.
// Pass nil to use http.DefaultClient.
func FetchAnthropicRates(client *http.Client) (map[string]Rate, error) {
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Get(AnthropicPricingURL)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing: fetch returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("pricing: read body: %w", err)
	}
	return parseAnthropicHTML(string(body))
}

var (
	rowRe    = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	cellRe   = regexp.MustCompile(`(?s)<t[dh][^>]*>(.*?)</t[dh]>`)
	tagRe    = regexp.MustCompile(`<[^>]+>`)
	dollarRe = regexp.MustCompile(`\$([\d.]+)`)
)

// parseAnthropicHTML extracts model rates from the pricing page HTML using
// regexp-based table scanning (no golang.org/x/net dependency).
//
// Column positions are detected by matching header cell text ("Input",
// "Output", etc.) rather than by fixed index — so adding a new column to the
// Anthropic page does not silently shift price assignments.
//
// Returns an error when Input/Output header columns cannot be found, or when
// fewer than 2 distinct model families are parsed.
func parseAnthropicHTML(body string) (map[string]Rate, error) {
	// Collect all rows' cell texts in a single pass.
	type htmlRow struct{ cells []string }
	var rows []htmlRow
	for _, rowMatch := range rowRe.FindAllStringSubmatch(body, -1) {
		cells := cellRe.FindAllStringSubmatch(rowMatch[1], -1)
		var texts []string
		for _, c := range cells {
			raw := tagRe.ReplaceAllString(c[1], " ")
			texts = append(texts, strings.TrimSpace(strings.Join(strings.Fields(raw), " ")))
		}
		if len(texts) > 0 {
			rows = append(rows, htmlRow{cells: texts})
		}
	}

	// Find column positions from the first header row (a row whose first cell
	// is NOT a Claude model ID). Detect by column name so that Anthropic can
	// add, remove, or reorder columns without silently misassigning prices.
	inputCol, outputCol := -1, -1
	cacheW5mCol, cacheW1hCol, cacheRCol := -1, -1, -1

	for _, r := range rows {
		if len(r.cells) == 0 || anthropicNameToID(r.cells[0]) != "" {
			continue // skip data rows
		}
		for i, cell := range r.cells {
			lower := strings.ToLower(cell)
			switch {
			case lower == "input":
				inputCol = i
			case lower == "output":
				outputCol = i
			case strings.Contains(lower, "5m") || strings.Contains(lower, "5 min"):
				cacheW5mCol = i
			case strings.Contains(lower, "1h") || strings.Contains(lower, "1 hour"):
				cacheW1hCol = i
			case strings.HasPrefix(lower, "cache") && strings.Contains(lower, "read"):
				cacheRCol = i
			}
		}
		if inputCol >= 0 && outputCol >= 0 {
			break
		}
		// Reset and try the next candidate header row
		inputCol, outputCol = -1, -1
		cacheW5mCol, cacheW1hCol, cacheRCol = -1, -1, -1
	}

	if inputCol < 0 || outputCol < 0 {
		return nil, fmt.Errorf("pricing: could not find Input/Output columns in pricing table — page structure may have changed")
	}

	getPrice := func(cells []string, col int) (float64, bool) {
		if col < 0 || col >= len(cells) {
			return 0, false
		}
		m := dollarRe.FindStringSubmatch(cells[col])
		if m == nil {
			return 0, false
		}
		v, err := strconv.ParseFloat(m[1], 64)
		return v, err == nil
	}

	rates := make(map[string]Rate)
	for _, r := range rows {
		if len(r.cells) == 0 {
			continue
		}
		modelID := anthropicNameToID(r.cells[0])
		if modelID == "" {
			continue
		}

		input, ok1 := getPrice(r.cells, inputCol)
		output, ok2 := getPrice(r.cells, outputCol)
		if !ok1 || !ok2 {
			continue
		}

		cw5m, okCw5m := getPrice(r.cells, cacheW5mCol)
		cw1h, okCw1h := getPrice(r.cells, cacheW1hCol)
		cr, okCr := getPrice(r.cells, cacheRCol)
		// Derive cache prices from the standard Anthropic ratios when not in the table.
		if !okCw5m {
			cw5m = input * 1.25
		}
		if !okCw1h {
			cw1h = input * 2.0
		}
		if !okCr {
			cr = input * 0.1
		}

		rates[modelID] = Rate{
			Input:        input,
			Output:       output,
			CacheWrite5m: cw5m,
			CacheWrite1h: cw1h,
			CacheRead:    cr,
		}
	}

	families := map[string]bool{}
	for k := range rates {
		parts := strings.SplitN(k, "-", 3)
		if len(parts) >= 2 {
			families[parts[1]] = true
		}
	}
	if len(families) < 2 {
		return nil, fmt.Errorf("pricing: parsed %d model(s) across %d family(ies) — page structure may have changed", len(rates), len(families))
	}

	return rates, nil
}

// anthropicNameToID converts Anthropic display names to internal model IDs.
// "Claude Opus 4.7" → "claude-opus-4-7", "Claude Haiku 3.5" → "claude-haiku-3-5"
// Rows with parenthesized annotations ("retired", "deprecated") or slash-combined
// entries ("Claude Opus 4.6 / Claude Opus 4.7") return "" and are skipped.
func anthropicNameToID(name string) string {
	// Strip parenthesized annotations (e.g. " (retired, except on Bedrock and Vertex AI)")
	if i := strings.Index(name, "("); i >= 0 {
		name = name[:i]
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	if !strings.HasPrefix(lower, "claude") {
		return ""
	}
	id := strings.ReplaceAll(lower, ".", "-")
	id = strings.ReplaceAll(id, " ", "-")
	for strings.Contains(id, "--") {
		id = strings.ReplaceAll(id, "--", "-")
	}
	id = strings.TrimRight(id, "-")
	// Reject slash-combined entries like "claude-opus-4-6-/-claude-opus-4-7"
	if strings.ContainsAny(id, "/,") {
		return ""
	}
	return id
}

// DiffRates returns per-model per-field deltas between old and updated rate
// tables, sorted by model then field.
//
// When a model appears only in updated (Old == 0), PctChange is set to +100
// by convention to indicate a new addition. When a model appears only in old
// (New == 0, PctChange ≈ -100), it was removed from the fetched page.
// SanityCheck skips new-model entries (Old == 0) so additions never block
// an otherwise safe refresh.
func DiffRates(old, updated map[string]Rate) []RateDiff {
	allModels := make(map[string]bool, len(old)+len(updated))
	for k := range old {
		allModels[k] = true
	}
	for k := range updated {
		allModels[k] = true
	}

	models := make([]string, 0, len(allModels))
	for m := range allModels {
		models = append(models, m)
	}
	sort.Strings(models)

	type fieldDesc struct {
		name string
		get  func(Rate) float64
	}
	fields := []fieldDesc{
		{"input", func(r Rate) float64 { return r.Input }},
		{"output", func(r Rate) float64 { return r.Output }},
		{"cache_write_5m", func(r Rate) float64 { return r.CacheWrite5m }},
		{"cache_write_1h", func(r Rate) float64 { return r.CacheWrite1h }},
		{"cache_read", func(r Rate) float64 { return r.CacheRead }},
	}

	var diffs []RateDiff
	for _, m := range models {
		oldR := old[m]
		newR := updated[m]
		for _, f := range fields {
			ov := f.get(oldR)
			nv := f.get(newR)
			if ov == nv {
				continue
			}
			var pct float64
			if ov != 0 {
				pct = (nv - ov) / ov * 100
			} else {
				pct = 100 // new model addition
			}
			diffs = append(diffs, RateDiff{
				Model: m, Field: f.name,
				Old: ov, New: nv, PctChange: pct,
			})
		}
	}
	return diffs
}

// SanityCheck returns true when no existing-model field change exceeds maxPct
// percent. New-model entries (Old == 0) are always allowed regardless of maxPct.
// A maxPct of 0 or negative disables the check entirely (always returns true).
func SanityCheck(diffs []RateDiff, maxPct float64) bool {
	if maxPct <= 0 {
		return true
	}
	for _, d := range diffs {
		if d.Old == 0 {
			continue // new model addition — never block
		}
		if math.Abs(d.PctChange) > maxPct {
			return false
		}
	}
	return true
}

// FormatDiff returns a human-readable summary of rate changes.
// Returns a short "up to date" message when diffs is empty.
func FormatDiff(diffs []RateDiff) string {
	if len(diffs) == 0 {
		return "pricing table is up to date — no changes detected\n"
	}
	var b strings.Builder
	for _, d := range diffs {
		var pctStr string
		switch {
		case d.Old == 0:
			pctStr = "(new)"
		case d.New == 0:
			pctStr = "(removed)"
		default:
			pctStr = fmt.Sprintf("(%+.1f%%)", d.PctChange)
		}
		fmt.Fprintf(&b, "  %-28s %-15s %8.4g → %8.4g  %s\n",
			d.Model, d.Field, d.Old, d.New, pctStr)
	}
	return b.String()
}
