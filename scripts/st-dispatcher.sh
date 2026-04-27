#!/bin/bash
# st-dispatcher: per-agent binary selection (I-404, I-428).
#
# Each agent has its own as clone at theraprac-agents/theraprac-agent-<x>/as
# with bin/st built from `make` in that clone. Each item in flight may also
# have a per-item worktree at <agent>/worktrees/<id>/as with its own bin/st.
# This dispatcher resolves to the right binary so each agent's `st` command
# runs ITS own build — and, while iterating on st-CLI items, picks up the
# worktree's freshly-built binary instead of the stale agent-root one.
#
# Resolution order:
#   1. Worktree preference (I-428): when PWD is inside
#      <agent>/worktrees/<id>/, prefer <agent>/worktrees/<id>/as/bin/st if it
#      exists. Goes ahead of CLAUDE_PROJECT_DIR so the hook-exported env var
#      (which points at the agent root, not the worktree) doesn't pin every
#      iteration to the stale main-checkout binary.
#   2. $CLAUDE_PROJECT_DIR/as/bin/st (when an agent's hook context propagates
#      the env var)
#   3. Walk up from $PWD looking for a theraprac-agents/theraprac-agent-<x>
#      ancestor; use that agent's as clone (covers Bash subshells where
#      CLAUDE_PROJECT_DIR is not exported). When this path matches, ST_ROOT
#      is pinned to that agent's workspace so the binary's cwd-walk fallback
#      doesn't cross-route to a sibling agent's clone (I-418).
#   4. Legacy /Users/jfinlinson/Dev/as/bin/st (operator-direct / fallback)

set -e

# 1. Worktree preference: walk up looking for <agent>/worktrees/<id>/.
#    First match wins; if the worktree's bin/st exists, use it. If the
#    worktree exists but no binary has been built there yet, break out
#    silently and fall through to the agent-root binary below.
dir=$(pwd)
while [ "$dir" != "/" ]; do
  parent=$(dirname "$dir")
  if [ "$(basename "$parent")" = "worktrees" ]; then
    grandparent=$(dirname "$parent")
    case "$(basename "$grandparent")" in
      theraprac-agent-*)
        candidate="$dir/as/bin/st"
        if [ -x "$candidate" ]; then
          # Pin ST_ROOT to the agent's workspace (same reason as the
          # agent-root branch below).
          export ST_ROOT="$grandparent/theraprac-workspace"
          exec "$candidate" "$@"
        fi
        # Worktree detected, no binary built — break out of step 1 and
        # let the rest of the chain (CLAUDE_PROJECT_DIR, then agent-root
        # walk-up, then legacy) resolve normally. In production the hook
        # exports CLAUDE_PROJECT_DIR=<this agent>, so step 2 lands on the
        # agent's main bin/st.
        break
        ;;
    esac
  fi
  dir="$parent"
done

# 2. Explicit env var
if [ -n "$CLAUDE_PROJECT_DIR" ] && [ -x "$CLAUDE_PROJECT_DIR/as/bin/st" ]; then
  exec "$CLAUDE_PROJECT_DIR/as/bin/st" "$@"
fi

# 3. Walk up from PWD to find an agent root
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

# 4. Legacy fallback
LEGACY="/Users/jfinlinson/Dev/as/bin/st"
if [ -x "$LEGACY" ]; then
  exec "$LEGACY" "$@"
fi

echo "st-dispatcher: no usable binary found." >&2
echo "  Walked up from: $(pwd)" >&2
echo "  Tried legacy: $LEGACY" >&2
echo "  Fix: cd into your agent's as clone and run 'make'" >&2
exit 127
