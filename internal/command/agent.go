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

type AgentWorkspaceCreateOpts struct {
	Agent  string
	Branch string
	Full   bool
	DryRun bool
	Repair bool
}

type AgentWorkspaceStatusOpts struct {
	Agent string
}

type AgentWorkspaceDestroyOpts struct {
	Agent  string
	DryRun bool
	Force  bool
}

var agentWorkspaceRepos = []string{
	"theraprac-api",
	"theraprac-web",
	"theraprac-infra",
	"theraprac-workspace",
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

func AgentWorkspaceCreate(cfg *config.Config, opts AgentWorkspaceCreateOpts) int {
	plan, err := buildAgentWorkspacePlan(cfg, opts.Agent, opts.Branch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent workspace create: %v\n", err)
		return 1
	}
	printAgentWorkspacePlan(plan, opts.DryRun, opts.Full, opts.Repair)
	if opts.DryRun {
		return 0
	}
	if !opts.Full {
		fmt.Fprintln(os.Stderr, "agent workspace create: non-dry-run create requires --full")
		return 2
	}
	if err := applyAgentWorkspaceCreate(plan, opts.Repair); err != nil {
		fmt.Fprintf(os.Stderr, "agent workspace create: %v\n", err)
		return 1
	}
	return 0
}

func AgentWorkspaceStatus(cfg *config.Config, opts AgentWorkspaceStatusOpts) int {
	plan, err := buildAgentWorkspacePlan(cfg, opts.Agent, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent workspace status: %v\n", err)
		return 1
	}
	fmt.Printf("Agent workspace: %s\n", plan.AgentID)
	fmt.Printf("  path: %s\n", plan.TargetDir)
	fmt.Printf("  identity: %s\n", plan.AgentID)
	fmt.Printf("  ports: api=%d web=%d db=%d mailpit=%d stripe=%d\n",
		plan.Ports.API, plan.Ports.Web, plan.Ports.DB, plan.Ports.Mailpit, plan.Ports.Stripe)
	fmt.Printf("  compose: %s\n", plan.ComposeProject)
	fmt.Println("  repos:")
	for _, repo := range plan.Repos {
		state := repoState(repo.TargetPath)
		dirty := "unknown"
		if isGitDir(repo.TargetPath) {
			out, err := runGit(repo.TargetPath, "status", "--short")
			if err == nil && strings.TrimSpace(out) == "" {
				dirty = "clean"
			} else if err == nil {
				dirty = "dirty"
			}
		}
		fmt.Printf("    %-20s %-10s dirty=%s path=%s\n", repo.Name, state, dirty, repo.TargetPath)
	}
	fmt.Println("  service_health: unknown (status command does not start services)")
	return 0
}

func AgentWorkspaceDestroy(cfg *config.Config, opts AgentWorkspaceDestroyOpts) int {
	plan, err := buildAgentWorkspacePlan(cfg, opts.Agent, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent workspace destroy: %v\n", err)
		return 1
	}
	if !strings.HasPrefix(filepath.Base(plan.TargetDir), "theraprac-agent-") {
		fmt.Fprintln(os.Stderr, "agent workspace destroy: refusing non-agent workspace path")
		return 1
	}
	fmt.Printf("Destroy agent workspace: %s\n", plan.AgentID)
	fmt.Printf("  path: %s\n", plan.TargetDir)
	fmt.Println("  docker resources:")
	fmt.Printf("    compose project: %s\n", plan.ComposeProject)
	fmt.Printf("    label selector: theraprac.agent=%s\n", plan.AgentID)
	fmt.Println("  repos:")
	for _, repo := range plan.Repos {
		fmt.Printf("    %s\n", repo.TargetPath)
	}
	if opts.DryRun {
		fmt.Println("  dry-run: no files, containers, networks, or volumes removed")
		return 0
	}
	if !opts.Force {
		for _, repo := range plan.Repos {
			if isGitDir(repo.TargetPath) {
				if out, err := runGit(repo.TargetPath, "status", "--short"); err == nil && strings.TrimSpace(out) != "" {
					fmt.Fprintf(os.Stderr, "agent workspace destroy: refusing dirty repo %s (use --force only after review)\n", repo.TargetPath)
					return 1
				}
			}
		}
	}
	if err := os.RemoveAll(plan.TargetDir); err != nil {
		fmt.Fprintf(os.Stderr, "agent workspace destroy: %v\n", err)
		return 1
	}
	return 0
}

type agentWorkspacePlan struct {
	AgentID        string
	AgentsRoot     string
	TargetDir      string
	Branch         string
	Ports          agentWorkspacePorts
	ComposeProject string
	Repos          []agentWorkspaceRepoPlan
}

type agentWorkspacePorts struct {
	Web     int
	API     int
	DB      int
	Mailpit int
	Stripe  int
}

type agentWorkspaceRepoPlan struct {
	Name       string
	SourcePath string
	RemoteURL  string
	TargetPath string
	State      string
}

func buildAgentWorkspacePlan(cfg *config.Config, agentArg, branch string) (agentWorkspacePlan, error) {
	agentID := normalizeWorkspaceAgentID(agentArg)
	if agentID == "" {
		agentID = resolveAgentName(cfg, "")
	}
	if !strings.HasPrefix(agentID, "agent-") {
		return agentWorkspacePlan{}, fmt.Errorf("agent id must be like agent-a or b")
	}
	if branch == "" {
		branch = "main"
	}
	root, err := resolveAgentsRoot(cfg)
	if err != nil {
		return agentWorkspacePlan{}, err
	}
	targetDir := filepath.Join(root, "theraprac-"+agentID)
	plan := agentWorkspacePlan{
		AgentID:        agentID,
		AgentsRoot:     root,
		TargetDir:      targetDir,
		Branch:         branch,
		Ports:          portBlockForAgent(agentID),
		ComposeProject: strings.ReplaceAll("theraprac_"+agentID, "-", "_"),
	}
	for _, repo := range agentWorkspaceRepos {
		source := filepath.Join(filepath.Dir(filepath.Dir(cfg.Root())), repo)
		remote := ""
		if isGitDir(source) {
			if out, err := runGit(source, "remote", "get-url", "origin"); err == nil {
				remote = strings.TrimSpace(out)
			}
		}
		target := filepath.Join(targetDir, repo)
		plan.Repos = append(plan.Repos, agentWorkspaceRepoPlan{
			Name:       repo,
			SourcePath: source,
			RemoteURL:  remote,
			TargetPath: target,
			State:      repoState(target),
		})
	}
	return plan, nil
}

func printAgentWorkspacePlan(plan agentWorkspacePlan, dryRun, full, repair bool) {
	fmt.Printf("Agent workspace create plan: %s\n", plan.AgentID)
	fmt.Printf("  target: %s\n", plan.TargetDir)
	fmt.Printf("  branch: %s\n", plan.Branch)
	fmt.Printf("  mode: full=%t repair=%t dry-run=%t\n", full, repair, dryRun)
	fmt.Printf("  ports: api=%d web=%d db=%d mailpit=%d stripe=%d\n",
		plan.Ports.API, plan.Ports.Web, plan.Ports.DB, plan.Ports.Mailpit, plan.Ports.Stripe)
	fmt.Printf("  compose_project: %s\n", plan.ComposeProject)
	fmt.Printf("  docker_label: theraprac.agent=%s\n", plan.AgentID)
	fmt.Printf("  registry: %s\n", agentWorkspaceRegistryPath(plan))
	fmt.Printf("  workspace_config: %s\n", agentWorkspaceLocalConfigPath(plan))
	fmt.Println("  env files: symlink from source repo when present; otherwise leave absent")
	fmt.Println("  repos:")
	for _, repo := range plan.Repos {
		action := "clone"
		switch repo.State {
		case "git":
			action = "repair/check"
		case "symlink":
			action = "repair symlink -> independent clone"
		case "dir":
			action = "refuse unless empty/non-git directory is reviewed"
		}
		fmt.Printf("    %-20s state=%-8s action=%-38s target=%s\n",
			repo.Name, repo.State, action, repo.TargetPath)
	}
	if dryRun {
		fmt.Println("  dry-run: no filesystem, git, Docker, or env changes will be made")
	}
}

func applyAgentWorkspaceCreate(plan agentWorkspacePlan, repair bool) error {
	if err := os.MkdirAll(plan.TargetDir, 0755); err != nil {
		return err
	}
	for _, repo := range plan.Repos {
		switch repo.State {
		case "git":
			if _, err := runGit(repo.TargetPath, "fetch", "origin"); err != nil {
				return fmt.Errorf("%s fetch: %w", repo.Name, err)
			}
		case "symlink":
			if !repair {
				return fmt.Errorf("%s is a symlink at %s; rerun with --repair to replace it", repo.Name, repo.TargetPath)
			}
			if err := os.Remove(repo.TargetPath); err != nil {
				return err
			}
			fallthrough
		case "absent":
			if repo.RemoteURL == "" {
				return fmt.Errorf("no origin remote discovered for %s from %s", repo.Name, repo.SourcePath)
			}
			if _, err := runGit(plan.TargetDir, "clone", repo.RemoteURL, repo.Name); err != nil {
				return fmt.Errorf("%s clone: %w", repo.Name, err)
			}
		default:
			return fmt.Errorf("%s already exists as %s at %s", repo.Name, repo.State, repo.TargetPath)
		}
		if _, err := runGit(repo.TargetPath, "checkout", plan.Branch); err != nil {
			return fmt.Errorf("%s checkout %s: %w", repo.Name, plan.Branch, err)
		}
		if plan.Branch == "main" {
			_, _ = runGit(repo.TargetPath, "pull", "--ff-only", "origin", "main")
		}
		symlinkEnv(repo.SourcePath, repo.TargetPath, ".env")
		symlinkEnv(repo.SourcePath, repo.TargetPath, ".env.local")
	}
	if err := persistAgentWorkspaceConfig(plan); err != nil {
		return err
	}
	return nil
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

func normalizeWorkspaceAgentID(agent string) string {
	a := strings.TrimSpace(agent)
	if a == "" {
		return ""
	}
	if strings.HasPrefix(a, "agent-") {
		return a
	}
	return "agent-" + a
}

func resolveAgentsRoot(cfg *config.Config) (string, error) {
	if root := os.Getenv("THERAPRAC_AGENTS_ROOT"); root != "" {
		if filepath.Base(root) != "theraprac-agents" {
			return "", fmt.Errorf("THERAPRAC_AGENTS_ROOT must end in theraprac-agents: %s", root)
		}
		return root, nil
	}
	if cfg == nil {
		return "", fmt.Errorf("config is required")
	}
	root := cfg.Root()
	parent := filepath.Dir(root)
	if strings.HasPrefix(filepath.Base(parent), "theraprac-agent-") {
		agentsRoot := filepath.Dir(parent)
		if filepath.Base(agentsRoot) == "theraprac-agents" {
			return agentsRoot, nil
		}
	}
	grandparent := filepath.Dir(parent)
	if filepath.Base(grandparent) == "theraprac-agents" {
		return grandparent, nil
	}
	return "", fmt.Errorf("could not determine theraprac-agents root; set THERAPRAC_AGENTS_ROOT")
}

func portBlockForAgent(agentID string) agentWorkspacePorts {
	offset := 0
	suffix := strings.TrimPrefix(agentID, "agent-")
	if len(suffix) == 1 && suffix[0] >= 'a' && suffix[0] <= 'z' {
		offset = int(suffix[0]-'a') * 100
	} else {
		for _, r := range suffix {
			offset += int(r)
		}
		offset = (offset % 20) * 100
	}
	return agentWorkspacePorts{
		Web:     3000 + offset,
		API:     8080 + offset,
		DB:      5432 + offset,
		Mailpit: 8025 + offset,
		Stripe:  12111 + offset,
	}
}

func repoState(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "absent"
		}
		return "unknown"
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "symlink"
	}
	if isGitDir(path) {
		return "git"
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err == nil && len(entries) == 0 {
			return "empty"
		}
		return "dir"
	}
	return "file"
}

