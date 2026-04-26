package command

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ReconcileOpts configures the reconcile command.
type ReconcileOpts struct {
	DryRun bool

	// Injectable dependencies (nil = use defaults). Struct-based for thread safety.
	ToolCheck   func(string) bool
	BranchCheck func(*config.Config, string) bool
	PRFetch     func(*config.Config, string) (string, []string)
	S3Check     func(string) bool
}

func (o *ReconcileOpts) toolCheck() func(string) bool {
	if o.ToolCheck != nil {
		return o.ToolCheck
	}
	return toolAvailable
}

func (o *ReconcileOpts) branchCheck() func(*config.Config, string) bool {
	if o.BranchCheck != nil {
		return o.BranchCheck
	}
	return branchExistsOnRemote
}

func (o *ReconcileOpts) prFetch() func(*config.Config, string) (string, []string) {
	if o.PRFetch != nil {
		return o.PRFetch
	}
	return getPRState
}

func (o *ReconcileOpts) s3Check() func(string) bool {
	if o.S3Check != nil {
		return o.S3Check
	}
	return s3Exists
}

// Reconcile syncs delivery stages with GitHub state and performs housekeeping.
// Phases:
// 0: coding/committed → pushed (if branch exists on remote)
// 1: pushed → pr_open/merged (via gh pr list)
// 2: pr_open → merged (via gh pr view)
// 3: completed items → archive/ (move files)
// 4: regenerate index.md
func Reconcile(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	var updates int

	// Check tool availability
	hasGH := opts.toolCheck()("gh")

	if !hasGH {
		fmt.Println("  (gh not available — skipping GitHub phases)")
	}

	// Phase 0: coding/committed → pushed
	fmt.Println("Phase 0: Check branch existence")
	n := reconcileBranchPush(s, cfg, opts)
	updates += n

	// Phase 1: pushed → pr_open/merged
	if hasGH {
		fmt.Println("Phase 1: Check PR state (pushed → pr_open/merged)")
		n = reconcilePRState(s, cfg, opts)
		updates += n
	}

	// Phase 2: pr_open → merged
	if hasGH {
		fmt.Println("Phase 2: Check merge state (pr_open → merged)")
		n = reconcileMergeState(s, cfg, opts)
		updates += n
	}

	// Phase 3: merged → deployed_dev (AWS orchestrator check)
	hasAWS := opts.toolCheck()("aws")
	if hasAWS {
		fmt.Println("Phase 3: Check deployment state (merged → deployed_dev)")
		n = reconcileDeployState(s, cfg, opts)
		updates += n
	} else {
		fmt.Println("Phase 3: (aws not available — skipping deployment check)")
	}

	// Phase 4: Move completed items to archive
	fmt.Println("Phase 4: Archive completed items")
	n = reconcileArchive(s, cfg, opts)
	updates += n

	// Phase 5: Regenerate index
	fmt.Println("Phase 5: Regenerate index")
	if !opts.DryRun {
		Index(s, cfg)
	}

	// Summary
	if opts.DryRun {
		fmt.Printf("\nreconcile dry run: %d updates detected\n", updates)
	} else {
		fmt.Printf("\nreconcile: %d updates applied\n", updates)
	}

	return 0
}

// reconcileBranchPush checks if items at coding/committed stage have branches
// that exist on remote, and advances them to pushed.
func reconcileBranchPush(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	updates := 0
	for _, item := range s.List() {
		stage, _ := getNestedField(item, "delivery", "stage")
		if stage != "coding" && stage != "committed" {
			continue
		}

		branch, _ := getNestedField(item, "work_tracking", "branch")
		if branch == "" || branch == "null" {
			continue
		}

		// Check if branch exists on any remote
		if opts.branchCheck()(cfg, branch) {
			updates++
			fmt.Printf("  %s: %s → pushed (branch %s found on remote)\n", item.ID, stage, branch)
			if !opts.DryRun {
				itemID := item.ID
				if err := s.Mutate(itemID, func(item *model.Item) error {
					item.SetNested("delivery", "stage", "pushed")
					return nil
				}); err != nil {
					fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", itemID, err)
				}
			}
		}
	}
	return updates
}

