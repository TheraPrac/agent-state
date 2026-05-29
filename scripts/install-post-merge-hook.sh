#!/bin/bash
# install-post-merge-hook.sh — install the post-merge git hook into this as clone.
#
# Idempotent: re-running overwrites with the current hook version.
# Uses --git-common-dir so it correctly targets the shared .git/hooks/ from
# both the main clone and any worktree (same pattern as install-dispatcher.sh).
#
# Run once per machine via `make install-hook` (or implicitly via `make install-wrapper`).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HOOK_SRC="$SCRIPT_DIR/post-merge-hook.sh"

if [ ! -f "$HOOK_SRC" ]; then
  echo "install-post-merge-hook: source $HOOK_SRC not found" >&2
  exit 2
fi

# Resolve the shared git hooks directory.  --git-common-dir is relative in the
# main clone (".git") and absolute in any worktree.  Both resolve correctly.
COMMON_GIT="$(git -C "$SCRIPT_DIR/.." rev-parse --git-common-dir 2>/dev/null || echo ".git")"
if [ "${COMMON_GIT#/}" != "$COMMON_GIT" ]; then
  # Absolute path from a worktree context.
  GIT_HOOKS_DIR="$COMMON_GIT/hooks"
else
  # Relative — resolve from the as repo root.
  AS_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
  GIT_HOOKS_DIR="$AS_ROOT/$COMMON_GIT/hooks"
fi

mkdir -p "$GIT_HOOKS_DIR"
install -m 755 "$HOOK_SRC" "$GIT_HOOKS_DIR/post-merge"

echo "Installed post-merge hook: $GIT_HOOKS_DIR/post-merge"
echo "  source: $HOOK_SRC"
echo "  fires: after git merge / git pull in this as clone"
