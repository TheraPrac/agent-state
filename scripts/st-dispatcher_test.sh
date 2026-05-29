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

# --- Freshness / nag_if_stale tests ---
# Each test creates a minimal git repo to drive nag_if_stale. All tests pin
# THERAPRAC_ST_CACHE_DIR to a temp sub-dir and use THERAPRAC_ST_AUTO_UPDATE_INTERVAL
# to force/suppress the cache check without touching the real filesystem.

CACHE="$TMP/nag-cache"
mkdir -p "$CACHE"

# Helper: create a local clone with a controllable remote.
# $1 = clone path, $2 = remote path (bare), $3 = branch (default: main)
make_git_pair() {
  local clone="$1" remote="$2" branch="${3:-main}"
  git init --bare "$remote" -q
  git clone "$remote" "$clone" -q 2>/dev/null
  git -C "$clone" checkout -b "$branch" -q 2>/dev/null || git -C "$clone" checkout "$branch" -q 2>/dev/null || true
  git -C "$clone" config user.email "test@test.com"
  git -C "$clone" config user.name "Test"
  # Seed with an initial commit so the branch exists on origin.
  git -C "$clone" commit --allow-empty -m "initial" -q
  git -C "$clone" push -u origin "$branch" -q 2>/dev/null
}

# --- Freshness case A: throttled by cache (recent cache file → no fetch fires) ---
CLONE_A="$TMP/nag-a/as"
REMOTE_A="$TMP/nag-a-remote.git"
make_git_pair "$CLONE_A" "$REMOTE_A" main
touch "$CACHE/fresh-marker"  # simulate a fresh cache file
# Patch dispatcher pointing legacy at CLONE_A so nag runs on step 4.
PATCHED_NAG="$TMP/dispatcher-nag.sh"
sed "s|/Users/jfinlinson/Dev/as/bin/st|$CLONE_A/bin/st|g" "$DISPATCHER" > "$PATCHED_NAG"
chmod +x "$PATCHED_NAG"
# Build a fake cache key matching what the dispatcher would compute.
_key_a="$(printf '%s' "$CLONE_A" | cksum | awk '{print $1}')"
_base_a="$(basename "$CLONE_A" | tr -cd 'a-zA-Z0-9_-' | head -c 20)"
cache_key_a="${_base_a}-${_key_a}"
# Touch the cache file so it's within 9999 minutes.
touch "$CACHE/$cache_key_a"
# THERAPRAC_ST_AUTO_UPDATE_INTERVAL=9999 → throttle active
mkdir -p "$CLONE_A/bin" && printf '#!/bin/bash\necho ok\n' > "$CLONE_A/bin/st" && chmod +x "$CLONE_A/bin/st"
nag_out=$(cd /tmp && env -i HOME="$HOME" PATH="$PATH" \
  THERAPRAC_ST_CACHE_DIR="$CACHE" THERAPRAC_ST_AUTO_UPDATE_INTERVAL=9999 \
  bash "$PATCHED_NAG" 2>&1)
if echo "$nag_out" | grep -q 'behind'; then
  fail "freshness check throttled by cache — unexpected nag: $nag_out"
else
  pass "freshness check throttled by cache"
fi

# --- Freshness case B: nag emitted when behind ---
CLONE_B="$TMP/nag-b/as"
REMOTE_B="$TMP/nag-b-remote.git"
make_git_pair "$CLONE_B" "$REMOTE_B" main
mkdir -p "$CLONE_B/bin" && printf '#!/bin/bash\necho ok\n' > "$CLONE_B/bin/st" && chmod +x "$CLONE_B/bin/st"
# Add a commit to the remote but not the local clone.
(cd "$TMP/nag-b-work" 2>/dev/null || git clone "$REMOTE_B" "$TMP/nag-b-work" -q 2>/dev/null; true)
git clone "$REMOTE_B" "$TMP/nag-b-work" -q 2>/dev/null || true
git -C "$TMP/nag-b-work" config user.email "test@test.com" 2>/dev/null || true
git -C "$TMP/nag-b-work" config user.name "Test" 2>/dev/null || true
git -C "$TMP/nag-b-work" commit --allow-empty -m "origin ahead" -q 2>/dev/null || true
git -C "$TMP/nag-b-work" push -q 2>/dev/null || true
PATCHED_NAG_B="$TMP/dispatcher-nag-b.sh"
sed "s|/Users/jfinlinson/Dev/as/bin/st|$CLONE_B/bin/st|g" "$DISPATCHER" > "$PATCHED_NAG_B"
chmod +x "$PATCHED_NAG_B"
_key_b="$(printf '%s' "$CLONE_B" | cksum | awk '{print $1}')"
_base_b="$(basename "$CLONE_B" | tr -cd 'a-zA-Z0-9_-' | head -c 20)"
cache_key_b="${_base_b}-${_key_b}"
rm -f "$CACHE/$cache_key_b"  # stale cache
nag_out_b=$(cd /tmp && env -i HOME="$HOME" PATH="$PATH" \
  THERAPRAC_ST_CACHE_DIR="$CACHE" THERAPRAC_ST_AUTO_UPDATE_INTERVAL=0 \
  bash "$PATCHED_NAG_B" 2>&1)
if echo "$nag_out_b" | grep -q 'behind origin/'; then
  pass "freshness nag emitted when behind"
else
  fail "freshness nag emitted when behind — got: $nag_out_b"
fi