// reconcilePRState checks pushed items for open or merged PRs and advances stage.
func reconcilePRState(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	updates := 0
	for _, item := range s.List() {
		stage, _ := getNestedField(item, "delivery", "stage")
		if stage != "pushed" {
			continue
		}

		branch, _ := getNestedField(item, "work_tracking", "branch")
		if branch == "" || branch == "null" {
			continue
		}

		// Check PR state via gh
		prState, prURLs := opts.prFetch()(cfg, branch)
		if prState == "" {
			continue
		}

		newStage := ""
		switch prState {
		case "OPEN":
			newStage = "pr_open"
		case "MERGED":
			newStage = "merged"
		case "CLOSED":
			fmt.Fprintf(os.Stderr, "  warning: %s PRs closed without merging (branch: %s)\n", item.ID, branch)
			continue
		}

		if newStage != "" {
			updates++
			fmt.Printf("  %s: pushed → %s (PR %s)\n", item.ID, newStage, prState)
			if !opts.DryRun {
				itemID := item.ID
				capturedStage := newStage
				capturedURLs := prURLs
				if err := s.Mutate(itemID, func(item *model.Item) error {
					item.SetNested("delivery", "stage", capturedStage)
					if len(capturedURLs) > 0 {
						storePRURLs(item, capturedURLs)
					}
					return nil
				}); err != nil {
					fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", itemID, err)
				}
			}
		}
	}
	return updates
}

// reconcileMergeState checks pr_open items to see if their PRs have been merged.
func reconcileMergeState(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	updates := 0
	for _, item := range s.List() {
		stage, _ := getNestedField(item, "delivery", "stage")
		if stage != "pr_open" {
			continue
		}

		branch, _ := getNestedField(item, "work_tracking", "branch")
		if branch == "" || branch == "null" {
			continue
		}

		prState, _ := opts.prFetch()(cfg, branch)
		if prState == "MERGED" {
			updates++
			fmt.Printf("  %s: pr_open → merged\n", item.ID)
			if !opts.DryRun {
				itemID := item.ID
				if err := s.Mutate(itemID, func(item *model.Item) error {
					item.SetNested("delivery", "stage", "merged")
					return nil
				}); err != nil {
					fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", itemID, err)
				}
			}
		}
	}
	return updates
}

// reconcileArchive moves completed/resolved items from tasks/issues to archive.
func reconcileArchive(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	updates := 0
	for _, item := range s.List() {
		tc, ok := cfg.Types[item.Type]
		if !ok {
			continue
		}

		// Check if item is in a terminal status
		isTerminal := false
		for _, ts := range tc.TerminalStatuses {
			if item.Status == ts {
				isTerminal = true
				break
			}
		}
		if !isTerminal {
			continue
		}

		// Check if it's already in archive directory
		path, ok := s.Path(item.ID)
		if !ok {
			continue
		}
		if strings.Contains(path, "/archive/") {
			continue
		}

		updates++
		fmt.Printf("  %s: move to archive (%s)\n", item.ID, item.Status)
		if !opts.DryRun {
			// Backfill completed timestamp if missing
			if item.Completed == nil {
				itemID := item.ID
				nowStr := time.Now().Format(time.RFC3339)
				if err := s.Mutate(itemID, func(item *model.Item) error {
					item.Doc.SetField("completed", nowStr)
					return nil
				}); err != nil {
					fmt.Fprintf(os.Stderr, "  error backfilling completed on %s: %v\n", itemID, err)
				}
			}
			if err := s.Move(item.ID); err != nil {
				fmt.Fprintf(os.Stderr, "  error moving %s: %v\n", item.ID, err)
			}
		}
	}
	return updates
}

