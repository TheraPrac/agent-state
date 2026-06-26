# Contributing to agent-state

## Development setup

**Requirements:** Go 1.24+, `make`

```bash
git clone https://github.com/TheraPrac/agent-state.git
cd agent-state
make install   # build and install bin/st → /usr/local/bin/st
```

## Running tests

```bash
go test ./...                                    # unit tests (fast, always run)
go test -tags multiagent ./internal/command/...  # multi-agent harness (slower)
```

Tests must be deterministic. Never re-run-until-green — fix flaky tests immediately.

## Making changes

- All behavior changes need tests.
- The config parser in `internal/config/config.go` is hand-rolled (no `yaml.Unmarshal`). New config keys need cases in `applyValue`, `applyListItem`, and/or `applyInlineList`.
- Keep `HardRedPatterns` in `internal/classify/denylist.go` generic. Project-specific deny patterns belong in `.as/config.yaml` under `classify.deny_path_prefixes`.

## Pull requests

- One logical change per PR.
- All tests must pass: `go test ./... && go vet ./...`
- Commit messages follow conventional commits (`feat:`, `fix:`, `chore:`, etc.).
