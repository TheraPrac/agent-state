package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

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
			// Auto-pull latest changes before scanning items
			_ = store.GitPull(appCfg)

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
			exitCode = command.Show(appStore, args[0], command.ShowOpts{Brief: brief, Field: field, Raw: raw})
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
		Short: "Update a field on an item",
		Args:  cobra.RangeArgs(2, 3),
		Run: func(cmd *cobra.Command, args []string) {
			stdinFlag, _ := cmd.Flags().GetBool("stdin")
			var value string
			if stdinFlag {
				data, _ := io.ReadAll(os.Stdin)
				value = strings.TrimRight(string(data), "\n")
			} else if len(args) >= 3 {
				value = args[2]
			} else {
				fmt.Fprintln(os.Stderr, "usage: st update <id> <field> <value> or --stdin")
				exitCode = 2
				return
			}
			exitCode = command.Update(appStore, appCfg, args[0], args[1], value)
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

	// --- Workflow commands ---

	startCmd := &cobra.Command{
		Use:   "start <id>",
		Short: "Activate an item and create worktree branches",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			slug, _ := cmd.Flags().GetString("slug")
			repos, _ := cmd.Flags().GetStringSlice("repos")
			exitCode = command.Start(appStore, appCfg, args[0], command.StartOpts{Slug: slug, Repos: repos})
		},
	}
	startCmd.Flags().String("slug", "", "branch name slug")
	startCmd.Flags().StringSlice("repos", nil, "repos to create worktrees for")
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
		Long:  "Without --run: records a manual test pass. With --run: executes the suite command, captures output, uploads evidence.",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			run, _ := cmd.Flags().GetBool("run")
			cov, _ := cmd.Flags().GetBool("coverage")
			exitCode = command.TestRecord(appStore, appCfg, args[0], args[1], command.TestRecordOpts{
				Run: run, Coverage: cov,
			})
		},
	}
	testRecordCmd.Flags().Bool("run", false, "execute the suite command and capture evidence")
	testRecordCmd.Flags().Bool("coverage", false, "enforce per-file coverage thresholds (requires --run)")
	root.AddCommand(testRecordCmd)

	editCmd := &cobra.Command{
		Use:   "edit <id> <field>",
		Short: "Edit a field in $EDITOR, or read from stdin when piped",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			stdinFlag, _ := cmd.Flags().GetBool("stdin")
			exitCode = command.Edit(appStore, appCfg, args[0], args[1], stdinFlag)
		},
	}
	editCmd.Flags().Bool("stdin", false, "read new value from stdin instead of opening $EDITOR")
	root.AddCommand(editCmd)

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
			exitCode = command.Status(appStore, appCfg, id, command.StatusOpts{
				Issues: issues, Tasks: tasks, Recent: recent,
				All: all, Completed: completed, Check: check,
				Tag: tag, Epic: epic,
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
			exitCode = command.QueueRm(appCfg, args[0])
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
			exitCode = command.QueueMove(appCfg, args[0], pos)
		},
	}
	queueCmd.AddCommand(queueMoveCmd)
	queueCmd.AddCommand(&cobra.Command{
		Use:   "approve <id>",
		Short: "Approve an agent-proposed queue item",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.QueueApprove(appCfg, args[0])
		},
	})
	root.AddCommand(queueCmd)

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
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("st 0.6.0")
			exitCode = 0
		},
	}
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
