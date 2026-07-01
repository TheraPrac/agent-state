// Package session manages ephemeral session identity, claim tracking,
// and stale-session detection for the st CLI.
package session

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session represents an active CLI session (one Claude Code run).
//
// Heritage fields (ParentAgentID, RootAgentID, Role, SpawnedBySession,
// DelegatedItemID) are populated when this session inherits identity from a
// spawning agent. They preserve attribution across parent → child agent
// chains so usage rollups and claim tracking can credit the full lineage.
type Session struct {
	ID           string
	StartedAt    time.Time
	AgentID      string
	Sprint       string
	LastActive   time.Time
	ClaimedItems []string

	ParentAgentID    string
	RootAgentID      string
	Role             string
	SpawnedBySession string
	DelegatedItemID  string

	Pairing *Pairing
}

// Pairing marks a session as mid-flight on the I-1700 `/pair` live-iteration
// mode. It is session-local, ephemeral state — not git-shared item data — so
// it is never changelog-logged or synced; it lives only in this session's
// yaml. Hooks (plan-before-code-guard.sh, session-stop.sh,
// model-check-on-start.sh, context-hygiene-nudge.sh, heuristic-nudge.sh) read
// it via the shared pairing-mode.sh bash fragment to relax in-session
// friction for the paired item.
type Pairing struct {
	Active      bool
	Item        string
	Worktree    string
	ActivatedAt time.Time
}

// pairingAbandonedAfter bounds how long PruneStaleSessions protects an
// active pairing marker from deletion. A session that crashes or is
// force-killed while paired never runs `st pair --off`, so without this
// ceiling its .as/sessions/<id>.yaml would be exempt from the TTL sweep
// forever (Pairing.Active alone is not evidence the marker is still
// meaningful — only that nothing ever cleared it). Deliberately much
// longer than the stale-claim TTL (default 2h) so a genuinely active
// paired session is never pruned mid-use.
const pairingAbandonedAfter = 24 * time.Hour

// Manager provides session lifecycle operations.
type Manager struct {
	dir string // .as/sessions/
	ttl time.Duration
}

// NewManager creates a session manager.
// dir is the .as/sessions/ directory path.
// ttl is the stale claim threshold.
func NewManager(dir string, ttl time.Duration) *Manager {
	return &Manager{dir: dir, ttl: ttl}
}

