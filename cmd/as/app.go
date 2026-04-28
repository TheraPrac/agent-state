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
			case "version", "init":
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

	// --- State commands ---

	showCmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Display item details",
		Long:  "Display item details. Use --raw to see the full markdown file.",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			brief, _ := cmd.Flags().GetBool("brief")
			field, _ := cmd.Flags().GetString("field")
			raw, _ := cmd.Flags().GetBool("raw")
			exitCode = command.Show(appStore, appCfg, args[0], command.ShowOpts{Brief: brief, Field: field, Raw: raw})
		},
	}
	showCmd.Flags().BoolP("brief", "b", false, "compact one-line output")
	showCmd.Flags().StringP("field", "f", "", "show single field value")
	showCmd.Flags().BoolP("raw", "r", false, "print the raw markdown file")
	root.AddCommand(showCmd)

	listCmd := &cobra.Command{
		Use:     "list",
		Short:   "List items with optional filters",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			typeF, _ := cmd.Flags().GetString("type")
			statusF, _ := cmd.Flags().GetString("status")
			tagF, _ := cmd.Flags().GetString("tag")
			assignedF, _ := cmd.Flags().GetString("assigned")
			exitCode = command.List(appStore, appCfg, command.ListOpts{Type: typeF, Status: statusF, Tag: tagF, Assigned: assignedF})
		},
	}
	listCmd.Flags().StringP("type", "T", "", "filter by type (task, issue, idea)")
	listCmd.Flags().StringP("status", "s", "", "filter by status")
	listCmd.Flags().String("tag", "", "filter by tag")
	listCmd.Flags().String("assigned", "", "filter by assigned agent")
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
			exitCode = command.Create(appStore, appCfg, args[0], args[1], command.CreateOpts{
				Priority: priority, Severity: severity, Tag: tag, Depends: depends, Sprint: sprint,
			})
		},
	}
	createCmd.Flags().IntP("priority", "p", 2, "priority 0-4 (0=highest)")
	createCmd.Flags().String("severity", "", "issue severity (critical, high, medium, low)")
	createCmd.Flags().String("tag", "", "initial tag")
	createCmd.Flags().String("depends", "", "depends on item ID")
	createCmd.Flags().String("sprint", "", "assign to sprint on creation")
	root.AddCommand(createCmd)

	updateCmd := &cobra.Command{
		Use:   "update <id> <field> [value]",
		Short: "Update a field on an item (positional value, $EDITOR, or --stdin)",
		Long: `Update a field on an item using one of three input modes:

  st update <id> <field> <value>   # positional — set directly
  st update <id> <field>           # no value — open $EDITOR seeded with current value
  st update <id> <field> --stdin   # read new value from stdin (pipe or heredoc)

Long-form fields (description, summary, context, notes) round-trip as
YAML block scalars so multi-line values replace cleanly.`,
		Args: cobra.RangeArgs(2, 3),
		Run: func(cmd *cobra.Command, args []string) {
			stdinFlag, _ := cmd.Flags().GetBool("stdin")
			id, field := args[0], args[1]
			switch {
			case stdinFlag:
				exitCode = command.Update(appStore, appCfg, id, field, "", command.UpdateModeStdin)
			case len(args) >= 3:
				exitCode = command.Update(appStore, appCfg, id, field, args[2], command.UpdateModeValue)
			case command.StdinIsPiped():
				exitCode = command.Update(appStore, appCfg, id, field, "", command.UpdateModeStdin)
			default:
				exitCode = command.Update(appStore, appCfg, id, field, "", command.UpdateModeEditor)
			}
		},
	}
	updateCmd.Flags().Bool("stdin", false, "read value from stdin instead of opening $EDITOR")
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
		Short: "Manage local agent identity and auth",
	}
	agentBootstrapCmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap AWS and GitHub credentials for an agent",
		Args:  cobra.NoArgs,
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
		Args:  cobra.NoArgs,
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
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.AgentList(appCfg)
		},
	}
	agentCmd.AddCommand(agentListCmd)

	agentIdentityCmd := &cobra.Command{
		Use:   "identity",
		Short: "Inspect resolved agent identity",
	}
	agentIdentityShowCmd := &cobra.Command{
		Use:   "show",
		Short: "Print the resolved agent identity (id, source, parent/root heritage)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.AgentIdentityShow(appCfg)
		},
	}
	agentIdentityCmd.AddCommand(agentIdentityShowCmd)
	agentCmd.AddCommand(agentIdentityCmd)

	agentWorkspaceCmd := &cobra.Command{
		Use:   "workspace",
		Short: "Create, inspect, and remove local agent workspaces",
	}
	agentWorkspaceCreateCmd := &cobra.Command{
		Use:   "create <agent>",
		Short: "Create or repair an independent agent workspace",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			branch, _ := cmd.Flags().GetString("branch")
			full, _ := cmd.Flags().GetBool("full")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			repair, _ := cmd.Flags().GetBool("repair")
			exitCode = command.AgentWorkspaceCreate(appCfg, command.AgentWorkspaceCreateOpts{
				Agent: args[0], Branch: branch, Full: full, DryRun: dryRun, Repair: repair,
			})
		},
	}
	agentWorkspaceCreateCmd.Flags().String("branch", "main", "branch to check out in each repo")
	agentWorkspaceCreateCmd.Flags().Bool("full", false, "create independent non-symlink clones")
	agentWorkspaceCreateCmd.Flags().Bool("dry-run", false, "print the plan without filesystem, git, or Docker changes")
	agentWorkspaceCreateCmd.Flags().Bool("repair", false, "replace known-safe partial workspace symlinks")
	agentWorkspaceCmd.AddCommand(agentWorkspaceCreateCmd)

	agentWorkspaceStatusCmd := &cobra.Command{
		Use:   "status [agent]",
		Short: "Show resolved paths, ports, repo state, and service-health placeholders",
		Args:  cobra.MaximumNArgs(1),
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
		Args:  cobra.ExactArgs(1),
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

	// --- Workflow commands ---

	startCmd := &cobra.Command{
		Use:   "start <id>",
		Short: "Activate an item and create worktree branches",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			slug, _ := cmd.Flags().GetString("slug")
			repos, _ := cmd.Flags().GetStringSlice("repos")
			noPush, _ := cmd.Flags().GetBool("no-push")
			exitCode = command.Start(appStore, appCfg, args[0], command.StartOpts{Slug: slug, Repos: repos, NoPush: noPush})
		},
	}
	startCmd.Flags().String("slug", "", "branch name slug")
	startCmd.Flags().StringSlice("repos", nil, "repos to create worktrees for")
	startCmd.Flags().Bool("no-push", false, "skip auto-push onto the work stack")
	root.AddCommand(startCmd)

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
			exitCode = command.Status(appStore, appCfg, id, command.StatusOpts{
				Issues: issues, Tasks: tasks, Recent: recent,
				All: all, Completed: completed, Check: check,
				Tag: tag, Epic: epic, NoRefresh: noRefresh,
				Sprints: sprints, SprintsID: sprintsID,
				SprintsAll: sprintsAll, SprintsClosed: sprintsClosed,
				SprintsRunning: sprintsRunning,
				Filters: filters, Sort: sortStr, Since: since, JSON: jsonOut,
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
	statusCmd.Flags().String("since", "",
		"only items touched within this duration (e.g. 7d, 24h, 30m)")
	statusCmd.Flags().Bool("json", false, "emit JSON instead of human-readable text")
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
		Short: "Add a dependency (id depends on dep-id)",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.DepAdd(appStore, appCfg, args[0], args[1])
		},
	}
	depRmCmd := &cobra.Command{
		Use:   "rm <id> <dep-id>",
		Short: "Remove a dependency",
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
			format, _ := cmd.Flags().GetString("format")
			compact, _ := cmd.Flags().GetBool("compact")
			exitCode = command.Prime(appStore, appCfg, command.PrimeOpts{Format: format, Compact: compact})
		},
	}
	primeCmd.Flags().String("format", "markdown", "output format (markdown, json)")
	primeCmd.Flags().Bool("compact", false, "minimal output for hook injection")
	root.AddCommand(primeCmd)

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
	root.AddCommand(epicCmd)

	sprintCmd := &cobra.Command{
		Use:   "sprint",
		Short: "Manage sprints within epics",
	}
	sprintCreateCmd := &cobra.Command{
		Use:   "create <epic-id> <title>",
		Short: "Create a sprint under an epic",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintCreate(appCfg, args[0], args[1])
		},
	}
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
	sprintCmd.AddCommand(sprintCreateCmd, sprintListCmd, sprintAddCmd, sprintRmCmd, sprintShowCmd, sprintPlanCmd, sprintRecoverCmd, sprintArchiveCmd, sprintDeleteCmd, sprintJoinCmd, sprintLeaveCmd, sprintStatusCmd)
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

	prepCmd := &cobra.Command{
		Use:   "prep [sprint]",
		Short: "Generate implementation plans for unplanned sprint items",
		Long: `Prep launches Claude Code to explore the codebase and create structured
implementation plans for each unplanned item in a sprint.

For each item, Claude analyzes the codebase and proposes:
- Approach and scope (which repos are affected)
- Implementation steps and files to create/modify
- Acceptance criteria (executable cmd: checks)

You review each plan with Accept/Reject/Chat before it's saved.
Plans are stored as .plans/<id>.md sidecars and injected into the
implement step during st run.`,
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			item, _ := cmd.Flags().GetString("item")
			model, _ := cmd.Flags().GetString("model")
			includeRejected, _ := cmd.Flags().GetBool("include-rejected")
			opts := command.PrepOpts{
				DryRun:          dryRun,
				Model:           model,
				ItemFilter:      item,
				IncludeRejected: includeRejected,
			}
			engine := command.DefaultRunEngine()
			if len(args) > 0 {
				exitCode = command.Prep(appStore, appCfg, args[0], opts, engine)
			} else if item != "" {
				// Resolve sprint from item
				it, ok := appStore.Get(item)
				if !ok {
					fmt.Fprintf(os.Stderr, "item not found: %s\n", item)
					exitCode = 1
					return
				}
				if it.Sprint == "" {
					fmt.Fprintf(os.Stderr, "item %s has no sprint assigned\n", item)
					exitCode = 1
					return
				}
				exitCode = command.Prep(appStore, appCfg, it.Sprint, opts, engine)
			} else {
				exitCode = command.PrepInteractive(appStore, appCfg, opts, engine)
			}
		},
	}
	prepCmd.Flags().Bool("dry-run", false, "show which items would be planned")
	prepCmd.Flags().String("item", "", "prep only this item ID")
	prepCmd.Flags().String("model", "", "model to use (overrides config)")
	prepCmd.Flags().Bool("include-rejected", false, "re-process previously rejected plans")
	root.AddCommand(prepCmd)

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
			exitCode = command.StackPush(appStore, appCfg, args[0], reason)
		},
	}
	pushCmd.Flags().String("reason", "", "why this item is being pushed (what blocked the parent)")
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
	queueCmd.AddCommand(&cobra.Command{
		Use:     "show",
		Short:   "Display the ordered work queue",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.QueueShow(appStore, appCfg)
		},
	})
	queueCmd.AddCommand(&cobra.Command{
		Use:   "next",
		Short: "Print the next approved, unblocked item",
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.QueueNext(appStore, appCfg)
		},
	})
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
	queueCmd.AddCommand(&cobra.Command{
		Use:   "approve <id>",
		Short: "Approve an agent-proposed queue item",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.QueueApprove(appStore, appCfg, args[0])
		},
	})
	queueCmd.AddCommand(&cobra.Command{
		Use:   "prune",
		Short: "Drop terminal (resolved/completed/etc) items from the queue",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.QueuePrune(appStore, appCfg)
		},
	})
	root.AddCommand(queueCmd)

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
				message = strings.TrimRight(string(data), "\n")
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
			exitCode = command.Sync(appStore, msg)
		},
	}
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

	return root
}
