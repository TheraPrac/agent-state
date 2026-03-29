# Design: Sprint-Driven Multi-Agent Workflow

**Status:** Draft — pending user approval
**Date:** 2026-03-28
**Context:** Conversation between user and agent designing multi-agent support for `st` CLI

## Problem

`st` currently has one global queue and one global stack. In practice, the user runs multiple Claude Code sessions simultaneously — one on TheraPrac app work, another on `st` CLI development. The single queue/stack model doesn't represent this reality, and agents can't self-organize their work.

The user doesn't want to micromanage queue ordering. They want to define a scope of work (a sprint), approve a plan, and let the agent execute autonomously.

## Design Principles

1. **The sprint is the execution unit.** Not just a grouping — the sprint defines scope, execution order, and progress tracking for one or more agent sessions.
2. **Three human gates, everything else autonomous.** Sprint gate (scope), Plan gate (approach), UAT gate (results). Between gates, the agent runs without asking.
3. **Sessions are ephemeral, sprints are durable.** A session claims work and manages its interrupt stack. The sprint survives across sessions and tracks cumulative progress.
4. **Claims prevent contention.** Multiple sessions on one sprint, each claiming different items. `st start` is atomic — second claimer fails.
5. **Agents create items freely, design requires approval.** Discovered blockers become issues immediately. But if the plan needs to change (new scope, reordering, cross-sprint promotion), the agent enters plan mode.

## Model

### Epics Own Sprints, Sprints Are Parallel Batches