// Load reads a session file. Returns nil, nil if not found.
func (m *Manager) Load(sessionID string) (*Session, error) {
	path := m.path(sessionID)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	s := &Session{}
	scanner := bufio.NewScanner(f)
	var inClaimed bool
	var inPairing bool
	var pairing *Pairing

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if trimmed == "claimed_items:" {
			inClaimed = true
			continue
		}

		if strings.HasPrefix(trimmed, "- ") && inClaimed {
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			item = strings.Trim(item, `"'`)
			if item != "" && item != "[]" {
				s.ClaimedItems = append(s.ClaimedItems, item)
			}
			continue
		}

		if inClaimed && !strings.HasPrefix(trimmed, "- ") {
			inClaimed = false
		}

		if trimmed == "pairing:" {
			inPairing = true
			continue
		}

		if inPairing {
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(trimmed, "- ") {
				if idx := strings.Index(trimmed, ":"); idx >= 0 {
					pkey := strings.TrimSpace(trimmed[:idx])
					pval := strings.Trim(strings.TrimSpace(trimmed[idx+1:]), `"'`)
					if pairing == nil {
						pairing = &Pairing{}
					}
					switch pkey {
					case "active":
						pairing.Active = pval == "true"
					case "item":
						pairing.Item = pval
					case "worktree":
						pairing.Worktree = pval
					case "activated_at":
						pairing.ActivatedAt = parseTime(pval)
					}
				}
				continue
			}
			inPairing = false
		}

		if idx := strings.Index(trimmed, ":"); idx >= 0 {
			key := strings.TrimSpace(trimmed[:idx])
			val := strings.TrimSpace(trimmed[idx+1:])
			val = strings.Trim(val, `"'`)

			switch key {
			case "id":
				s.ID = val
			case "started_at":
				s.StartedAt = parseTime(val)
			case "agent_id":
				s.AgentID = val
			case "sprint":
				s.Sprint = val
			case "last_active":
				s.LastActive = parseTime(val)
			case "parent_agent_id":
				s.ParentAgentID = val
			case "root_agent_id":
				s.RootAgentID = val
			case "role":
				s.Role = val
			case "spawned_by_session":
				s.SpawnedBySession = val
			case "delegated_item":
				s.DelegatedItemID = val
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	s.Pairing = pairing
	return s, nil
}

// Save writes a session file.
func (m *Manager) Save(s *Session) error {
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("id: %s\n", s.ID))
	b.WriteString(fmt.Sprintf("started_at: %s\n", s.StartedAt.Format(time.RFC3339)))
	if s.AgentID != "" {
		b.WriteString(fmt.Sprintf("agent_id: %s\n", s.AgentID))
	}
	if s.Sprint != "" {
		b.WriteString(fmt.Sprintf("sprint: %s\n", s.Sprint))
	}
	if s.ParentAgentID != "" {
		b.WriteString(fmt.Sprintf("parent_agent_id: %s\n", s.ParentAgentID))
	}
	if s.RootAgentID != "" {
		b.WriteString(fmt.Sprintf("root_agent_id: %s\n", s.RootAgentID))
	}
	if s.Role != "" {
		b.WriteString(fmt.Sprintf("role: %s\n", s.Role))
	}
	if s.SpawnedBySession != "" {
		b.WriteString(fmt.Sprintf("spawned_by_session: %s\n", s.SpawnedBySession))
	}
	if s.DelegatedItemID != "" {
		b.WriteString(fmt.Sprintf("delegated_item: %s\n", s.DelegatedItemID))
	}
	b.WriteString(fmt.Sprintf("last_active: %s\n", s.LastActive.Format(time.RFC3339)))
	b.WriteString("claimed_items:\n")
	if len(s.ClaimedItems) == 0 {
		b.WriteString("  - []\n")
	} else {
		for _, id := range s.ClaimedItems {
			b.WriteString(fmt.Sprintf("  - %s\n", id))
		}
	}
	if s.Pairing != nil {
		b.WriteString("pairing:\n")
		activeStr := "false"
		if s.Pairing.Active {
			activeStr = "true"
		}
		b.WriteString(fmt.Sprintf("  active: %s\n", activeStr))
		if s.Pairing.Item != "" {
			b.WriteString(fmt.Sprintf("  item: %s\n", s.Pairing.Item))
		}
		if s.Pairing.Worktree != "" {
			b.WriteString(fmt.Sprintf("  worktree: %s\n", s.Pairing.Worktree))
		}
		if !s.Pairing.ActivatedAt.IsZero() {
			b.WriteString(fmt.Sprintf("  activated_at: %s\n", s.Pairing.ActivatedAt.Format(time.RFC3339)))
		}
	}

	return os.WriteFile(m.path(s.ID), []byte(b.String()), 0644)
}

// IdentitySpec carries optional sub-agent heritage fields when creating a
// session via EnsureSessionWithIdentity. Mirrors config.Identity but is
// declared here to avoid an import cycle (session ← config).
type IdentitySpec struct {
	AgentID          string
	ParentAgentID    string
	RootAgentID      string
	Role             string
	SpawnedBySession string
	DelegatedItemID  string
}

// EnsureSession loads or creates a session for the given ID.
// If the session doesn't exist, a new one is created with the current time.
func (m *Manager) EnsureSession(sessionID, agentID string) (*Session, error) {
	return m.EnsureSessionWithIdentity(sessionID, IdentitySpec{AgentID: agentID})
}

// EnsureSessionWithIdentity loads or creates a session, recording full
// sub-agent heritage on creation. Existing sessions are returned unchanged
// — heritage is only set on first creation.
func (m *Manager) EnsureSessionWithIdentity(sessionID string, spec IdentitySpec) (*Session, error) {
	s, err := m.Load(sessionID)
	if err != nil {
		return nil, err
	}
	if s != nil {
		return s, nil
	}

	now := time.Now()
	s = &Session{
		ID:               sessionID,
		StartedAt:        now,
		AgentID:          spec.AgentID,
		ParentAgentID:    spec.ParentAgentID,
		RootAgentID:      spec.RootAgentID,
		Role:             spec.Role,
		SpawnedBySession: spec.SpawnedBySession,
		DelegatedItemID:  spec.DelegatedItemID,
		LastActive:       now,
	}
	if err := m.Save(s); err != nil {
		return nil, err
	}
	return s, nil
}

// Touch updates last_active on a session (heartbeat).
func (m *Manager) Touch(sessionID string) error {
	s, err := m.Load(sessionID)
	if err != nil {
		return err
	}
	if s == nil {
		return nil // session doesn't exist yet — will be created on first mutating command
	}
	s.LastActive = time.Now()
	return m.Save(s)
}

// AddClaim records that a session has claimed an item.
func (m *Manager) AddClaim(sessionID, itemID string) error {
	s, err := m.Load(sessionID)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	for _, id := range s.ClaimedItems {
		if id == itemID {
			return nil // already claimed
		}
	}
	s.ClaimedItems = append(s.ClaimedItems, itemID)
	s.LastActive = time.Now()
	return m.Save(s)
}

// RemoveClaim removes a claim from a session.
func (m *Manager) RemoveClaim(sessionID, itemID string) error {
	s, err := m.Load(sessionID)
	if err != nil {
		return err
	}
	if s == nil {
		return nil // session gone, nothing to remove
	}

	var filtered []string
	for _, id := range s.ClaimedItems {
		if id != itemID {
			filtered = append(filtered, id)
		}
	}
	s.ClaimedItems = filtered
	s.LastActive = time.Now()
	return m.Save(s)
}

// SetPairing writes the pairing marker onto a session, activating the I-1700
// `/pair` live-iteration mode for the given item/worktree (`st pair`).
func (m *Manager) SetPairing(sessionID string, p *Pairing) error {
	s, err := m.Load(sessionID)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	s.Pairing = p
	s.LastActive = time.Now()
	return m.Save(s)
}

// ClearPairing removes the pairing marker from a session (`st pair --off`).
// A missing session, a session with no marker, or an already-inactive
// marker is a no-op — mirrors RemoveClaim's tolerant-of-absence behavior.
// The returned bool distinguishes "an active pairing was actually cleared"
// from "there was nothing to clear", so callers can report the true
// outcome instead of a blanket success message either way.
func (m *Manager) ClearPairing(sessionID string) (bool, error) {
	s, err := m.Load(sessionID)
	if err != nil {
		return false, err
	}
	if s == nil || s.Pairing == nil || !s.Pairing.Active {
		return false, nil
	}
	s.Pairing = nil
	s.LastActive = time.Now()
	if err := m.Save(s); err != nil {
		return false, err
	}
	return true, nil
}

// IsStale returns true if the session's last_active is older than the configured TTL.
func (m *Manager) IsStale(s *Session) bool {
	if m.ttl <= 0 {
		return false
	}
	return time.Since(s.LastActive) > m.ttl
}

// ListSessions returns all session files in the directory.
func (m *Manager) ListSessions() ([]*Session, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []*Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".yaml")
		s, err := m.Load(id)
		if err != nil {
			continue
		}
		if s != nil {
			sessions = append(sessions, s)
		}
	}
	return sessions, nil
}

