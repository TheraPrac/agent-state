#!/bin/bash
# st-dispatcher: per-agent binary selection (I-404).
#
# Each agent has its own as clone at theraprac-agents/theraprac-agent-<x>/as
# with bin/st built from `make` in that clone. This dispatcher resolves to
# that binary so each agent's `st` command runs ITS own build — no cross-
# agent clobbering when multiple agents are working on st CLI code in
# parallel.
#
# Resolution order:
#   1. $CLAUDE_PROJECT_DIR/as/bin/st (when an agent's hook context propagates
#      the env var)
#   2. Walk up from $PWD looking for a theraprac-agents/theraprac-agent-<x>
#      ancestor; use that agent's as clone (covers Bash subshells where
#      CLAUDE_PROJECT_DIR is not exported). When this path matches, ST_ROOT
#      is pinned to that agent's workspace so the binary's cwd-walk fallback
#      doesn't cross-route to a sibling agent's clone (I-418).
#   3. Legacy /Users/jfinlinson/Dev/as/bin/st (operator-direct / fallback)

set -e

# 1. Explicit env var
if [ -n "$CLAUDE_PROJECT_DIR" ] && [ -x "$CLAUDE_PROJECT_DIR/as/bin/st" ]; then
  exec "$CLAUDE_PROJECT_DIR/as/bin/st" "$@"
fi

# 2. Walk up from PWD to find an agent root
dir=$(pwd)
while [ "$dir" != "/" ]; do
  parent=$(dirname "$dir")
  if [ "$(basename "$parent")" = "theraprac-agents" ]; then
    case "$(basename "$dir")" in
      theraprac-agent-*)
        candidate="$dir/as/bin/st"
        if [ -x "$candidate" ]; then
          # Pin ST_ROOT to this agent's workspace so the binary's cwd-walk
          # fallback in discover() doesn't cross-route to a sibling agent's
          # clone (I-418). Workspace is a symlink to the canonical clone,
          # but the per-agent path keeps path-derived identity correct.
          export ST_ROOT="$dir/theraprac-workspace"
          exec "$candidate" "$@"
        fi
        ;;
    esac
  fi
  dir="$parent"
done

# 3. Legacy fallback
LEGACY="/Users/jfinlinson/Dev/as/bin/st"
if [ -x "$LEGACY" ]; then
  exec "$LEGACY" "$@"
fi

echo "st-dispatcher: no usable binary found." >&2
echo "  Walked up from: $(pwd)" >&2
echo "  Tried legacy: $LEGACY" >&2
echo "  Fix: cd into your agent's as clone and run 'make'" >&2
exit 127
