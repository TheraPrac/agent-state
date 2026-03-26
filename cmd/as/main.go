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

var (
	cfg *config.Config
	s   *store.Store
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "as",
	Short: "agent-state orchestrator",
	Long:  "as — a CLI agent orchestrator for tracking tasks, enforcing gates, and coordinating work.",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Skip store loading for commands that don't need it
		if cmd.Name() == "version" {
			return nil
		}

		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfg, err = config.Load(cwd)
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		s, err = store.New(cfg)
		if err != nil {
			return fmt.Errorf("loading items: %w", err)
		}
		return nil
	},
}

func init() {
	// State commands
	rootCmd.AddCommand(showCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(checkCmd)

	// Workflow commands
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(closeCmd)
	rootCmd.AddCommand(readyCmd)
	rootCmd.AddCommand(finishCmd)

	// Read/query commands
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(statsCmd)
	rootCmd.AddCommand(depCmd)
	depCmd.AddCommand(depTreeCmd)
	depCmd.AddCommand(depGraphCmd)
	rootCmd.AddCommand(primeCmd)

	// Maintenance commands
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(indexCmd)
	rootCmd.AddCommand(versionCmd)

	// Flags: status
	statusCmd.Flags().BoolP("issues", "i", false, "show open issues detail")
	statusCmd.Flags().BoolP("tasks", "t", false, "show queued tasks detail")
	statusCmd.Flags().BoolP("recent", "r", false, "show recently closed (7 days)")
	statusCmd.Flags().BoolP("all", "a", false, "expand all sections")
	statusCmd.Flags().BoolP("completed", "d", false, "show completed/archived items")
	statusCmd.Flags().BoolP("check", "c", false, "run validation checks")

	// Flags: stats
	statsCmd.Flags().Bool("json", false, "output as JSON")
	statsCmd.Flags().Bool("time", false, "include time tracking summaries")

	// Flags: dep tree
	depTreeCmd.Flags().IntP("depth", "d", 10, "max tree depth")

	// Flags: dep graph
	depGraphCmd.Flags().Bool("json", false, "output as JSON")

	// Flags: prime
	primeCmd.Flags().String("format", "markdown", "output format (markdown, json)")

	// Flags: show
	showCmd.Flags().BoolP("brief", "b", false, "compact one-line output")
	showCmd.Flags().StringP("field", "f", "", "show single field value")

	// Flags: list
	listCmd.Flags().StringP("type", "T", "", "filter by type (task, issue, idea)")
	listCmd.Flags().StringP("status", "s", "", "filter by status")
	listCmd.Flags().String("tag", "", "filter by tag")
	listCmd.Flags().String("assigned", "", "filter by agent assignment")

	// Flags: create
	createCmd.Flags().IntP("priority", "p", 2, "priority (0=highest, 4=lowest)")
	createCmd.Flags().String("tag", "", "tag to add")
	createCmd.Flags().String("depends", "", "depends on item ID")

	// Flags: update
	updateCmd.Flags().Bool("stdin", false, "read value from stdin (for multiline)")

	// Flags: check
	checkCmd.Flags().BoolP("quiet", "q", false, "exit code only, no output")

	// Flags: close
	closeCmd.Flags().String("reason", "", "reason for abandonment/wontfix")

	// Flags: ready
	readyCmd.Flags().StringP("type", "T", "", "filter by type")
	readyCmd.Flags().String("tag", "", "filter by tag")
	readyCmd.Flags().IntP("limit", "n", 0, "max items to show")

	// Flags: finish
	finishCmd.Flags().Bool("dry-run", false, "preview without deleting")
	finishCmd.Flags().Bool("force", false, "remove even with uncommitted changes")
	finishCmd.Flags().BoolP("list", "l", false, "list all active worktrees")
}

// --- State ---

var showCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show item detail",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		brief, _ := cmd.Flags().GetBool("brief")
		field, _ := cmd.Flags().GetString("field")
		os.Exit(command.Show(s, args[0], command.ShowOpts{Brief: brief, Field: field}))
	},
}

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List items",
	Run: func(cmd *cobra.Command, args []string) {
		typeF, _ := cmd.Flags().GetString("type")
		statusF, _ := cmd.Flags().GetString("status")
		tagF, _ := cmd.Flags().GetString("tag")
		assignedF, _ := cmd.Flags().GetString("assigned")
		os.Exit(command.List(s, cfg, command.ListOpts{
			Type: typeF, Status: statusF, Tag: tagF, Assigned: assignedF,
		}))
	},
}