// reconcileDeployState checks if merged items have been deployed via the orchestrator.
// Queries AWS S3 for dedupe records containing deployment status.
func reconcileDeployState(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	updates := 0
	orchBucket := os.Getenv("ORCH_BUCKET")
	if orchBucket == "" {
		orchBucket = "theraprac-orchestrator-queue"
	}

	for _, item := range s.List() {
		stage, _ := getNestedField(item, "delivery", "stage")
		if stage != "merged" {
			continue
		}

		branch, _ := getNestedField(item, "work_tracking", "branch")
		if branch == "" || branch == "null" {
			continue
		}

		// Get PR URLs to find merge commits
		allDeployed := true
		checkedAny := false

		prURLs := getPRURLsFromItem(item)
		for _, prURL := range prURLs {
			owner, repo, prNum := parsePRURL(prURL)
			if owner == "" || prNum == "" {
				continue
			}

			// Get merge commit SHA
			sha := getMergeCommitSHA(owner+"/"+repo, prNum)
			if sha == "" {
				allDeployed = false
				continue
			}

			// Check orchestrator S3 for deployment record
			repoSafe := strings.ReplaceAll(owner+"/"+repo, "/", "__")
			deployed := checkOrchDeployment(orchBucket, repoSafe, sha)
			checkedAny = true
			if !deployed {
				allDeployed = false
			}
		}

		if checkedAny && allDeployed {
			updates++
			fmt.Printf("  %s: merged → deployed_dev (all PRs deployed)\n", item.ID)
			if !opts.DryRun {
				itemID := item.ID
				deployedDate := time.Now().Format("2006-01-02")
				if err := s.Mutate(itemID, func(item *model.Item) error {
					item.SetNested("delivery", "stage", "deployed_dev")
					item.SetNested("delivery", "deployed_date", deployedDate)
					return nil
				}); err != nil {
					fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", itemID, err)
				}
			}
		}
	}
	return updates
}

// getPRURLsFromItem extracts PR URLs from the work_tracking.pr field.
func getPRURLsFromItem(item *model.Item) []string {
	if item.Doc == nil {
		return nil
	}
	var urls []string
	inPR := false
	inWT := false
	for _, line := range item.Doc.Lines {
		if line.Key == "work_tracking" && line.Indent == 0 {
			inWT = true
			continue
		}
		if inWT && line.Indent == 0 && !line.IsEmpty {
			break
		}
		if inWT && line.Key == "pr" {
			inPR = true
			continue
		}
		if inPR && line.IsList && line.Indent > 0 {
			val := strings.TrimSpace(line.Raw)
			val = strings.TrimPrefix(val, "- ")
			val = strings.Trim(val, `"'`)
			if strings.HasPrefix(val, "http") {
				urls = append(urls, val)
			}
		}
		if inPR && line.Indent == 0 {
			break
		}
	}
	return urls
}

// parsePRURL extracts owner, repo, and PR number from a GitHub PR URL.
func parsePRURL(url string) (owner, repo, prNum string) {
	// https://github.com/TheraPrac/theraprac-api/pull/123
	parts := strings.Split(url, "/")
	for i, p := range parts {
		if p == "pull" && i >= 2 && i+1 < len(parts) {
			owner = parts[i-2]
			repo = parts[i-1]
			prNum = parts[i+1]
			return
		}
	}
	return "", "", ""
}

