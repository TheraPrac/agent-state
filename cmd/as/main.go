package main

import (
	"fmt"
	"os"

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

	// Maintenance commands
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(indexCmd)
	rootCmd.AddCommand(versionCmd)
}

// --- State ---

var showCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show item detail",
	Args:  cobra.MinimumNArgs(1),
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Show(s, args))
	},
}

var listCmd = &cobra.Command{
	Use:                "list",
	Aliases:            []string{"ls"},
	Short:              "List items",
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.List(s, cfg, args))
	},
}

var createCmd = &cobra.Command{
	Use:                "create <type> <title>",
	Aliases:            []string{"new"},
	Short:              "Create new item",
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Create(s, cfg, args))
	},
}

var updateCmd = &cobra.Command{
	Use:                "update <id> <field> [value]",
	Short:              "Update a field on an item",
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Update(s, args))
	},
}

var checkCmd = &cobra.Command{
	Use:                "check",
	Short:              "Validate all item files",
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Check(s, cfg, args))
	},
}

// --- Workflow ---

var startCmd = &cobra.Command{
	Use:   "start <id>",
	Short: "Claim and activate an item",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Start(s, cfg, args))
	},
}

var closeCmd = &cobra.Command{
	Use:                "close <id> <resolution>",
	Short:              "Close an item (enforces gates)",
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Close(s, cfg, args))
	},
}

var readyCmd = &cobra.Command{
	Use:                "ready",
	Short:              "Show unblocked items sorted by priority",
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Ready(s, cfg, args))
	},
}

// --- Maintenance ---

var syncCmd = &cobra.Command{
	Use:   "sync [message]",
	Short: "Git commit and push agent-state changes",
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Sync(s, args))
	},
}

var indexCmd = &cobra.Command{
	Use:   "index",
	Short: "Regenerate index.md from item state",
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(command.Index(s, cfg, args))
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("as 0.1.0")
	},
}
