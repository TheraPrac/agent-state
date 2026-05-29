package command

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	// Yes confirms a non-dry-run create. Without it, the command
	// prints the plan and refuses to apply. (Renamed from `Full` —
	// the old name implied a symlink-vs-clone toggle, but the field
	// has always just been a confirmation gate. I-559.)
	Yes     bool
	DryRun  bool
	Repair  bool
	SkipAWS bool
	SkipGH  bool
	Owner   string
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
	"as",
}

// sharedSymlinkRepos lists repos that are NOT cloned into each agent's
// workspace — instead, the agent dir gets a symlink to a single canonical
// sibling clone. theraprac-workspace holds shared agent-state, so giving
// each agent its own clone causes push/rebase contention (I-418). The
// canonical clone lives at <agentsRoot>/<name>.
var sharedSymlinkRepos = map[string]bool{
	"theraprac-workspace": true,
}

func AgentBootstrap(cfg *config.Config, opts AgentBootstrapOpts) int {
	name := resolveAgentName(cfg, opts.Name)
	dir, err := agentScriptsDir(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent bootstrap: %v\n", err)
		return 1
	}

	awsState := "skipped"
	ghState := "skipped"

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
		if opts.DryRun {
			awsState = "dry-run"
		} else {
			awsState = "ok"
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
		if opts.DryRun {
			ghState = "dry-run"
		} else {
			ghState = "ok"
		}
	}

	// I-560: single flow-level completion marker. The step scripts
	// print step-level markers ("AWS step complete." / "GitHub step
	// complete."); this is the one canonical line monitoring/watcher
	// scripts can grep to know the wrapped flow finished. Failure
	// short-circuits earlier and never reaches this line, so an exit
	// code of 0 plus this marker means full success.
	fmt.Fprintf(os.Stdout, "agent bootstrap complete: %s (aws=%s gh=%s)\n", name, awsState, ghState)
	return 0
}

func AgentWorkspaceCreate(cfg *config.Config, opts AgentWorkspaceCreateOpts) int {
	plan, err := buildAgentWorkspacePlan(cfg, opts.Agent, opts.Branch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent workspace create: %v\n", err)
		return 1
	}
	printAgentWorkspacePlan(plan, opts.DryRun, opts.Repair)
	printIdentityBootstrapPlan(plan.AgentID, opts.SkipAWS, opts.SkipGH)
	if opts.DryRun {
		return 0
	}
	if !opts.Yes {
		fmt.Fprintln(os.Stderr, "agent workspace create: non-dry-run create requires --yes")
		return 2
	}
	if err := applyWorkspaceCreate(plan, opts.Repair); err != nil {
		fmt.Fprintf(os.Stderr, "agent workspace create: %v\n", err)
		return 1
	}
	if err := runIdentityBootstrap(cfg, plan.AgentID, opts.Owner, opts.SkipAWS, opts.SkipGH); err != nil {
		fmt.Fprintf(os.Stderr,
			"agent workspace create: identity bootstrap failed for %s: %v\n  retry with: st agent bootstrap --name %s\n",
			plan.AgentID, err, plan.AgentID)
		return 1
	}
	return 0
}

