.PHONY: build test clean install install-wrapper reconcile-verify docs

# WRAPPER_PATH is where the per-agent dispatcher lands. The default
# (~/bin/st) is in PATH ahead of /usr/local/bin on the developer machine,
# so installing to it overrides the legacy /usr/local/bin/st without
# needing sudo. Override on the command line if your setup differs:
#   make install-wrapper WRAPPER_PATH=/usr/local/bin/st
WRAPPER_PATH ?= $(HOME)/bin/st

# Build identity, injected via -ldflags so each agent's binary knows
# which commit it was built from. Surfaced by `st version` and recorded
# in agent registration so `st status` can flag binary drift between
# parallel agents.
GIT_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
GIT_DIRTY  := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo 1 || echo 0)
BUILT_AT   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X 'github.com/jfinlinson/agent-state/internal/buildinfo.Commit=$(GIT_COMMIT)' \
              -X 'github.com/jfinlinson/agent-state/internal/buildinfo.Dirty=$(GIT_DIRTY)' \
              -X 'github.com/jfinlinson/agent-state/internal/buildinfo.Built=$(BUILT_AT)'

build:
	go build -ldflags "$(LDFLAGS)" -o bin/st ./cmd/as

test:
	go test ./... -cover

# I-569 step 10 CI invariant: any item touched in the last 7 days whose
# recorded real_tokens exceeds 1.5x the JSONL ground truth fails the
# build. Run against the live workspace by default; CI sets --root to
# its checkout. Skipped silently when the workspace doesn't exist (e.g.
# fresh clone with no .claude/projects yet).
reconcile-verify:
	@if [ -d "$${ST_WORKSPACE_ROOT:-../theraprac-workspace}" ]; then \
	  go run ./cmd/reconcile-tokens verify --since 7d --root "$${ST_WORKSPACE_ROOT:-../theraprac-workspace}" || exit $$?; \
	else \
	  echo "reconcile-verify: skipped (no workspace at $${ST_WORKSPACE_ROOT:-../theraprac-workspace})"; \
	fi

clean:
	rm -f bin/st

# install builds bin/st in THIS clone — never touches another agent's binary.
# The dispatcher at $(WRAPPER_PATH) (installed once via install-wrapper) picks
# the right per-agent binary based on $$CLAUDE_PROJECT_DIR at runtime.
install: build
	@xattr -cr bin/st 2>/dev/null || true
	@echo "Installed $(CURDIR)/bin/st"
	@if [ ! -x "$(WRAPPER_PATH)" ]; then \
	  echo ""; \
	  echo "Note: dispatcher not found at $(WRAPPER_PATH)."; \
	  echo "Run 'make install-wrapper' once so 'st' resolves per-agent."; \
	fi

# install-wrapper drops the per-agent dispatcher onto PATH. Run once per
# machine; subsequent `make install` from any agent's clone is enough to
# update that agent's binary. Delegates to scripts/install-dispatcher.sh
# so the same install logic is reachable without make (e.g. from a
# bootstrap script on a fresh machine).
install-wrapper:
	@WRAPPER_PATH="$(WRAPPER_PATH)" bash scripts/install-dispatcher.sh

# I-738: regenerate theraprac-workspace/docs/st-cli-reference.md from
# the live cobra tree. Skips silently if the workspace docs dir isn't
# resolvable from this clone — matches the reconcile-verify pattern so
# non-canonical clone layouts get a useful message instead of a stray
# file. Override with ST_WORKSPACE_ROOT for unusual setups.
docs:
	@WS="$${ST_WORKSPACE_ROOT:-../theraprac-workspace}"; \
	if [ ! -d "$$WS/docs" ]; then \
	  echo "docs: skipped (no workspace docs/ at $$WS — set ST_WORKSPACE_ROOT)"; \
	  exit 0; \
	fi; \
	go run ./cmd/as docgen --output "$$WS/docs/st-cli-reference.md"