# --- Freshness case C: skipped on feature branch ---
CLONE_C="$TMP/nag-c/as"
REMOTE_C="$TMP/nag-c-remote.git"
make_git_pair "$CLONE_C" "$REMOTE_C" main
mkdir -p "$CLONE_C/bin" && printf '#!/bin/bash\necho ok\n' > "$CLONE_C/bin/st" && chmod +x "$CLONE_C/bin/st"
git -C "$CLONE_C" checkout -b feat/i-721-test -q 2>/dev/null
PATCHED_NAG_C="$TMP/dispatcher-nag-c.sh"
sed "s|/Users/jfinlinson/Dev/as/bin/st|$CLONE_C/bin/st|g" "$DISPATCHER" > "$PATCHED_NAG_C"
chmod +x "$PATCHED_NAG_C"
_key_c="$(printf '%s' "$CLONE_C" | cksum | awk '{print $1}')"
_base_c="$(basename "$CLONE_C" | tr -cd 'a-zA-Z0-9_-' | head -c 20)"
cache_key_c="${_base_c}-${_key_c}"
rm -f "$CACHE/$cache_key_c"
nag_out_c=$(cd /tmp && env -i HOME="$HOME" PATH="$PATH" \
  THERAPRAC_ST_CACHE_DIR="$CACHE" THERAPRAC_ST_AUTO_UPDATE_INTERVAL=0 \
  bash "$PATCHED_NAG_C" 2>&1)
if echo "$nag_out_c" | grep -q 'behind'; then
  fail "freshness skipped on feature branch — unexpected nag: $nag_out_c"
else
  pass "freshness skipped on feature branch"
fi

# --- Freshness case D: skipped without upstream ---
CLONE_D="$TMP/nag-d/as"
git init "$CLONE_D" -q
git -C "$CLONE_D" config user.email "test@test.com"
git -C "$CLONE_D" config user.name "Test"
git -C "$CLONE_D" checkout -b main -q 2>/dev/null || true
git -C "$CLONE_D" commit --allow-empty -m "no upstream" -q
mkdir -p "$CLONE_D/bin" && printf '#!/bin/bash\necho ok\n' > "$CLONE_D/bin/st" && chmod +x "$CLONE_D/bin/st"
PATCHED_NAG_D="$TMP/dispatcher-nag-d.sh"
sed "s|/Users/jfinlinson/Dev/as/bin/st|$CLONE_D/bin/st|g" "$DISPATCHER" > "$PATCHED_NAG_D"
chmod +x "$PATCHED_NAG_D"
_key_d="$(printf '%s' "$CLONE_D" | cksum | awk '{print $1}')"
_base_d="$(basename "$CLONE_D" | tr -cd 'a-zA-Z0-9_-' | head -c 20)"
cache_key_d="${_base_d}-${_key_d}"
rm -f "$CACHE/$cache_key_d"
nag_out_d=$(cd /tmp && env -i HOME="$HOME" PATH="$PATH" \
  THERAPRAC_ST_CACHE_DIR="$CACHE" THERAPRAC_ST_AUTO_UPDATE_INTERVAL=0 \
  bash "$PATCHED_NAG_D" 2>&1)
if echo "$nag_out_d" | grep -q 'behind'; then
  fail "freshness skipped without upstream — unexpected nag: $nag_out_d"
else
  pass "freshness skipped without upstream"
fi

# --- Freshness case E: cache file touched after check ---
# Use clone B (up to date after its fetch) and verify cache file is updated.
# Delete the key_b cache file first, force a check, then verify it exists.
rm -f "$CACHE/$cache_key_b"
cd /tmp && env -i HOME="$HOME" PATH="$PATH" \
  THERAPRAC_ST_CACHE_DIR="$CACHE" THERAPRAC_ST_AUTO_UPDATE_INTERVAL=0 \
  bash "$PATCHED_NAG_B" >/dev/null 2>&1 || true
if [ -f "$CACHE/$cache_key_b" ]; then
  pass "cache file touched after check"
else
  fail "cache file touched after check — file missing: $CACHE/$cache_key_b"
fi

# --- Freshness case F: silent when up-to-date ---
# Sync clone B to match remote so it's not behind.
git -C "$CLONE_B" fetch -q 2>/dev/null && git -C "$CLONE_B" merge --ff-only -q 2>/dev/null || true
rm -f "$CACHE/$cache_key_b"  # force check
nag_out_f=$(cd /tmp && env -i HOME="$HOME" PATH="$PATH" \
  THERAPRAC_ST_CACHE_DIR="$CACHE" THERAPRAC_ST_AUTO_UPDATE_INTERVAL=0 \
  bash "$PATCHED_NAG_B" 2>&1)
if echo "$nag_out_f" | grep -q 'behind'; then
  fail "freshness silent when up-to-date — unexpected nag: $nag_out_f"
else
  pass "freshness silent when up-to-date"
fi

# --- Freshness case G: tolerates fetch failure ---
# Disconnect clone B from remote to force a fetch failure.
git -C "$CLONE_B" remote set-url origin "file:///nonexistent/path.git" 2>/dev/null
rm -f "$CACHE/$cache_key_b"
# Should complete (nag exec ok, no abort)
if (cd /tmp && env -i HOME="$HOME" PATH="$PATH" \
    THERAPRAC_ST_CACHE_DIR="$CACHE" THERAPRAC_ST_AUTO_UPDATE_INTERVAL=0 \
    bash "$PATCHED_NAG_B" >/dev/null 2>&1); then
  pass "freshness tolerates fetch failure"
else
  fail "freshness tolerates fetch failure — dispatcher exited non-zero"
fi

echo
if [ $FAIL -ne 0 ]; then
  echo "SOME TESTS FAILED"
  exit 1
fi
echo "ALL TESTS PASSED"
exit 0
