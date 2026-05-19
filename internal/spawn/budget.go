package spawn

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CoordinatorYAMLPath returns the absolute path to the autonomy-boundary
// file. It is operator-owned and lives at <workspace>/.as/coordinator.yaml
// (the same .as/ that holds agents/ and config.yaml). `st spawn` READS
// it, never writes it (contract §11).
func CoordinatorYAMLPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".as", "coordinator.yaml")
}

// yamlLevel is one open mapping level while scanning coordinator.yaml.
type yamlLevel struct {
	indent int
	key    string
}

// ParsePerItemBudget extracts escalation.budget_cap_usd.per_item from
// coordinator.yaml and returns it as a positive USD amount.
//
// The as repo deliberately carries no YAML dependency (every other YAML
// surface is hand-rolled), so this is a tiny indentation-tracking
// path-matcher scoped to the one key we need. It is intentionally strict:
// a missing file, a missing key, or a non-positive / unparseable value
// is a hard error so `st spawn` NEVER launches an uncapped worker — the
// K1 cap is a process-enforced circuit breaker, not advisory (§11/§13).
func ParsePerItemBudget(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("autonomy boundary not found at %s — refusing to spawn an uncapped worker (contract §11)", path)
		}
		return 0, fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer f.Close()

	// want is the nested key sequence we match; stack holds the
	// (indent, key) of each mapping level currently open.
	want := []string{"escalation", "budget_cap_usd", "per_item"}
	var stack []yamlLevel

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw := sc.Text()

		// Strip a trailing inline comment. coordinator.yaml values are
		// bare numbers (no quoting), so a '#' unambiguously starts a
		// comment here.
		if i := strings.IndexByte(raw, '#'); i >= 0 {
			raw = raw[:i]
		}
		if strings.TrimSpace(raw) == "" {
			continue
		}

		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		trimmed := strings.TrimSpace(raw)

		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			continue // list items / continuations — not on our path
		}
		key := strings.TrimSpace(trimmed[:colon])
		value := strings.TrimSpace(trimmed[colon+1:])

		// Pop levels that this line is not nested under.
		for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, yamlLevel{indent: indent, key: key})

		if !pathMatches(stack, want) {
			continue
		}
		if value == "" {
			return 0, fmt.Errorf("%s: %s is present but has no value", path, strings.Join(want, "."))
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %s = %q is not a number", path, strings.Join(want, "."), value)
		}
		if n <= 0 {
			return 0, fmt.Errorf("%s: %s = %v must be > 0 (an uncapped worker is never allowed)", path, strings.Join(want, "."), n)
		}
		return n, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	return 0, fmt.Errorf("%s: %s not found — refusing to spawn an uncapped worker (contract §11)", path, strings.Join(want, "."))
}

// pathMatches reports whether the open key stack is exactly the wanted
// nested path (keys equal, in order, same depth).
func pathMatches(stack []yamlLevel, want []string) bool {
	if len(stack) != len(want) {
		return false
	}
	for i := range want {
		if stack[i].key != want[i] {
			return false
		}
	}
	return true
}
