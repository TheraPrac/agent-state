package main

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/command"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		printUsage()
		return 2
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// Load config
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "getwd: %v\n", err)
		return 1
	}

	cfg, err := config.Load(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}

	// Commands that don't need a store
	switch cmd {
	case "version":
		fmt.Println("as 0.1.0")
		return 0
	case "help", "--help", "-h":
		printUsage()
		return 0
	}

	// Load store (scans all items)
	s, err := store.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading items: %v\n", err)
		return 1
	}

	switch cmd {
	case "show":
		return command.Show(s, args)
	case "list", "ls":
		return command.List(s, cfg, args)
	case "check":
		return command.Check(s, cfg, args)
	case "ready":
		return command.Ready(s, cfg, args)
	case "create", "new":
		return command.Create(s, cfg, args)
	case "start":
		return command.Start(s, cfg, args)
	case "close":
		return command.Close(s, cfg, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		return 2
	}
}

func printUsage() {
	fmt.Println(`as — agent-state orchestrator

Usage: as <command> [arguments]

State:
  show <id>         Show item detail
  list              List items (--type, --status, --tag, --assigned)
  create <type>     Create new item
  check             Validate all files

Workflow:
  start <id>        Claim and activate item
  close <id> <res>  Close item (enforces gates)
  ready             Show unblocked items by priority

Run 'as <command> --help' for command-specific options.`)
}
