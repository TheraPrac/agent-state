
## Review skip list (`review_skips`)

The code_review pipeline step must address every finding reported by `/code-review` and every Cursor Bugbot comment. To declare a specific finding as a false positive or an intentional non-fix, the operator pre-records it in the item's `review_skips` field (list of maps). Each entry must include the finding description and the operator's reason. Anything not on this list must be fixed — the agent never decides to skip on its own.

Example (added via `st update <id> review_skips`):

```yaml
review_skips:
- finding: "phase8-artifacts bucket policy may overwrite existing external policy"
  reason: "No other Terraform module manages this bucket's policy — access is via IAM only (operator-confirmed 2026-04-05)"
  operator: jfinlinson
```

## Agent-State Guard Hook (ACTIVE)

`hooks/agent-state-guard.sh` blocks direct Edit / Write to agent-state item files and redirects to `st` CLI commands. It allows edits to `index.md` and `templates/` — only task/issue/archive files are protected.