// StaleSessions returns sessions that are past the TTL.
func (m *Manager) StaleSessions() ([]*Session, error) {
	all, err := m.ListSessions()
	if err != nil {
		return nil, err
	}
	var stale []*Session
	for _, s := range all {
		if m.IsStale(s) {
			stale = append(stale, s)
		}
	}
	return stale, nil
}

// PruneStaleSessions removes session files that are stale (last_active older than TTL)
// and have no claimed items. Returns the number of sessions pruned.
func (m *Manager) PruneStaleSessions() (int, error) {
	all, err := m.ListSessions()
	if err != nil {
		return 0, err
	}

	pruned := 0
	for _, s := range all {
		if !m.IsStale(s) {
			continue
		}
		if len(s.ClaimedItems) > 0 {
			continue // still has claims — needs recovery first
		}
		if s.Pairing != nil && s.Pairing.Active && time.Since(s.Pairing.ActivatedAt) < pairingAbandonedAfter {
			continue // I-1704: still paired and recently active — /pair --off releases it, not a TTL sweep
		}
		// A pairing marker that outlived pairingAbandonedAfter belongs to a
		// crashed/killed session that never ran `st pair --off` — fall
		// through and prune it like any other stale session rather than
		// retaining a ghost marker forever.
		path := m.path(s.ID)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			continue
		}
		pruned++
	}
	return pruned, nil
}

// DeleteSession removes a session file.
func (m *Manager) DeleteSession(sessionID string) error {
	return os.Remove(m.path(sessionID))
}

func (m *Manager) path(sessionID string) string {
	return filepath.Join(m.dir, sessionID+".yaml")
}

func parseTime(s string) time.Time {
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05Z",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