An epic is a long-lived work stream. It contains an **ordered sequence of sprints**. Each sprint is a batch of items that can be worked **concurrently** by N agents. Sprint N+1 starts when Sprint N completes (or when all of Sprint N+1's cross-sprint dependencies are satisfied).

```
Epic: st CLI Multi-Agent
  Sprint 1 (wave 1): [T-164, T-165]     ← parallelizable, no cross-deps
    Session abc-123: claimed T-164
    Session def-456: claimed T-165
  Sprint 2 (wave 2): [T-166, T-167]     ← depends on Sprint 1 completion
    (not started — waiting for Sprint 1)
  Sprint 3 (wave 3): [T-157]            ← depends on Sprint 2
```

Within a sprint, the dependency graph determines what can be truly parallel vs. what must be sequential. The sprint is the **unit of planning** — when the user says "work on this," they point to a sprint, and the agent(s) figure out the internal order from dependencies + priorities.

Between sprints, ordering comes from the epic's sprint sequence plus cross-sprint dependency checks. A sprint can start early if its items' dependencies are satisfied, even if the previous sprint isn't fully complete.

### Core Concepts

| Concept | Persistence | Identity | Purpose |
|---------|-------------|----------|---------|
| **Epic** | Permanent | Named (petname) | Long-lived work stream, owns ordered sprints |
| **Sprint** | Durable (across sessions) | Named (petname) | Parallel batch of items, unit of planning and execution |
| **Session** | Ephemeral (one Claude run) | UUID from Claude Code | Claims items, manages interrupt stack |
| **Item** | Permanent | T-XXX / I-XXX | Unit of work |
| **Claim** | Session-scoped | Session UUID on item | Prevents contention |

### Sprint Lifecycle

```
1. CREATE      User creates sprint, assigns to epic
2. POPULATE    Agent (or user) adds items: st sprint add <sprint> <ids...>
3. PLAN        Agent reads items + deps, proposes execution order → plan gate
4. APPROVE     User approves plan → sprint.plan_approved = true
5. EXECUTE     Agent(s) join sprint, claim items, work autonomously
6. RE-PLAN     When plan needs to change → agent enters plan mode again
7. UAT         Per-item UAT gate (same as today)
8. COMPLETE    All items done → sprint status = completed
```

### Session Lifecycle

```
1. GENERATE    Startup hook generates session UUID, exports AS_SESSION_ID
2. JOIN        st sprint join <sprint> — binds session to sprint
3. PRIME       st prime — sprint-scoped, returns next unclaimed unblocked item
4. CLAIM       st start <id> — sets claimed_by/claimed_at on item
5. WORK        Agent executes (code, test, pr, merge, etc.)
6. INTERRUPT   st push <blocker> — push current item, work blocker
7. RESUME      st pop — return to previous item after blocker resolved
8. RELEASE     st close <id> or session exit — clears claim
9. DEATH       Session crashes → stale claim, cleaned by next session or st sprint recover
```

### Three Gates (Revised)

**Gate 1 — Sprint Gate (scope)**
- User defines the sprint and its items (or approves agent-proposed sprint from epic)
- This replaces the per-item queue gate
- User controls WHAT gets worked on by controlling sprint contents

**Gate 2 — Plan Gate (approach)**
- Agent reads sprint items, dependency graph, proposes execution order + high-level approach
- User approves
- Re-entered when: new blocker discovered that changes plan, cross-sprint dependency promotion needed, scope change
- NOT re-entered for: routine blockers the agent can handle within existing plan

**Gate 3 — UAT Gate (results)**
- Per-item, same as today
- Agent runs acceptance criteria, presents evidence, user approves/rejects

## Data Model Changes

### Epic (in `.as/epics.yaml`)

```yaml
epics:
  - id: nicely-promoted-seahorse
    title: Sprint-Driven Multi-Agent Workflow
    status: active
    sprint_order:               # ordered sequence of sprints (wave 1, wave 2, ...)
      - reasonably-warm-joey    # Sprint 1: foundation
      - initially-normal-muskrat           # Sprint 2: execution
      - pleasantly-absolute-oryx           # Sprint 3: coordination + capstone
```

### Sprint (in `.as/epics.yaml`)

```yaml
sprints:
  - id: reasonably-warm-joey
    title: "Phase 1: Foundation"
    epic: nicely-promoted-seahorse
    status: active              # active | completed | paused
    sequence: 1                 # position in epic's sprint order
    items:                      # items in this batch (parallelizable)
      - T-164
      - T-165
    plan_approved: true
    plan_approved_at: 2026-03-28T10:00:00-06:00
    plan_approved_by: user      # or agent ID if delegated
```

The `items` list within a sprint is NOT a strict execution order — it's a set of items that form a parallelizable batch. The dependency graph determines internal ordering. The agent computes: "which items in this sprint have no unsatisfied deps? Those are claimable now."

### Item — New Fields

```yaml
# Added to item frontmatter
claimed_by: 550e8400-e29b-41d4-a716-446655440000    # session UUID, null when unclaimed
claimed_at: 2026-03-28T10:00:00-06:00                # when claimed
sprint: happy-jumping-elephant                        # already exists, now load-bearing
```

The `sessions` field (already exists as a list) continues to accumulate every session that touches the item.

### Stack — Already Per-Agent

Stack files already support per-agent isolation: `.as/stacks/{agent-id}.yaml` when `$AS_AGENT_ID` is set, `.as/stack.yaml` otherwise.

For the sprint model, we scope stacks by session instead of (or in addition to) agent: `.as/stacks/{session-id}.yaml`. This is cleaner because sessions are the actual execution unit.

### Config Changes

```yaml
# New section in .as/config.yaml
sprints:
  plan_required: true              # require plan approval before agents can start sprint work
  allow_cross_sprint_promotion: true  # agents can propose pulling deps from other sprints
  stale_claim_ttl: 7200            # seconds before a claim is considered stale (2 hours)

session:
  id_source: env                   # "env" = read $AS_SESSION_ID, "auto" = generate on first command
```

## Command Changes

### New Commands

#### `st sprint add <sprint> <item-ids...>`
Add items to a sprint's execution plan. Appends to the end of the plan order.
```bash
st sprint add happy-jumping-elephant T-151 T-163 T-157
```

#### `st sprint rm <sprint> <item-id>`
Remove item from sprint. Does not delete the item, just unassigns it.

#### `st sprint show <sprint>`
Enhanced display showing items with status, claims, blockers, and plan order.
```
Sprint: happy-jumping-elephant — Pipeline Commands
Epic:   eagerly-clean-cricket — Agent Tooling
Status: active   Plan: approved (2026-03-28)

  #  ID       Title                          Status      Claimed By
  1  T-151    merge + deploy-check + smoke   completed   —
  2  T-163    st uat — acceptance criteria    in-progress session:abc-123
  3  T-157    st advance + st run            blocked     —
                                              └─ blocked by: T-163

Progress: 1/3 complete, 1 in-progress, 1 blocked
Active sessions: 1
```

#### `st sprint join <sprint>`
Bind current session to a sprint. Writes session→sprint mapping to `.as/sessions/{session-id}.yaml`. All subsequent sprint-scoped commands (prime, queue next) filter to this sprint.
```bash
st sprint join happy-jumping-elephant
```

#### `st sprint leave`
Unbind current session from sprint. Releases any claims held by this session.

#### `st sprint recover <sprint>`
Release stale claims (older than `stale_claim_ttl`). Run at session startup or manually.
```bash
st sprint recover happy-jumping-elephant
# Released 1 stale claim: T-163 (claimed 3 hours ago by dead session abc-123)
```

#### `st sprint plan <sprint>`
Analyze the sprint's items, show dependency graph, identify what's parallelizable vs. sequential, and flag cross-sprint blockers.
```bash
st sprint plan reasonably-warm-joey
# Sprint: reasonably-warm-joey — Phase 1: Foundation
#
# Parallel group 1 (no deps):
#   T-164  Sprint promotion          ready
#   T-165  Session identity + claims  ready
#
# Cross-sprint deps: none
# ✓ All items parallelizable — 2 agents can work simultaneously
```

```bash
st sprint plan initially-normal-muskrat
# Sprint: initially-normal-muskrat — Phase 2: Execution
#
# Parallel group 1 (after Sprint 1):
#   T-166  Sprint-aware execution     blocked by T-164, T-165
#   T-167  Multi-session coordination blocked by T-165
#
# Cross-sprint deps:
#   T-166 → T-164 (Sprint 1), T-165 (Sprint 1)
#   T-167 → T-165 (Sprint 1)
# ⚠ Sprint blocked until Sprint 1 items complete
```

#### `st sprint status [sprint]`
Coordinator view — all sprints or one sprint, showing sessions, progress, blockers.
```
Active Sprints:
  happy-jumping-elephant  Pipeline Commands     1/3 done  1 session   no blockers
  another-sprint          Clinical Features     3/8 done  2 sessions  1 cross-sprint blocker
```

### Modified Commands

#### `st prime` — Sprint-Aware
If session is joined to a sprint (`AS_SESSION_ID` has a session file with sprint binding):
- Scopes output to sprint items only
- "Next Action" considers sprint plan order, not global queue
- Shows sprint progress, cross-sprint blockers
- Falls back to global view if no sprint joined

#### `st start <id>` — Claims
- Sets `claimed_by` and `claimed_at` on item
- Fails if already claimed by another session (unless stale)
- Adds session UUID to item's `sessions` list
- If item is not in current sprint, warns (but doesn't block — agent may have promoted it)

#### `st close <id>` / `st release <id>` — Clears Claims
- `st close` clears claim as part of closing
- `st release` clears claim without closing (for: session ending, giving up, re-assigning)

#### `st push <id>` / `st pop` — Sprint-Scoped Stack
- Stack file keyed by session ID: `.as/stacks/{session-id}.yaml`
- `st push` creates the blocker item automatically if it doesn't exist
- Stack operations unchanged semantically, just session-scoped

#### `st create` — Sprint Option
```bash
st create issue "Testcontainer leak" --sprint happy-jumping-elephant
```
Adds to sprint automatically. Without `--sprint`, item is unsprinted (backlog).

#### `st queue` — Becomes Backlog
The global queue becomes the backlog of unsprinted work. Sprint items are managed via sprint plan, not the queue. `st queue` still exists for items not assigned to any sprint.

Alternatively: `st queue` stays as-is for backward compatibility, and sprint-assigned items just don't appear in it. The queue is "stuff not yet in a sprint."

## Session Identity

### Generation
The Claude Code startup hook generates a session UUID and exports it:

```bash
# In .claude/hooks/session-start.sh (or settings.json hook)
if [ -z "$AS_SESSION_ID" ]; then
  export AS_SESSION_ID=$(uuidgen)
fi
```

If Claude Code provides `--session-id <uuid>` natively, use that instead.

### Storage
Session metadata lives in `.as/sessions/{session-id}.yaml`:
```yaml
id: 550e8400-e29b-41d4-a716-446655440000
started_at: 2026-03-28T10:00:00-06:00
agent_id: st-cli                              # optional, from $AS_AGENT_ID
sprint: happy-jumping-elephant                 # set by st sprint join
last_active: 2026-03-28T10:30:00-06:00        # updated on each st command
claimed_items:
  - T-163
```

### Stale Detection
`st sprint recover` or startup hook checks: if `last_active` older than `stale_claim_ttl`, release claims and mark session as dead.

Each `st` command updates `last_active` as a side effect (lightweight heartbeat).

## Cross-Sprint Dependencies

When `st prime` (or `st sprint plan`) detects a cross-sprint blocker:

```
Sprint: happy-jumping-elephant — Pipeline Commands
  ⚠ T-163 blocked by T-162 (not in this sprint)
    T-162 is in sprint: (none) — unsprinted backlog

  Options:
    1. st sprint add happy-jumping-elephant T-162  — pull into this sprint
    2. Skip T-163, work next unblocked item
    3. Wait for another sprint/agent to complete T-162
```

The agent enters plan mode to propose option 1 (promotion) or option 2 (reorder). User approves.

If T-162 is in another active sprint and claimed, the agent reports: "T-163 blocked by T-162 (Sprint X, in-progress by session Y). Skipping to next unblocked item."

## Agent Item Creation

Agents can create items freely:

```bash
# Agent discovers a blocker while working T-053
st create issue "Testcontainer leak blocks integration tests" --sprint happy-jumping-elephant
st push I-XXX --reason "Blocks T-053 integration tests"
```

This does NOT require plan approval — it's a tactical interrupt within the existing plan. The agent pushes it to its stack and works it.

Plan approval IS required when:
- The sprint's execution order needs to change
- A new item changes the approach/design for other sprint items
- Cross-sprint dependency promotion

The distinction: **creating an item = autonomous. Changing the plan = gate.**

## Backward Compatibility

- `st queue` continues to work for unsprinted items
- `st stack` continues to work (defaults to session-scoped if `AS_SESSION_ID` set, else global)
- `st prime` without a sprint gives the same global output as today
- Items without `claimed_by`/`sprint` fields work exactly as before
- `$AS_AGENT_ID` continues to work for agent identification (independent of sessions)

## `st run` + `st advance` — Configurable Pipeline Sprint Runner (T-157)

**Status:** Design approved — 2026-03-29
**Substrate:** Claude Code `claude -p` in headless mode

### Key Design Decision

Use `st start` for worktree setup (handles multi-repo: api + web + infra), NOT Claude's `--worktree` flag (single-repo only). Launch `claude -p` with CWD set to the worktree directory.

Validated via smoke test (2026-03-28):
- `claude -p --dangerously-skip-permissions` works from inside a git repo
- `--output-format json` returns structured results
- Base context cost ~$0.06 per invocation (CLAUDE.md cache)
- `--max-budget-usd` is optional per-item cost cap

### Configurable Step Pipeline

`st run` is a generic pipeline engine. Each project defines its pipeline as a list of named steps in config. The engine iterates steps per item, stopping on failure.

**Step types:**

| Type | Calls | Purpose |
|------|-------|---------|
| `claude` | `exec.Command("claude", "-p", ...)` | AI work: coding, code review, bugbot fixes |
| `test` | `TestRecord()` with `--run [--coverage]` | Test execution → S3 evidence → SHA + URI → coverage |
| `pr` | `PR()` | Git diff → file classification → blob hashes → scope suites |
| `merge` | `Merge()` | Close gates → merge → evidence → merge SHA capture |
| `merge_precheck` | Pre-checks only from `Merge()` | CI watch without merging (e.g., `gh pr checks --watch`) |
| `deploy` | `DeployCheck()` | Health URL polling + deploy verification |
| `smoke` | `Smoke()` | Smoke test execution + evidence |
| `uat` | `UAT()` | Cross-cutting checks + AC evaluation + evidence report |
| `gate` | stdin prompt | User approval — hard stop, no bypass |
| `close` | `Close()` | Full gate enforcement: deps, testing, manifest, stage |
| `command` | `runCmdInDir()` | Raw shell command (escape hatch) |

Typed steps preserve quality infrastructure. Raw `command` would bypass coverage enforcement, blob hash provenance, evidence upload, and gate validation.

### Example Pipeline (TheraPrac)

```yaml
run:
  permission_mode: "dangerously-skip-permissions"
  default_model: "sonnet"
  max_parallelism: 1
  default_budget_usd: 5.0

  pipeline:
    # Phase A: Implement (claude -p)
    - step: implement
      type: claude
      # Default prompt includes: item context, required test suites,
      # "tests must pass BEFORE commit", self-review, create PR, st pr

    # Phase B: Post-PR quality gates
    - step: ci_pr
      type: merge_precheck    # gh pr checks --watch

    - step: code_review
      type: claude
      prompt: |
        Review the open PR for {id}. Run /code-review and fix ALL findings.
        Check for cursor bugbot comments and resolve all open issues.
        Push any fixes. Do NOT merge.

    - step: ci_post_review
      type: merge_precheck    # CI again after review fixes

    # Phase C: Merge + Deploy
    - step: merge
      type: merge

    - step: deploy_watch
      type: deploy

    - step: smoke
      type: smoke

    # Phase D: UAT + Approval
    - step: uat
      type: uat

    - step: approval
      type: gate

    - step: close
      type: close
      resolution: completed
```

### Execution Flow Per Item

```
st run <sprint>
  For each unblocked item (up to --parallelism concurrent):

  1. st start <id> → create worktrees + claim

  2. [implement] claude -p in worktree (CWD = primary repo worktree)
     ├── Reads CLAUDE.md automatically
     ├── Prompt includes: summary, ACs, required test suites
     ├── Codes the fix/feature
     ├── Runs st test {id} <suite> --run for each required suite
     │   └── Each: execute → S3 evidence → SHA + URI → coverage thresholds
     ├── Self-reviews: git diff
     ├── Commits + pushes
     ├── Creates PR: gh pr create
     ├── Records PR: st pr {id} --repo <repo> --pr <N>
     │   └── File analysis → blob hashes → scope suite triggering
     └── STOPS (does not merge)

  3. [ci_pr] gh pr checks --watch (pre-merge CI)

  4. [code_review] claude -p (fresh subprocess)
     ├── Runs /code-review, fixes findings
     ├── Resolves cursor bugbot issues
     └── Pushes fixes

  5. [ci_post_review] gh pr checks --watch (post-review CI)

  6. [merge] Merge()
     ├── Pre-merge gate evaluation
     ├── gh pr merge --squash --delete-branch
     ├── Merge SHA capture
     └── Evidence → S3

  7. [deploy_watch] DeployCheck()
     ├── Health URL polling until 200
     └── Records deployed_dev stage

  8. [smoke] Smoke()

  9. [uat] UAT()
     ├── Test suites pass? Manifest exists? Evidence URIs?
     ├── AC evaluation (auto/cmd/manual)
     └── UAT report → S3

  10. [approval] Prompt: "Approve {id}? [y/N]" — hard stop

  11. [close] Close()
      └── Gates: deps_resolved, testing_complete, stage_reached, manifest_exists
```

### Claude Subprocess Details

Each `claude` step launches a separate `claude -p` process:

```
claude -p <prompt>
  --dangerously-skip-permissions     (or --permission-mode auto)
  --output-format json               (structured result)
  --add-dir <agent-state-root>       (access to st commands)
  --max-budget-usd N                 (per-item cost cap)
  --model <model>                    (configurable)
  --no-session-persistence           (ephemeral)
```

- CWD = primary repo worktree (e.g., `worktrees/T-xxx/theraprac-api/`)
- Environment: `AS_SESSION_ID=<unique-per-invocation>`, `AS_AGENT_ID=<from-config>`
- Template variables in prompts: `{id}`, `{sprint}`, `{branch}`, `{worktree}`

### Default `implement` Prompt

Includes:
- Item ID, title, summary, acceptance criteria
- Required test suites from `testing.required_suites` config
- "ALL required test suites must pass BEFORE committing"
- "Self-review: run `git diff` and review all changes"
- `st pr {id} --repo <repo> --pr <N>` instructions
- "Do NOT merge — stop after PR is created and recorded"

### Parallelism

- Parallel groups (from `computeParallelGroups()`) run sequentially
- Items within a group run concurrently up to `--parallelism`
- Bounded via semaphore channel + `sync.WaitGroup`
- Each goroutine reloads its own `store.Store` (thread safety)
- Gates serialized via `sync.Mutex` (one approval prompt at a time)

### Commands

```
st run <sprint>     [--dry-run] [--max-budget-usd N] [--parallelism N] [--item ID] [--model M] [--permission-mode M]
st advance <sprint> [--dry-run] [--max-budget-usd N] [--model M] [--permission-mode M] [--step <name>]
```

- `st run` — full autonomous sprint loop
- `st advance` — single item advancement (all steps, or up to `--step <name>`)
- `--dry-run` — show step-by-step plan without executing

### Config

```go
type RunConfig struct {
    PermissionMode   string           // "dangerously-skip-permissions" or "auto"
    DefaultModel     string           // "sonnet", "opus"
    MaxParallelism   int
    DefaultBudgetUSD float64
    Pipeline         []PipelineStepDef
}

type PipelineStepDef struct {
    Step       string // unique name
    Type       string // claude, test, pr, merge, merge_precheck, deploy, smoke, uat, gate, close, command
    Command    string // for command type
    Prompt     string // for claude type (optional)
    Resolution string // for close type
    Timeout    int    // for watch/deploy (seconds)
    Coverage   bool   // for test type
}
```

## Resolved Design Questions

1. **Session ID persistence**: Solved — file-based session ID in `.as/sessions/`. Each `st` command reads from env or file.
2. **Sprint plan storage**: Registry (`epics.yaml`) — simpler, proven in Sprint 1-2 implementation.
3. **Queue fate**: Coexists — queue is backlog for unsprinted items. Sprint items managed via sprint commands.
4. **Agent ID vs Session ID**: Both kept. Agent ID = optional grouping, Session ID = required for claims.

## Task Breakdown

**Epic:** nicely-promoted-seahorse — Sprint-Driven Multi-Agent Workflow

### Sprint 1 (parallelizable): Foundation

| Task | Title | Deps | Scope |
|------|-------|------|-------|
| T-164 | Sprint promotion — items list, plan ordering, enhanced display | none | Sprint data model, `sprint add/rm/show/plan` commands |
| T-165 | Session identity + claims — contention prevention and stale detection | none | Session UUID, `claimed_by/claimed_at` fields, `st start` claims, `st release`, stale TTL |

These two have no cross-dependencies. Two agents can build them in parallel.

### Sprint 2 (after Sprint 1): Execution

| Task | Title | Deps | Scope |
|------|-------|------|-------|
| T-166 | Sprint-aware execution — scoped prime, join/leave, cross-sprint deps | T-164, T-165 | `sprint join/leave`, sprint-scoped `prime`, cross-sprint dependency detection + promotion, `--sprint` flag on `create` |
| T-167 | Multi-session coordination — concurrent sessions, coordinator views, recovery | T-165 | `sprint status` coordinator view, `sprint recover`, concurrent claim handling, session heartbeat |

T-166 needs both sprint infrastructure (T-164) and claims (T-165). T-167 needs claims (T-165) but could start before T-164 is fully done.

### Sprint 3 (after Sprint 2): Capstone

| Task | Title | Deps | Scope |
|------|-------|------|-------|
| T-157 (revised) | `st advance` + `st run` — autonomous sprint execution loop | T-166, T-167 | Sprint-scoped advance/run, join→prime→claim→execute→test→pr→merge→UAT→close→next |

### Existing Tasks (unchanged, parallel track)

| Task | Title | Status | Notes |
|------|-------|--------|-------|
| T-151 | `st merge` + `st deploy-check` + `st smoke` | active, queued | Pipeline commands — independent of multi-agent work |
| T-163 | `st uat` — executable acceptance criteria | queued | UAT refinement — feeds into T-157 but not blocked by multi-agent |

### Known Bugs (fix opportunistically during sprint work)

1. `st update` dotted-path writes don't round-trip (parser bug)
2. S3 upload needs AWS_PROFILE for `st test --run`
3. `st pr` can't run after merge (git diff gone)
