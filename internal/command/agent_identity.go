package command

import (
	"fmt"
	"io"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
)

// AgentIdentityShow prints the resolved agent identity to stdout. Used by
// `st agent identity show` to answer "who am I to st, and where did that come
// from?" — diagnostics for the resolution chain in config.Identity().
func AgentIdentityShow(cfg *config.Config) int {
	return agentIdentityShowTo(cfg, os.Stdout)
}

func agentIdentityShowTo(cfg *config.Config, w io.Writer) int {
	id := cfg.Identity()

	if id.ID == "" {
		fmt.Fprintln(w, "id:             (unset)")
		fmt.Fprintln(w, "# Set via $AS_AGENT_ID, .as/local-agent.yaml (id: <name>),")
		fmt.Fprintln(w, "# or place this workspace under a directory named theraprac-agent-<suffix>.")
	} else {
		fmt.Fprintf(w, "id:             %s\n", id.ID)
	}
	if id.DisplayName != "" {
		fmt.Fprintf(w, "display_name:   %s\n", id.DisplayName)
	}
	if id.Source != "" {
		fmt.Fprintf(w, "source:         %s\n", id.Source)
	}
	if id.WorkspacePath != "" {
		fmt.Fprintf(w, "workspace_path: %s\n", id.WorkspacePath)
	}
	if id.ParentID != "" {
		fmt.Fprintf(w, "parent_id:      %s\n", id.ParentID)
	}
	if id.RootID != "" && id.RootID != id.ID {
		fmt.Fprintf(w, "root_id:        %s\n", id.RootID)
	}
	if id.SpawnedBySession != "" {
		fmt.Fprintf(w, "spawned_by:     %s\n", id.SpawnedBySession)
	}
	if id.DelegatedItemID != "" {
		fmt.Fprintf(w, "delegated_item: %s\n", id.DelegatedItemID)
	}
	if id.Role != "" {
		fmt.Fprintf(w, "role:           %s\n", id.Role)
	}
	return 0
}
