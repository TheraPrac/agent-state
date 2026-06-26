package command

import (
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/deps"
	"github.com/theraprac/agent-state/internal/registry"
	"github.com/theraprac/agent-state/internal/sprintinherit"
	"github.com/theraprac/agent-state/internal/store"
)

// resolveSprintInheritance applies the I-681 mid-sprint follow-up rule for
// `id`: if it has no sprint of its own but blocks a member of an active
// sprint, report the sprint to inherit (or the ambiguous set when it
// straddles more than one active sprint). Returns (nil, nil) — a clean
// no-op — when the item already has a sprint, the registry is unreadable,
// or there is nothing to inherit. Callers decide policy (auto-add on push,
// hard-gate on start).
func resolveSprintInheritance(s *store.Store, cfg *config.Config, id string) (*sprintinherit.Target, []string) {
	it, ok := s.Get(id)
	if !ok || it.Sprint != "" {
		return nil, nil
	}
	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		return nil, nil
	}
	g := deps.Build(s.All(), cfg)
	return sprintinherit.Resolve(id, s.All(), g, reg)
}
