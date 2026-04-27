#!/bin/bash
# install-dispatcher.sh — install the per-agent st-dispatcher to PATH.
#
# Run once per machine. Subsequent `make install` from any agent's clone
# updates that agent's binary; this script only manages the dispatcher
# at $WRAPPER_PATH (default ~/bin/st) that selects which per-agent
# binary to run.
#
# Idempotent: re-running this script overwrites the dispatcher with the
# current repo version, picking up any new resolution rules (e.g.,
# ST_ROOT pinning from I-418, or future env-var exports).
#
# Override the install target:
#   WRAPPER_PATH=/usr/local/bin/st bash as/scripts/install-dispatcher.sh
#
# Filed: I-419 — was an inline fix on one machine until shipped here.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DISPATCHER="$SCRIPT_DIR/st-dispatcher.sh"
WRAPPER_PATH="${WRAPPER_PATH:-$HOME/bin/st}"

if [ ! -f "$DISPATCHER" ]; then
  echo "install-dispatcher: source $DISPATCHER not found" >&2
  exit 2
fi

mkdir -p "$(dirname "$WRAPPER_PATH")"
install -m 755 "$DISPATCHER" "$WRAPPER_PATH"

echo "Installed dispatcher: $WRAPPER_PATH"
echo "  source: $DISPATCHER"
echo "  resolves 'st' per-agent via \$CLAUDE_PROJECT_DIR or PWD walk-up"
echo "  pins ST_ROOT to <agent>/theraprac-workspace when walk-up matches"