var createCmd = &cobra.Command{
	Use:     "create <type> <title>",
	Aliases: []string{"new"},
	Short:   "Create new item",
	Args:    cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		priority, _ := cmd.Flags().GetInt("priority")
		tag, _ := cmd.Flags().GetString("tag")
		depends, _ := cmd.Flags().GetString("depends")
		os.Exit(command.Create(s, cfg, args[0], args[1], command.CreateOpts{
			Priority: priority, Tag: tag, Depends: depends,
		}))
	},
}

var updateCmd = &cobra.Command{
	Use:   "update <id> <field> [value]",
	Short: "Update a field on an item",
	Args:  cobra.RangeArgs(2, 3),
	Run: func(cmd *cobra.Command, args []string) {
		stdinFlag, _ := cmd.Flags().GetBool("stdin")
		var value string
		if stdinFlag {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "reading stdin: %v\n", err)
				os.Exit(1)
			}
			value = strings.TrimRight(string(data), "\n")
		} else if len(args) >= 3 {
			value = args[2]
		} else {
			fmt.Fprintln(os.Stderr, "usage: as update <id> <field> <value> or --stdin")
			os.Exit(2)
		}
		os.Exit(command.Update(s, args[0], args[1], value))
	},
}

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Validate all item files",
	Run: func(cmd *cobra.Command, args []string) {
		quiet, _ := cmd.Flags().GetBool("quiet")
		os.Exit(command.Check(s, cfg, quiet))
	},
}

// --- Workflow ---

var startCmd = &cobra.Command{
	Use:   "start <id>",
	Short: "Claim and activate an item",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Start(s, cfg, args[0]))
	},
}

var closeCmd = &cobra.Command{
	Use:   "close <id> <resolution>",
	Short: "Close an item (enforces gates)",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		reason, _ := cmd.Flags().GetString("reason")
		os.Exit(command.Close(s, cfg, args[0], args[1], command.CloseOpts{Reason: reason}))
	},
}

var readyCmd = &cobra.Command{
	Use:   "ready",
	Short: "Show unblocked items sorted by priority",
	Run: func(cmd *cobra.Command, args []string) {
		typeF, _ := cmd.Flags().GetString("type")
		tagF, _ := cmd.Flags().GetString("tag")
		limit, _ := cmd.Flags().GetInt("limit")
		os.Exit(command.Ready(s, cfg, command.ReadyOpts{Type: typeF, Tag: tagF, Limit: limit}))
	},
}

var finishCmd = &cobra.Command{
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
			os.Exit(2)
		}
		os.Exit(command.Finish(s, cfg, id, command.FinishOpts{
			DryRun: dryRun, Force: force, ListAll: listAll,
		}))
	},
}

// --- Maintenance ---

var syncCmd = &cobra.Command{
	Use:   "sync [message]",
	Short: "Git commit and push agent-state changes",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		msg := ""
		if len(args) > 0 {
			msg = args[0]
		}
		os.Exit(command.Sync(s, msg))
	},
}

var indexCmd = &cobra.Command{
	Use:   "index",
	Short: "Regenerate index.md from item state",
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Index(s, cfg))
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("as 0.2.0")
	},
}

// --- Read/Query ---

var statusCmd = &cobra.Command{
	Use:   "status [id]",
	Short: "Show dashboard or item detail",
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
		os.Exit(command.Status(s, cfg, id, command.StatusOpts{
			Issues: issues, Tasks: tasks, Recent: recent,
			All: all, Completed: completed, Check: check,
		}))
	},
}

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show summary statistics",
	Run: func(cmd *cobra.Command, args []string) {
		jsonF, _ := cmd.Flags().GetBool("json")
		timeF, _ := cmd.Flags().GetBool("time")
		os.Exit(command.Stats(s, cfg, command.StatsOpts{JSON: jsonF, Time: timeF}))
	},
}

var depCmd = &cobra.Command{
	Use:   "dep",
	Short: "Dependency operations",
}

var depTreeCmd = &cobra.Command{
	Use:   "tree <id>",
	Short: "Show dependency tree for an item",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		depth, _ := cmd.Flags().GetInt("depth")
		os.Exit(command.DepTree(s, cfg, args[0], command.DepTreeOpts{Depth: depth}))
	},
}

var depGraphCmd = &cobra.Command{
	Use:   "graph",
	Short: "Show full dependency graph",
	Run: func(cmd *cobra.Command, args []string) {
		jsonF, _ := cmd.Flags().GetBool("json")
		os.Exit(command.DepGraph(s, cfg, command.DepGraphOpts{JSON: jsonF}))
	},
}

var primeCmd = &cobra.Command{
	Use:   "prime",
	Short: "Output session context for hooks",
	Run: func(cmd *cobra.Command, args []string) {
		format, _ := cmd.Flags().GetString("format")
		os.Exit(command.Prime(s, cfg, command.PrimeOpts{Format: format}))
	},
}
