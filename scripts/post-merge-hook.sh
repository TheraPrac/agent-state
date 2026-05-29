#!/bin/bash
# post-merge-hook.sh — auto-rebuild and deploy st binary after git merge.
#
# Installed as .git/hooks/post-merge via `make install-hook`. Fires automatically
# after every `git merge` (including `git pull --ff-only`) in this as clone.
#
# Design (I-439 contract: git pull must not block):
#   Phase 1 (synchronous): if WS_BIN does not yet exist (first-time setup),
#     deploy the most-recently built binary immediately so the workspace is
#     usable before Phase 2 finishes. In steady state WS_BIN already exists;
#     Phase 1 is skipped to avoid overwriting a potentially newer version
#     deployed by another agent's Phase 2.
#   Phase 2 (background): run `make install` to stamp a binary from the just-
#     merged commit, then deploy atomically. Agents starting after this completes
#     pick up the correctly-stamped fresh binary.
#
# Deploy path resolution:
#   Operates on the MAIN as clone (not a worktree) so `make install` runs in a
#   clean environment and `../theraprac-workspace` resolves correctly.
#   ST_WORKSPACE_ROOT overrides the workspace binary target path.

set -euo pipefail

# Resolve the as clone root.
#
# When invoked as a git hook, git sets CWD to the working tree root and
# GIT_DIR, so `git rev-parse --git-common-dir` resolves cleanly:
#   - Main clone:  returns relative ".git" → CWD is AS_ROOT
#   - Worktree:    returns absolute path to main .git → AS_ROOT is its parent
#
# When invoked directly (acceptance tests, CI), git may not be usable from CWD.
# Fall back to the script's own location: this file lives in <as-root>/scripts/,
# so dirname(dirname($0)) is the as root.  Only kicks in when git fails.
_resolve_as_root() {
  local cg
  cg="$(git rev-parse --git-common-dir 2>/dev/null || true)"
  if [ -n "$cg" ]; then
    if [ "${cg#/}" != "$cg" ]; then
      # Absolute path → worktree; main clone is the parent of the .git dir.
      printf '%s' "$(dirname "$cg")"
    else
      # Relative (".git") → main clone; return the top-level work-tree path.
      printf '%s' "$(git rev-parse --show-toplevel 2>/dev/null || true)"
    fi
  else
    # CWD is not inside a git repo (direct invocation).  Derive from $0.
    local sd
    sd="$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)"
    case "$sd" in
      */scripts) printf '%s' "$(dirname "$sd")" ;;
      *) : ;; # unknown context — return empty, caller will exit 0
    esac
  fi
}

AS_ROOT="$(_resolve_as_root)"
# If we can't determine where to run `make install`, exit cleanly rather than
# failing git pull with "hook post-merge failed".
if [ -z "$AS_ROOT" ] || [ ! -d "$AS_ROOT" ]; then exit 0; fi

# Workspace binary target. ST_WORKSPACE_ROOT overrides; default is
# ../theraprac-workspace relative to the agent root (one level up from AS_ROOT).
AGENT_ROOT="$(dirname "$AS_ROOT")"
WS_BIN="${ST_WORKSPACE_ROOT:-$AGENT_ROOT/theraprac-workspace}/bin/st"

# Phase 1: deploy an existing binary ONLY when WS_BIN does not exist yet.
# Skipping when WS_BIN is present prevents a brief downgrade in multi-agent
# setups where another agent's Phase 2 may have already deployed a newer binary
# to the shared WS_BIN path.  Uses PID-scoped temp to avoid racing with any
# concurrent invocation.
if [ ! -x "$WS_BIN" ] && [ -x "$AS_ROOT/bin/st" ] && [ -d "$(dirname "$WS_BIN")" ]; then
  _tmp="${WS_BIN}.$$"
  cp "$AS_ROOT/bin/st" "$_tmp" 2>/dev/null && \
  mv "$_tmp" "$WS_BIN" 2>/dev/null || rm -f "$_tmp" 2>/dev/null
fi

# Phase 2: background make install + re-deploy, stamped with the new commit.
# Keeps git pull responsive (I-439). PID-scoped temp avoids the race where two
# concurrent processes both `cp ... ${WS_BIN}.new && mv ...` and one process's
# mv atomically installs the other's partially-written file.
_tmp="${WS_BIN}.$$"
(
  cd "$AS_ROOT" && \
  make install >/dev/null 2>&1 && \
  [ -x "$AS_ROOT/bin/st" ] && \
  [ -d "$(dirname "$WS_BIN")" ] && \
  cp "$AS_ROOT/bin/st" "$_tmp" 2>/dev/null && \
  mv "$_tmp" "$WS_BIN" 2>/dev/null
  rm -f "$_tmp" 2>/dev/null
) </dev/null >/dev/null 2>&1 &
disown
