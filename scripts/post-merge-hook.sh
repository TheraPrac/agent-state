#!/bin/bash
# post-merge-hook.sh — auto-rebuild and deploy st binary after git merge.
#
# Installed as .git/hooks/post-merge via `make install-hook`. Fires automatically
# after every `git merge` (including `git pull --ff-only`) in this as clone.
#
# Design (I-439 contract: git pull must not block):
#   Phase 1 (synchronous): if WS_BIN does not yet exist on this machine (first-
#     time setup), deploy the most-recently built binary immediately so the
#     workspace is usable before Phase 2 finishes. In steady state WS_BIN
#     already exists; Phase 1 is skipped to avoid overwriting a potentially
#     newer version deployed by another agent's Phase 2.
#   Phase 2 (background): run `make install` to stamp a binary from the just-
#     merged commit, then deploy atomically. Agents starting after this completes
#     pick up the correctly-stamped fresh binary.
#
# Deploy path resolution:
#   Always operates on the MAIN as clone (not a worktree) so `make install`
#   runs in a clean environment and `../theraprac-workspace` resolves correctly.
#   ST_WORKSPACE_ROOT overrides the default for non-standard layouts.

set -euo pipefail

# Resolve the main as clone root from the shared git directory.
# --git-common-dir is relative (".git") in the main clone, absolute in worktrees.
# Guard: if git fails for any reason, exit 0 so we never abort git pull.
COMMON_GIT="$(git rev-parse --git-common-dir 2>/dev/null || true)"
if [ -z "$COMMON_GIT" ]; then exit 0; fi

if [ "${COMMON_GIT#/}" != "$COMMON_GIT" ]; then
  # Absolute path → we are in a worktree; the main clone is one dir up from .git.
  AS_ROOT="$(dirname "$COMMON_GIT")"
else
  # Relative (".git") → we are in the main clone.
  AS_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
fi
if [ -z "$AS_ROOT" ]; then exit 0; fi

# Workspace binary target. ST_WORKSPACE_ROOT overrides; default is
# ../theraprac-workspace relative to the agent root (one level up from AS_ROOT).
AGENT_ROOT="$(dirname "$AS_ROOT")"
WS_BIN="${ST_WORKSPACE_ROOT:-$AGENT_ROOT/theraprac-workspace}/bin/st"

# Phase 1: deploy an existing binary ONLY when WS_BIN does not exist yet (first-
# time setup). Skipping Phase 1 when WS_BIN is already present prevents a brief
# downgrade in multi-agent setups where another agent's Phase 2 may have already
# deployed a newer binary to the shared WS_BIN path.
# Use a PID-scoped temp name to avoid racing with concurrent invocations.
if [ ! -x "$WS_BIN" ] && [ -x "$AS_ROOT/bin/st" ] && [ -d "$(dirname "$WS_BIN")" ]; then
  _tmp="${WS_BIN}.$$"
  cp "$AS_ROOT/bin/st" "$_tmp" 2>/dev/null && \
  mv "$_tmp" "$WS_BIN" 2>/dev/null || rm -f "$_tmp" 2>/dev/null
fi

# Phase 2: background make install + re-deploy, stamped with the new commit.
# Keeps git pull responsive (I-439). Uses a PID-scoped temp name so concurrent
# invocations from multiple agents don't race on the same intermediate file.
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
