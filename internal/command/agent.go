package command

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
)

type AgentBootstrapOpts struct {
	Name        string
	SkipAWS     bool
	SkipGH      bool
	RotateKey   bool
	DryRun      bool
	Owner       string
	Port        string
	SkipInstall bool
}

type AgentAuthOpts struct {
	Name    string
	SkipAWS bool
	SkipGH  bool
	Force   bool
}

func AgentBootstrap(cfg *config.Config, opts AgentBootstrapOpts) int {
	name := resolveAgentName(cfg, opts.Name)
	dir, err := agentScriptsDir(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent bootstrap: %v\n", err)
		return 1
	}

	if !opts.SkipAWS {
		args := []string{"--name", name}
		if opts.RotateKey {
			args = append(args, "--rotate-key")
		}
		if opts.DryRun {
			args = append(args, "--dry-run")
		}
		if code := runAgentScript(filepath.Join(dir, "agent-bootstrap-aws.sh"), args, os.Stdout, os.Stderr); code != 0 {
			return code
		}
	}

	if !opts.SkipGH {
		args := []string{"--name", name}
		if opts.Owner != "" {
			args = append(args, "--owner", opts.Owner)
		}
		if opts.Port != "" {
			args = append(args, "--port", opts.Port)
		}
		if opts.SkipInstall {
			args = append(args, "--skip-install")
		}
		if code := runAgentScript(filepath.Join(dir, "agent-bootstrap-gh.sh"), args, os.Stdout, os.Stderr); code != 0 {
			return code
		}
	}

	return 0
}

func AgentAuth(cfg *config.Config, opts AgentAuthOpts) int {
	name := resolveAgentName(cfg, opts.Name)
	dir, err := agentScriptsDir(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent auth: %v\n", err)
		return 1
	}

	args := []string{"--name", name}
	if opts.SkipAWS {
		args = append(args, "--skip-aws")
	}
	if opts.SkipGH {
		args = append(args, "--skip-gh")
	}
	if opts.Force {
		args = append(args, "--force")
	}

	return runAgentScript(filepath.Join(dir, "agent-env.sh"), args, os.Stdout, os.Stderr)
}

func AgentList(cfg *config.Config) int {
	dir, err := agentCredentialsDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent list: %v\n", err)
		return 1
	}

	agents := map[string]map[string]bool{}
	for _, prefix := range []string{"aws", "gh"} {
		matches, _ := filepath.Glob(filepath.Join(dir, prefix+"-*.json"))
		for _, match := range matches {
			base := filepath.Base(match)
			if strings.HasSuffix(base, "-session.json") {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(base, prefix+"-"), ".json")
			if name == "" {
				continue
			}
			if agents[name] == nil {
				agents[name] = map[string]bool{}
			}
			agents[name][prefix] = true
		}
	}

	if len(agents) == 0 {
		fmt.Println("No configured agents found")
		return 0
	}

	current := cfg.AgentID()
	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Println("AGENT      AWS  GH  CURRENT")
	for _, name := range names {
		currentMark := ""
		if name == current {
			currentMark = "*"
		}
		fmt.Printf("%-10s %-4s %-3s %s\n", name, yesNo(agents[name]["aws"]), yesNo(agents[name]["gh"]), currentMark)
	}
	return 0
}

func AgentAutoAuth(cfg *config.Config, agentID string) {
	if agentID == "" {
		return
	}
	dir, err := agentScriptsDir(cfg)
	if err != nil {
		return
	}
	if !agentCredentialsExist(agentID) {
		return
	}
	code := runAgentScript(filepath.Join(dir, "agent-env.sh"), []string{"--name", agentID}, io.Discard, os.Stderr)
	if code != 0 {
		fmt.Fprintf(os.Stderr, "warning: agent auth failed for %s (run `eval \"$(st agent auth --name %s)\"` for details)\n", agentID, agentID)
	}
}

func resolveAgentName(cfg *config.Config, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if cfg != nil {
		if id := cfg.AgentID(); id != "" {
			return id
		}
	}
	return "agent-a"
}

func agentScriptsDir(cfg *config.Config) (string, error) {
	if dir := os.Getenv("ST_AGENT_SCRIPTS_DIR"); dir != "" {
		return requireAgentScriptDir(dir)
	}

	var candidates []string
	if cfg != nil {
		root := cfg.Root()
		candidates = append(candidates,
			filepath.Join(root, "..", "theraprac-infra", "scripts"),
			filepath.Join(root, "theraprac-infra", "scripts"),
		)
	}
	candidates = append(candidates,
		filepath.Join(os.Getenv("HOME"), "Dev", "theraprac-agents", "theraprac-agent-a", "theraprac-infra", "scripts"),
	)

	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "agent-env.sh")); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("could not find agent scripts; set ST_AGENT_SCRIPTS_DIR")
}

func requireAgentScriptDir(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "agent-env.sh")); err != nil {
		return "", fmt.Errorf("agent-env.sh not found in %s", dir)
	}
	return dir, nil
}

func runAgentScript(path string, args []string, stdout, stderr io.Writer) int {
	cmd := exec.Command(path, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "%s: %v\n", filepath.Base(path), err)
		return 1
	}
	return 0
}

func agentCredentialsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".theraprac"), nil
}

func agentCredentialsExist(agentID string) bool {
	dir, err := agentCredentialsDir()
	if err != nil {
		return false
	}
	for _, prefix := range []string{"aws", "gh"} {
		if _, err := os.Stat(filepath.Join(dir, prefix+"-"+agentID+".json")); err != nil {
			return false
		}
	}
	return true
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
