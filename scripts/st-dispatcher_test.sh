#!/bin/bash
# Tests for st-dispatcher.sh — verify per-agent binary selection (I-404).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DISPATCHER="$SCRIPT_DIR/st-dispatcher.sh"

FAIL=0
fail() { echo "FAIL: $*" >&2; FAIL=1; }
pass() { echo "PASS: $*"; }

TMP=$(mktemp -d)
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

# --- Build a fake filesystem mimicking the production layout ---
mkdir -p "$TMP/theraprac-agents/theraprac-agent-a/as/bin"
mkdir -p "$TMP/theraprac-agents/theraprac-agent-b/as/bin"
mkdir -p "$TMP/legacy-as/bin"

# Each "binary" is a marker script that prints its path
for path in \
  "$TMP/theraprac-agents/theraprac-agent-a/as/bin/st" \
  "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st" \
  "$TMP/legacy-as/bin/st"; do
  printf '#!/bin/bash\necho "%s"\n' "$path" > "$path"
  chmod +x "$path"
done

# Patch the dispatcher to use $TMP instead of the hard-coded /Users/jfinlinson/Dev/as
PATCHED="$TMP/dispatcher.sh"
sed "s|/Users/jfinlinson/Dev/as/bin/st|$TMP/legacy-as/bin/st|g" "$DISPATCHER" > "$PATCHED"
chmod +x "$PATCHED"

# --- Case 1: PWD inside agent-b → uses agent-b's binary ---
out=$(cd "$TMP/theraprac-agents/theraprac-agent-b" && env -i HOME="$HOME" PATH="$PATH" bash "$PATCHED" 2>&1)
if [ "$out" = "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st" ]; then
  pass "PWD agent-b → agent-b binary"
else
  fail "PWD agent-b → got '$out'"
fi

# --- Case 2: PWD inside agent-a → uses agent-a's binary ---
out=$(cd "$TMP/theraprac-agents/theraprac-agent-a" && env -i HOME="$HOME" PATH="$PATH" bash "$PATCHED" 2>&1)
if [ "$out" = "$TMP/theraprac-agents/theraprac-agent-a/as/bin/st" ]; then
  pass "PWD agent-a → agent-a binary"
else
  fail "PWD agent-a → got '$out'"
fi

# --- Case 3: PWD deep inside agent-b → still resolves to agent-b ---
mkdir -p "$TMP/theraprac-agents/theraprac-agent-b/some/nested/dir"
out=$(cd "$TMP/theraprac-agents/theraprac-agent-b/some/nested/dir" && env -i HOME="$HOME" PATH="$PATH" bash "$PATCHED" 2>&1)
if [ "$out" = "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st" ]; then
  pass "PWD deep inside agent-b → agent-b binary"
else
  fail "PWD deep agent-b → got '$out'"
fi

# --- Case 4: PWD outside any agent → legacy fallback ---
out=$(cd "$TMP" && env -i HOME="$HOME" PATH="$PATH" bash "$PATCHED" 2>&1)
if [ "$out" = "$TMP/legacy-as/bin/st" ]; then
  pass "PWD outside agents → legacy fallback"
else
  fail "PWD outside agents → got '$out'"
fi

# --- Case 5: CLAUDE_PROJECT_DIR explicitly set, agent-b → agent-b binary ---
# PWD is /tmp (outside any worktree), so step 1 (worktree preference) finds
# no match and step 2 (env var) takes over. Case 12 covers the inverse:
# when PWD IS inside a worktree, the worktree binary beats the env var.
out=$(cd /tmp && env -i HOME="$HOME" PATH="$PATH" CLAUDE_PROJECT_DIR="$TMP/theraprac-agents/theraprac-agent-b" bash "$PATCHED" 2>&1)
if [ "$out" = "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st" ]; then
  pass "CLAUDE_PROJECT_DIR=agent-b, PWD outside worktree → env var binary"
else
  fail "CLAUDE_PROJECT_DIR agent-b → got '$out'"
fi

# --- Case 6: CLAUDE_PROJECT_DIR set but binary missing → walk-up still works ---
rm "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st"
out=$(cd "$TMP/theraprac-agents/theraprac-agent-a" && env -i HOME="$HOME" PATH="$PATH" CLAUDE_PROJECT_DIR="$TMP/theraprac-agents/theraprac-agent-b" bash "$PATCHED" 2>&1)
if [ "$out" = "$TMP/theraprac-agents/theraprac-agent-a/as/bin/st" ]; then
  pass "CLAUDE_PROJECT_DIR points at unbuilt clone → walk-up agent-a wins"
else
  fail "missing CLAUDE_PROJECT_DIR binary → got '$out'"
fi
# Restore for case 7
printf '#!/bin/bash\necho "%s"\n' "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st" > "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st"
chmod +x "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st"

# --- Case 7: agent dir exists but no clone built → legacy fallback ---
rm -rf "$TMP/theraprac-agents/theraprac-agent-a/as"
out=$(cd "$TMP/theraprac-agents/theraprac-agent-a" && env -i HOME="$HOME" PATH="$PATH" bash "$PATCHED" 2>&1)
if [ "$out" = "$TMP/legacy-as/bin/st" ]; then
  pass "agent-a unbuilt → legacy fallback"
else
  fail "agent-a unbuilt → got '$out'"
fi

