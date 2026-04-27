.PHONY: build test clean install install-wrapper

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
