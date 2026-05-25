package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/buildinfo"
	"github.com/jfinlinson/agent-state/internal/command"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/freshness"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/spf13/cobra"
)

// exitCode captures the return code from command functions.
var exitCode int

// newApp builds the full cobra command tree.
func newApp(cwd string) *cobra.Command {
	var appCfg *config.Config
	var appStore *store.Store

	root := &cobra.Command{
		Use:   "st",
		Short: "State tracker for AI agent workflows",
		Long: `st — track tasks, issues, and dependencies with config-driven validation.

Auto-fixes consistency issues, enforces delivery gates, and generates
context for LLM agents. Works standalone or with CI/hooks.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Commands that don't need config/store
			switch cmd.Name() {
			case "version", "init", "docgen":
				return nil
			}
			dir := cwd
			if dir == "" {
				var err error
				dir, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			var err error
			appCfg, err = config.Load(dir)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			if !appCfg.Discovered {
				return fmt.Errorf("no st project found (looked up from %s)\n\n  Run `st init` to create one, add a .st-root file, or set $ST_ROOT", dir)
			}
			// I-624: surface in-session drift between the running st
			// binary and the local as/ HEAD. The session-start hook only
			// auto-rebuilds on startup/resume, so a long-running session
			// can run a stale binary for days; this catches it on every
			// invocation (silent + cheap when there is no drift).
			command.MaybeWarnSingleAgentDrift(appCfg, os.Stderr)
			// Auto-pull latest changes before scanning items.
			// I-380: status owns its own RefreshWorkspace call so it can show
			// a banner reflecting the outcome — skip the silent pre-run pull
			// here to avoid the double-pull and let status's banner be
			// authoritative. `st run status` follows the same convention.
			switch cmd.Name() {
			case "status":
				// handled inside command.Status via refreshAndReload
			case "run":
				if len(args) >= 1 && args[0] == "status" {
					// st run status — handled inside command.RunStatus
					break
				}
				_ = store.GitPull(appCfg)
			default:
				_ = store.GitPull(appCfg)
			}

			appStore, err = store.New(appCfg)
			if err != nil {
				return fmt.Errorf("loading items: %w", err)
			}
			return nil
		},
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			// Heartbeat: update session.last_active on every command
			if appCfg != nil {
				if sid := appCfg.SessionID(); sid != "" {
					mgr := session.NewManager(
						appCfg.SessionsDir(),
						time.Duration(appCfg.StaleClaimTTL())*time.Second,
					)
					_ = mgr.Touch(sid) // best-effort, don't fail the command
				}
			}
			return nil
		},
		SilenceUsage: true,
	}

	// Groups surfaced in `st help` and rendered as section headers by
	// the docgen subcommand. Group titles are user-facing copy.
	root.AddGroup(
		&cobra.Group{ID: "queue-stack", Title: "Queue & Stack"},
		&cobra.Group{ID: "state-mgmt", Title: "State Management"},
		&cobra.Group{ID: "workflow", Title: "Workflow"},
		&cobra.Group{ID: "testing", Title: "Testing & Evidence"},
		&cobra.Group{ID: "uat-pipeline", Title: "UAT & Pipeline"},
		&cobra.Group{ID: "querying", Title: "Querying"},
		&cobra.Group{ID: "deps", Title: "Dependencies"},
		&cobra.Group{ID: "epics-sprints-notes", Title: "Epics, Sprints, Notes"},
		&cobra.Group{ID: "arcs", Title: "Arcs"},
		&cobra.Group{ID: "agents", Title: "Agents"},
		&cobra.Group{ID: "autonomy", Title: "Autonomy & Execution"},
		&cobra.Group{ID: "maintenance", Title: "Maintenance"},
	)

	// --- State commands ---

	showCmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Display item details",
		Long: "Display item details. --raw prints the markdown file; " +
			"--full renders the composite item view (every facet as a\n" +
			"self-documenting section; --all expands the machine sections too).",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			brief, _ := cmd.Flags().GetBool("brief")
			field, _ := cmd.Flags().GetString("field")
			raw, _ := cmd.Flags().GetBool("raw")
			full, _ := cmd.Flags().GetBool("full")
			fullAll, _ := cmd.Flags().GetBool("all")
			exitCode = command.Show(appStore, appCfg, args[0], command.ShowOpts{
				Brief: brief, Field: field, Raw: raw, Full: full, FullAll: fullAll,
			})
		},
	}
	showCmd.Flags().BoolP("brief", "b", false, "compact one-line output")
	showCmd.Flags().StringP("field", "f", "", "show single field value")
	showCmd.Flags().BoolP("raw", "r", false, "print the raw markdown file")
	showCmd.Flags().Bool("full", false, "composite item view: every facet as a self-documenting section")
	showCmd.Flags().Bool("all", false, "with --full: expand the machine sections too (default: collapsed)")
	root.AddCommand(showCmd)

	modelRecCmd := &cobra.Command{
		Use:   "model-rec [<id>]",
		Short: "Recommend a model tier (haiku|sonnet|opus) for an item",
		Long: "Recommend a model tier for the given item via a one-shot Haiku call.\n" +
			"With no argument, returns the default fallback (sonnet). Output:\n" +
			"  tier:<haiku|sonnet|opus>|reason:<short text>\n" +
			"Results cache to .as/runs/model-rec-cache.json keyed by item modtime.\n" +
			"Setting `model_tier: <tier>` on an item bypasses the recommender.\n" +
			"On any failure (engine missing, API down, parse error) the command\n" +
			"falls back to sonnet and exits 0 — the recommender is advisory.",
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			itemID := ""
			if len(args) == 1 {
				itemID = args[0]
			}
			noCache, _ := cmd.Flags().GetBool("no-cache")
			exitCode = command.ModelRec(appStore, appCfg, command.ModelRecOpts{
				ItemID:  itemID,
				Engine:  command.DefaultRunEngine(),
				NoCache: noCache,
			}, cmd.OutOrStdout())
		},
	}
	modelRecCmd.Flags().Bool("no-cache", false, "skip the cache (force a fresh recommender call)")
	root.AddCommand(modelRecCmd)

	costCmd := &cobra.Command{
		Use:   "cost",
		Short: "Per-item synthetic cost estimate based on logged token usage",
		Long: "Roll up the synthetic API cost estimate per item from accumulated\n" +
			"time_tracking.real_tokens × current pricing rates. Default scope is\n" +
			"open items only. Flags --touched-since, --item, --agent, --all\n" +
			"narrow or widen scope. Output is informational — on Max plan there\n" +
			"is no per-call billing to compare against.\n\n" +
			"Note: --touched-since filters by item.last_touched, which ticks on\n" +
			"ANY field write (status change, queue reorder, sync) — not strictly\n" +
			"on token-logging events. An item that did its expensive work weeks\n" +
			"ago but had its status changed today WILL appear in --touched-since\n" +
			"today with its full historical cost. True 'spend since X' rollup\n" +
			"requires per-turn timestamps; not built yet.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			sinceStr, _ := cmd.Flags().GetString("touched-since")
			itemID, _ := cmd.Flags().GetString("item")
			agent, _ := cmd.Flags().GetString("agent")
			all, _ := cmd.Flags().GetBool("all")

			opts := command.CostOpts{ItemID: itemID, Agent: agent, All: all}
			if sinceStr != "" {
				t, err := time.Parse("2006-01-02", sinceStr)
				if err != nil {
					fmt.Fprintf(os.Stderr, "cost: --touched-since must be YYYY-MM-DD: %v\n", err)
					exitCode = 2
					return
				}
				opts.Since = t
			}
			exitCode = command.Cost(appStore, appCfg, opts, cmd.OutOrStdout())
		},
	}
	costCmd.Flags().String("touched-since", "", "filter items with last_touched ≥ this date (YYYY-MM-DD); see note about semantic in --help")
	costCmd.Flags().String("item", "", "limit to a single item ID")
	costCmd.Flags().String("agent", "", "limit to items assigned to this agent (e.g. agent-g)")
	costCmd.Flags().Bool("all", false, "include archived items (default: open only)")
	root.AddCommand(costCmd)

	tuiCmd := &cobra.Command{
		Use:   "tui",
		Short: "Layout-A orchestration TUI (live by default; --once for static snapshot)",
		Long: "Open the Layout-A frame: top agent strip, focused composite\n" +
			"item pane (st show --full), planning queue (st recommend),\n" +
			"bottom alerts band. Default is LIVE — fsnotify-driven\n" +
			"soft-refresh on substrate change (T-373). --once renders the\n" +
			"frame statically and exits (T-372). q / Ctrl-C / Esc quits.\n" +
			"--item focuses a specific item; default = the next eligible\n" +
			"queue pick (the same dispatch source the coordinator uses).\n" +
			"The §3/§5 navigation/keyboard model is T-374.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			item, _ := cmd.Flags().GetString("item")
			width, _ := cmd.Flags().GetInt("width")
			once, _ := cmd.Flags().GetBool("once")
			exitCode = command.Tui(appStore, appCfg, command.TuiOpts{
				Item: item, Width: width, Once: once,
			})
		},
	}
	tuiCmd.Flags().String("item", "", "focus a specific item id (default: next queue pick)")
	tuiCmd.Flags().Int("width", 0, "render width override (default: 120; live mode reads terminal width)")
	tuiCmd.Flags().Bool("once", false, "render the static frame once and exit (T-372 behaviour)")
	root.AddCommand(tuiCmd)

	artifactCmd := &cobra.Command{
		Use:   "artifact <id> <kind>",
		Short: "Introspect one facet of an item (plan, AC, testing, PR, deps, history, etc.)",
		Long: "Expose each of an item's ~12 artifact facets through one\n" +
			"uniform, stdout-able command (TUI-design §4). <kind> is one\n" +
			"of: item, plan, ac, history, testing, pr, uat, commits, deps,\n" +
			"bus, worktree, accounting — or 'all' for every facet. Each\n" +
			"facet reads its existing source (no new storage); --format\n" +
			"json is the stable contract `st show --full` and the TUI\n" +
			"consume. Composition only — no facet logic is duplicated here.",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			format, _ := cmd.Flags().GetString("format")
			exitCode = command.Artifact(appStore, appCfg, args[0],
				command.ArtifactOpts{Kind: args[1], Format: format})
		},
	}
	artifactCmd.Flags().String("format", "text", "output format: text | json")
	root.AddCommand(artifactCmd)

	transcriptCmd := &cobra.Command{
		Use:   "transcript <item|agent|session>",
		Short: "Render a session's JSONL transcript (human-readable)",
		Long: "Resolve a selector (item id, agent id, or session id) to its " +
			"on-disk Claude Code session JSONL, merge the agent-state " +
			"conversation channel (changelog/mail), and render it readably " +
			"(tool calls collapsed to one line, reasoning as prose).",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			since, _ := cmd.Flags().GetString("since")
			grep, _ := cmd.Flags().GetString("grep")
			ag, _ := cmd.Flags().GetString("agent")
			asJSON, _ := cmd.Flags().GetBool("json")
			exitCode = command.Transcript(appStore, appCfg, args[0], command.TranscriptOpts{
				Since: since, Grep: grep, Agent: ag, JSON: asJSON,
			})
		},
	}
	transcriptCmd.Flags().String("since", "", "only rows newer than this (duration like 7d/1d12h, or RFC3339)")
	transcriptCmd.Flags().String("grep", "", "only rendered lines containing this substring")
	transcriptCmd.Flags().String("agent", "", "restrict to one session tag (e.g. A, a-1)")
	transcriptCmd.Flags().Bool("json", false, "emit raw rows as JSON (pre-render, for machines)")
	root.AddCommand(transcriptCmd)

	watchCmd := &cobra.Command{
		Use:   "watch",
		Short: "Live unified stream — one compressed line per live agent",
		Long: "Enumerate live agents (process-tree liveness), tail each one's " +
			"session JSONL, and print a compressed per-agent strip (what each " +
			"is doing now) — not a raw firehose. Backs off when idle; Ctrl-C " +
			"prints a final snapshot and exits.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			intv, _ := cmd.Flags().GetDuration("interval")
			once, _ := cmd.Flags().GetBool("once")
			exitCode = command.Watch(appCfg, command.WatchOpts{Interval: intv, Once: once})
		},
	}
	watchCmd.Flags().Duration("interval", time.Second, "base poll interval (backs off geometrically when idle)")
	watchCmd.Flags().Bool("once", false, "single snapshot pass then exit (no follow)")
	root.AddCommand(watchCmd)

	listCmd := &cobra.Command{
		Use:     "list",
		Short:   "List items with optional filters (use 'goal list' to list goals)",
		Aliases: []string{"ls"},
		Long: `List tasks, issues, and ideas with optional filters. Default shows all non-terminal items.

Filters stack (AND logic). For richer queries — sort by cost/time/LOC, filter by
multiple comma-separated priorities, or filter by agent — use:
  st status --filter key:value   (keys: type, status, tag, assigned, priority, epic, sprint)
  st status --sort field[,asc|desc]

To list goals with weights use:
  st goal list`,
		Run: func(cmd *cobra.Command, args []string) {
			typeF, _ := cmd.Flags().GetString("type")
			statusF, _ := cmd.Flags().GetString("status")
			tagF, _ := cmd.Flags().GetString("tag")
			assignedF, _ := cmd.Flags().GetString("assigned")
			goalF, _ := cmd.Flags().GetString("goal")
			priorityF, _ := cmd.Flags().GetString("priority")
			sprintF, _ := cmd.Flags().GetString("sprint")
			epicF, _ := cmd.Flags().GetString("epic")
			arcF, _ := cmd.Flags().GetString("arc")
			exitCode = command.List(appStore, appCfg, command.ListOpts{
				Type: typeF, Status: statusF, Tag: tagF, Assigned: assignedF, Goal: goalF,
				Priority: priorityF, Sprint: sprintF, Epic: epicF, Arc: arcF,
			})
		},
	}
	listCmd.Flags().StringP("type", "T", "", "filter by type (task, issue, idea)")
	listCmd.Flags().StringP("status", "s", "", "filter by status")
	listCmd.Flags().String("tag", "", "filter by tag")
	listCmd.Flags().String("assigned", "", "filter by assigned agent")
	listCmd.Flags().String("goal", "", "filter by goal ID (items whose goals: field contains this ID)")
	listCmd.Flags().StringP("priority", "p", "", "filter by priority: single value or comma-list (e.g. 0, 0,1)")
	listCmd.Flags().String("sprint", "", "filter by sprint ID")
	listCmd.Flags().String("epic", "", "filter by epic ID")
	listCmd.Flags().String("arc", "", "filter by arc name")
	root.AddCommand(listCmd)

	createCmd := &cobra.Command{
		Use:     "create <type> <title>",
		Short:   "Create a new task, issue, or idea",
		Aliases: []string{"new"},
		Args:    cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			priority, _ := cmd.Flags().GetInt("priority")
			severity, _ := cmd.Flags().GetString("severity")
			tag, _ := cmd.Flags().GetString("tag")
			depends, _ := cmd.Flags().GetString("depends")
			sprint, _ := cmd.Flags().GetString("sprint")
			goals, _ := cmd.Flags().GetStringSlice("goals")
			exitCode = command.Create(appStore, appCfg, args[0], args[1], command.CreateOpts{
				Priority: priority, Severity: severity, Tag: tag, Depends: depends, Sprint: sprint,
				Goals: goals,
				// I-588: wire the run engine so post-create spawns the
				// SBAR/title sub-agent self-review. In-process callers
				// (tests, migrations) leave Engine zero and skip the review.
				Engine: command.DefaultRunEngine(),
			})
		},
	}
	createCmd.Flags().IntP("priority", "p", 2, "priority 0-4 (0=highest)")
	// I-406: --severity is deprecated. Stays registered so callers passing
	// it get the migration message from Create() instead of "unknown flag".
	createCmd.Flags().String("severity", "", "DEPRECATED — use --priority (I-406)")
	_ = createCmd.Flags().MarkDeprecated("severity", "use --priority (I-406)")
	createCmd.Flags().String("tag", "", "initial tag")
	createCmd.Flags().String("depends", "", "depends on item ID")
	createCmd.Flags().String("sprint", "", "assign to sprint on creation")
	createCmd.Flags().StringSlice("goals", nil, "goal IDs to associate on creation (comma-separated)")
	// T-382: post-create launcher flag removed. Use `st update <id> sbar --stdin` post-create.
	root.AddCommand(createCmd)

	updateCmd := &cobra.Command{
		Use:   "update <id> <field> [value] | <id> field=value [field=value ...]",
		Short: "Update one or more fields on an item",
		Long: `Update fields on an item.

Single-field modes:
  st update <id> <field> <value>           # positional — set directly
  st update <id> <field>                   # no value — open $EDITOR seeded with current value
  st update <id> <field> --stdin           # read new value from stdin (pipe or heredoc)

Batch mode (I-504) — one commit, one push, one changelog flush:
  st update <id> field1=value1 field2=value2 ...

Batch mode is atomic: any pair that fails vocab/range validation
rejects the whole batch before any write. Long-form fields, list
fields, and the SBAR composite stay on the single-field paths.`,
		Args: cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			stdinFlag, _ := cmd.Flags().GetBool("stdin")
			id := args[0]

			// I-504: batch form fires when every arg after <id> looks
			// like `key=value`. The legacy `<id> <field> <value>`
			// case where the value happens to contain a literal `=`
			// (e.g. `st update I-001 message foo=bar`) is preserved
			// because args[1] = "message" has no `=`, so
			// allLookLikePairs returns false and we route to
			// single-field. --stdin always wins over batch dispatch
			// since stdin mode targets a single field.
			if len(args) >= 2 && !stdinFlag && allLookLikePairs(args[1:]) {
				pairs := make([]command.FieldValue, 0, len(args)-1)
				for _, a := range args[1:] {
					eq := strings.Index(a, "=")
					pairs = append(pairs, command.FieldValue{
						Field: a[:eq],
						Value: a[eq+1:],
					})
				}
				exitCode = command.UpdateBatch(appStore, appCfg, id, pairs)
				return
			}

			if len(args) > 3 {
				fmt.Fprintln(os.Stderr,
					"update: too many args for single-field form. Use field=value pairs for batch mode.")
				exitCode = 2
				return
			}

			field := args[1]
			switch {
			case stdinFlag:
				exitCode = command.Update(appStore, appCfg, id, field, "", command.UpdateModeStdin)
			case len(args) >= 3:
				exitCode = command.Update(appStore, appCfg, id, field, args[2], command.UpdateModeValue)
			case command.StdinIsPiped():
				exitCode = command.Update(appStore, appCfg, id, field, "", command.UpdateModeStdin)
			default:
				// T-382: every "no value, no --stdin" path now
				// refuses (was: sbar opened $EDITOR via the
				// I-493 flow). Agents drive every write via
				// stdin-based heredocs; the editor flow had no
				// production callers.
				fmt.Fprintf(os.Stderr,
					"update: no value supplied for %s.%s\n"+
						"  st update <id> <field> <value>           # short value\n"+
						"  st update <id> <field> --stdin            # multi-line via stdin\n"+
						"  st update <id> field1=v field2=v ...     # batch (I-504)\n"+
						"  st update <id> sbar --stdin               # SBAR composite via 4-section buffer\n",
					id, field)
				exitCode = 2
			}
		},
	}
	updateCmd.Flags().Bool("stdin", false, "read value from stdin")
	root.AddCommand(updateCmd)

	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Validate all items and auto-fix consistency issues",
		Run: func(cmd *cobra.Command, args []string) {
			quiet, _ := cmd.Flags().GetBool("quiet")
			fix, _ := cmd.Flags().GetBool("fix")
			exitCode = command.Check(appStore, appCfg, quiet, fix)
		},
	}
	checkCmd.Flags().BoolP("quiet", "q", false, "exit code only, no output (for CI/hooks)")
	checkCmd.Flags().Bool("fix", false, "auto-repair fixable issues (default when not quiet)")
	root.AddCommand(checkCmd)

	tagCmd := &cobra.Command{
		Use:   "tag <id> <add|rm> <tag>",
		Short: "Add or remove a tag",
		Args:  cobra.ExactArgs(3),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Tag(appStore, appCfg, args[0], args[1], args[2])
		},
	}
	root.AddCommand(tagCmd)

	agentCmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage local agent identity, auth, and isolated agent workspaces",
		Long: `Manage local agent identity, auth, and isolated workspaces.

An "agent workspace" is one fully-isolated copy of the project on disk
that runs as its own logical operator. Multiple agents can run in
parallel against the same upstream repos without stepping on each
other because each agent gets:

  - its own filesystem checkout under theraprac-agents/theraprac-agent-<x>
  - its own AWS access key (assumed role + per-agent SSM scope)
  - its own GitHub App installation token (cached at
    ~/.theraprac/gh-agent-<x>-session.json)
  - its own host port block (a→100, b→200, c→300, …; per-letter offset
    so dev-services on parallel agents don't collide)

The shared spine is theraprac-workspace: there is a single canonical
clone at theraprac-agents/theraprac-workspace, and each agent's
theraprac-agent-<x>/theraprac-workspace is a SYMLINK to it (I-418).
All agent-state writes serialize through one .git, eliminating
push/rebase contention between agents. Use --full on create to opt
out of the symlink and get a real clone instead — almost never needed.

Lifecycle, in order:

  1. st agent workspace create <name>    one-shot: clone repos, start
                                         Docker, bootstrap AWS+GH
  2. st agent bootstrap                   (auto-chained by create)
                                         provisions AWS key + GH App
                                         install for the new agent
  3. st agent auth                        refresh cached AWS+GH
                                         sessions; emit shell exports
  4. st agent workspace status            inspect resolved paths /
                                         ports / repo state
  5. st agent workspace destroy <name>    tear down after safety
                                         checks (or --force after
                                         operator review)

bootstrap is one-time per agent; auth is the routine refresh. The
agent identity is derived from the parent directory name
(theraprac-agent-<x>), so .as/local-agent.yaml is gitignored to
keep identity from clobbering across agents.`,
		Example: `  # see who I am and where my workspace points
  st agent identity show

  # list every agent registered on this machine
  st agent list

  # stand up a brand-new agent end-to-end
  st agent workspace create agent-c

  # refresh creds (typical: cache hit, no AWS/GH calls)
  st agent auth`,
	}
	agentBootstrapCmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap AWS and GitHub credentials for an agent",
		Long: `Bootstrap AWS and GitHub credentials for an agent — one-time per agent.

Provisions a per-agent AWS access key (rotatable via --rotate-key) and
walks through the GitHub App install flow so the agent can mint
installation tokens going forward. Idempotent: re-running on a fully
bootstrapped agent is a no-op unless --rotate-key is set.

Prerequisites:
  - Caller has aws sso login session (covers the bootstrap call itself).
  - For --skip-gh runs: nothing else.
  - For full runs: a browser to authorize the GitHub App install
    against the org you control. The command prints the URL to paste —
    it never opens a browser itself (this machine's "open" routes to a
    cmux embedded browser with no github.com session — T-301).

Side effects:
  - Creates / rotates IAM access key for theraprac-agent-<name>.
  - Writes ~/.theraprac/agent-<name>-aws.json (cached SSO session).
  - Triggers GitHub App install; on success caches
    ~/.theraprac/gh-agent-<name>-session.json.
  - On --rotate-key, the previous access key is scheduled for
    deactivation (not immediate delete — gives time for rolling).

--skip-aws / --skip-gh let you redo just half of the dance when only
one identity is broken (e.g., rotating the GH App without touching
AWS).`,
		Example: `  # full bootstrap, first time for this agent
  st agent bootstrap --name agent-c

  # rotate AWS access key for the current agent (most common use)
  st agent bootstrap --rotate-key --skip-gh

  # dry-run AWS-only to preview what would change without mutating
  st agent bootstrap --skip-gh --dry-run --rotate-key`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			name, _ := cmd.Flags().GetString("name")
			skipAWS, _ := cmd.Flags().GetBool("skip-aws")
			skipGH, _ := cmd.Flags().GetBool("skip-gh")
			rotateKey, _ := cmd.Flags().GetBool("rotate-key")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			owner, _ := cmd.Flags().GetString("owner")
			port, _ := cmd.Flags().GetString("port")
			skipInstall, _ := cmd.Flags().GetBool("skip-install")
			exitCode = command.AgentBootstrap(appCfg, command.AgentBootstrapOpts{
				Name: name, SkipAWS: skipAWS, SkipGH: skipGH, RotateKey: rotateKey,
				DryRun: dryRun, Owner: owner, Port: port, SkipInstall: skipInstall,
			})
		},
	}
	agentBootstrapCmd.Flags().String("name", "", "agent name (default: derived agent id or agent-a)")
	agentBootstrapCmd.Flags().Bool("skip-aws", false, "skip AWS bootstrap")
	agentBootstrapCmd.Flags().Bool("skip-gh", false, "skip GitHub bootstrap")
	agentBootstrapCmd.Flags().Bool("rotate-key", false, "rotate AWS access key")
	agentBootstrapCmd.Flags().Bool("dry-run", false, "print AWS bootstrap actions without mutating AWS")
	agentBootstrapCmd.Flags().String("owner", "", "GitHub owner/org for App install")
	agentBootstrapCmd.Flags().String("port", "", "localhost callback port for GitHub bootstrap")
	agentBootstrapCmd.Flags().Bool("skip-install", false, "skip GitHub App install step")
	agentCmd.AddCommand(agentBootstrapCmd)

	agentAuthCmd := &cobra.Command{
		Use:   "auth",
		Short: "Refresh agent AWS/GitHub auth and print shell exports",
		Long: `Refresh the cached AWS and GitHub sessions for an agent and emit
shell-eval-able export lines for AWS_ACCESS_KEY_ID / AWS_SESSION_TOKEN /
GH_TOKEN. This is the routine refresh path; bootstrap is the
one-time provisioning path.

Behavior:
  - When a cache hit is available (~/.theraprac/agent-<name>-aws.json
    and ~/.theraprac/gh-agent-<name>-session.json have time-left), the
    command exits in milliseconds and prints the cached exports —
    suitable to wrap in 'eval "$(st agent auth)"' in a shell prompt.
  - On cache miss (or --force), re-mints from the upstream identity
    sources: assumes the per-agent AWS role and mints a fresh GH App
    installation token. This makes API calls, so don't loop it.

Prerequisites:
  - agent must already have completed 'st agent bootstrap' once. If
    not, auth fails fast with a "no bootstrap state" message and points
    you at bootstrap. auth never silently bootstraps.

--skip-aws / --skip-gh print only the half you ask for.
--force ignores the cache and re-mints regardless of TTL.`,
		Example: `  # eval into the current shell — typical invocation
  eval "$(st agent auth)"

  # force a re-mint after rotating the AWS access key
  st agent auth --force --skip-gh

  # only show GitHub token (e.g., piping to gh auth setup-git)
  st agent auth --skip-aws`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			name, _ := cmd.Flags().GetString("name")
			skipAWS, _ := cmd.Flags().GetBool("skip-aws")
			skipGH, _ := cmd.Flags().GetBool("skip-gh")
			force, _ := cmd.Flags().GetBool("force")
			exitCode = command.AgentAuth(appCfg, command.AgentAuthOpts{Name: name, SkipAWS: skipAWS, SkipGH: skipGH, Force: force})
		},
	}
	agentAuthCmd.Flags().String("name", "", "agent name (default: derived agent id or agent-a)")
	agentAuthCmd.Flags().Bool("skip-aws", false, "skip AWS auth")
	agentAuthCmd.Flags().Bool("skip-gh", false, "skip GitHub auth")
	agentAuthCmd.Flags().Bool("force", false, "ignore cached sessions")
	agentCmd.AddCommand(agentAuthCmd)

	agentListCmd := &cobra.Command{
		Use:   "list",
		Short: "List configured local agents",
		Long: `List every agent registered on this machine, by name, with the
status of each one's AWS/GH session caches and workspace path.

Source of truth is the union of:
  - directories matching theraprac-agents/theraprac-agent-<x>/
  - ~/.theraprac/agent-*-aws.json session caches
  - ~/.theraprac/gh-agent-*-session.json session caches

An agent listed with no workspace clone usually means its directory
was removed manually without 'st agent workspace destroy' — re-run
destroy with --force to clean up the dangling session caches.`,
		Example: `  # see who's registered locally
  st agent list`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.AgentList(appCfg)
		},
	}
	agentCmd.AddCommand(agentListCmd)

	// use = cobra Use string; invoked = how the user types it (for
	// help Example + error-message prefix, so `st agents` never prints
	// "agent ps:").
	newAgentPSCmd := func(use, invoked string) *cobra.Command {
		c := &cobra.Command{
			Use:   use,
			Short: "Global process-table of the agent fleet (live, uptime, last-update, current item)",
			Long: `A read-only snapshot of every agent in the workspace roster:
which are live (process-tree liveness), how long each has been running,
when each last updated (session-JSONL freshness), and the agent-state
item each is currently on.

The static-snapshot sibling of 'st watch' (live stream) and
'st transcript' (history). Idle/unregistered roster agents are still
listed (shown as '—'); a registration whose PID is dead shows 'stale'.`,
			Example: "  st " + invoked + "\n  st " + invoked + " --workspace agent-b\n  st " + invoked + " --json",
			Args:    cobra.NoArgs,
			Run: func(cmd *cobra.Command, args []string) {
				ws, _ := cmd.Flags().GetString("workspace")
				asJSON, _ := cmd.Flags().GetBool("json")
				exitCode = command.AgentPS(appStore, appCfg, command.AgentPSOpts{Workspace: ws, JSON: asJSON, Invoked: invoked})
			},
		}
		c.Flags().String("workspace", "", "only agents whose workspace path contains this substring")
		c.Flags().Bool("json", false, "emit the joined rows as JSON (pre-render, for machines)")
		return c
	}
	agentCmd.AddCommand(newAgentPSCmd("ps", "agent ps"))
	// Top-level `st agents` — alias for `st agent ps` for muscle-memory convenience.
	agentsAlias := newAgentPSCmd("agents", "agents")
	agentsAlias.Short = "Agent fleet process table — alias for 'agent ps'"
	root.AddCommand(agentsAlias)

	agentRegisterCmd := &cobra.Command{
		Use:   "register",
		Short: "Record this workspace agent's live session (invoked by SessionStart hook)",
		Long: `Record this workspace agent's live Claude session in
.as/agents/<id>.yaml so the registration-derived columns (UPTIME,
authoritative SESSION, PID liveness) populate in 'st agent ps' and
'st watch'.

Invoked automatically by the SessionStart hook with the Claude PID and
session id; rarely run by hand. Idempotent and hook-safe: it always
exits 0 (a missing identity or write failure is a stderr warning, never
a broken session start). It overwrites only this agent's own record
(never the shared dir's peer registrations); UPTIME stays continuous
across a same-session resume/compact.`,
		Example: `  # done for you by the SessionStart hook
  st agent register --pid 12345 --session 0f630d0d-...`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			pid, _ := cmd.Flags().GetInt("pid")
			sess, _ := cmd.Flags().GetString("session")
			exitCode = command.AgentRegister(appCfg, command.AgentRegisterOpts{PID: pid, SessionID: sess})
		},
	}
	agentRegisterCmd.Flags().Int("pid", 0, "process to track for liveness (0 = parent process; the SessionStart hook passes the Claude PID)")
	agentRegisterCmd.Flags().String("session", "", "Claude session id")
	agentCmd.AddCommand(agentRegisterCmd)

	agentDeregisterCmd := &cobra.Command{
		Use:   "deregister",
		Short: "Remove this workspace agent's registration (explicit/scripted; not hook-wired)",
		Long: `Remove this workspace agent's .as/agents/<id>.yaml registration.

Idempotent (a no-op if already absent). Deliberately NOT wired to any
hook: Claude Code has no SessionEnd event and the Stop hook fires every
turn, so automatic deregistration would flap mid-session. A stale
registration is instead rendered 'stale' by 'st agent ps' (T-356
liveness) until a command that runs agent.Sweep (e.g. 'st start')
removes it; 'st agent register' overwrites only this agent's own
record, never peers'. Provided for explicit/scripted cleanup and
future 'st spawn' workers.`,
		Example: `  st agent deregister`,
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.AgentDeregister(appCfg)
		},
	}
	agentCmd.AddCommand(agentDeregisterCmd)

	agentIdentityCmd := &cobra.Command{
		Use:   "identity",
		Short: "Inspect resolved agent identity",
		Long: `Inspect the agent identity that 'st' resolves for the current
process — useful when ST_ROOT, AS_AGENT_ID, or .as/local-agent.yaml
disagree about who this shell is.`,
		Example: `  # show resolved identity for the current shell
  st agent identity show`,
	}
	agentIdentityShowCmd := &cobra.Command{
		Use:   "show",
		Short: "Print the resolved agent identity (id, source, parent/root heritage)",
		Long: `Print the agent ID 'st' resolved for this invocation, plus the
source it came from (env var, .as/local-agent.yaml, or parent-dir
inference) and the parent/root heritage chain when the agent was
spawned by another agent (T-326).

Use this when:
  - You're inside a worktree and unsure which agent you'll act as.
  - 'st queue add' or 'st commit' is attributing work to the wrong
    agent (check the source field).
  - Debugging why ST_ROOT routes you to an unexpected agent root.

Side effects: none — pure read of resolved config.`,
		Example: `  # who am I in this shell?
  st agent identity show

  # combined with a worktree cd to confirm identity resolution
  cd worktrees/I-402 && st agent identity show`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.AgentIdentityShow(appCfg)
		},
	}
	agentIdentityCmd.AddCommand(agentIdentityShowCmd)
	agentCmd.AddCommand(agentIdentityCmd)

	agentWorkspaceCmd := &cobra.Command{
		Use:   "workspace",
		Short: "Create, inspect, and remove local agent workspaces",
		Long: `Manage the on-disk lifecycle of an agent workspace. An agent
workspace is the directory theraprac-agents/theraprac-agent-<x>/
plus the symlinks/clones it owns: theraprac-api, theraprac-web,
theraprac-infra, as, and a theraprac-workspace symlink to the
canonical workspace (I-418).

Subcommands:
  create  — stand up a brand-new workspace end-to-end
  status  — inspect resolved paths, ports, repo state
  destroy — tear down after safety checks (or --force)`,
		Example: `  # see what's there for a specific agent
  st agent workspace status agent-c

  # stand up a new agent (chains bootstrap)
  st agent workspace create agent-d

  # remove an agent after safety checks
  st agent workspace destroy agent-d`,
	}
	agentWorkspaceCreateCmd := &cobra.Command{
		Use:   "create <agent>",
		Short: "Create or repair an independent agent workspace (auto-chains AWS+GH identity bootstrap)",
		Long: `Create or repair an independent agent workspace.

Stands up theraprac-agents/theraprac-agent-<name>/ with:
  - clones (or symlinks per I-418) of theraprac-api, theraprac-web,
    theraprac-infra, and as
  - a theraprac-workspace symlink pointing at the canonical workspace
  - per-agent Docker services brought up on the agent's port block
  - chained 'st agent bootstrap' to provision AWS+GH identity

After cloning repos and starting Docker services, this command
auto-chains 'st agent bootstrap' for the new agent, provisioning
both AWS and GitHub identities. Use --skip-aws / --skip-gh to opt
out of either half (e.g., to reuse a shared identity).

--repair fixes a known-safe partial state:
  - missing theraprac-workspace symlink → recreate
  - dangling per-agent Docker compose project → re-create with the
    canonical name
  - missing .as/local-agent.yaml → write fresh with the parent-dir
    inferred ID
  - half-cloned repos (clone exists but .git is empty) → re-clone
It does NOT touch dirty working trees, won't overwrite a non-empty
.as/local-agent.yaml, and won't touch the workspace canonical clone.
Anything else (missing creds, broken Docker, bad SSO) it leaves alone
so you can see what's wrong.

Prerequisites:
  - Caller has aws sso login (chained bootstrap needs it).
  - Docker daemon running locally if you want the per-agent services.
  - For non-dry-run: --yes (the gate that turns plan into apply).

Side effects:
  - Disk writes under theraprac-agents/theraprac-agent-<name>/
  - Docker compose up on the per-agent port block
  - IAM access key issued (chained bootstrap)
  - GitHub App install request (chained bootstrap)`,
		Example: `  # plan-only: see what create would do
  st agent workspace create agent-d --dry-run

  # apply: confirm with --yes; chains AWS+GH bootstrap
  st agent workspace create agent-d --yes

  # heal a partially-broken workspace without re-bootstrapping creds
  st agent workspace create agent-d --repair --skip-aws --skip-gh

  # reuse a shared GH App install (skip the chained --gh half)
  st agent workspace create agent-d --yes --skip-gh`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			branch, _ := cmd.Flags().GetString("branch")
			yes, _ := cmd.Flags().GetBool("yes")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			repair, _ := cmd.Flags().GetBool("repair")
			skipAWS, _ := cmd.Flags().GetBool("skip-aws")
			skipGH, _ := cmd.Flags().GetBool("skip-gh")
			owner, _ := cmd.Flags().GetString("owner")
			exitCode = command.AgentWorkspaceCreate(appCfg, command.AgentWorkspaceCreateOpts{
				Agent: args[0], Branch: branch, Yes: yes, DryRun: dryRun, Repair: repair,
				SkipAWS: skipAWS, SkipGH: skipGH, Owner: owner,
			})
		},
	}
	agentWorkspaceCreateCmd.Flags().String("branch", "main", "branch to check out in each repo")
	agentWorkspaceCreateCmd.Flags().Bool("yes", false, "confirm non-dry-run create (without it, the command refuses to apply)")
	agentWorkspaceCreateCmd.Flags().Bool("dry-run", false, "print the plan without filesystem, git, or Docker changes")
	agentWorkspaceCreateCmd.Flags().Bool("repair", false, "replace known-safe partial workspace symlinks")
	agentWorkspaceCreateCmd.Flags().Bool("skip-aws", false, "do not chain AWS identity bootstrap")
	agentWorkspaceCreateCmd.Flags().Bool("skip-gh", false, "do not chain GitHub identity bootstrap")
	agentWorkspaceCreateCmd.Flags().String("owner", "", "GitHub org for the chained GH App install (forwarded to agent bootstrap)")
	agentWorkspaceCmd.AddCommand(agentWorkspaceCreateCmd)

	agentWorkspaceStatusCmd := &cobra.Command{
		Use:   "status [agent]",
		Short: "Show resolved paths, ports, repo state, and service-health placeholders",
		Long: `Show the resolved layout of an agent workspace.

Reports:
  - workspace root path (theraprac-agents/theraprac-agent-<x>/)
  - per-repo presence + dirty/clean working-tree state
  - allocated port block (a→100, b→200, …)
  - cached AWS / GH session expiry
  - service-health placeholders (Postgres / cmux / mail relay) — these
    are currently stubbed; the cells are wired in but the actual health
    checks land later. Treat the service column as advisory until that
    work merges.

With no agent argument, reports on the current shell's resolved
agent (per 'st agent identity show').

Side effects: none — pure read.`,
		Example: `  # current shell's agent
  st agent workspace status

  # specific agent
  st agent workspace status agent-c`,
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			agent := ""
			if len(args) > 0 {
				agent = args[0]
			}
			exitCode = command.AgentWorkspaceStatus(appCfg, command.AgentWorkspaceStatusOpts{Agent: agent})
		},
	}
	agentWorkspaceCmd.AddCommand(agentWorkspaceStatusCmd)

	agentWorkspaceDestroyCmd := &cobra.Command{
		Use:   "destroy <agent>",
		Short: "Remove an agent workspace after safety checks",
		Long: `Tear down an agent workspace after running safety checks.

Safety checks (all must pass without --force):
  - working tree clean in every per-agent repo clone (theraprac-api,
    theraprac-web, theraprac-infra, as) — uncommitted work is the #1
    way agents lose hours, so destroy refuses by default.
  - no unpushed commits on any agent-owned branch.
  - per-agent Docker compose project is shut down or already absent.
  - no in-flight 'st run' / 'st start' for any item on the agent.

When all checks pass, destroy:
  - stops per-agent Docker services and removes the compose project
  - deletes the per-agent IAM access key
  - removes ~/.theraprac/agent-<name>-*.json session caches
  - rm -rf theraprac-agents/theraprac-agent-<name>/

--force overrides the working-tree-clean and unpushed-commits checks
after operator review. It does NOT bypass the in-flight-run check —
nothing should kill an executing pipeline mid-step. To clear an
in-flight, run 'st release' against the active items first.

--dry-run prints the full action plan without doing anything.`,
		Example: `  # always start here
  st agent workspace destroy agent-d --dry-run

  # apply after dry-run looks right
  st agent workspace destroy agent-d

  # I reviewed the dirty repos and accept the data loss
  st agent workspace destroy agent-d --force`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			force, _ := cmd.Flags().GetBool("force")
			exitCode = command.AgentWorkspaceDestroy(appCfg, command.AgentWorkspaceDestroyOpts{
				Agent: args[0], DryRun: dryRun, Force: force,
			})
		},
	}
	agentWorkspaceDestroyCmd.Flags().Bool("dry-run", false, "print what would be stopped or removed")
	agentWorkspaceDestroyCmd.Flags().Bool("force", false, "allow removal despite dirty repos after operator review")
	agentWorkspaceCmd.AddCommand(agentWorkspaceDestroyCmd)
	agentCmd.AddCommand(agentWorkspaceCmd)
	root.AddCommand(agentCmd)

	// --- Spawn ---
	// `st spawn <item>` (T-360): launch a budget-capped, JSONL-observable
	// reasoning worker on an item — the Shape-3 §10/§13 linchpin.
	// `st spawn child <item>` (T-326): the older registration-only path.

	spawnCmd := &cobra.Command{
		Use:   "spawn <item>",
		Short: "Launch a budget-capped Claude worker on an item",
		Long: "Launch a detached, budget-capped, JSONL-observable reasoning\n" +
			"worker (`claude -p`, resolved binary) that drives <item> through\n" +
			"the full CLAUDE.md delivery loop. The per-item budget is read from\n" +
			"theraprac-workspace/.as/coordinator.yaml (never spawned uncapped);\n" +
			"the autonomy boundary there governs the worker via the existing\n" +
			"per-worker enforcement hooks. Observe with `st watch` /\n" +
			"`st transcript <item>`.\n\n" +
			"--dry-run prints the fully-resolved launch plan and exits without\n" +
			"launching, registering, or starting anything.",
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) == 0 {
				_ = cmd.Help()
				exitCode = 2
				return
			}
			budget, _ := cmd.Flags().GetFloat64("budget")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			exitCode = command.Spawn(appStore, appCfg, command.SpawnOpts{
				Item:           args[0],
				BudgetOverride: budget,
				DryRun:         dryRun,
			})
		},
	}
	spawnCmd.Flags().Float64("budget", 0,
		"LOWER the per-item USD cap below the coordinator.yaml value "+
			"(e.g. spend $1 live-verifying on a throwaway item); a value "+
			"above the coordinator cap is rejected, not honored")
	spawnCmd.Flags().Bool("dry-run", false,
		"print the resolved launch plan and exit without launching")
	spawnChildCmd := &cobra.Command{
		Use:   "child <item>",
		Short: "Materialize a child agent registration under the calling identity",
		Long: "Spawn a child agent that inherits the caller's identity as parent.\n" +
			"V1 supports same-item spawn only — the parent's claim covers the\n" +
			"child's work, no new worktree is created. Different-item spawn is\n" +
			"a tracked follow-up.\n\n" +
			"Prints `<child-id><TAB><pid>` on stdout so the caller can pipe the\n" +
			"id into a subprocess launcher (e.g. `AS_AGENT_ID=$(...) st run`).",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SpawnChild(appStore, appCfg, command.SpawnChildOpts{Item: args[0]})
		},
	}
	spawnCmd.AddCommand(spawnChildCmd)
	root.AddCommand(spawnCmd)

	// --- Coordinate ---
	// `st coordinate` (T-363): the Shape-3 coordinator loop. Picks the
	// next approved/unblocked queue item, spawns ONE budget-capped worker
	// (reusing `st spawn`), supervises it on substrate ground truth,
	// applies the B1/C2/D2 stall heuristics against the .as/coordinator.yaml
	// autonomy boundary, and on any contract-§7 predicate emits a deduped,
	// substrate-durable escalation and STOPS rather than exceed the
	// boundary. Single in-flight worker; parallelism_cap is a hard ceiling
	// (concurrent fan-out is T-364).
	coordinateCmd := &cobra.Command{
		Use:   "coordinate",
		Short: "Pick the next queue item, spawn a budget-capped worker, and supervise it",
		Long: "Pick the next approved/unblocked queue item, spawn ONE\n" +
			"budget-capped reasoning worker (via `st spawn`), supervise it\n" +
			"through the observability substrate (registry PID / session\n" +
			"JSONL / changelog — never worker self-report), apply the\n" +
			"B1/C2/D2 stall heuristics against theraprac-workspace/.as/\n" +
			"coordinator.yaml, and on any contract-§7 predicate file a\n" +
			"deduped, dependency-linked blocker + durable record and STOP\n" +
			"rather than exceed the autonomy boundary.\n\n" +
			"--dry-run resolves the boundary + the next pick and prints the\n" +
			"would-be plan, launching/escalating nothing (contract §11 read-only).",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			once, _ := cmd.Flags().GetBool("once")
			maxItems, _ := cmd.Flags().GetInt("max-items")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			budget, _ := cmd.Flags().GetFloat64("budget")
			interval, _ := cmd.Flags().GetDuration("interval")
			maxIdle, _ := cmd.Flags().GetDuration("max-idle")
			exitCode = command.Coordinate(appStore, appCfg, command.CoordinateOpts{
				Once:           once,
				MaxItems:       maxItems,
				DryRun:         dryRun,
				BudgetOverride: budget,
				PollInterval:   interval,
				PollMaxIdle:    maxIdle,
			})
		},
	}
	coordinateCmd.Flags().Bool("once", false,
		"process exactly one item (pick→spawn→supervise→escalate|advance) then exit")
	coordinateCmd.Flags().Int("max-items", 1,
		"max items to process before exiting; 0 = unbounded (long-running coordinator)")
	coordinateCmd.Flags().Bool("dry-run", false,
		"resolve the boundary + next pick and print the plan; launch/escalate nothing")
	coordinateCmd.Flags().Float64("budget", 0,
		"LOWER the per-item USD cap for the spawned worker (forwarded to `st spawn`; "+
			"a value above the coordinator.yaml cap is rejected)")
	coordinateCmd.Flags().Duration("interval", 20*time.Second,
		"base supervision poll cadence (backs off geometrically when idle)")
	coordinateCmd.Flags().Duration("max-idle", 5*time.Minute,
		"backoff cap for the idle supervision cadence")
	root.AddCommand(coordinateCmd)

	// --- Workflow commands ---

	startCmd := &cobra.Command{
		Use:   "start <id>",
		Short: "Activate an item and create worktree branches",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			slug, _ := cmd.Flags().GetString("slug")
			repos, _ := cmd.Flags().GetStringSlice("repos")
			noPush, _ := cmd.Flags().GetBool("no-push")
			force, _ := cmd.Flags().GetBool("force")
			addToSprint, _ := cmd.Flags().GetBool("add-to-sprint")
			ackDrift, _ := cmd.Flags().GetString("ack-drift")
			exitCode = command.Start(appStore, appCfg, args[0], command.StartOpts{
				Slug: slug, Repos: repos, NoPush: noPush, Force: force,
				AddToSprint: addToSprint, AckDrift: ackDrift,
			})
		},
	}
	startCmd.Flags().String("slug", "", "`SLUG` for branch name (single segment). Example: --slug cost-ground-truth → fix/I-579-cost-ground-truth. A leading <type>/<id>- prefix is stripped if present, so fix/I-579-cost-ground-truth is also accepted.")
	startCmd.Flags().StringSlice("repos", nil, "repos to create worktrees for")
	startCmd.Flags().Bool("no-push", false, "skip auto-push onto the work stack")
	startCmd.Flags().Bool("force", false, "bypass the I-490 queue-approval gate and the I-681 sprint-inheritance gate (logs to changelog). NOTE: does NOT bypass the I-711 freshness gate — use --ack-drift for Drift; Stale requires re-prep.")
	startCmd.Flags().Bool("add-to-sprint", false, "resolve the I-681 sprint-inheritance gate by adding this item to the active sprint of an in-progress item it blocks")
	startCmd.Flags().String("ack-drift", "", "operator-supplied one-line note acknowledging plan drift surfaced by the I-711 freshness gate; proceeds activation despite drift findings")
	root.AddCommand(startCmd)

	// I-711: `st cache prune` removes freshness cache entries older
	// than the configured max age so the on-disk cache stays
	// bounded. Default max age is 30 days.
	cacheCmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage on-disk cache directories (freshness verdicts, etc.)",
	}
	cachePruneCmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove freshness cache entries older than --max-age (default 30 days)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			maxAge, _ := cmd.Flags().GetDuration("max-age")
			pruned, err := freshness.PruneCache(appCfg.Root(), maxAge, time.Now())
			if err != nil {
				fmt.Fprintf(os.Stderr, "cache prune: %v\n", err)
				exitCode = 1
				return
			}
			fmt.Printf("pruned %d freshness cache entries (max-age %s)\n", pruned, maxAge)
		},
	}
	cachePruneCmd.Flags().Duration("max-age", 30*24*time.Hour, "remove cache entries older than this duration")
	cacheCmd.AddCommand(cachePruneCmd)
	root.AddCommand(cacheCmd)

	closeCmd := &cobra.Command{
		Use:   "close <id> <resolution>",
		Short: "Close an item with gate enforcement",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			reason, _ := cmd.Flags().GetString("reason")
			force, _ := cmd.Flags().GetBool("force")
			exitCode = command.Close(appStore, appCfg, args[0], args[1], command.CloseOpts{Reason: reason, Force: force})
		},
	}
	closeCmd.Flags().String("reason", "", "reason for closing (required for abandon)")
	closeCmd.Flags().Bool("force", false, "bypass gate checks")
	root.AddCommand(closeCmd)

	classifyCmd := &cobra.Command{
		Use:   "classify <id>",
		Short: "Run the binary autonomy classifier on an item (green/red verdict)",
		Long: `Run the binary autonomy classifier on an item.

The classifier returns a binary verdict: green (auto-run the full
delivery loop) or red (stop and surface to operator). Verdict and
reason are persisted under the item's classification.* fields.

Evaluation order:
  1. Static deny-list (theraprac-infra/state/, RBAC handlers, IAM/
     secrets terraform, *.pem/*.key) — any match forces red.
  2. Cached prior verdict (cache key = sha256(inputs)) — skipped with
     --force.
  3. LLM judge — a one-shot ` + "`claude -p`" + ` subprocess that emits a
     JSON {verdict, reason, confidence} envelope.

Use --dry-run to print the assembled prompt without calling the model
(useful for prompt iteration without burning tokens).`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			force, _ := cmd.Flags().GetBool("force")
			files, _ := cmd.Flags().GetStringSlice("files")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			exitCode = command.Classify(appStore, appCfg, args[0], command.ClassifyOpts{
				Force:  force,
				Files:  files,
				DryRun: dryRun,
			})
		},
	}
	classifyCmd.Flags().Bool("force", false, "bypass cache; re-run even when input_hash matches")
	classifyCmd.Flags().StringSlice("files", nil, "comma-separated touched-file paths (overrides manifest-derived list)")
	classifyCmd.Flags().Bool("dry-run", false, "print the assembled prompt and exit without calling the model")
	root.AddCommand(classifyCmd)

	decideCmd := &cobra.Command{
		Use:   "decide <id> <approve|reject|defer>",
		Short: "Resolve an item paused in awaiting_decision (binary autonomy loop)",
		Long: `Resolve a paused item the binary autonomy loop has handed off.

The classifier flips an item to awaiting_decision when it returns red.
The agent halts there with a decision card describing risk, files
touched, and the ask. ` + "`st decide`" + ` is how the operator clears that pause.

Actions:
  approve  — back to active; agent resumes the delivery loop
  reject   — close as abandoned (requires --reason)
  defer    — back to queued; classification cache cleared so the next
             ` + "`st classify`" + ` re-runs

Every decision is appended to .as/classify-corpus.jsonl. The classifier
reads recent entries as in-context examples on subsequent calls so the
verdict drifts toward what the operator actually accepts.`,
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			id, action := args[0], args[1]
			reason, _ := cmd.Flags().GetString("reason")
			exitCode = command.Decide(appStore, appCfg, id, command.DecideOpts{
				Action: command.DecideAction(action),
				Reason: reason,
			})
		},
	}
	decideCmd.Flags().String("reason", "", "operator reason (required for reject; recorded in changelog + corpus)")
	root.AddCommand(decideCmd)

	readyCmd := &cobra.Command{
		Use:   "ready",
		Short: "Show unblocked items ready to start",
		Run: func(cmd *cobra.Command, args []string) {
			typeF, _ := cmd.Flags().GetString("type")
			tagF, _ := cmd.Flags().GetString("tag")
			limit, _ := cmd.Flags().GetInt("limit")
			exitCode = command.Ready(appStore, appCfg, command.ReadyOpts{Type: typeF, Tag: tagF, Limit: limit})
		},
	}
	readyCmd.Flags().StringP("type", "T", "", "filter by type")
	readyCmd.Flags().String("tag", "", "filter by tag")
	readyCmd.Flags().IntP("limit", "n", 0, "max items to show")
	root.AddCommand(readyCmd)

	recommendCmd := &cobra.Command{
		Use:   "recommend",
		Short: "Rank workable items with an inspectable 'why this next' rationale",
		Long: "Score the workable items and print them ranked, each with a\n" +
			"decomposed rationale (priority · unblock leverage · sprint\n" +
			"completion · goal weight · age). Priority dominates by construction;\n" +
			"the other factors only order within a priority band.\n\n" +
			"Default candidate set is the PLANNING view (ready + unblocked\n" +
			"+ unassigned). --queue switches to the DISPATCH view (queue +\n" +
			"EligibleForDispatch) — exactly what `st coordinate` selects,\n" +
			"so operator and coordinator read the identical rationale. This\n" +
			"is the CLI brain the coordinator's dispatch surfaces as text\n" +
			"(operating-contract §4.2 — never an opaque choice).",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			jsonOut, _ := cmd.Flags().GetBool("json")
			top, _ := cmd.Flags().GetInt("top")
			scope, _ := cmd.Flags().GetString("scope")
			queue, _ := cmd.Flags().GetBool("queue")
			brief, _ := cmd.Flags().GetBool("brief")
			exitCode = command.Recommend(appStore, appCfg, command.RecommendOpts{
				JSON: jsonOut, Top: top, Scope: scope, Queue: queue, Brief: brief,
			})
		},
	}
	recommendCmd.Flags().Bool("json", false,
		"machine output (the stable contract the T-348 TUI planning panel consumes)")
	recommendCmd.Flags().IntP("top", "n", 10, "max ranked rows to print")
	recommendCmd.Flags().String("scope", "all",
		"candidate scope: all | sprint (members of an active sprint only)")
	recommendCmd.Flags().Bool("queue", false,
		"score the DISPATCH view (queue + EligibleForDispatch) — what `st coordinate` sees")
	recommendCmd.Flags().Bool("brief", false,
		"one-line render: <ID> p<N>  <title> — <rationale> (used by `st next`)")
	root.AddCommand(recommendCmd)

	nextCmd := &cobra.Command{
		Use:   "next",
		Short: "Print the single top-ranked workable item (alias: recommend --top 1 --brief)",
		Long: "Alias for `st recommend --top 1 --brief`: scores the PLANNING view\n" +
			"and prints the top pick as one line — ID, priority, title, and rationale.\n" +
			"Goal weight, unblock leverage, sprint pressure, and age all contribute;\n" +
			"priority dominates by construction.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Recommend(appStore, appCfg, command.RecommendOpts{Top: 1, Brief: true})
		},
	}
	root.AddCommand(nextCmd)

	finishCmd := &cobra.Command{
		Use:   "finish [id]",
		Short: "Clean up worktrees after merge",
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			listAll, _ := cmd.Flags().GetBool("list")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			force, _ := cmd.Flags().GetBool("force")
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			if !listAll && id == "" {
				cmd.Usage()
				exitCode = 2
				return
			}
			exitCode = command.Finish(appStore, appCfg, id, command.FinishOpts{DryRun: dryRun, Force: force, ListAll: listAll})
		},
	}
	finishCmd.Flags().Bool("dry-run", false, "show what would be cleaned up")
	finishCmd.Flags().Bool("force", false, "force cleanup even with uncommitted changes")
	finishCmd.Flags().BoolP("list", "l", false, "list active worktrees")
	root.AddCommand(finishCmd)

	releaseCmd := &cobra.Command{
		Use:   "release <id>",
		Short: "Unassign an item from the current agent",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Release(appStore, appCfg, args[0])
		},
	}
	root.AddCommand(releaseCmd)

	unlockCmd := &cobra.Command{
		Use:   "unlock <id>",
		Short: "Force-release the item lock (use when a pipeline is stuck)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			id := args[0]
			if !store.IsLocked(appCfg, id) {
				fmt.Fprintf(os.Stderr, "%s is not locked\n", id)
				exitCode = 1
				return
			}
			store.UnlockItem(appCfg, id)
			fmt.Printf("Unlocked %s\n", id)
		},
	}
	root.AddCommand(unlockCmd)

	commitCmd := &cobra.Command{
		Use:   "commit <id> <message>",
		Short: "Record a commit against an item",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Commit(appStore, appCfg, args[0], args[1])
		},
	}
	root.AddCommand(commitCmd)

	prCmd := &cobra.Command{
		Use:   "pr <id>",
		Short: "Record PR manifest with file analysis",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			repo, _ := cmd.Flags().GetString("repo")
			prNum, _ := cmd.Flags().GetInt("pr")
			exitCode = command.PR(appStore, appCfg, args[0], command.PROpts{Repo: repo, PRNumber: prNum})
		},
	}
	prCmd.Flags().String("repo", "", "short repo name (e.g. api, web)")
	prCmd.Flags().Int("pr", 0, "PR number")
	_ = prCmd.MarkFlagRequired("repo")
	_ = prCmd.MarkFlagRequired("pr")
	root.AddCommand(prCmd)

	testRecordCmd := &cobra.Command{
		Use:   "test <id> <suite>",
		Short: "Record or execute a test suite for an item",
		Long:  "Without --run: records a manual test pass. With --run: executes the suite command, captures output, uploads evidence. With --skip <reason>: marks a scope suite as intentionally skipped (scope suites only — required suites cannot be skipped).",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			run, _ := cmd.Flags().GetBool("run")
			cov, _ := cmd.Flags().GetBool("coverage")
			skip, _ := cmd.Flags().GetString("skip")
			agent, _ := cmd.Flags().GetString("agent")
			exitCode = command.TestRecord(appStore, appCfg, args[0], args[1], command.TestRecordOpts{
				Run: run, Coverage: cov, Skip: skip, Agent: agent,
			})
		},
	}
	testRecordCmd.Flags().Bool("run", false, "execute the suite command and capture evidence")
	testRecordCmd.Flags().Bool("coverage", false, "enforce per-file coverage thresholds (requires --run)")
	testRecordCmd.Flags().String("skip", "", "mark a scope suite as intentionally skipped with the given reason (scope suites only)")
	testRecordCmd.Flags().String("agent", "", "agent workspace/runtime to target when executing a suite")
	root.AddCommand(testRecordCmd)

	revertCmd := &cobra.Command{
		Use:   "revert <id> [step]",
		Short: "Revert item to pre-step snapshot state",
		Long:  `Restore an item to its state before the most recent snapshot. If step is given, reverts to the snapshot from that specific step (e.g., "plan_review", "implement").`,
		Args:  cobra.RangeArgs(1, 2),
		Run: func(cmd *cobra.Command, args []string) {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			step := ""
			if len(args) > 1 {
				step = args[1]
			}
			exitCode = command.Revert(appStore, appCfg, args[0], step, dryRun)
		},
	}
	revertCmd.Flags().Bool("dry-run", false, "show what would be reverted without making changes")
	root.AddCommand(revertCmd)

	// --- Read/query commands ---

	statusCmd := &cobra.Command{
		Use:   "status [id]",
		Short: "Dashboard overview or single-item detail",
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			issues, _ := cmd.Flags().GetBool("issues")
			tasks, _ := cmd.Flags().GetBool("tasks")
			recent, _ := cmd.Flags().GetBool("recent")
			all, _ := cmd.Flags().GetBool("all")
			completed, _ := cmd.Flags().GetBool("completed")
			check, _ := cmd.Flags().GetBool("check")
			tag, _ := cmd.Flags().GetString("tag")
			epic, _ := cmd.Flags().GetString("epic")
			noRefresh, _ := cmd.Flags().GetBool("no-refresh")
			sprints, _ := cmd.Flags().GetBool("sprints")
			sprintsID, _ := cmd.Flags().GetString("id")
			sprintsAll, _ := cmd.Flags().GetBool("sprints-all")
			sprintsClosed, _ := cmd.Flags().GetBool("sprints-closed")
			sprintsRunning, _ := cmd.Flags().GetBool("sprints-running")
			// T-329: query/sort/filter on the unified status surface.
			filters, _ := cmd.Flags().GetStringSlice("filter")
			sortStr, _ := cmd.Flags().GetString("sort")
			since, _ := cmd.Flags().GetString("since")
			jsonOut, _ := cmd.Flags().GetBool("json")
			// T-347: --global overrides agent-scoping inside an agent
			// workspace. --all also implies AgentAll so the operator's
			// "show me everything" knob is consistent.
			globalView, _ := cmd.Flags().GetBool("global")
			// T-377 (I-712): per-agent 4-dimension rollup. --arc filters (T-378).
			me, _ := cmd.Flags().GetBool("me")
			agentOverride, _ := cmd.Flags().GetString("agent")
			arc, _ := cmd.Flags().GetString("arc")
			exitCode = command.Status(appStore, appCfg, id, command.StatusOpts{
				Issues: issues, Tasks: tasks, Recent: recent,
				All: all, Completed: completed, Check: check,
				Tag: tag, Epic: epic, NoRefresh: noRefresh,
				Sprints: sprints, SprintsID: sprintsID,
				SprintsAll: sprintsAll, SprintsClosed: sprintsClosed,
				SprintsRunning: sprintsRunning,
				Filters:        filters, Sort: sortStr, Since: since, JSON: jsonOut,
				AgentAll: globalView || all,
				Me:       me, Agent: agentOverride, Arc: arc,
			})
		},
	}
	statusCmd.Flags().BoolP("issues", "i", false, "show open issues")
	statusCmd.Flags().BoolP("tasks", "t", false, "show queued tasks")
	statusCmd.Flags().BoolP("recent", "r", false, "show recently closed")
	statusCmd.Flags().BoolP("all", "a", false, "show all sections (excludes completed)")
	statusCmd.Flags().BoolP("completed", "d", false, "show completed items")
	statusCmd.Flags().BoolP("check", "c", false, "run validation checks")
	statusCmd.Flags().String("tag", "", "filter queued tasks by tag")
	statusCmd.Flags().String("epic", "", "filter queued tasks by epic ID")
	statusCmd.Flags().Bool("no-refresh", false, "skip the auto-pull from origin (for scripts/CI/hot loops)")
	statusCmd.Flags().Bool("sprints", false, "show tabular epic/sprint progress view (T-325; replaces `st run status`)")
	statusCmd.Flags().String("id", "", "with --sprints: filter to a single epic or sprint by slug")
	statusCmd.Flags().Bool("sprints-all", false, "with --sprints: include archived")
	statusCmd.Flags().Bool("sprints-closed", false, "with --sprints: only closed/archived")
	statusCmd.Flags().Bool("sprints-running", false, "with --sprints: only sprints with a running pipeline")
	// T-329: query/sort/filter on the unified status surface.
	statusCmd.Flags().StringSlice("filter", nil,
		"filter spec key:value, repeatable (keys: agent, assigned, status, type, tag, priority, epic, sprint)")
	statusCmd.Flags().String("sort", "",
		"sort field[,asc|desc] (fields: cost, time, lines, last_touched, priority, id)")
	// T-377 (I-712): per-agent 4-dimension rollup.
	statusCmd.Flags().Bool("me", false,
		"per-agent rollup: DONE / IN-FLIGHT / NEEDS-YOU / PROPOSED-NEXT (--since window, default 24h)")
	statusCmd.Flags().String("agent", "",
		"with --me: override the agent id (default: cfg.Identity().ID)")
	// T-378 (I-712): filter the --me rollup to one arc.
	statusCmd.Flags().String("arc", "",
		"with --me: filter every section to items in the given arc")
	statusCmd.Flags().String("since", "",
		"only items touched within this duration (e.g. 7d, 24h, 30m)")
	statusCmd.Flags().Bool("json", false, "emit JSON instead of human-readable text")
	statusCmd.Flags().Bool("global", false, "show items from every agent (default inside an agent workspace is to scope to that agent)")
	root.AddCommand(statusCmd)

	statsCmd := &cobra.Command{
		Use:   "stats",
		Short: "Show item statistics and counts",
		Run: func(cmd *cobra.Command, args []string) {
			jsonF, _ := cmd.Flags().GetBool("json")
			timeF, _ := cmd.Flags().GetBool("time")
			exitCode = command.Stats(appStore, appCfg, command.StatsOpts{JSON: jsonF, Time: timeF})
		},
	}
	statsCmd.Flags().Bool("json", false, "output as JSON")
	statsCmd.Flags().Bool("time", false, "include time tracking")

	// st stats meta — T-327: per-agent meta-work readout from orphan.log.
	statsMetaCmd := &cobra.Command{
		Use:   "meta",
		Short: "Show meta-work (orphan-log turns) grouped by agent or reason",
		Long: "Reads .as/sessions/orphan.log and aggregates per-agent " +
			"deliberation/between-item turns. Use --agent self to filter to " +
			"the calling agent; --since 7d for a time window.",
		Run: func(cmd *cobra.Command, args []string) {
			agent, _ := cmd.Flags().GetString("agent")
			since, _ := cmd.Flags().GetString("since")
			by, _ := cmd.Flags().GetString("by")
			jsonF, _ := cmd.Flags().GetBool("json")
			exitCode = command.StatsMeta(appCfg, command.StatsMetaOpts{
				Agent: agent,
				Since: since,
				By:    by,
				JSON:  jsonF,
			})
		},
	}
	statsMetaCmd.Flags().String("agent", "", "filter to one agent id (or 'self' for the calling agent)")
	statsMetaCmd.Flags().String("since", "", "time window like '7d', '24h', '30m' (empty = all time)")
	statsMetaCmd.Flags().String("by", "agent", "group by 'agent' (default) or 'reason'")
	statsMetaCmd.Flags().Bool("json", false, "output as JSON")
	statsCmd.AddCommand(statsMetaCmd)

	root.AddCommand(statsCmd)

	depCmd := &cobra.Command{
		Use:   "dep",
		Short: "Manage dependencies between items",
	}
	depTreeCmd := &cobra.Command{
		Use:   "tree <id>",
		Short: "Show dependency tree for an item",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			depth, _ := cmd.Flags().GetInt("depth")
			exitCode = command.DepTree(appStore, appCfg, args[0], command.DepTreeOpts{Depth: depth})
		},
	}
	depTreeCmd.Flags().IntP("depth", "d", 10, "max tree depth")
	depGraphCmd := &cobra.Command{
		Use:   "graph",
		Short: "Show full dependency graph",
		Run: func(cmd *cobra.Command, args []string) {
			jsonF, _ := cmd.Flags().GetBool("json")
			exitCode = command.DepGraph(appStore, appCfg, command.DepGraphOpts{JSON: jsonF})
		},
	}
	depGraphCmd.Flags().Bool("json", false, "output as JSON")
	depAddCmd := &cobra.Command{
		Use:   "add <id> <dep-id>",
		Short: "Add a dependency: <id> will be blocked by <dep-id>",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.DepAdd(appStore, appCfg, args[0], args[1])
		},
	}
	depRmCmd := &cobra.Command{
		Use:   "rm <id> <dep-id>",
		Short: "Remove a dependency between two items",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.DepRm(appStore, appCfg, args[0], args[1])
		},
	}
	depCmd.AddCommand(depTreeCmd, depGraphCmd, depAddCmd, depRmCmd)
	root.AddCommand(depCmd)

	primeCmd := &cobra.Command{
		Use:   "prime",
		Short: "Export context for LLM consumption",
		Run: func(cmd *cobra.Command, args []string) {
			if resume, _ := cmd.Flags().GetBool("resume"); resume {
				// I-679: --resume regenerates the cross-session resume
				// prompt for the active/stack-top item from the live
				// changelog (never a stored breadcrumb).
				exitCode = command.Resume(appStore, appCfg, command.ResumeOpts{})
				return
			}
			format, _ := cmd.Flags().GetString("format")
			compact, _ := cmd.Flags().GetBool("compact")
			globalView, _ := cmd.Flags().GetBool("global")
			exitCode = command.Prime(appStore, appCfg, command.PrimeOpts{
				Format: format, Compact: compact, AgentAll: globalView,
			})
		},
	}
	primeCmd.Flags().String("format", "markdown", "output format (markdown, json)")
	primeCmd.Flags().Bool("compact", false, "minimal output for hook injection")
	primeCmd.Flags().Bool("global", false, "show items from every agent (default inside an agent workspace is to scope to that agent)")
	primeCmd.Flags().Bool("resume", false, "regenerate the cross-session resume prompt for the active/stack-top item (I-679)")
	root.AddCommand(primeCmd)

	// st resume [<id>] — I-679 cross-session execution & decision replay.
	resumeCmd := &cobra.Command{
		Use:   "resume [id]",
		Short: "Regenerate the session-resume prompt from the live changelog",
		Long: "Rebuilds a fresh session's starting context for a long-running item\n" +
			"from the LIVE changelog — typed decision/exec/transition replay, the\n" +
			"plan, declarative state, and a self-attestation banner that loudly\n" +
			"flags any gap between the recorded exec tape and git ground truth.\n" +
			"Never stores or trusts a snapshot. No argument ⇒ stack top, then\n" +
			"first active item.",
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			opts := command.ResumeOpts{}
			if len(args) == 1 {
				opts.ID = args[0]
			}
			exitCode = command.Resume(appStore, appCfg, opts)
		},
	}
	root.AddCommand(resumeCmd)

	// st capture-decision — I-679 Phase B native-structured decision
	// capture. Hidden: this is hook-invoked machine glue (the
	// capture-decision.sh PostToolUse hook for AskUserQuestion /
	// ExitPlanMode), not a human verb. It exists so the changelog write
	// stays in one tested place rather than being reimplemented in bash.
	captureDecisionCmd := &cobra.Command{
		Use:    "capture-decision",
		Short:  "Record a native-structured decision from a PostToolUse hook (I-679)",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			id, _ := cmd.Flags().GetString("item")
			trigger, _ := cmd.Flags().GetString("trigger")
			reason, _ := cmd.Flags().GetString("reason")
			exitCode = command.CaptureDecision(appStore, appCfg, command.CaptureDecisionOpts{
				ID:      id,
				Trigger: trigger,
				Reason:  reason,
			})
		},
	}
	captureDecisionCmd.Flags().String("item", "", "explicit item id; empty ⇒ stack top, then this agent's first active item")
	captureDecisionCmd.Flags().String("trigger", "", "originating channel (ask_user_question | exit_plan_mode)")
	captureDecisionCmd.Flags().String("reason", "", "verbatim decision text; empty ⇒ nothing to capture")
	root.AddCommand(captureDecisionCmd)

	// st extract-decisions — I-679 Phase C lossy backstop. Hidden:
	// hook-invoked machine glue (precompact.sh / stop-extract.sh), not a
	// human verb. Mines the about-to-be-summarized transcript for prose
	// forks and appends the uncovered ones as source=extracted, after
	// reconciling against existing structured/extracted entries.
	extractDecisionsCmd := &cobra.Command{
		Use:    "extract-decisions",
		Short:  "Recover prose decision forks from a transcript window (I-679 Phase C)",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			tp, _ := cmd.Flags().GetString("transcript")
			id, _ := cmd.Flags().GetString("item")
			trigger, _ := cmd.Flags().GetString("trigger")
			session, _ := cmd.Flags().GetString("session")
			exitCode = command.ExtractDecisions(appStore, appCfg, command.ExtractDecisionsOpts{
				TranscriptPath: tp,
				ID:             id,
				Trigger:        trigger,
				Session:        session,
			})
		},
	}
	extractDecisionsCmd.Flags().String("transcript", "", "path to the session JSONL (from the PreCompact/Stop hook)")
	extractDecisionsCmd.Flags().String("item", "", "explicit item id; empty ⇒ stack top, then this agent's first active item")
	extractDecisionsCmd.Flags().String("trigger", "", "originating hook (precompact | precompact_<t> | stop)")
	extractDecisionsCmd.Flags().String("session", "", "session_id from the hook stdin (tags entries + the stop finalize marker)")
	root.AddCommand(extractDecisionsCmd)

	redCmd := &cobra.Command{
		Use:   "red",
		Short: "List items awaiting an operator decision (binary autonomy loop)",
		Long: `List items currently parked in awaiting_decision with each
item's decision card rendered inline.

From inside an agent workspace, defaults to the current agent's items
only — peer-agent reds stay hidden so the operator's attention isn't
fragmented. ` + "`--all`" + ` shows every agent's awaiting items, plus the
` + "`owner:`" + ` line on each so the operator can tell who owns what.

Resolve any listed item with ` + "`st decide <id> approve|reject|defer`" + `.`,
		Run: func(cmd *cobra.Command, args []string) {
			all, _ := cmd.Flags().GetBool("all")
			exitCode = command.Red(appStore, appCfg, command.RedOpts{AgentAll: all})
		},
	}
	redCmd.Flags().Bool("all", false, "show awaiting items from every agent (not just the current agent)")
	root.AddCommand(redCmd)

	logCmd := &cobra.Command{
		Use:   "log [id]",
		Short: "View changelog for an item or all items",
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			limit, _ := cmd.Flags().GetInt("limit")
			exitCode = command.Log(appStore, appCfg, id, command.LogOpts{Limit: limit})
		},
	}
	logCmd.Flags().IntP("limit", "n", 0, "max entries to show")
	root.AddCommand(logCmd)

	// --- Epic/Sprint/Note ---

	epicCmd := &cobra.Command{
		Use:   "epic",
		Short: "Manage epics",
	}
	epicCmd.AddCommand(&cobra.Command{
		Use:   "create <title>",
		Short: "Create a new epic",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.EpicCreate(appCfg, args[0])
		},
	})
	epicCmd.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List all epics",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.EpicList(appStore, appCfg)
		},
	})
	epicCmd.AddCommand(&cobra.Command{
		Use:   "archive <epic-id>",
		Short: "Archive an epic (all sprints must be archived/completed)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.EpicArchive(appStore, appCfg, args[0])
		},
	})
	epicCmd.AddCommand(&cobra.Command{
		Use:   "delete <epic-id>",
		Short: "Delete an epic with no sprints",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.EpicDelete(appCfg, args[0])
		},
	})
	epicCmd.AddCommand(&cobra.Command{
		Use:   "move <epic-id> <position>",
		Short: "Set the priority of an epic (1 = highest); renumbers others",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			pos, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "position must be a number")
				exitCode = 2
				return
			}
			exitCode = command.EpicMove(appStore, appCfg, args[0], pos)
		},
	})
	root.AddCommand(epicCmd)

	// T-407: Goal as first-class st type.
	goalCmd := &cobra.Command{
		Use:   "goal",
		Short: "Manage strategic goals",
	}
	goalCreateCmd := &cobra.Command{
		Use:   "create <title>",
		Short: "Create a new goal in draft status",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			weight, _ := cmd.Flags().GetInt("weight")
			exitCode = command.GoalCreate(appStore, appCfg, args[0], weight)
		},
	}
	goalCreateCmd.Flags().Int("weight", 0, "strategic weight 1-100 (active goals must sum to ≤100)")
	_ = goalCreateCmd.MarkFlagRequired("weight")
	goalCmd.AddCommand(goalCreateCmd)
	goalCmd.AddCommand(&cobra.Command{
		Use:   "activate <goal-id>",
		Short: "Transition a goal from draft to active (enforces ≤100 weight sum)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.GoalActivate(appStore, appCfg, args[0])
		},
	})
	goalCmd.AddCommand(&cobra.Command{
		Use:   "mark-met <goal-id>",
		Short: "Transition a goal from active to met (terminal)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.GoalMarkMet(appStore, appCfg, args[0])
		},
	})
	goalDropCmd := &cobra.Command{
		Use:   "drop <goal-id>",
		Short: "Transition a goal to dropped (terminal); requires --reason",
		Long: `Drop a goal and record why it was abandoned. Valid reasons:
  superseded       — a newer goal supersedes this one
  premise-invalid  — the original premise no longer holds
  out-of-strategy  — outside current strategic direction
  duplicate        — covered by another goal
  unactionable     — cannot be driven to completion

Note: "aged" is not a valid reason — goals are dropped by deliberate decision, not by time.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			reason, _ := cmd.Flags().GetString("reason")
			exitCode = command.GoalDrop(appStore, appCfg, args[0], reason)
		},
	}
	goalDropCmd.Flags().String("reason", "", "drop reason (superseded|premise-invalid|out-of-strategy|duplicate|unactionable)")
	_ = goalDropCmd.MarkFlagRequired("reason")
	goalCmd.AddCommand(goalDropCmd)
	goalCmd.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List all goals grouped by lifecycle",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.GoalList(appStore, appCfg)
		},
	})

	// T-413: goal review — active-goal health + orphan queue reconciliation.
	goalReviewCmd := &cobra.Command{
		Use:   "review",
		Short: "Show active-goal health and reconcile orphan queue items",
		Run: func(cmd *cobra.Command, args []string) {
			countF, _ := cmd.Flags().GetBool("count")
			listF, _ := cmd.Flags().GetBool("list")
			exitCode = command.GoalReview(appStore, appCfg, command.GoalReviewOpts{
				Count: countF,
				List:  listF,
			})
		},
	}
	goalReviewCmd.Flags().Bool("count", false, "print orphan count only and exit 0")
	goalReviewCmd.Flags().Bool("list", false, "print one orphan ID per line and exit 0")
	goalCmd.AddCommand(goalReviewCmd)
	root.AddCommand(goalCmd)

	// T-410: item goals subcommands — item goals add/remove
	itemGoalsCmd := &cobra.Command{Use: "goals", Short: "Manage item goal membership"}
	itemGoalsCmd.AddCommand(&cobra.Command{
		Use:   "add <item-id> <goal-id...>",
		Short: "Add goal IDs to an item's goals list",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.ItemGoalsAdd(appStore, appCfg, args[0], args[1:])
		},
	})
	itemGoalsCmd.AddCommand(&cobra.Command{
		Use:   "remove <item-id> <goal-id...>",
		Short: "Remove goal IDs from an item's goals list",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.ItemGoalsRemove(appStore, appCfg, args[0], args[1:])
		},
	})
	itemCmd := &cobra.Command{Use: "item", Short: "Manage item metadata"}
	itemCmd.AddCommand(itemGoalsCmd)
	root.AddCommand(itemCmd)

	// T-378 (I-712): strategic-work-stream arc tagging. Any name an
	// operator uses IS the arc — no registry, no predefined list.
	arcCmd := &cobra.Command{
		Use:   "arc",
		Short: "Strategic work-stream tagging (sibling of sprint/epic)",
	}
	arcCmd.AddCommand(&cobra.Command{
		Use:   "add <name> <id…>",
		Short: "Tag items with an arc (overwrites prior arc)",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.ArcAdd(appStore, appCfg, args[0], args[1:])
		},
	})
	arcCmd.AddCommand(&cobra.Command{
		Use:   "rm <id…>",
		Short: "Clear the arc on items",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.ArcRm(appStore, appCfg, args)
		},
	})
	arcShowCmd := &cobra.Command{
		Use:   "show <name>",
		Short: "List items in an arc",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			jsonOut, _ := cmd.Flags().GetBool("json")
			exitCode = command.ArcShow(appStore, appCfg, args[0], jsonOut)
		},
	}
	arcShowCmd.Flags().Bool("json", false, "machine output")
	arcCmd.AddCommand(arcShowCmd)

	arcListCmd := &cobra.Command{
		Use:     "list",
		Short:   "List arcs in use (with counts)",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			jsonOut, _ := cmd.Flags().GetBool("json")
			exitCode = command.ArcList(appStore, appCfg, jsonOut)
		},
	}
	arcListCmd.Flags().Bool("json", false, "machine output")
	arcCmd.AddCommand(arcListCmd)
	root.AddCommand(arcCmd)

	sprintCmd := &cobra.Command{
		Use:   "sprint",
		Short: "Manage sprints within epics",
	}
	sprintCreateCmd := &cobra.Command{
		Use:   "create <epic-id> <title>",
		Short: "Create a sprint under <epic-id>",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			desc, _ := cmd.Flags().GetString("description")
			exitCode = command.SprintCreate(appCfg, args[0], args[1], command.SprintCreateOpts{
				Description: desc,
			})
		},
	}
	sprintCreateCmd.Flags().String("description", "", "optional free-form goal/intent for the sprint (I-405)")
	sprintListCmd := &cobra.Command{
		Use:     "list [epic-id]",
		Short:   "List sprints",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			epicID, _ := cmd.Flags().GetString("epic")
			if epicID == "" && len(args) > 0 {
				epicID = args[0]
			}
			exitCode = command.SprintList(appCfg, epicID)
		},
	}
	sprintListCmd.Flags().String("epic", "", "filter by epic ID")
	sprintAddCmd := &cobra.Command{
		Use:   "add <sprint-id> <item-ids...>",
		Short: "Add items to a sprint",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintAdd(appStore, appCfg, args[0], args[1:])
		},
	}
	sprintRmCmd := &cobra.Command{
		Use:   "rm <sprint-id> <item-id>",
		Short: "Remove an item from a sprint",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintRm(appStore, appCfg, args[0], args[1])
		},
	}
	sprintShowCmd := &cobra.Command{
		Use:   "show <sprint-id>",
		Short: "Show sprint details and item status",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintShow(appStore, appCfg, args[0])
		},
	}
	sprintPlanCmd := &cobra.Command{
		Use:   "plan <sprint-id>",
		Short: "Analyze sprint dependency graph and parallel groups",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintPlan(appStore, appCfg, args[0])
		},
	}
	sprintRecoverCmd := &cobra.Command{
		Use:   "recover <sprint-id>",
		Short: "Release stale claims in a sprint",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintRecover(appStore, appCfg, args[0])
		},
	}
	sprintArchiveCmd := &cobra.Command{
		Use:   "archive <sprint-id>",
		Short: "Archive a sprint (all items must be done)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintArchive(appStore, appCfg, args[0])
		},
	}
	sprintDeleteCmd := &cobra.Command{
		Use:   "delete <sprint-id>",
		Short: "Delete an empty sprint",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintDelete(appCfg, args[0])
		},
	}
	sprintJoinCmd := &cobra.Command{
		Use:   "join <sprint-id>",
		Short: "Bind current session to a sprint",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintJoin(appCfg, args[0])
		},
	}
	sprintLeaveCmd := &cobra.Command{
		Use:   "leave",
		Short: "Unbind current session from sprint and release claims",
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintLeave(appStore, appCfg)
		},
	}
	sprintStatusCmd := &cobra.Command{
		Use:   "status [sprint-id]",
		Short: "Coordinator view — all active sprints or single sprint detail",
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			sprintID := ""
			if len(args) > 0 {
				sprintID = args[0]
			}
			exitCode = command.SprintStatus(appStore, appCfg, sprintID)
		},
	}
	sprintNextCmd := &cobra.Command{
		Use:   "next <sprint-id>",
		Short: "Print the next approved, unblocked item in this sprint",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintNext(appStore, appCfg, args[0])
		},
	}
	sprintMoveCmd := &cobra.Command{
		Use:   "move <sprint-id> <position>",
		Short: "Reorder a sprint within its parent epic (1 = first)",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			pos, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "position must be a number")
				exitCode = 2
				return
			}
			exitCode = command.SprintMove(appStore, appCfg, args[0], pos)
		},
	}

	sprintCmd.AddCommand(sprintCreateCmd, sprintListCmd, sprintAddCmd, sprintRmCmd, sprintShowCmd, sprintNextCmd, sprintMoveCmd, sprintPlanCmd, sprintRecoverCmd, sprintArchiveCmd, sprintDeleteCmd, sprintJoinCmd, sprintLeaveCmd, sprintStatusCmd)
	root.AddCommand(sprintCmd)

	uatCmd := &cobra.Command{
		Use:   "uat <id>",
		Short: "Run automated UAT verification and produce evidence report",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.UAT(appStore, appCfg, args[0], command.UATOpts{})
		},
	}
	root.AddCommand(uatCmd)

	mergeCmd := &cobra.Command{
		Use:   "merge <id>",
		Short: "Verify gates and merge PR",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Merge(appStore, appCfg, args[0], command.PipelineOpts{})
		},
	}
	root.AddCommand(mergeCmd)

	deployCheckCmd := &cobra.Command{
		Use:   "deploy-check <id>",
		Short: "Verify deployment succeeded",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.DeployCheck(appStore, appCfg, args[0], command.PipelineOpts{})
		},
	}
	root.AddCommand(deployCheckCmd)

	smokeCmd := &cobra.Command{
		Use:   "smoke <id>",
		Short: "Run smoke tests on deployed environment",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Smoke(appStore, appCfg, args[0], command.PipelineOpts{})
		},
	}
	root.AddCommand(smokeCmd)

	// --- Run / Advance ---

	runCmd := &cobra.Command{
		Use:   "run [sprint]",
		Short: "Autonomously execute a sprint using claude -p subprocesses",
		Long: `Run launches Claude Code (claude -p) subprocesses to autonomously work sprint items.
Each item walks a configurable pipeline of typed steps (claude, merge, deploy, uat, etc.).

Without arguments, enters interactive mode: shows sprints with work remaining,
lets you pick one, validates the plan, and starts execution.`,
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			budget, _ := cmd.Flags().GetFloat64("max-budget-usd")
			parallelism, _ := cmd.Flags().GetInt("parallelism")
			item, _ := cmd.Flags().GetString("item")
			model, _ := cmd.Flags().GetString("model")
			permMode, _ := cmd.Flags().GetString("permission-mode")
			fresh, _ := cmd.Flags().GetBool("fresh")
			runningOnly, _ := cmd.Flags().GetBool("running")
			statusID, _ := cmd.Flags().GetString("id")
			showAll, _ := cmd.Flags().GetBool("all")
			closedOnly, _ := cmd.Flags().GetBool("closed")
			noCoord, _ := cmd.Flags().GetBool("no-coordination")
			opts := command.RunOpts{
				DryRun:         dryRun,
				MaxBudgetUSD:   budget,
				Parallelism:    parallelism,
				ItemFilter:     item,
				Model:          model,
				PermissionMode: permMode,
				Fresh:          fresh,
				NoCoordination: noCoord,
			}
			engine := command.DefaultRunEngine()
			if len(args) == 1 && args[0] == "status" {
				// T-325: `st run status` is now a thin alias for
				// `st status --sprints`. Print a one-line notice (so muscle
				// memory eventually retrains) and call the same renderer.
				fmt.Fprintln(os.Stderr, "Note: `st run status` is now `st status --sprints` (alias preserved).")
				noRefresh, _ := cmd.Flags().GetBool("no-refresh")
				exitCode = command.RunStatus(appStore, appCfg, command.RunStatusOpts{
					RunningOnly: runningOnly,
					ID:          statusID,
					ShowAll:     showAll,
					ClosedOnly:  closedOnly,
					NoRefresh:   noRefresh,
				})
			} else if len(args) == 0 && item != "" {
				exitCode = command.RunItem(appStore, appCfg, item, opts, engine)
			} else if len(args) == 0 {
				exitCode = command.RunInteractive(appStore, appCfg, opts, engine)
			} else {
				exitCode = command.Run(appStore, appCfg, args[0], opts, engine)
			}
		},
	}
	runCmd.Flags().Bool("dry-run", false, "show execution plan without running")
	runCmd.Flags().Float64("max-budget-usd", 0, "per-item cost cap (0 = use config default)")
	runCmd.Flags().Int("parallelism", 0, "max concurrent claude processes (0 = use config default)")
	runCmd.Flags().String("item", "", "run only this item ID")
	runCmd.Flags().String("model", "", "model to use (overrides config)")
	runCmd.Flags().String("permission-mode", "", "claude permission mode (overrides config)")
	runCmd.Flags().Bool("fresh", false, "ignore saved progress, restart pipeline from step 0")
	runCmd.Flags().Bool("running", false, "with 'status': show only sprints currently being executed")
	runCmd.Flags().String("id", "", "with 'status': show only this epic or sprint (by slug)")
	runCmd.Flags().Bool("all", false, "with 'status': show all epics/sprints including archived")
	runCmd.Flags().BoolP("closed", "c", false, "with 'status': show only closed/archived epics and sprints")
	runCmd.Flags().Bool("no-refresh", false, "with 'status': skip the auto-pull from origin (for scripts/CI/hot loops)")
	runCmd.Flags().Bool("no-coordination", false, "skip the T-314 multi-agent coordination block in claude prompts (tests/minimal prompts)")
	root.AddCommand(runCmd)

	// T-376: shared dispatch helper used by both `st prep` (deprecated
	// top-level alias) and `st plan prep` (new subcommand under the
	// `plan` verb group). Returns the exit code; callers assign to
	// `exitCode` so cobra's binding surface stays minimal.
	runPrepDispatch := func(cmd *cobra.Command, args []string) int {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		item, _ := cmd.Flags().GetString("item")
		model, _ := cmd.Flags().GetString("model")
		includeRejected, _ := cmd.Flags().GetBool("include-rejected")
		writeOnly, _ := cmd.Flags().GetBool("write-only")
		opts := command.PrepOpts{
			DryRun:          dryRun,
			Model:           model,
			ItemFilter:      item,
			IncludeRejected: includeRejected,
			WriteOnly:       writeOnly,
		}
		engine := command.DefaultRunEngine()
		if len(args) > 0 {
			// I-512: when the positional arg is an item ID rather than a
			// sprint slug, derive the sprint and prep just that item. This
			// is the UX from the issue: `st prep I-509` instead of
			// `st prep <sprint> --item I-509`. Sprint slugs (which are
			// name-generator strings like "mainly-popular-gorilla") never
			// match an item ID lookup, so the back-compat path is intact.
			//
			// I-571: when the item has no sprint, route to PrepStandalone
			// instead of erroring — no sprint required.
			arg := args[0]
			if it, ok := appStore.Get(arg); ok {
				if it.Sprint == "" {
					opts.ItemFilter = ""
					return command.PrepStandalone(appStore, appCfg, arg, opts, engine)
				}
				opts.ItemFilter = arg
				return command.Prep(appStore, appCfg, it.Sprint, opts, engine)
			}
			return command.Prep(appStore, appCfg, arg, opts, engine)
		} else if item != "" {
			it, ok := appStore.Get(item)
			if !ok {
				fmt.Fprintf(os.Stderr, "item not found: %s\n", item)
				return 1
			}
			if it.Sprint == "" {
				// I-571: --item form also gets the standalone path.
				opts.ItemFilter = ""
				return command.PrepStandalone(appStore, appCfg, item, opts, engine)
			}
			return command.Prep(appStore, appCfg, it.Sprint, opts, engine)
		}
		return command.PrepInteractive(appStore, appCfg, opts, engine)
	}

	// prepFlags registers the shared flag surface on both the
	// top-level prep alias and the plan prep subcommand. Keeping the
	// flag declarations in one place prevents drift between the two
	// bindings during the deprecation window. T-376.
	prepFlags := func(c *cobra.Command) {
		c.Flags().Bool("dry-run", false, "show which items would be planned")
		c.Flags().String("item", "", "prep only this item ID")
		c.Flags().String("model", "", "model to use (overrides config)")
		c.Flags().Bool("include-rejected", false, "re-process previously rejected plans")
		c.Flags().Bool("write-only", false, "skip interactive review; write plan + report sidecars and exit")
	}

	prepCmd := &cobra.Command{
		Use:    "prep [sprint|item]",
		Hidden: true, // DEPRECATED — use `st plan prep`; alias will be removed after next release
		Short:  "DEPRECATED — use `st plan prep`",
		Long: `Prep launches Claude Code to explore the codebase and create structured
implementation plans for each unplanned item.

T-376: this top-level alias is DEPRECATED — use ` + "`" + `st plan prep` + "`" + ` instead.
The alias will be removed after the next release.

Three forms:
  st plan prep <sprint>     — plan every unplanned item in a sprint (batch)
  st plan prep <id>         — plan a single item (sprint inferred, or standalone
                              when the item has no sprint — no sprint required)
  st plan prep --item <id>  — same as positional <id>; legacy/long-form

For each item, Claude analyzes the codebase and proposes:
- Approach and scope (which repos are affected)
- Implementation steps and files to create/modify
- Acceptance criteria (executable cmd: checks)

You review each plan with Accept/Reject/Chat before it's saved.
Plans are stored as .plans/<id>.md sidecars and injected into the
implement step during st run.`,
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			// T-376 deprecation banner. Clearly-identifiable prefix
			// (`as: deprecation:`) so CI grep can filter it.
			fmt.Fprintln(os.Stderr, "as: deprecation: `st prep` is deprecated; use `st plan prep`. The top-level alias will be removed after the next release.")
			exitCode = runPrepDispatch(cmd, args)
		},
	}
	prepFlags(prepCmd)
	root.AddCommand(prepCmd)

	splitCmd := &cobra.Command{
		Use:   "split <id>",
		Short: "Split a full-stack item into linked Part A (backend) + Part B (frontend) items",
		Long: `Split splits a full-stack item into two linked items so a review
finding in one layer doesn't cascade into reworking the other.

Part A inherits the api/contract scope and api-shaped acceptance
criteria. Part B inherits the web scope, frontend ACs, and
depends_on Part A.

The parent is closed with resolution=split and scope_flags pointing
at the linked items. The decision is recorded so retrospective
analysis can correlate split-vs-unified outcomes against ci_fix
rates. I-180.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SplitCommand(appStore, appCfg, args[0])
		},
	}
	root.AddCommand(splitCmd)

	advanceCmd := &cobra.Command{
		Use:   "advance <sprint>",
		Short: "Execute pipeline steps for the next unblocked sprint item",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			budget, _ := cmd.Flags().GetFloat64("max-budget-usd")
			item, _ := cmd.Flags().GetString("item")
			model, _ := cmd.Flags().GetString("model")
			permMode, _ := cmd.Flags().GetString("permission-mode")
			step, _ := cmd.Flags().GetString("step")
			exitCode = command.Advance(appStore, appCfg, args[0], command.RunOpts{
				DryRun:         dryRun,
				MaxBudgetUSD:   budget,
				Parallelism:    1,
				ItemFilter:     item,
				Model:          model,
				PermissionMode: permMode,
				StepFilter:     step,
			}, command.DefaultRunEngine())
		},
	}
	advanceCmd.Flags().Bool("dry-run", false, "show what would be executed")
	advanceCmd.Flags().Float64("max-budget-usd", 0, "cost cap")
	advanceCmd.Flags().String("item", "", "advance this specific item")
	advanceCmd.Flags().String("model", "", "model to use")
	advanceCmd.Flags().String("permission-mode", "", "claude permission mode")
	advanceCmd.Flags().String("step", "", "stop after this step name")
	root.AddCommand(advanceCmd)

	stackCmd := &cobra.Command{
		Use:   "stack",
		Short: "Show the current work stack",
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.StackShow(appStore, appCfg)
		},
	}
	root.AddCommand(stackCmd)

	pushCmd := &cobra.Command{
		Use:   "push <id>",
		Short: "Push an item onto the work stack",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			reason, _ := cmd.Flags().GetString("reason")
			fromPending, _ := cmd.Flags().GetBool("from-pending")
			exitCode = command.StackPush(appStore, appCfg, args[0], command.StackPushOpts{
				Reason:      reason,
				FromPending: fromPending,
			})
		},
	}
	pushCmd.Flags().String("reason", "", "why this item is being pushed (what blocked the parent)")
	pushCmd.Flags().Bool("from-pending", false, "allow pushing an item that's pending operator approval (I-490 escape hatch)")
	root.AddCommand(pushCmd)

	popCmd := &cobra.Command{
		Use:   "pop",
		Short: "Pop the top item from the work stack",
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.StackPop(appStore, appCfg)
		},
	}
	root.AddCommand(popCmd)

	queueCmd := &cobra.Command{
		Use:   "queue",
		Short: "Manage the user-controlled work queue",
	}
	queueCmd.AddCommand(&cobra.Command{
		Use:   "add <id>",
		Short: "Add an item to the queue",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			reason, _ := cmd.Flags().GetString("reason")
			exitCode = command.QueueAdd(appStore, appCfg, args[0], command.QueueOpts{Reason: reason})
		},
	})
	queueCmd.Commands()[0].Flags().String("reason", "", "why this item is in the queue")
	queueShowCmd := &cobra.Command{
		Use:     "show",
		Short:   "Display the ordered work queue",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			all, _ := cmd.Flags().GetBool("all")
			exitCode = command.QueueShow(appStore, appCfg, command.QueueShowOpts{AgentAll: all})
		},
	}
	queueShowCmd.Flags().Bool("all", false, "show global queue without agent-scoped visual treatment")
	queueCmd.AddCommand(queueShowCmd)
	queueNextCmd := &cobra.Command{
		Use:   "next",
		Short: "Print the next approved, unblocked item",
		Run: func(cmd *cobra.Command, args []string) {
			sprint, _ := cmd.Flags().GetString("sprint")
			exitCode = command.QueueNext(appStore, appCfg, command.QueueNextOpts{Sprint: sprint})
		},
	}
	queueNextCmd.Flags().String("sprint", "", "filter to items belonging to this sprint slug")
	queueCmd.AddCommand(queueNextCmd)
	queueCmd.AddCommand(&cobra.Command{
		Use:   "rm <id>",
		Short: "Remove an item from the queue",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.QueueRm(appStore, appCfg, args[0])
		},
	})
	queueMoveCmd := &cobra.Command{
		Use:   "move <id> <position>",
		Short: "Move an item to a specific position (1-indexed)",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			pos, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "position must be a number")
				exitCode = 2
				return
			}
			exitCode = command.QueueMove(appStore, appCfg, args[0], pos)
		},
	}
	queueCmd.AddCommand(queueMoveCmd)
	queueApproveCmd := &cobra.Command{
		Use:   "approve [id]",
		Short: "Approve an agent-proposed queue item (or a whole sprint)",
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			sprint, _ := cmd.Flags().GetString("sprint")
			bypassPlan, _ := cmd.Flags().GetBool("bypass-plan")
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			exitCode = command.QueueApprove(appStore, appCfg, id, command.QueueApproveOpts{
				Sprint:     sprint,
				BypassPlan: bypassPlan,
			})
		},
	}
	queueApproveCmd.Flags().String("sprint", "", "bulk-approve all pending sprint members (mutually exclusive with <id>)")
	queueApproveCmd.Flags().Bool("bypass-plan", false, "bypass the I-491 plan-required gate (logs to changelog)")
	queueCmd.AddCommand(queueApproveCmd)
	queueCmd.AddCommand(&cobra.Command{
		Use:   "prune",
		Short: "Drop terminal (resolved/completed/etc) items from the queue",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.QueuePrune(appStore, appCfg)
		},
	})
	queueCmd.AddCommand(&cobra.Command{
		Use:   "auto-approve",
		Short: "Bulk-approve all pending queue entries that are goal-reachable (T-412)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.QueueAutoApprove(appStore, appCfg)
		},
	})
	root.AddCommand(queueCmd)

	// I-178: plan-approval primitives. The `plan-before-code-guard.sh`
	// hook (Phase B per-agent install) calls `st plan check <id>` to
	// decide whether to deny Edit/Write tool use against application
	// code. `st plan approve` and `st plan reset` toggle the gate; the
	// audit fields PlanApprovedAt + PlanApprovedBy track who/when so a
	// reviewer can trace the approval back.
	//
	// T-376: `st plan prep` was added to the same verb group as the
	// canonical name for plan drafting (the top-level `st prep` alias
	// remains for one release window with a deprecation banner). The
	// hook ecosystem comment above only enumerates the approval gate
	// surfaces; prep does not affect the gate state but lives under
	// the same `plan` verb for discoverability.
	planCmd := &cobra.Command{
		Use:   "plan",
		Short: "Manage per-item plan approvals (plan-before-code gate; hook-enforced)",
	}
	planApproveCmd := &cobra.Command{
		Use:   "approve <id>",
		Short: "Mark an item's plan as approved to allow code edits",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			strict, _ := cmd.Flags().GetBool("strict")
			bypassReview, _ := cmd.Flags().GetBool("bypass-review")
			engine := command.DefaultRunEngine()
			exitCode = command.PlanApprove(appStore, appCfg, args[0], command.PlanApproveOpts{
				Strict:       strict,
				Engine:       &engine,
				BypassReview: bypassReview,
			})
		},
	}
	planApproveCmd.Flags().Bool("strict", false, "deprecated alias — AC verifiability gate now fires unconditionally (I-710); flag preserved for CI back-compat")
	planApproveCmd.Flags().Bool("bypass-review", false, "skip the I-710 plan-review sub-agent (operator escape hatch when the sub-agent is broken or the plan has been manually reviewed) — I-752")
	planResetCmd := &cobra.Command{
		Use:   "reset <id>",
		Short: "Revoke plan approval; same plan body, needs re-approval",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.PlanReset(appStore, appCfg, args[0])
		},
	}
	planInvalidateCmd := &cobra.Command{
		Use:   "invalidate <id>",
		Short: "Discard a plan body so it can be re-authored from scratch",
		Long: `Plan invalidate deletes the .plans/<id>.md sidecar (and its
.report.md), clears the item's approval stamp, and drops the
dangling sidecar path from linked_plans. The item becomes
genuinely unplanned, so the next ` + "`st plan prep <id>`" + ` re-runs
Claude against an empty slate.

Use this when an item's implementation APPROACH fundamentally
changes and the existing plan body is obsolete — not merely
pending re-approval.

  st plan reset <id>      — revoke approval; same plan, re-approve it
  st plan invalidate <id> — discard the plan; re-author it`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.PlanInvalidate(appStore, appCfg, args[0])
		},
	}
	planCheckCmd := &cobra.Command{
		Use:   "check <id>",
		Short: "Print plan-approval state and exit 0 if approved, 1 otherwise (for hook integration)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.PlanCheck(appStore, appCfg, args[0])
		},
	}
	planShowCmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Display plan-approval state and linked plan files",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.PlanShow(appStore, appCfg, args[0])
		},
	}

	// T-376: `st plan prep` — the canonical subcommand for drafting
	// implementation plans. Shares `runPrepDispatch` and `prepFlags`
	// with the deprecated top-level `st prep` alias above.
	planPrepCmd := &cobra.Command{
		Use:   "prep [sprint|item]",
		Short: "Generate implementation plans for unplanned items (sprint or standalone, no sprint required)",
		Long: `Plan prep launches Claude Code to explore the codebase and create
structured implementation plans for each unplanned item.

Three forms:
  st plan prep <sprint>     — plan every unplanned item in a sprint (batch)
  st plan prep <id>         — plan a single item (sprint inferred, or standalone
                              when the item has no sprint — no sprint required)
  st plan prep --item <id>  — same as positional <id>; legacy/long-form

For each item, Claude analyzes the codebase and proposes:
- Approach and scope (which repos are affected)
- Implementation steps and files to create/modify
- Acceptance criteria (executable cmd: checks)

You review each plan with Accept/Reject/Chat before it's saved.
Plans are stored as .plans/<id>.md sidecars and injected into the
implement step during st run.`,
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = runPrepDispatch(cmd, args)
		},
	}
	prepFlags(planPrepCmd)

	planCmd.AddCommand(planApproveCmd, planResetCmd, planInvalidateCmd, planCheckCmd, planShowCmd, planPrepCmd)
	root.AddCommand(planCmd)

	filesCmd := &cobra.Command{
		Use:   "files <id>",
		Short: "Show live file changes across item worktrees (diff from origin/main merge-base)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			jsonF, _ := cmd.Flags().GetBool("json")
			exitCode = command.Files(appStore, appCfg, args[0], command.FilesOpts{JSON: jsonF})
		},
	}
	filesCmd.Flags().Bool("json", false, "output as JSON")
	root.AddCommand(filesCmd)

	sessionCmd := &cobra.Command{
		Use:   "session",
		Short: "Manage session metrics",
	}
	sessionCmd.AddCommand(&cobra.Command{
		Use:   "log",
		Short: "Accrue per-turn metrics onto the stack-top item (reads JSON from stdin)",
		Long: `Read a JSON SessionLogPayload from stdin and apply it to the stack-top
item (or an explicit item_id). Called by the Claude Code Stop hook and by
st run's metric recorder. Empty stack or missing item writes to
sessions/orphan.log — metrics are never silently dropped.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SessionLogCLI(appStore, appCfg, os.Stdin)
		},
	})
	root.AddCommand(sessionCmd)

	noteCmd := &cobra.Command{
		Use:   "note",
		Short: "Manage session notes",
	}
	noteCmd.AddCommand(&cobra.Command{
		Use:   "add <message>",
		Short: "Add a note",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.NoteAdd(appCfg, args[0])
		},
	})
	noteListCmd := &cobra.Command{
		Use:     "list",
		Short:   "List recent notes",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			limit, _ := cmd.Flags().GetInt("limit")
			exitCode = command.NoteList(appCfg, limit)
		},
	}
	noteListCmd.Flags().IntP("limit", "n", 10, "max notes to show")
	noteCmd.AddCommand(noteListCmd)
	noteEditCmd := &cobra.Command{
		Use:   "edit <id> [message]",
		Short: "Update a note's message",
		Args:  cobra.RangeArgs(1, 2),
		Run: func(cmd *cobra.Command, args []string) {
			stdinFlag, _ := cmd.Flags().GetBool("stdin")
			var message string
			if stdinFlag {
				data, _ := io.ReadAll(os.Stdin)
				// Trim a trailing CRLF/LF/CR terminator so a single line
				// piped in (incl. from CRLF tooling) is not spuriously
				// rejected by the I-673 single-line guard; an interior
				// newline still (correctly) trips ValidateNoteMessage.
				message = strings.TrimRight(string(data), "\r\n")
			} else if len(args) >= 2 {
				message = args[1]
			} else {
				exitCode = 2
				return
			}
			exitCode = command.NoteEdit(appCfg, args[0], message)
		},
	}
	noteEditCmd.Flags().Bool("stdin", false, "read message from stdin")
	noteCmd.AddCommand(noteEditCmd)
	noteCmd.AddCommand(&cobra.Command{
		Use:   "rm <id>",
		Short: "Delete a note",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.NoteRm(appCfg, args[0])
		},
	})
	root.AddCommand(noteCmd)

	// --- Mailbox (T-313) ---

	mailCmd := &cobra.Command{
		Use:   "mail",
		Short: "Inter-agent mailbox: send/list/show/archive messages between live agents",
	}

	mailSendCmd := &cobra.Command{
		Use:   "send <to>",
		Short: "Write a message into <to>'s mailbox",
		Long: `Send a kind-tagged message to another agent's mailbox. Surfaced
to the recipient by st run's between-step poll, or via st mail list.

Kinds:
  warning    informational FYI, may affect your work
  need_help  I'm blocked, someone pick up
  request    code review, opinion, etc.
  alert      stop everything, critical issue
  pause      stop touching this repo (force-push imminent, schema change)
  resume     OK to continue`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			kind, _ := cmd.Flags().GetString("kind")
			body, _ := cmd.Flags().GetString("body")
			item, _ := cmd.Flags().GetString("item")
			from, _ := cmd.Flags().GetString("from")
			exitCode = command.MailSend(appStore, appCfg, args[0], command.MailSendOpts{
				Kind: kind, Body: body, Item: item, From: from,
			})
		},
	}
	mailSendCmd.Flags().String("kind", "", "message kind (warning|need_help|request|alert|pause|resume)")
	mailSendCmd.Flags().String("body", "", "message body")
	mailSendCmd.Flags().String("item", "", "related item id (optional)")
	mailSendCmd.Flags().String("from", "", "override sender id (default: this agent)")
	_ = mailSendCmd.MarkFlagRequired("kind")
	_ = mailSendCmd.MarkFlagRequired("body")
	mailCmd.AddCommand(mailSendCmd)

	mailListCmd := &cobra.Command{
		Use:   "list",
		Short: "List pending mail (default: this agent)",
		Run: func(cmd *cobra.Command, args []string) {
			recipient, _ := cmd.Flags().GetString("agent")
			exitCode = command.MailList(appCfg, command.MailListOpts{Agent: recipient})
		},
	}
	mailListCmd.Flags().String("agent", "", "recipient (default: this agent)")
	mailCmd.AddCommand(mailListCmd)

	mailShowCmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Print one message (does NOT consume)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			recipient, _ := cmd.Flags().GetString("agent")
			exitCode = command.MailShow(appCfg, recipient, args[0])
		},
	}
	mailShowCmd.Flags().String("agent", "", "recipient (default: this agent)")
	mailCmd.AddCommand(mailShowCmd)

	mailArchiveCmd := &cobra.Command{
		Use:   "archive <id>",
		Short: "Move a pending message to archive (read receipt)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			recipient, _ := cmd.Flags().GetString("agent")
			exitCode = command.MailArchive(appStore, appCfg, recipient, args[0])
		},
	}
	mailArchiveCmd.Flags().String("agent", "", "recipient (default: this agent)")
	mailCmd.AddCommand(mailArchiveCmd)

	root.AddCommand(mailCmd)

	// --- Maintenance ---

	syncCmd := &cobra.Command{
		Use:   "sync [message]",
		Short: "Git commit and push agent-state changes",
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			msg := ""
			if len(args) > 0 {
				msg = args[0]
			}
			allowNonState, _ := cmd.Flags().GetBool("allow-non-state")
			exitCode = command.Sync(appStore, msg, allowNonState)
		},
	}
	syncCmd.Flags().Bool("allow-non-state", false, "bypass the non-state gate for this sync (ST_SYNC_ALLOW_NON_STATE=1 equivalent)")
	root.AddCommand(syncCmd)

	indexCmd := &cobra.Command{
		Use:   "index",
		Short: "Regenerate index.md from current items",
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Index(appStore, appCfg)
		},
	}
	root.AddCommand(indexCmd)

	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Normalize item file format",
		Run: func(cmd *cobra.Command, args []string) {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			scope, _ := cmd.Flags().GetString("scope")
			exitCode = command.Migrate(appStore, appCfg, command.MigrateOpts{DryRun: dryRun, Scope: scope})
		},
	}
	migrateCmd.Flags().Bool("dry-run", false, "show changes without applying")
	migrateCmd.Flags().String("scope", "", "scope: archive, active, or empty for all")
	root.AddCommand(migrateCmd)

	reconcileCmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Sync delivery stages with GitHub and AWS",
		Run: func(cmd *cobra.Command, args []string) {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			exitCode = command.Reconcile(appStore, appCfg, command.ReconcileOpts{DryRun: dryRun})
		},
	}
	reconcileCmd.Flags().Bool("dry-run", false, "show updates without applying")
	root.AddCommand(reconcileCmd)

	inferStageCmd := &cobra.Command{
		Use:   "infer-stage [<id>]",
		Short: "Infer delivery.stage from branch/PR state (forward-only)",
		Long: "Probes branch-on-remote and gh PR state to advance delivery.stage when an interactive\n" +
			"workflow (git push, gh pr create, GitHub UI merge) bypassed the verb side that would\n" +
			"normally call advanceDeliveryStage. With no id arg, infers for the stack-top item.\n" +
			"Forward-only — never regresses a later stage. Returns 0 on every nothing-to-do path\n" +
			"so stop hooks can call this without ever blocking session end.",
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			exitCode = command.InferStage(appStore, appCfg, id, command.InferStageOpts{})
		},
	}
	root.AddCommand(inferStageCmd)

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version + build identity",
		Run: func(cmd *cobra.Command, args []string) {
			short, _ := cmd.Flags().GetBool("short")
			if short {
				// Stable, parseable form for scripts: "<commit> <dirty>"
				fmt.Printf("%s %s\n", buildinfo.Commit, buildinfo.Dirty)
				exitCode = 0
				return
			}
			dirtyMark := ""
			if buildinfo.Dirty == "1" {
				dirtyMark = " (dirty)"
			}
			fmt.Printf("st %s\n", buildinfo.Version)
			fmt.Printf("commit: %s%s\n", buildinfo.Commit, dirtyMark)
			fmt.Printf("built:  %s\n", buildinfo.Built)
			exitCode = 0
		},
	}
	versionCmd.Flags().Bool("short", false, "print commit + dirty flag only (machine-readable)")
	root.AddCommand(versionCmd)

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new st project in the current directory",
		Run: func(cmd *cobra.Command, args []string) {
			dir := cwd
			if dir == "" {
				dir, _ = os.Getwd()
			}
			exitCode = command.Init(dir)
		},
	}
	root.AddCommand(initCmd)

	root.AddCommand(newDocgenCmd())

	// Apply group assignments after every command is registered. Keeping
	// the taxonomy centralized in commandGroupAssignments (vs. setting
	// GroupID inline at each command-creation block) keeps the group
	// map reviewable in one place and lets unannotated commands fall
	// through to the "Other" bucket without per-command edits.
	for _, c := range root.Commands() {
		if gid, ok := commandGroupAssignments[c.Name()]; ok {
			c.GroupID = gid
		}
	}

	return root
}

// commandGroupAssignments maps a top-level command's name() to the
// GroupID of its docs/help section. Names match cobra's Name() (first
// word of Use). Add a command here once it should appear in a named
// section of docs/st-cli-reference.md; until then it lands in "Other".
var commandGroupAssignments = map[string]string{
	// Queue & Stack
	"queue": "queue-stack",
	"stack": "queue-stack",
	"push":  "queue-stack",
	"pop":   "queue-stack",

	// State Management
	"show":   "state-mgmt",
	"list":   "state-mgmt",
	"create": "state-mgmt",
	"update": "state-mgmt",
	"check":  "state-mgmt",
	"tag":    "state-mgmt",
	"item":   "state-mgmt",

	// Workflow
	"start":        "workflow",
	"close":        "workflow",
	"finish":       "workflow",
	"release":      "workflow",
	"commit":       "workflow",
	"plan":         "workflow",
	"revert":       "workflow",
	"split":        "workflow",
	"unlock":       "workflow",
	"infer-stage":  "workflow",

	// Testing & Evidence
	"test": "testing",
	"pr":   "testing",

	// UAT & Pipeline
	"uat":          "uat-pipeline",
	"merge":        "uat-pipeline",
	"deploy-check": "uat-pipeline",
	"smoke":        "uat-pipeline",

	// Querying
	"status":     "querying",
	"stats":      "querying",
	"ready":      "querying",
	"prime":      "querying",
	"resume":     "querying",
	"log":        "querying",
	"recommend":  "querying",
	"next":       "querying",
	"artifact":   "querying",
	"watch":      "querying",
	"tui":        "querying",
	"cost":       "querying",
	"model-rec":  "querying",
	"files":      "querying",
	"transcript": "querying",

	// Dependencies
	"dep": "deps",

	// Epics, Sprints, Notes
	"epic":   "epics-sprints-notes",
	"sprint": "epics-sprints-notes",
	"note":   "epics-sprints-notes",
	"goal":   "epics-sprints-notes",

	// Arcs
	"arc": "arcs",

	// Agents
	"agent":  "agents",
	"agents": "agents",
	"mail":   "agents",

	// Autonomy & Execution
	"classify":   "autonomy",
	"decide":     "autonomy",
	"red":        "autonomy",
	"coordinate": "autonomy",
	"run":        "autonomy",
	"advance":    "autonomy",
	"spawn":      "autonomy",

	// Maintenance
	"index":     "maintenance",
	"migrate":   "maintenance",
	"reconcile": "maintenance",
	"sync":      "maintenance",
	"cache":     "maintenance",
	"session":   "maintenance",
}

// allLookLikePairs reports whether every argument is of the form
// `key=value` with a non-empty key (i.e., contains an `=` at a
// non-zero index). I-504: routes `st update <id> field=value
// field=value ...` to batch mode while leaving the single-field
// `<id> <field> <value>` form (where args[1] is the field name
// without `=`) untouched.
func allLookLikePairs(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, a := range args {
		idx := strings.Index(a, "=")
		if idx <= 0 {
			return false
		}
	}
	return true
}
