package validate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// WriteError is the structured error returned by WriteOK when a
// pending mutation would land an item with invalid type, status, or
// missing required fields. The Errors slice contains every failing
// check so callers can render the full picture; Error() produces a
// stable, human-readable summary.
type WriteError struct {
	Errors []Error
}

func (e *WriteError) Error() string {
	if len(e.Errors) == 0 {
		return "validate: empty write error"
	}
	var lines []string
	for _, er := range e.Errors {
		lines = append(lines, er.String())
	}
	return strings.Join(lines, "; ")
}

// ErrInvalidWrite is the sentinel for `errors.Is` checks against
// WriteError. Callers can write `errors.Is(err, validate.ErrInvalidWrite)`
// without depending on the concrete type.
var ErrInvalidWrite = errors.New("invalid item write")

// Is implements errors.Is so WriteError matches ErrInvalidWrite.
func (e *WriteError) Is(target error) bool {
	return target == ErrInvalidWrite
}

// WriteOK validates an item that is about to be written. Returns nil
// when the write is allowed, or a *WriteError listing every failing
// check.
//
// Strict vocab gates (always enforced, regardless of pre-state):
//   - type is known to cfg.Types
//   - status is valid for type
//
// Use WriteOKDelta when you have the pre-mutation snapshot — it adds
// the "required field was present and just got dropped" check, which
// requires before/after context. WriteOK alone is the strict-vocab-only
// path used at brand-new item creation. I-508.
func WriteOK(item *model.Item, cfg *config.Config) error {
	r := &Result{}
	checkVocab(r, item, cfg)
	checkRequiredFields(r, item, cfg)
	if r.OK() {
		return nil
	}
	return &WriteError{Errors: r.Errors}
}

// WriteOKDelta validates a pending mutation given the pre-mutation
// snapshot. Same strict vocab gates as WriteOK, plus a regression
// guard on required fields: if the pre-state had a required field
// present and the post-state drops it, reject. Pre-existing missing
// fields are NOT retroactively rejected — that would freeze recovery
// updates on legacy items. I-508.
func WriteOKDelta(before, after *model.Item, cfg *config.Config) error {
	r := &Result{}
	checkVocab(r, after, cfg)
	checkRequiredFieldsDelta(r, before, after, cfg)
	if r.OK() {
		return nil
	}
	return &WriteError{Errors: r.Errors}
}

func checkVocab(r *Result, item *model.Item, cfg *config.Config) {
	if item.Type == "" {
		r.add(item.ID, "type", "required")
	} else if _, ok := cfg.Types[item.Type]; !ok {
		validTypes := make([]string, 0, len(cfg.Types))
		for k := range cfg.Types {
			validTypes = append(validTypes, k)
		}
		r.add(item.ID, "type", fmt.Sprintf("unknown type %q — valid: %s",
			item.Type, strings.Join(validTypes, ", ")))
	}

	if item.Type != "" && item.Status != "" {
		validStatuses := cfg.ValidStatuses(item.Type)
		if len(validStatuses) > 0 && !contains(validStatuses, item.Status) {
			msg := fmt.Sprintf("invalid status %q for type %q — valid: %s",
				item.Status, item.Type, strings.Join(validStatuses, ", "))
			if hint := suggestStatus(item.Status); hint != "" {
				msg += fmt.Sprintf(" — suggestion: did you mean %q? (legacy alias from pre-I-433)", hint)
			}
			r.add(item.ID, "status", msg)
		}
	}
}

// checkRequiredFields enforces RequiredFields presence on non-terminal
// items. Used by WriteOK (at Create time, when there is no pre-state).
func checkRequiredFields(r *Result, item *model.Item, cfg *config.Config) {
	if item.Type == "" || item.Doc == nil || cfg.IsTerminalStatus(item.Type, item.Status) {
		return
	}
	tc, ok := cfg.Types[item.Type]
	if !ok {
		return
	}
	for _, field := range tc.RequiredFields {
		if !HasField(item.Doc, field) {
			r.add(item.ID, field, fmt.Sprintf("required for type %q", item.Type))
		}
	}
}

// checkRequiredFieldsDelta enforces "required field present in pre-state
// must remain present in post-state." Pre-existing missing fields are
// not retroactively rejected — that would freeze recovery updates on
// legacy items. I-508.
func checkRequiredFieldsDelta(r *Result, before, after *model.Item, cfg *config.Config) {
	if after.Type == "" || after.Doc == nil || cfg.IsTerminalStatus(after.Type, after.Status) {
		return
	}
	tc, ok := cfg.Types[after.Type]
	if !ok {
		return
	}
	for _, field := range tc.RequiredFields {
		hadBefore := before != nil && before.Doc != nil && HasField(before.Doc, field)
		hasAfter := HasField(after.Doc, field)
		if hadBefore && !hasAfter {
			r.add(after.ID, field, fmt.Sprintf("required for type %q (cannot be removed once present)", after.Type))
		}
	}
}