func symlinkEnv(sourceDir, targetDir, name string) {
	source := filepath.Join(sourceDir, name)
	if _, err := os.Stat(source); err != nil {
		return
	}
	target := filepath.Join(targetDir, name)
	if _, err := os.Lstat(target); err == nil {
		return
	}
	_ = os.Symlink(source, target)
}

func persistAgentWorkspaceConfig(plan agentWorkspacePlan) error {
	content := renderAgentWorkspaceConfig(plan)
	for _, path := range []string{agentWorkspaceRegistryPath(plan), agentWorkspaceLocalConfigPath(plan)} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return err
		}
	}
	return nil
}

func agentWorkspaceRegistryPath(plan agentWorkspacePlan) string {
	return filepath.Join(plan.AgentsRoot, ".as", "agent-workspaces", plan.AgentID+".yaml")
}

func agentWorkspaceLocalConfigPath(plan agentWorkspacePlan) string {
	return filepath.Join(plan.TargetDir, ".as", "agent-workspace.yaml")
}

func renderAgentWorkspaceConfig(plan agentWorkspacePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent_id: %s\n", plan.AgentID)
	fmt.Fprintf(&b, "path: %s\n", plan.TargetDir)
	fmt.Fprintf(&b, "branch: %s\n", plan.Branch)
	fmt.Fprintf(&b, "compose_project: %s\n", plan.ComposeProject)
	fmt.Fprintf(&b, "docker_label: theraprac.agent=%s\n", plan.AgentID)
	fmt.Fprintf(&b, "ports:\n")
	fmt.Fprintf(&b, "  web: %d\n", plan.Ports.Web)
	fmt.Fprintf(&b, "  api: %d\n", plan.Ports.API)
	fmt.Fprintf(&b, "  db: %d\n", plan.Ports.DB)
	fmt.Fprintf(&b, "  mailpit: %d\n", plan.Ports.Mailpit)
	fmt.Fprintf(&b, "  stripe: %d\n", plan.Ports.Stripe)
	fmt.Fprintf(&b, "repos:\n")
	for _, repo := range plan.Repos {
		fmt.Fprintf(&b, "  %s: %s\n", repo.Name, repo.TargetPath)
	}
	return b.String()
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
