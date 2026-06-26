package command

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/pricing"
)

// PricingRefreshOpts controls st pricing refresh behaviour.
type PricingRefreshOpts struct {
	// DryRun prints the diff without writing any files.
	DryRun bool
	// SanityPct is the max allowed per-field % change for existing models.
	// 0 or negative disables the sanity check (any change is accepted).
	SanityPct float64

	// TablePath overrides the default resolved path to table.go (for tests).
	TablePath string
	// AsDir overrides the as-repo root used for go build and git ops (for tests).
	AsDir string
	// Fetcher overrides the live HTTP fetch (for tests).
	Fetcher func(*http.Client) (map[string]pricing.Rate, error)
	// RunCmd overrides os/exec command execution (for tests).
	RunCmd func(dir string, args ...string) error
}

// PricingRefresh implements `st pricing refresh`: fetches Anthropic pricing,
// diffs against the hardcoded table, and auto-commits updates within the
// sanity bound. Returns a shell exit code (0 = success, 1 = error/blocked).
func PricingRefresh(cfg *config.Config, opts PricingRefreshOpts) int {
	// Resolve paths
	asDir := opts.AsDir
	if asDir == "" {
		root := cfg.AgentRoot()
		if root == "" {
			fmt.Fprintln(os.Stderr, "pricing refresh: cannot resolve agent root")
			return 1
		}
		asDir = filepath.Join(root, "as")
	}
	tablePath := opts.TablePath
	if tablePath == "" {
		tablePath = filepath.Join(asDir, "internal", "pricing", "table.go")
	}

	// Fetch live rates
	fetcher := opts.Fetcher
	if fetcher == nil {
		fetcher = pricing.FetchAnthropicRates
	}
	fetched, err := fetcher(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pricing refresh: %v\n", err)
		return 1
	}

	// Diff against the hardcoded table and show to the operator
	diffs := pricing.DiffRates(pricing.KnownRates(), fetched)
	diffText := pricing.FormatDiff(diffs)
	fmt.Print(diffText)
	if len(diffs) == 0 {
		return 0
	}

	if opts.DryRun {
		fmt.Println("dry run — no files modified")
		return 0
	}

	// Resolve the command runner once for all subsequent exec operations.
	runner := opts.RunCmd
	if runner == nil {
		runner = runExec
	}

	// Sanity check — block if any existing-model field changed beyond SanityPct.
	// New-model additions (Old==0) are always allowed; see SanityCheck docs.
	if !pricing.SanityCheck(diffs, opts.SanityPct) {
		fmt.Fprintf(os.Stderr, "pricing refresh: rate change exceeds %.0f%% sanity bound — filing issue for manual review\n", opts.SanityPct)
		_ = createSanityIssue(diffText, opts.SanityPct, runner)
		return 1
	}

	// Rewrite table.go
	src := pricing.RenderTable(fetched)
	if err := os.WriteFile(tablePath, []byte(src), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "pricing refresh: write table.go: %v\n", err)
		return 1
	}

	// Verify the generated file compiles; restore on failure.
	if err := runner(asDir, "go", "build", "./..."); err != nil {
		fmt.Fprintf(os.Stderr, "pricing refresh: go build failed after table update: %v — restoring original\n", err)
		if restErr := runner(asDir, "git", "checkout", "--", tablePath); restErr != nil {
			fmt.Fprintf(os.Stderr, "pricing refresh: restore failed: %v — table.go may be inconsistent\n", restErr)
		}
		return 1
	}

	// Commit table.go only (using positional path to avoid sweeping pre-staged changes)
	// and push.
	commitMsg := "chore: update pricing table via st pricing refresh"
	if err := runner(asDir, "git", "commit", tablePath, "-m", commitMsg); err != nil {
		fmt.Fprintf(os.Stderr, "pricing refresh: git commit: %v\n", err)
		return 1
	}
	if err := runner(asDir, "git", "push", "origin", "HEAD"); err != nil {
		fmt.Fprintf(os.Stderr, "pricing refresh: git push: %v\n", err)
		return 1
	}

	fmt.Println("pricing table updated, committed, and pushed")
	return 0
}

// runExec runs a command in dir, streaming stdout/stderr to the terminal.
// An empty dir uses the current working directory.
func runExec(dir string, args ...string) error {
	if len(args) == 0 {
		return fmt.Errorf("runExec: no command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// createSanityIssue files a GitHub issue in TheraPrac/agent-state with the
// diff body using the provided runner. The sanityPct is included in the title
// and body so the issue accurately reflects the configured threshold.
// Failures are non-fatal — the command still returns 1.
func createSanityIssue(diffText string, sanityPct float64, runner func(dir string, args ...string) error) error {
	title := fmt.Sprintf("Pricing sanity: >%.0f%% rate change detected — manual review required", sanityPct)
	body := fmt.Sprintf(
		"Automated `st pricing refresh` detected a rate change exceeding the %.0f%% sanity bound.\n\n```\n%s\n```\n\nVerify against https://docs.anthropic.com/en/docs/about-claude/pricing and update table.go manually.",
		sanityPct, strings.TrimSpace(diffText),
	)
	return runner("", "gh", "issue", "create",
		"--repo", "TheraPrac/agent-state",
		"--title", title,
		"--body", body,
	)
}
