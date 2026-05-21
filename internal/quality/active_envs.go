package quality

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

// ActiveEnvs is the parsed shape of theraprac-workspace/.as/active-envs.yaml.
//
// I-731: the file is the OPERATOR's authoritative declaration of which
// envs are operationally-active. Agents must consult it before any
// AWS-touching op. This struct is the typed handle the st check
// sentinel and the active-envs-guard hook both consume.
type ActiveEnvs struct {
	DeclaredBy     string    // operator handle
	DeclaredAt     time.Time // when the operator last edited active_envs
	LastReconciled time.Time // when the file was last verified against AWS reality
	Active         []string  // envs the operator declares active right now
	TornDown       []string  // envs the operator declares torn down
}

// ParseActiveEnvs reads and decodes the active-envs.yaml at path.
// Returns (nil, error) only on file-read errors; malformed content
// produces a partial struct + nil error so the validator (not the
// parser) decides what's a warning.
//
// Hand-rolled minimal YAML parser to avoid pulling in a yaml
// dependency the rest of the codebase doesn't use (CLAUDE.md
// "Never introduce new patterns or dependencies without explicit
// instruction"). The file's shape is small and stable: top-level
// scalars (declared_by, declared_at, last_reconciled) and two
// list-of-strings (active_envs, torn_down). Comments (# …) and
// blank lines are skipped.
//
// Time-string parse failures land in DeclaredAt / LastReconciled
// as the zero time (validator surfaces them via the freshness
// check).
func ParseActiveEnvs(path string) (*ActiveEnvs, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ae := &ActiveEnvs{}
	scanner := bufio.NewScanner(f)
	currentList := "" // "active_envs" | "torn_down" | "" (top-level)

	for scanner.Scan() {
		raw := scanner.Text()
		// Strip line comments (# …) and trailing whitespace. Don't
		// strip leading whitespace — indentation distinguishes
		// list items from top-level keys.
		if i := strings.Index(raw, "#"); i >= 0 {
			raw = raw[:i]
		}
		raw = strings.TrimRight(raw, " \t")
		if strings.TrimSpace(raw) == "" {
			// Blank line resets list context per YAML block-scalar
			// convention (a blank line between two list-typed keys
			// is the natural separator).
			continue
		}

		// List item under the current list-context key.
		trimmed := strings.TrimLeft(raw, " \t")
		if strings.HasPrefix(trimmed, "- ") {
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			// Strip quotes a YAML author might add: "demo" or 'demo'.
			item = strings.Trim(item, `"'`)
			if currentList == "active_envs" {
				ae.Active = append(ae.Active, item)
			} else if currentList == "torn_down" {
				ae.TornDown = append(ae.TornDown, item)
			}
			continue
		}

		// Top-level `key: value` or `key:` (introducing a list).
		// Only handle keys at column 0 — indented `key: value`
		// lines are nested structure we don't expect in this file.
		if raw != trimmed {
			// Indented but not a list item — ignore (forward-compat
			// for future nested fields the parser hasn't learned).
			continue
		}
		colon := strings.Index(trimmed, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:colon])
		value := strings.TrimSpace(trimmed[colon+1:])
		// Strip quotes if the operator wrote `key: "value"`.
		value = strings.Trim(value, `"'`)

		switch key {
		case "declared_by":
			ae.DeclaredBy = value
			currentList = ""
		case "declared_at":
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				ae.DeclaredAt = t
			}
			currentList = ""
		case "last_reconciled":
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				ae.LastReconciled = t
			}
			currentList = ""
		case "active_envs":
			// Either `active_envs: []` inline-empty or a list
			// continuation on following lines.
			if value == "[]" {
				ae.Active = []string{}
				currentList = ""
			} else {
				currentList = "active_envs"
			}
		case "torn_down":
			if value == "[]" {
				ae.TornDown = []string{}
				currentList = ""
			} else {
				currentList = "torn_down"
			}
		default:
			// Unknown key — reset list context so stray list items
			// don't accumulate into the wrong field.
			currentList = ""
		}
	}
	if err := scanner.Err(); err != nil {
		return ae, fmt.Errorf("active-envs.yaml scan: %w", err)
	}
	return ae, nil
}

