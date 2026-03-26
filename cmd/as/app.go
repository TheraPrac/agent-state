package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/command"
	"github.com/jfinlinson/agent-state/internal/config"
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
		Use:   "as",
		Short: "agent-state orchestrator",
		Long:  "as — a CLI agent orchestrator for tracking tasks, enforcing gates, and coordinating work.",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Name() == "version" {
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
			appStore, err = store.New(appCfg)
			if err != nil {
				return fmt.Errorf("loading items: %w", err)
			}
			return nil
		},
		SilenceUsage: true,
	}

	// --- State commands ---

	showCmd := &cobra.Command{
		Use:  "show <id>",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			brief, _ := cmd.Flags().GetBool("brief")
			field, _ := cmd.Flags().GetString("field")
			exitCode = command.Show(appStore, args[0], command.ShowOpts{Brief: brief, Field: field})
		},
	}
	showCmd.Flags().BoolP("brief", "b", false, "compact one-line output")
	showCmd.Flags().StringP("field", "f", "", "show single field value")
	root.AddCommand(showCmd)

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			typeF, _ := cmd.Flags().GetString("type")
			statusF, _ := cmd.Flags().GetString("status")
			tagF, _ := cmd.Flags().GetString("tag")
			assignedF, _ := cmd.Flags().GetString("assigned")
			exitCode = command.List(appStore, appCfg, command.ListOpts{Type: typeF, Status: statusF, Tag: tagF, Assigned: assignedF})
		},
	}
	listCmd.Flags().StringP("type", "T", "", "filter by type")
	listCmd.Flags().StringP("status", "s", "", "filter by status")
	listCmd.Flags().String("tag", "", "filter by tag")
	listCmd.Flags().String("assigned", "", "filter by agent")
	root.AddCommand(listCmd)

	createCmd := &cobra.Command{
		Use:     "create <type> <title>",
		Aliases: []string{"new"},
		Args:    cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			priority, _ := cmd.Flags().GetInt("priority")
			severity, _ := cmd.Flags().GetString("severity")
			tag, _ := cmd.Flags().GetString("tag")
			depends, _ := cmd.Flags().GetString("depends")
			exitCode = command.Create(appStore, appCfg, args[0], args[1], command.CreateOpts{
				Priority: priority, Severity: severity, Tag: tag, Depends: depends,
			})
		},
	}
	createCmd.Flags().IntP("priority", "p", 2, "priority 0-4")
	createCmd.Flags().String("severity", "", "severity")
	createCmd.Flags().String("tag", "", "tag")
	createCmd.Flags().String("depends", "", "depends on")
	root.AddCommand(createCmd)

	updateCmd := &cobra.Command{
		Use:  "update <id> <field> [value]",
		Args: cobra.RangeArgs(2, 3),
		Run: func(cmd *cobra.Command, args []string) {
			stdinFlag, _ := cmd.Flags().GetBool("stdin")
			var value string
			if stdinFlag {
				data, _ := io.ReadAll(os.Stdin)
				value = strings.TrimRight(string(data), "\n")
			} else if len(args) >= 3 {
				value = args[2]
			} else {
				fmt.Fprintln(os.Stderr, "usage: as update <id> <field> <value> or --stdin")
				exitCode = 2
				return
			}
			exitCode = command.Update(appStore, appCfg, args[0], args[1], value)
		},
	}
	updateCmd.Flags().Bool("stdin", false, "read value from stdin")
	root.AddCommand(updateCmd)

	checkCmd := &cobra.Command{
		Use: "check",
		Run: func(cmd *cobra.Command, args []string) {
			quiet, _ := cmd.Flags().GetBool("quiet")
			fix, _ := cmd.Flags().GetBool("fix")
			exitCode = command.Check(appStore, appCfg, quiet, fix)
		},
	}
	checkCmd.Flags().BoolP("quiet", "q", false, "exit code only")
	checkCmd.Flags().Bool("fix", false, "auto-repair fixable issues")
	root.AddCommand(checkCmd)

	tagCmd := &cobra.Command{
		Use:  "tag <id> <add|rm> <tag>",
		Args: cobra.ExactArgs(3),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Tag(appStore, appCfg, args[0], args[1], args[2])
		},
	}
	root.AddCommand(tagCmd)

	// --- Workflow commands ---

	startCmd := &cobra.Command{
		Use:  "start <id>",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			slug, _ := cmd.Flags().GetString("slug")
			repos, _ := cmd.Flags().GetStringSlice("repos")
			exitCode = command.Start(appStore, appCfg, args[0], command.StartOpts{Slug: slug, Repos: repos})
		},
	}
	startCmd.Flags().String("slug", "", "branch slug")
	startCmd.Flags().StringSlice("repos", nil, "repos")
	root.AddCommand(startCmd)

	closeCmd := &cobra.Command{
		Use:  "close <id> <resolution>",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			reason, _ := cmd.Flags().GetString("reason")
			force, _ := cmd.Flags().GetBool("force")
			exitCode = command.Close(appStore, appCfg, args[0], args[1], command.CloseOpts{Reason: reason, Force: force})
		},
	}
	closeCmd.Flags().String("reason", "", "reason")
	closeCmd.Flags().Bool("force", false, "bypass gates")
	root.AddCommand(closeCmd)

	readyCmd := &cobra.Command{
		Use: "ready",
		Run: func(cmd *cobra.Command, args []string) {
			typeF, _ := cmd.Flags().GetString("type")
			tagF, _ := cmd.Flags().GetString("tag")
			limit, _ := cmd.Flags().GetInt("limit")
			exitCode = command.Ready(appStore, appCfg, command.ReadyOpts{Type: typeF, Tag: tagF, Limit: limit})
		},
	}
	readyCmd.Flags().StringP("type", "T", "", "")
	readyCmd.Flags().String("tag", "", "")
	readyCmd.Flags().IntP("limit", "n", 0, "")
	root.AddCommand(readyCmd)

	finishCmd := &cobra.Command{
		Use:  "finish [id]",
		Args: cobra.MaximumNArgs(1),
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
	finishCmd.Flags().Bool("dry-run", false, "")
	finishCmd.Flags().Bool("force", false, "")
	finishCmd.Flags().BoolP("list", "l", false, "")
	root.AddCommand(finishCmd)

	releaseCmd := &cobra.Command{
		Use:  "release <id>",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Release(appStore, appCfg, args[0])
		},
	}
	root.AddCommand(releaseCmd)

	commitCmd := &cobra.Command{
		Use:  "commit <id> <message>",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Commit(appStore, appCfg, args[0], args[1])
		},
	}
	root.AddCommand(commitCmd)

	editCmd := &cobra.Command{
		Use:  "edit <id> <field>",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Edit(appStore, appCfg, args[0], args[1])
		},
	}
	root.AddCommand(editCmd)

	// --- Read/query commands ---

	statusCmd := &cobra.Command{
		Use:  "status [id]",
		Args: cobra.MaximumNArgs(1),
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
			exitCode = command.Status(appStore, appCfg, id, command.StatusOpts{
				Issues: issues, Tasks: tasks, Recent: recent,
				All: all, Completed: completed, Check: check,
			})
		},
	}
	statusCmd.Flags().BoolP("issues", "i", false, "")
	statusCmd.Flags().BoolP("tasks", "t", false, "")
	statusCmd.Flags().BoolP("recent", "r", false, "")
	statusCmd.Flags().BoolP("all", "a", false, "")
	statusCmd.Flags().BoolP("completed", "d", false, "")
	statusCmd.Flags().BoolP("check", "c", false, "")
	root.AddCommand(statusCmd)

	statsCmd := &cobra.Command{
		Use: "stats",
		Run: func(cmd *cobra.Command, args []string) {
			jsonF, _ := cmd.Flags().GetBool("json")
			timeF, _ := cmd.Flags().GetBool("time")
			exitCode = command.Stats(appStore, appCfg, command.StatsOpts{JSON: jsonF, Time: timeF})
		},
	}
	statsCmd.Flags().Bool("json", false, "")
	statsCmd.Flags().Bool("time", false, "")
	root.AddCommand(statsCmd)

	depCmd := &cobra.Command{Use: "dep"}
	depTreeCmd := &cobra.Command{
		Use:  "tree <id>",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			depth, _ := cmd.Flags().GetInt("depth")
			exitCode = command.DepTree(appStore, appCfg, args[0], command.DepTreeOpts{Depth: depth})
		},
	}
	depTreeCmd.Flags().IntP("depth", "d", 10, "")
	depGraphCmd := &cobra.Command{
		Use: "graph",
		Run: func(cmd *cobra.Command, args []string) {
			jsonF, _ := cmd.Flags().GetBool("json")
			exitCode = command.DepGraph(appStore, appCfg, command.DepGraphOpts{JSON: jsonF})
		},
	}
	depGraphCmd.Flags().Bool("json", false, "")
	depAddCmd := &cobra.Command{
		Use:  "add <id> <dep-id>",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.DepAdd(appStore, appCfg, args[0], args[1])
		},
	}
	depRmCmd := &cobra.Command{
		Use:  "rm <id> <dep-id>",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.DepRm(appStore, appCfg, args[0], args[1])
		},
	}
	depCmd.AddCommand(depTreeCmd, depGraphCmd, depAddCmd, depRmCmd)
	root.AddCommand(depCmd)

	primeCmd := &cobra.Command{
		Use: "prime",
		Run: func(cmd *cobra.Command, args []string) {
			format, _ := cmd.Flags().GetString("format")
			compact, _ := cmd.Flags().GetBool("compact")
			exitCode = command.Prime(appStore, appCfg, command.PrimeOpts{Format: format, Compact: compact})
		},
	}
	primeCmd.Flags().String("format", "markdown", "")
	primeCmd.Flags().Bool("compact", false, "")
	root.AddCommand(primeCmd)

	logCmd := &cobra.Command{
		Use:  "log [id]",
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			limit, _ := cmd.Flags().GetInt("limit")
			exitCode = command.Log(appStore, appCfg, id, command.LogOpts{Limit: limit})
		},
	}
	logCmd.Flags().IntP("limit", "n", 0, "")
	root.AddCommand(logCmd)

	// --- Epic/Sprint/Note ---

	epicCmd := &cobra.Command{Use: "epic"}
	epicCmd.AddCommand(&cobra.Command{
		Use:  "create <title>",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.EpicCreate(appCfg, args[0])
		},
	})
	epicCmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.EpicList(appStore, appCfg)
		},
	})
	root.AddCommand(epicCmd)

	sprintCmd := &cobra.Command{Use: "sprint"}
	sprintCreateCmd := &cobra.Command{
		Use:  "create <epic-id> <title>",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.SprintCreate(appCfg, args[0], args[1])
		},
	}
	sprintListCmd := &cobra.Command{
		Use:     "list [epic-id]",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			epicID, _ := cmd.Flags().GetString("epic")
			if epicID == "" && len(args) > 0 {
				epicID = args[0]
			}
			exitCode = command.SprintList(appCfg, epicID)
		},
	}
	sprintListCmd.Flags().String("epic", "", "")
	sprintCmd.AddCommand(sprintCreateCmd, sprintListCmd)
	root.AddCommand(sprintCmd)

	noteCmd := &cobra.Command{Use: "note"}
	noteCmd.AddCommand(&cobra.Command{
		Use:  "add <message>",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.NoteAdd(appCfg, args[0])
		},
	})
	noteListCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			limit, _ := cmd.Flags().GetInt("limit")
			exitCode = command.NoteList(appCfg, limit)
		},
	}
	noteListCmd.Flags().IntP("limit", "n", 10, "")
	noteCmd.AddCommand(noteListCmd)
	noteEditCmd := &cobra.Command{
		Use:  "edit <id> [message]",
		Args: cobra.RangeArgs(1, 2),
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
	noteEditCmd.Flags().Bool("stdin", false, "")
	noteCmd.AddCommand(noteEditCmd)
	noteCmd.AddCommand(&cobra.Command{
		Use:  "rm <id>",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.NoteRm(appCfg, args[0])
		},
	})
	root.AddCommand(noteCmd)

	// --- Maintenance ---

	syncCmd := &cobra.Command{
		Use:  "sync [message]",
		Args: cobra.MaximumNArgs(1),
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
		Use: "index",
		Run: func(cmd *cobra.Command, args []string) {
			exitCode = command.Index(appStore, appCfg)
		},
	}
	root.AddCommand(indexCmd)

	migrateCmd := &cobra.Command{
		Use: "migrate",
		Run: func(cmd *cobra.Command, args []string) {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			scope, _ := cmd.Flags().GetString("scope")
			exitCode = command.Migrate(appStore, appCfg, command.MigrateOpts{DryRun: dryRun, Scope: scope})
		},
	}
	migrateCmd.Flags().Bool("dry-run", false, "")
	migrateCmd.Flags().String("scope", "", "scope: archive, active, or empty for all")
	root.AddCommand(migrateCmd)

	reconcileCmd := &cobra.Command{
		Use: "reconcile",
		Run: func(cmd *cobra.Command, args []string) {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			exitCode = command.Reconcile(appStore, appCfg, command.ReconcileOpts{DryRun: dryRun})
		},
	}
	reconcileCmd.Flags().Bool("dry-run", false, "")
	root.AddCommand(reconcileCmd)

	versionCmd := &cobra.Command{
		Use: "version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("as 0.4.0")
			exitCode = 0
		},
	}
	root.AddCommand(versionCmd)

	return root
}
