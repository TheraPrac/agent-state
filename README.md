# agent-state (binary: `st`)

Go CLI for tracking tasks, issues, and delivery workflow in a local `agent-state/` directory.

## Naming

The project was originally called **agent-state** (abbreviated `as`). The user-facing command was later renamed to **`st`**, but the rename was intentionally only applied to the binary and CLI surface. Everything else kept its original name.

| Layer | Name |
|---|---|
| Installed binary | `st` |
| CLI verb | `st <cmd>` |
| Repo directory | `/Users/jfinlinson/Dev/as/` |
| GitHub repo | `github.com/JoeFinlinson/agent-state` |
| Go module | `github.com/jfinlinson/agent-state` |
| Inner package | `cmd/as/` |

A full rename (repo + module path + package tree + GitHub repo) was considered and declined — not worth the churn across every import. If you see `as` in a path and `st` on the command line, that is expected, not a bug.

## Build & install

```bash
make install   # builds cmd/as → /usr/local/bin/st (follows symlink to final target)
```

Never `go install` — use `make install`. See `Makefile` for details.

## Where it's used

The installed `st` binary is invoked from the TheraPrac workspace at `/Users/jfinlinson/Dev/theraprac-agents/theraprac-agent-a/theraprac-workspace/`. That repo's `bin/st` is the symlink target that `make install` writes to.

## Tests

```bash
go test ./...                                    # unit tests, fast
go test -tags multiagent ./internal/command/...  # multi-agent integration harness (T-328)
```

The `multiagent` build tag is opt-in. The harness compiles `st` once and races two subprocess agents against a tempdir workspace, asserting cross-process invariants (atomic claims, mail atomicity, stale-PID sweep). Unit-test runs skip the file by default so iteration stays fast.
