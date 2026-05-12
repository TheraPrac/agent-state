package command

import (
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Release clears the session claim and agent assignment on an item,
// allowing another session/agent to work on it.
func Release(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	// I-408: an item can land in a "stuck active" state (assigned/claim
	// cleared by hand, status still active) — that's exactly what this
	// PR is fixing. Allow release to proceed when status is the type's
	// ActiveStatus even if the cached read shows no assignment, so the
	// recovery path is reachable. The cached read is only an early-exit
	// optimization; the Mutate callback is the source of truth.
	stuckActive := false
	if tc, ok := cfg.Types[item.Type]; ok && item.Status == tc.ActiveStatus {
		stuckActive = true
	}
	if item.AssignedTo == "" && item.ClaimedBy == "" && !stuckActive {
		fmt.Fprintf(os.Stderr, "%s is not assigned or claimed\n", id)
		return 1
	}

	// Capture the live claimed_by under the flock and release the
	// session-manager record AFTER the Mutate, so we can't release the
	// wrong session if a re-claim landed between the cached read and
	// the lock acquisition.
	oldAgent := item.AssignedTo
	var liveClaim string
	// Captured under the flock so the changelog audit reflects the values
	// that were actually cleared, not a pre-lock cached read.
	oldHeritage := map[string]string{}
	// I-408: when an item is released from the active state, also reset
	// status back to the type's StartStatus (queued for both tasks and
	// issues under the I-433 unified vocab) so it disappears from
	// `--status active` lists. Captured under the lock so the changelog
	// reflects the value actually persisted.
	var statusBefore, statusAfter string

	if err := s.Mutate(id, func(item *model.Item) error {
		liveClaim = item.ClaimedBy
		if item.AssignedTo != "" {
			item.AssignedTo = ""
			item.Doc.SetField("assigned_to", "")
			// Capture and remove any inherited heritage from the previous
			// claim. Removal (not blanking) keeps the next non-heritage
			// start from inheriting stale empty meta keys.
			for _, key := range []string{"parent_id", "root_id", "role", "spawned_by", "delegated_item"} {
				if v, ok := item.Doc.GetNestedField("assigned_to_meta." + key); ok && v != "" {
					oldHeritage[key] = v
				}
				item.Doc.RemoveNestedField("assigned_to_meta." + key)
			}
		}
		if item.ClaimedBy != "" {
			item.ClaimedBy = ""
			item.ClaimedAt = ""
			item.Doc.SetField("claimed_by", "")
			item.Doc.SetField("claimed_at", "")
		}
		if tc, ok := cfg.Types[item.Type]; ok {
			if item.Status == tc.ActiveStatus {
				statusBefore = item.Status
				item.Status = tc.StartStatus
				item.Doc.SetField("status", tc.StartStatus)
				statusAfter = item.Status
			}
		} else if item.Status == "active" {
			// Unknown type but status is active: warn so the operator
			// knows status was left as-is. Without this, the assignment
			// and claim clear but status stays "active" silently.
			fmt.Fprintf(os.Stderr,
				"warning: unknown type %q for %s — status %q left untouched; update manually if needed\n",
				item.Type, id, item.Status)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	oldClaim := liveClaim
	if liveClaim != "" {
		mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
		_ = mgr.RemoveClaim(liveClaim, id)
	}

	// Release item lock
	store.UnlockItem(cfg, id)

	changelog.Append(cfg, id, changelog.Entry{
		Op: "release", Field: "assigned_to", OldValue: oldAgent,
	})
	for _, key := range []string{"parent_id", "root_id", "role", "spawned_by", "delegated_item"} {
		if v, ok := oldHeritage[key]; ok {
			changelog.Append(cfg, id, changelog.Entry{
				Op: "release", Field: "assigned_to_meta." + key, OldValue: v,
			})
		}
	}
	if statusBefore != "" {
		changelog.Append(cfg, id, changelog.Entry{
			Op: "release", Field: "status", OldValue: statusBefore, NewValue: statusAfter,
		})
	}

	statusNote := ""
	if statusBefore != "" {
		statusNote = fmt.Sprintf(" (status: %s → %s)", statusBefore, statusAfter)
	}
	switch {
	case oldAgent != "" && oldClaim != "":
		fmt.Printf("Released %s — was assigned to %s, claimed by session %s%s\n", id, oldAgent, oldClaim, statusNote)
	case oldAgent != "":
		fmt.Printf("Released %s — was assigned to %s%s\n", id, oldAgent, statusNote)
	case oldClaim != "":
		fmt.Printf("Released %s — was claimed by session %s%s\n", id, oldClaim, statusNote)
	default:
		// I-408: stuck-active recovery — neither assigned nor claimed,
		// status was active. The status reset is the whole story.
		fmt.Printf("Released %s — recovered from stuck-active%s\n", id, statusNote)
	}

	// I-232: a released item is back to queued/start status; if it was
	// also sitting on the agent's stack, drop the ghost entry so
	// `st stack` doesn't keep advertising work the operator no longer
	// owns. Symmetric with the queue/stack auto-cleanup on close.
	// Best-effort: never blocks the release.
	if _, serr := removeFromStackSilently(cfg, id); serr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to remove %s from stack: %v\n", id, serr)
	}

	// Commit + push so the claim release is visible to other sessions
	// immediately. Best-effort — on failure the on-disk state is still
	// correct and a later sync will propagate it.
	if err := s.GitSync(fmt.Sprintf("st release: %s", id)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after release failed: %v\n", err)
	}
	return 0
}