// getMergeCommitSHA gets the merge commit SHA for a PR via gh.
func getMergeCommitSHA(fullRepo, prNum string) string {
	cmd := exec.Command("gh", "pr", "view", prNum,
		"--repo", fullRepo,
		"--json", "mergeCommit",
		"--jq", ".mergeCommit.oid",
	)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// checkOrchDeployment checks if a merge commit was deployed via the orchestrator S3 bucket.
func checkOrchDeployment(bucket, repoSafe, sha string) bool {
	dedupeKey := fmt.Sprintf("s3://%s/dedupe/%s/dev/%s.json", bucket, repoSafe, sha)
	cmd := exec.Command("aws", "s3", "cp", dedupeKey, "-")
	out, err := cmd.Output()
	if err != nil {
		return false // no dedupe record = not deployed yet
	}
	d, ok := parseDedupeJSON(out)
	if !ok {
		return false
	}
	return isDedupeDeployed(d, bucket, s3Exists)
}

// dedupeRecord is the JSON structure from the orchestrator S3 dedupe bucket.
type dedupeRecord struct {
	Status string `json:"status"`
	JobID  string `json:"job_id"`
}

// parseDedupeJSON parses the dedupe record. Returns the record and true if valid.
func parseDedupeJSON(data []byte) (dedupeRecord, bool) {
	var d dedupeRecord
	if err := json.Unmarshal(data, &d); err != nil {
		return d, false
	}
	return d, true
}

// isDedupeDeployed checks if a dedupe record indicates successful deployment.
// For "queued"/"processing" status, it cross-checks the completed S3 path.
func isDedupeDeployed(d dedupeRecord, bucket string, s3Check func(string) bool) bool {
	switch d.Status {
	case "success":
		return true
	case "queued", "processing":
		if d.JobID != "" {
			completedKey := fmt.Sprintf("s3://%s/completed/%s.json", bucket, d.JobID)
			if s3Check(completedKey) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// s3Exists checks if an S3 key exists via aws CLI.
func s3Exists(key string) bool {
	cmd := exec.Command("aws", "s3", "ls", key)
	return cmd.Run() == nil
}

// --- External tool helpers ---

func toolAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func branchExistsOnRemote(cfg *config.Config, branch string) bool {
	// Check all configured repo directories
	root := cfg.Root()
	if cfg.Worktree != nil && cfg.Worktree.Enabled {
		root = cfg.Worktree.ParentDir
		if root == "" {
			root = cfg.Root()
		}
	}

	repos := []string{"."}
	if cfg.Worktree != nil && len(cfg.Worktree.Repos) > 0 {
		repos = nil
		for _, shortName := range cfg.Worktree.Repos {
			if cfg.Worktree.RepoMap != nil {
				if mapped, ok := cfg.Worktree.RepoMap[shortName]; ok {
					repos = append(repos, mapped)
					continue
				}
			}
			repos = append(repos, shortName)
		}
	}

	for _, repo := range repos {
		repoDir := root
		if repo != "." {
			repoDir = root + "/" + repo
		}
		if _, err := os.Stat(repoDir); err != nil {
			continue
		}
		cmd := exec.Command("git", "ls-remote", "--heads", "origin", branch)
		cmd.Dir = repoDir
		out, err := cmd.Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			return true
		}
	}
	return false
}

type ghPR struct {
	URL      string `json:"url"`
	State    string `json:"state"`
	MergedAt string `json:"mergedAt"`
}

func getPRState(cfg *config.Config, branch string) (string, []string) {
	repos := repoFullNames(cfg)
	var allPRs []ghPR

	for _, repo := range repos {
		cmd := exec.Command("gh", "pr", "list",
			"--head", branch,
			"--repo", repo,
			"--state", "all",
			"--json", "url,state,mergedAt",
		)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		var prs []ghPR
		if err := json.Unmarshal(out, &prs); err != nil {
			continue
		}
		allPRs = append(allPRs, prs...)
	}

	if len(allPRs) == 0 {
		return "", nil
	}

	// Determine aggregate state
	var urls []string
	allMerged := true
	anyOpen := false
	for _, pr := range allPRs {
		urls = append(urls, pr.URL)
		if pr.State == "OPEN" {
			anyOpen = true
			allMerged = false
		} else if pr.State != "MERGED" {
			allMerged = false
		}
	}

	if allMerged && len(allPRs) > 0 {
		return "MERGED", urls
	}
	if anyOpen {
		return "OPEN", urls
	}
	// All PRs closed without merging — report as CLOSED so callers can warn
	if len(allPRs) > 0 {
		return "CLOSED", urls
	}
	return "", urls
}

func repoFullNames(cfg *config.Config) []string {
	// Try to detect GitHub org/repo from git remote
	root := cfg.Root()
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	// Parse org from remote URL (handles both HTTPS and SSH)
	remote := strings.TrimSpace(string(out))
	org := extractGitOrg(remote)
	if org == "" {
		return nil
	}

	if cfg.Worktree != nil && len(cfg.Worktree.Repos) > 0 {
		var names []string
		for _, shortName := range cfg.Worktree.Repos {
			repoName := shortName
			if cfg.Worktree.RepoMap != nil {
				if mapped, ok := cfg.Worktree.RepoMap[shortName]; ok {
					repoName = mapped
				}
			}
			names = append(names, org+"/"+repoName)
		}
		return names
	}

	return nil
}

func extractGitOrg(remote string) string {
	// SSH: git@github.com:org/repo.git
	if strings.Contains(remote, ":") && strings.Contains(remote, "git@") {
		parts := strings.SplitN(remote, ":", 2)
		if len(parts) == 2 {
			pathParts := strings.Split(parts[1], "/")
			if len(pathParts) >= 1 {
				return pathParts[0]
			}
		}
	}
	// HTTPS: https://github.com/org/repo.git
	if strings.Contains(remote, "github.com/") {
		idx := strings.Index(remote, "github.com/") + len("github.com/")
		rest := remote[idx:]
		parts := strings.Split(rest, "/")
		if len(parts) >= 1 {
			return parts[0]
		}
	}
	return ""
}

func storePRURLs(item *model.Item, urls []string) {
	// Update the work_tracking.pr field in the document
	if item.Doc == nil {
		return
	}

	// Find the pr: line within work_tracking and replace/add
	parentIdx := -1
	for i, line := range item.Doc.Lines {
		if line.Key == "work_tracking" && line.Indent == 0 {
			parentIdx = i
			break
		}
	}
	if parentIdx < 0 {
		return
	}

	// Find pr: line
	for i := parentIdx + 1; i < len(item.Doc.Lines); i++ {
		line := item.Doc.Lines[i]
		if line.Indent == 0 && !line.IsEmpty {
			break
		}
		if line.Key == "pr" && line.Indent > 0 {
			// Replace the pr line and any subsequent list items
			end := i + 1
			for end < len(item.Doc.Lines) {
				l := item.Doc.Lines[end]
				// Stop at next top-level key or sibling nested key
				if l.Indent == 0 && !l.IsEmpty {
					break
				}
				if l.Key != "" && l.Key != "pr" && l.Indent > 0 && l.BlockKey == "work_tracking" {
					break // next sibling field within work_tracking
				}
				// Include list items, blank lines, and continuation of pr block
				end++
			}

			// Build new PR list lines
			var newLines []model.Line
			if len(urls) == 0 {
				newLines = append(newLines, model.Line{Raw: "  pr: []", Key: "pr", Indent: 2, BlockKey: "work_tracking"})
			} else {
				newLines = append(newLines, model.Line{Raw: "  pr:", Key: "pr", Indent: 2, BlockKey: "work_tracking"})
				for _, url := range urls {
					newLines = append(newLines, model.Line{Raw: "  - " + url, IsList: true, Indent: 2, BlockKey: "work_tracking"})
				}
			}

			// Replace the slice
			item.Doc.Lines = append(item.Doc.Lines[:i], append(newLines, item.Doc.Lines[end:]...)...)
			return
		}
	}
}

func touchItem(item *model.Item, cfg *config.Config) {
	item.Doc.SetField("last_touched", formatNow())
	if agentID := cfg.AgentID(); agentID != "" {
		item.Doc.SetField("last_touched_by", agentID)
	}
}

func formatNow() string {
	return time.Now().Format(time.RFC3339)
}
