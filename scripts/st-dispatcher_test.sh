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
out=$(cd /tmp && env -i HOME="$HOME" PATH="$PATH" CLAUDE_PROJECT_DIR="$TMP/theraprac-agents/theraprac-agent-b" bash "$PATCHED" 2>&1)
if [ "$out" = "$TMP/theraprac-agents/theraprac-agent-b/as/bin/st" ]; then
  pass "CLAUDE_PROJECT_DIR=agent-b → agent-b binary (env var wins over PWD)"
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

echo
if [ $FAIL -ne 0 ]; then
  echo "SOME TESTS FAILED"
  exit 1
fi
echo "ALL TESTS PASSED"
exit 0
