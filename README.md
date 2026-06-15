# agent-state (binary: `st`)

Go CLI for tracking tasks, issues, and delivery workflow in a local `agent-state/` directory. Designed for agentic software development workflows where multiple AI agents share a single project state.

## Naming

The project was originally called **agent-state** (abbreviated `as`). The user-facing command was later renamed to **`st`**, but the rename was intentionally applied only to the binary and CLI surface.

| Layer | Name |
|---|---|
| Installed binary | `st` |
| CLI verb | `st <cmd>` |
| GitHub repo | `github.com/JoeFinlinson/agent-state` |
| Go module | `github.com/jfinlinson/agent-state` |
| Inner package | `cmd/as/` |

A full rename (repo + module path + package tree + GitHub repo) was considered and declined — not worth the churn across every import. If you see `as` in a path and `st` on the command line, that is expected, not a bug.

## Install

**From source:**

```bash
git clone https://github.com/JoeFinlinson/agent-state.git
cd agent-state
make install   # builds cmd/as → bin/st, then installs to /usr/local/bin/st
```

Never `go install` — use `make install`. See `Makefile` for details.

**Binary releases:** See [Releases](https://github.com/JoeFinlinson/agent-state/releases) for pre-built binaries (macOS arm64/amd64, Linux amd64/arm64).

## Quickstart

```bash
# Initialize a new project workspace
st init

# Create items
st create task "Add login page"
st create issue "Auth token expires too quickly"

# Work through the delivery loop
st next                        # top-ranked unblocked item
st start T-001 --slug add-login-page
# ... write code ...
st test T-001 unit --run       # run a test suite
st close T-001 done
```

## Item lifecycle

Items live in `agent-state/` and progress through a delivery pipeline:

```
queued → active (coding → committed → pushed → pr_open → merged → deployed_dev → uat_approved → deployed_prod → closed)
```

Items carry structured SBAR fields (Situation / Background / Assessment / Recommendation), dependency graphs, sprint assignments, and testing evidence records.

## Multi-agent

`st` is designed for teams of agents sharing a single `agent-state/` directory via git. Key coordination primitives:

- **Claiming**: `st start <id>` claims an item; `st next` filters out peer-claimed items.
- **Git sync**: mutating commands auto-commit and push via a file lock (`.st-git.lock`) to prevent conflicts.
- **Mail**: `st mail send <agent-id>` routes messages between agents asynchronously.
- **Coordinator**: `.as/coordinator.yaml` bounds the autonomous delivery loop — items that exceed the boundary surface to the operator.

The physical topology (how agents are laid out on disk, IAM credentials, GitHub tokens) is project-specific and configured per deployment.

## Configuration

Create `.as/config.yaml` in your project root. Key sections:

```yaml
project:
  name: my-project

paths:
  root: agent-state

testing:
  enabled: true
  required_suites:
    unit: go test ./...
    lint: golangci-lint run

classify:
  deny_path_prefixes:
    - infra/state/
    - internal/auth/
```

Run `st config show` to see the active resolved configuration.

## Autonomy classifier

`st classify <id>` runs a binary (green/red) classifier to decide whether an item's change set is safe to auto-ship. The classifier uses:

1. A hard-coded generic deny list (`HardRedPatterns`: IAM terraform, secrets files, private keys)
2. Project-specific deny prefixes from `classify.deny_path_prefixes` in `.as/config.yaml`
3. A Claude model for everything that doesn't match the deny list

## Tests

```bash
go test ./...                                    # unit tests, fast
go test -tags multiagent ./internal/command/...  # multi-agent integration harness
```

The `multiagent` build tag is opt-in. The harness compiles `st` once and races two subprocess agents against a tempdir workspace, asserting cross-process invariants (atomic claims, mail atomicity, stale-PID sweep).

## Build requirements

- Go 1.24+
- `make` (GNU or BSD)
