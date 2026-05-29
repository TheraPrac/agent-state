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

# nag_if_stale <clone>: emit a one-line stderr nag when the clone is behind its
# upstream. Throttled via a per-clone cache file (default 30-minute interval).
# Never blocks the exec — all failures degrade silently. I-721.
nag_if_stale() {
  { _nag_impl "$1"; } || true
}

_nag_impl() {
  local clone="$1"

  # Skip if not a git repo.
  git -C "$clone" rev-parse --git-dir >/dev/null 2>&1 || return 0

  # Skip if not on main/master — worktree/feature branches are expected to drift.
  local branch
  branch=$(git -C "$clone" rev-parse --abbrev-ref HEAD 2>/dev/null) || return 0
  case "$branch" in
    main|master) ;;
    *) return 0 ;;
  esac

  # Skip if no upstream tracked (orphan repo or detached HEAD).
  git -C "$clone" rev-parse --abbrev-ref --symbolic-full-name '@{u}' >/dev/null 2>&1 || return 0

  # Throttle: one cache file per clone, keyed by path.
  local cache_dir="${THERAPRAC_ST_CACHE_DIR:-$HOME/.theraprac/st-update-cache}"
  local cache_key
  cache_key="$(basename "$clone" | tr -cd 'a-zA-Z0-9_-' | head -c 20)-$(printf '%s' "$clone" | cksum | awk '{print $1}')"
  local cache_file="$cache_dir/$cache_key"
  local interval="${THERAPRAC_ST_AUTO_UPDATE_INTERVAL:-30}"

  # Cache hit: skip when file was touched within $interval minutes.
  if [ -f "$cache_file" ] && [ -n "$(find "$cache_file" -mmin "-$interval" 2>/dev/null)" ]; then
    return 0
  fi

  # Fetch: time-limited to avoid blocking the invocation.
  if command -v timeout >/dev/null 2>&1; then
    timeout 2 git -C "$clone" fetch --quiet --no-tags 2>/dev/null || true
  else
    git -C "$clone" fetch --quiet --no-tags 2>/dev/null || true
  fi

  # Touch cache file regardless of outcome so failing checks don't repeat-spam.
  mkdir -p "$cache_dir" && touch "$cache_file"

  # Report if behind.
  local behind
  behind=$(git -C "$clone" rev-list --count HEAD..@{u} 2>/dev/null) || return 0
  if [ "$behind" -gt 0 ]; then
    local upstream
    upstream=$(git -C "$clone" rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null) || upstream="origin/$branch"
    printf 'st: %s is %s commit(s) behind %s — run `cd %s && make install`\n' \
      "$(basename "$clone")" "$behind" "$upstream" "$clone" >&2
  fi
}

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
          nag_if_stale "$dir/as"
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
  nag_if_stale "$CLAUDE_PROJECT_DIR/as"
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
          nag_if_stale "$dir/as"
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
  nag_if_stale "${LEGACY%/bin/st}"
  exec "$LEGACY" "$@"
fi

echo "st-dispatcher: no usable binary found." >&2
echo "  Walked up from: $(pwd)" >&2
echo "  Tried legacy: $LEGACY" >&2
echo "  Fix: cd into your agent's as clone and run 'make'" >&2
exit 127
