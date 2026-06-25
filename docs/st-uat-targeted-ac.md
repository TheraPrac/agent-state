# st uat — Targeted AC Evaluation

When an acceptance criterion runs a filtered test command (e.g. `-run TestFoo`) and the suite
exits non-zero, `st uat` can distinguish the targeted test passing from an unrelated test failing.

## How it works

If the `cmd:` AC exits non-zero **and** the command contains a recognized test-name filter flag,
`st uat` scans the command output for the specific test's per-test PASS/FAIL line:

- If the targeted test **PASSED** and no FAIL line matches it → AC is overridden to **pass**,
  with a warning naming the unrelated failure.
- If the targeted test **FAILED** → AC fails normally (no override).
- If no per-test line is found (non-verbose output) → no override; exit-code behavior applies.

## Recognized filter flags

| Runner | Flag(s) |
|---|---|
| Go | `-run TestName`, `-run=TestName`, `RUN=TestName` (make variable) |
| Jest | `-t "name"`, `--testNamePattern "name"` |
| Vitest | `--grep "name"` |
| Playwright | `--grep "name"`, `-g "name"` |
| Pytest | `-k "expr"` |

## Verbose output required

Per-test lines (`--- PASS: TestFoo`) are only emitted with verbose output. For Go:

```
# AC that benefits from the override
cmd: go test ./internal/db/ -run TestDetermineCascadeAction -v -count=1

# Make target equivalent (passes -v via the Makefile)
cmd: make test-unit RUN=TestDetermineCascadeAction
```

Without `-v`, the override does not fire and exit code governs.

## Example output

```
✓ cmd: go test ./internal/db/ -run TestDetermineCascadeAction -v -count=1
     targeted test "TestDetermineCascadeAction" PASSED — suite exited non-zero due to an unrelated failure (use -v / --verbose to surface it)
```