// applyWorkspaceCreate is a package-level var so tests can stub the
// heavy apply path (real git clones + Docker) and exercise the wiring
// from AgentWorkspaceCreate into runIdentityBootstrap.
var applyWorkspaceCreate = applyAgentWorkspaceCreate

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
	fmt.Printf("    postgres container: %s\n", postgresContainerName(plan.AgentID))
	fmt.Printf("    postgres volume:    %s\n", postgresVolumeName(plan.AgentID))
	fmt.Printf("    mailpit container:  %s\n", mailpitContainerName(plan.AgentID))
	fmt.Println("  repos:")
	for _, repo := range plan.Repos {
		fmt.Printf("    %s\n", repo.TargetPath)
	}
	if opts.DryRun {
		fmt.Println("  dry-run: no files, containers, or volumes removed")
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
	if err := dockerRemoveAgent(plan.AgentID); err != nil {
		fmt.Fprintf(os.Stderr, "agent workspace destroy: docker cleanup: %v\n", err)
		return 1
	}
	if err := os.RemoveAll(plan.TargetDir); err != nil {
		fmt.Fprintf(os.Stderr, "agent workspace destroy: %v\n", err)
		return 1
	}
	registryPath := agentWorkspaceRegistryPath(plan)
	if err := os.Remove(registryPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "agent workspace destroy: registry cleanup: %v\n", err)
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
	// Source repos live as siblings of the running agent's workspace dir.
	// I-778: RepoParent() resolves per-agent repo parent via .as/agent-workspace.yaml
	// (honoring worktree.parent_dir overrides) so the bootstrap doesn't
	// clone from a peer agent's checkout under an ST_ROOT env leak
	// (cfg.Root() can resolve to the peer's workspace).
	repoParent := cfg.RepoParent()
	for _, repo := range agentWorkspaceRepos {
		source := filepath.Join(repoParent, repo)
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

func printAgentWorkspacePlan(plan agentWorkspacePlan, dryRun, repair bool) {
	fmt.Printf("Agent workspace create plan: %s\n", plan.AgentID)
	fmt.Printf("  target: %s\n", plan.TargetDir)
	fmt.Printf("  branch: %s\n", plan.Branch)
	fmt.Printf("  mode: dry-run=%t repair=%t\n", dryRun, repair)
	fmt.Printf("  ports: api=%d web=%d db=%d mailpit=%d stripe=%d\n",
		plan.Ports.API, plan.Ports.Web, plan.Ports.DB, plan.Ports.Mailpit, plan.Ports.Stripe)
	fmt.Printf("  compose_project: %s\n", plan.ComposeProject)
	fmt.Printf("  docker_label: theraprac.agent=%s\n", plan.AgentID)
	fmt.Printf("  registry: %s\n", agentWorkspaceRegistryPath(plan))
	fmt.Printf("  workspace_config: %s\n", agentWorkspaceLocalConfigPath(plan))
	fmt.Printf("  st_root: %s -> theraprac-workspace\n", filepath.Join(plan.TargetDir, ".st-root"))
	fmt.Printf("  postgres container: %s (image %s, host port %d)\n", postgresContainerName(plan.AgentID), postgresImage, plan.Ports.DB)
	fmt.Printf("  postgres volume:    %s\n", postgresVolumeName(plan.AgentID))
	fmt.Printf("  mailpit container:  %s (image %s, smtp host port %d, ui host port %d)\n",
		mailpitContainerName(plan.AgentID), mailpitImage, mailpitSMTPPort(plan.Ports.Mailpit), plan.Ports.Mailpit)
	fmt.Println("  env files: copy from source repo and overlay per-agent ports (DB_PORT, SERVER_PORT, API_BASE_URL, PORT)")
	fmt.Println("  repos:")
	for _, repo := range plan.Repos {
		var action string
		if sharedSymlinkRepos[repo.Name] {
			switch repo.State {
			case "absent":
				action = "create symlink -> ../" + repo.Name
			case "symlink":
				action = "verify symlink -> ../" + repo.Name
			case "git":
				action = "refuse: existing clone (use --repair to replace with symlink)"
			default:
				action = "refuse: " + repo.State
			}
		} else {
			switch repo.State {
			case "git":
				action = "repair/check"
			case "symlink":
				action = "repair symlink -> independent clone"
			case "dir":
				action = "refuse unless empty/non-git directory is reviewed"
			default:
				action = "clone"
			}
			if repo.Name == "as" {
				// applyAgentWorkspaceCreate runs make install on every
				// non-shared apply, including the existing-clone (git)
				// case, so the plan must surface it for every state.
				action += " + make install"
			}
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
		if sharedSymlinkRepos[repo.Name] {
			if err := ensureSharedSymlink(repo, plan.AgentsRoot, repair); err != nil {
				return err
			}
			continue
		}
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
		if err := materializeEnv(repo.Name, repo.SourcePath, repo.TargetPath, plan.Ports); err != nil {
			return fmt.Errorf("%s env materialize: %w", repo.Name, err)
		}
		if repo.Name == "as" {
			if err := runAsInstall(repo.TargetPath); err != nil {
				return fmt.Errorf("%s make install: %w", repo.Name, err)
			}
		}
	}
	if err := persistAgentWorkspaceConfig(plan); err != nil {
		return err
	}
	if err := writeStRoot(plan.TargetDir); err != nil {
		return fmt.Errorf("st-root: %w", err)
	}
	if err := linkClaudeContext(plan, repair); err != nil {
		return fmt.Errorf("claude context: %w", err)
	}
	if err := dockerStartPostgres(plan.AgentID, plan.Ports.DB); err != nil {
		return fmt.Errorf("postgres container: %w", err)
	}
	if err := dockerStartMailpit(plan.AgentID, mailpitSMTPPort(plan.Ports.Mailpit), plan.Ports.Mailpit); err != nil {
		return fmt.Errorf("mailpit container: %w", err)
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

// agentScriptTimeout bounds AWS IAM key creation + GitHub App install.
// These are network-bound; a stalled HTTPS connect must not hang
// `st agent workspace create` indefinitely. 10 minutes mirrors the
// runAsInstall budget from I-475.
const agentScriptTimeout = 10 * time.Minute

func runAgentScript(path string, args []string, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), agentScriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
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

// runIdentityBootstrap chains AgentBootstrap onto a successful workspace
// create so a single command produces a fully usable agent (clones +
// build + AWS + GH identity). Honors caller skip flags so operators can
// opt out of either half (e.g., reuse a shared AWS user). Resolves the
// bootstrap scripts from the *caller's* config — the new agent's clone
// may not be fully provisioned yet. Stub-friendly via package-level var
// to keep tests off live AWS/GH scripts.
var runIdentityBootstrap = func(cfg *config.Config, agentID, owner string, skipAWS, skipGH bool) error {
	if skipAWS && skipGH {
		// Both halves opted out — no work to do, and AgentBootstrap
		// resolves agentScriptsDir up-front, which can fail on a
		// machine without theraprac-infra cloned. Skip is skip.
		return nil
	}
	if code := AgentBootstrap(cfg, AgentBootstrapOpts{
		Name: agentID, Owner: owner, SkipAWS: skipAWS, SkipGH: skipGH,
	}); code != 0 {
		return fmt.Errorf("agent bootstrap exited with code %d", code)
	}
	return nil
}

// printIdentityBootstrapPlan surfaces the AWS/GH chain in dry-run output
// so operators discover the auto-bootstrap behavior without reading the
// help text.
func printIdentityBootstrapPlan(agentID string, skipAWS, skipGH bool) {
	state := func(skip bool) string {
		if skip {
			return "skip"
		}
		return "run"
	}
	fmt.Printf("  identity bootstrap: agent=%s aws=%s gh=%s\n", agentID, state(skipAWS), state(skipGH))
}

// runAsInstall builds the per-agent st binary in the freshly-cloned `as`
// repo. The dispatcher (I-419) walks up from $PWD to find theraprac-agent-*
// and uses ITS as/bin/st — so without this build, a new agent has no
// per-agent binary and the dispatcher silently falls back to a sibling's.
//
// A 10-minute deadline guards against module-download stalls; on a fresh
// machine `make` and `go` are required, so we pre-flight LookPath to
// surface a precise error instead of an exec-not-found message.
var runAsInstall = func(targetPath string) error {
	for _, tool := range []string{"make", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not on PATH (install xcode-select / build-essential / go toolchain): %w", tool, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "install")
	cmd.Dir = targetPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
	// Offsets start at 100 so the central dev stack on default ports
	// (5432 postgres, 8025 mailpit UI, 3000 web, 8080 api) is never
	// collided with by agent-a. a→100, b→200, ..., z→2600.
	offset := 100
	suffix := strings.TrimPrefix(agentID, "agent-")
	if len(suffix) == 1 && suffix[0] >= 'a' && suffix[0] <= 'z' {
		offset = (int(suffix[0]-'a') + 1) * 100
	} else {
		sum := 0
		for _, r := range suffix {
			sum += int(r)
		}
		offset = ((sum % 20) + 1) * 100
	}
	return agentWorkspacePorts{
		Web:     3000 + offset,
		API:     8080 + offset,
		DB:      5432 + offset,
		Mailpit: 8025 + offset,
		Stripe:  12111 + offset,
	}
}

// linkClaudeContext wires the per-agent Claude Code context to the
// committed claude-config and agent-memory inside the workspace repo.
// Four idempotent symlinks are created so a fresh agent dir picks up the
// shared CLAUDE.md, hooks, settings, and auto-memory:
//
//	<target>/CLAUDE.md             -> theraprac-workspace/claude-config/CLAUDE.md
//	<target>/.claude/hooks         -> theraprac-workspace/claude-config/hooks
//	<target>/.claude/settings.json -> theraprac-workspace/claude-config/settings.json
//	~/.claude/projects/<encoded>/memory -> theraprac-workspace/agent-memory
//
// <encoded> is the absolute target dir with "/" replaced by "-", matching
// Claude Code's per-cwd projects layout. A correct existing symlink is
// left alone; a wrong one is replaced when repair is true.
func linkClaudeContext(plan agentWorkspacePlan, repair bool) error {
	workspaceDir := filepath.Join(plan.TargetDir, "theraprac-workspace")
	claudeConfig := filepath.Join(workspaceDir, "claude-config")
	agentMemory := filepath.Join(workspaceDir, "agent-memory")

	for _, p := range []string{
		filepath.Join(claudeConfig, "CLAUDE.md"),
		filepath.Join(claudeConfig, "hooks"),
		filepath.Join(claudeConfig, "settings.json"),
		agentMemory,
	} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing in workspace repo: %s: %w", p, err)
		}
	}

	dotClaude := filepath.Join(plan.TargetDir, ".claude")
	if err := os.MkdirAll(dotClaude, 0755); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	encoded := strings.ReplaceAll(plan.TargetDir, "/", "-")
	projectDir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		return err
	}

	links := []struct{ link, target string }{
		{filepath.Join(plan.TargetDir, "CLAUDE.md"), filepath.Join(claudeConfig, "CLAUDE.md")},
		{filepath.Join(dotClaude, "hooks"), filepath.Join(claudeConfig, "hooks")},
		{filepath.Join(dotClaude, "settings.json"), filepath.Join(claudeConfig, "settings.json")},
		{filepath.Join(projectDir, "memory"), agentMemory},
	}
	for _, l := range links {
		if err := ensureSymlink(l.link, l.target, repair); err != nil {
			return err
		}
	}
	return nil
}

// ensureSymlink makes `link` point at `target`, creating it if missing,
// leaving it alone if already correct, and replacing a wrong symlink only
// when repair is true. A non-symlink at `link` is always an error so we
// never overwrite operator data.
func ensureSymlink(link, target string, repair bool) error {
	info, err := os.Lstat(link)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(target, link)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s exists and is not a symlink; remove it manually before re-running", link)
	}
	existing, err := os.Readlink(link)
	if err != nil {
		return err
	}
	if existing == target {
		return nil
	}
	if !repair {
		return fmt.Errorf("symlink %s points to %s, expected %s; rerun with --repair to replace", link, existing, target)
	}
	if err := os.Remove(link); err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// ensureSharedSymlink makes <agentDir>/<repo.Name> a symlink to the
// canonical sibling clone at <agentsRoot>/<repo.Name>. Used for repos in
// sharedSymlinkRepos (currently theraprac-workspace) so all agents read
// and write the same .git, eliminating push/rebase contention (I-418).
func ensureSharedSymlink(repo agentWorkspaceRepoPlan, agentsRoot string, repair bool) error {
	canonical := filepath.Join(agentsRoot, repo.Name)
	if !isGitDir(canonical) {
		return fmt.Errorf("%s: canonical clone missing at %s — bootstrap it first", repo.Name, canonical)
	}
	rel := filepath.Join("..", repo.Name)
	switch repo.State {
	case "absent":
		return os.Symlink(rel, repo.TargetPath)
	case "symlink":
		current, err := os.Readlink(repo.TargetPath)
		if err != nil {
			return fmt.Errorf("%s readlink: %w", repo.Name, err)
		}
		// Resolve to absolute for comparison.
		currentAbs := current
		if !filepath.IsAbs(currentAbs) {
			currentAbs = filepath.Join(filepath.Dir(repo.TargetPath), current)
		}
		currentAbs = filepath.Clean(currentAbs)
		if currentAbs == filepath.Clean(canonical) {
			return nil
		}
		if !repair {
			return fmt.Errorf("%s symlink points to %s, expected %s; rerun with --repair", repo.Name, current, rel)
		}
		if err := os.Remove(repo.TargetPath); err != nil {
			return err
		}
		return os.Symlink(rel, repo.TargetPath)
	case "git", "dir", "file":
		if !repair {
			return fmt.Errorf("%s already exists as %s at %s; rerun with --repair to replace with a symlink", repo.Name, repo.State, repo.TargetPath)
		}
		// With --repair, refuse to delete a non-empty git clone — operator must remove it deliberately.
		return fmt.Errorf("%s exists as %s at %s; --repair refuses to delete a clone — remove manually if intended", repo.Name, repo.State, repo.TargetPath)
	default:
		return fmt.Errorf("%s unexpected state %s", repo.Name, repo.State)
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

// materializeEnv copies .env / .env.local from the source repo into the
// agent workspace's repo dir and applies per-agent port overrides so each
// workspace can run its own dev stack against its own DB on its own ports.
//
// It deliberately does NOT symlink — symlinking the source agent's .env
// into the target would mean editing the per-agent port overrides on
// agent-b would also change agent-a's .env, defeating isolation.
func materializeEnv(repoName, sourceDir, targetDir string, ports agentWorkspacePorts) error {
	overrides := perRepoEnvOverrides(repoName, ports)
	for _, name := range []string{".env", ".env.local"} {
		source := filepath.Join(sourceDir, name)
		if _, err := os.Stat(source); err != nil {
			continue
		}
		target := filepath.Join(targetDir, name)
		// Replace any existing symlink left over from older create runs.
		if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
			_ = os.Remove(target)
		}
		body, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		out, err := applyEnvOverrides(string(body), overrides)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(out), 0644); err != nil {
			return err
		}
	}
	return nil
}

// perRepoEnvOverrides returns the env keys that should be rewritten for
// the given repo so that the agent's local stack uses its own port block.
func perRepoEnvOverrides(repoName string, ports agentWorkspacePorts) map[string]string {
	apiURL := fmt.Sprintf("http://localhost:%d", ports.API)
	switch repoName {
	case "theraprac-api":
		return map[string]string{
			"SERVER_PORT": fmt.Sprintf("%d", ports.API),
			"DB_HOST":     "localhost",
			"DB_PORT":     fmt.Sprintf("%d", ports.DB),
		}
	case "theraprac-web":
		return map[string]string{
			"PORT":         fmt.Sprintf("%d", ports.Web),
			"API_BASE_URL": apiURL,
		}
	}
	return nil
}

// applyEnvOverrides returns body with the given keys' values replaced.
// Keys absent from body are appended at the end, marked as agent-injected.
// Comments and blank lines are preserved.
func applyEnvOverrides(body string, overrides map[string]string) (string, error) {
	if len(overrides) == 0 {
		return body, nil
	}
	seen := make(map[string]bool, len(overrides))
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if v, ok := overrides[key]; ok {
			lines[i] = key + "=" + v
			seen[key] = true
		}
	}
	var missing []string
	for k := range overrides {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "# st agent workspace — per-agent overrides")
		for _, k := range missing {
			lines = append(lines, k+"="+overrides[k])
		}
	}
	return strings.Join(lines, "\n"), nil
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

func writeStRoot(targetDir string) error {
	return os.WriteFile(filepath.Join(targetDir, ".st-root"), []byte("theraprac-workspace"), 0644)
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