// ActiveEnvsValidateOpts controls the freshness threshold.
//
// Now is injectable so tests can pin time-since-DeclaredAt without
// touching the wall clock. Production passes time.Now.
type ActiveEnvsValidateOpts struct {
	StaleAfter time.Duration    // default 14 * 24h if zero
	Now        func() time.Time // default time.Now
}

// ValidateActiveEnvs returns warning-only Violations against an
// ActiveEnvs struct. All returned Violations are SeverityWarn — this
// sentinel is the surface layer (I-731 Layer 1), never the gate.
// The gate is the bash hook (Layer 2) at the PreToolUse boundary.
//
// Findings:
//
//   - active_envs empty: the operator removed every active env. Not
//     an error (they may be doing a maintenance window), but worth
//     surfacing — agents have nothing they're authorized to touch.
//   - declared_at older than StaleAfter: the declaration may not
//     match current operational reality. Operator should re-confirm
//     by editing the file or running `st reconcile`.
//   - env in both active_envs and torn_down: data error, the operator
//     hasn't cleaned up after a re-activation.
//   - declared_at unparseable or missing: validator surfaces the
//     parser silence.
func ValidateActiveEnvs(ae *ActiveEnvs, opts ActiveEnvsValidateOpts) []Violation {
	if ae == nil {
		return []Violation{{
			Severity: SeverityWarn,
			Field:    "active-envs.yaml",
			Message:  "no active-envs declaration found — every env is treated as undeclared and out-of-bounds",
		}}
	}
	stale := opts.StaleAfter
	if stale <= 0 {
		stale = 14 * 24 * time.Hour
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	var out []Violation
	if len(ae.Active) == 0 {
		out = append(out, Violation{
			Severity: SeverityWarn,
			Field:    "active_envs",
			Message:  "active_envs is empty — agents have no env they are authorized to touch",
		})
	}
	if ae.DeclaredAt.IsZero() {
		out = append(out, Violation{
			Severity: SeverityWarn,
			Field:    "declared_at",
			Message:  "declared_at is missing or unparseable — cannot evaluate declaration freshness",
		})
	} else if now().Sub(ae.DeclaredAt) > stale {
		age := now().Sub(ae.DeclaredAt).Round(time.Hour)
		out = append(out, Violation{
			Severity: SeverityWarn,
			Field:    "declared_at",
			Message:  fmt.Sprintf("declaration is %s old (>%s); operator should re-confirm via edit + `st reconcile`", age, stale),
		})
	}
	// Cross-list contamination: env can't be both active and torn-down.
	tornSet := map[string]struct{}{}
	for _, e := range ae.TornDown {
		tornSet[e] = struct{}{}
	}
	for _, e := range ae.Active {
		if _, ok := tornSet[e]; ok {
			out = append(out, Violation{
				Severity: SeverityWarn,
				Field:    "active_envs",
				Message:  fmt.Sprintf("env %q appears in both active_envs and torn_down — clean up the contradiction", e),
			})
		}
	}
	return out
}

// IsActive reports whether env is in the active_envs list.
// Case-sensitive — env names are conventionally lowercase.
func (ae *ActiveEnvs) IsActive(env string) bool {
	if ae == nil {
		return false
	}
	env = strings.TrimSpace(env)
	for _, e := range ae.Active {
		if e == env {
			return true
		}
	}
	return false
}

// IsTornDown reports whether env is in the torn_down list.
func (ae *ActiveEnvs) IsTornDown(env string) bool {
	if ae == nil {
		return false
	}
	env = strings.TrimSpace(env)
	for _, e := range ae.TornDown {
		if e == env {
			return true
		}
	}
	return false
}