# --- Case 8: ST_ROOT pinned to agent's workspace on PWD walk-up (I-418) ---
# Replace agent-b's binary with one that echoes the ST_ROOT it received,
# so we can verify the dispatcher exported the right value before exec.
cat > "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st" <<'EOF'
#!/bin/bash
echo "$ST_ROOT"
EOF
chmod +x "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st"
out=$(cd "$TMP/theraprac-agents/theraprac-agent-b" && env -i HOME="$HOME" PATH="$PATH" bash "$PATCHED" 2>&1)
# macOS resolves $TMP under /var to /private/var via $(pwd) in the dispatcher,
# so canonicalize the expected path through the same cd/pwd to compare.
expected=$(cd "$TMP/theraprac-agents/theraprac-agent-b" && pwd -P)/theraprac-workspace
if [ "$out" = "$expected" ]; then
  pass "PWD agent-b → ST_ROOT pinned to agent-b/theraprac-workspace"
else
  fail "ST_ROOT pinning → got '$out', want '$expected'"
fi

# --- Case 9: install-dispatcher.sh produces a working ~/bin/st (I-419) ---
INSTALLER="$SCRIPT_DIR/install-dispatcher.sh"
if [ ! -x "$INSTALLER" ]; then
  fail "installer $INSTALLER not executable"
else
  TARGET="$TMP/installed-st"
  WRAPPER_PATH="$TARGET" bash "$INSTALLER" >/dev/null 2>&1
  if [ -x "$TARGET" ]; then
    pass "install-dispatcher.sh wrote $TARGET"
  else
    fail "install-dispatcher.sh did not produce executable $TARGET"
  fi
  # Idempotent: second run must succeed (overwrites cleanly)
  if WRAPPER_PATH="$TARGET" bash "$INSTALLER" >/dev/null 2>&1; then
    pass "install-dispatcher.sh idempotent on re-run"
  else
    fail "install-dispatcher.sh failed on second run"
  fi
fi

# --- Case 10: PWD inside agent-b's worktree → uses worktree's binary (I-428) ---
# Build a worktree layout: <agent>/worktrees/I-428/as/bin/st. The marker
# bin prints "<path>|<ST_ROOT>" so we can verify both the binary chosen
# and the ST_ROOT pinned by the dispatcher.
mkdir -p "$TMP/theraprac-agents/theraprac-agent-b/worktrees/I-428/as/bin"
WT_BIN="$TMP/theraprac-agents/theraprac-agent-b/worktrees/I-428/as/bin/st"
cat > "$WT_BIN" <<EOF
#!/bin/bash
printf '%s|%s\n' "$WT_BIN" "\$ST_ROOT"
EOF
chmod +x "$WT_BIN"
# pwd -P inside the dispatcher canonicalizes /var/... to /private/var/... on
# macOS, so the ST_ROOT exported reflects that. The literal $WT_BIN string
# we wrote above is not canonicalized, so compare its un-canonicalized form
# on the left and the canonicalized agent path on the right.
expected_root=$(cd "$TMP/theraprac-agents/theraprac-agent-b" && pwd -P)/theraprac-workspace
out=$(cd "$TMP/theraprac-agents/theraprac-agent-b/worktrees/I-428" && env -i HOME="$HOME" PATH="$PATH" bash "$PATCHED" 2>&1)
if [ "$out" = "$WT_BIN|$expected_root" ]; then
  pass "PWD agent-b worktree → worktree binary, ST_ROOT pinned"
else
  fail "PWD worktree → got '$out' (expected '$WT_BIN|$expected_root')"
fi
# Run from a nested subdir within the worktree (the common case during dev)
mkdir -p "$TMP/theraprac-agents/theraprac-agent-b/worktrees/I-428/as/internal/command"
out=$(cd "$TMP/theraprac-agents/theraprac-agent-b/worktrees/I-428/as/internal/command" && env -i HOME="$HOME" PATH="$PATH" bash "$PATCHED" 2>&1)
if [ "$out" = "$WT_BIN|$expected_root" ]; then
  pass "PWD nested in worktree → worktree binary"
else
  fail "PWD nested in worktree → got '$out'"
fi

# --- Case 11: PWD inside a worktree but no bin built → falls through to agent-root ---
# Case 8 rewrote agent-b's main binary to echo $ST_ROOT; re-stamp it to its
# original "echo path" form so this case has a deterministic output.
printf '#!/bin/bash\necho "%s"\n' "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st" > "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st"
chmod +x "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st"
mkdir -p "$TMP/theraprac-agents/theraprac-agent-b/worktrees/I-999"
out=$(cd "$TMP/theraprac-agents/theraprac-agent-b/worktrees/I-999" && env -i HOME="$HOME" PATH="$PATH" bash "$PATCHED" 2>&1)
if [ "$out" = "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st" ]; then
  pass "worktree without built bin → falls through to agent-root binary"
else
  fail "worktree no-bin fallthrough → got '$out'"
fi

# --- Case 12: CLAUDE_PROJECT_DIR=<agent> AND PWD inside worktree → worktree wins ---
# This is the production scenario: the agent's hook sets CLAUDE_PROJECT_DIR
# to the agent root, but the agent is iterating inside a worktree. The
# worktree binary must beat the env var, otherwise every `make install`
# from the worktree is a no-op for the running session.
out=$(cd "$TMP/theraprac-agents/theraprac-agent-b/worktrees/I-428" \
  && env -i HOME="$HOME" PATH="$PATH" CLAUDE_PROJECT_DIR="$TMP/theraprac-agents/theraprac-agent-b" bash "$PATCHED" 2>&1)
if [ "$out" = "$WT_BIN|$expected_root" ]; then
  pass "CLAUDE_PROJECT_DIR=agent + PWD=worktree → worktree binary wins"
else
  fail "env+worktree precedence → got '$out'"
fi

echo
if [ $FAIL -ne 0 ]; then
  echo "SOME TESTS FAILED"
  exit 1
fi
echo "ALL TESTS PASSED"
exit 0
