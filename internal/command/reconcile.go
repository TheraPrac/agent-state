package command

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"bufio"

	"github.com/theraprac/agent-state/internal/agent"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/deps"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/registry"
	"github.com/theraprac/agent-state/internal/sprintinherit"
	"github.com/theraprac/agent-state/internal/store"
)

// ReconcileOpts configures the reconcile command.
type ReconcileOpts struct {
	DryRun bool

	// Injectable dependencies (nil = use defaults). Struct-based for thread safety.
	ToolCheck       func(string) bool
	BranchCheck     func(*config.Config, string) bool
	PRFetch         func(*config.Config, string) (string, []string)
	S3Check         func(string) bool
	WorktreeUnsaved func(*config.Config, string) bool
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

func (o *ReconcileOpts) worktreeUnsaved() func(*config.Config, string) bool {
	if o.WorktreeUnsaved != nil {
		return o.WorktreeUnsaved
	}
	return worktreeHasUnsavedWork
}

// Reconcile syncs delivery stages with GitHub state and performs housekeeping.
// Phases:
// 0: active-stage drift sweep — advance active items with open/merged PRs (cross-session catch-up)
// 1: coding/committed → pushed (if branch exists on remote)
// 2: pushed → pr_open/merged (via gh pr list)
// 3: pr_open → merged (via gh pr view)
// 4: completed items → archive/ (move files)
// 5: regenerate index.md
// 11: orphan worktree cleanup — prune worktree dirs for terminal items
func Reconcile(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	var updates int
	var movedPaths []string

	// Check tool availability
	hasGH := opts.toolCheck()("gh")

	if !hasGH {
		fmt.Println("  (gh not available — skipping GitHub phases)")
	}

	// Phase 0: active-stage drift — sweep active items for open/merged PRs regardless of recorded stage.
	// Catches items whose branch+PR were created in a parallel session that pushed after this session's
	// initial git pull. Pure remote gh call; no local repo dependency.
	if hasGH {
		fmt.Println("Phase 0: Active-stage drift (cross-session catch-up)")
		n := reconcileActiveStageDrift(s, cfg, opts)
		updates += n
	}

	// Phase 1: coding/committed → pushed
	fmt.Println("Phase 1: Check branch existence")
	n := reconcileBranchPush(s, cfg, opts)
	updates += n

	// Phase 2: pushed → pr_open/merged
	if hasGH {
		fmt.Println("Phase 2: Check PR state (pushed → pr_open/merged)")
		n = reconcilePRState(s, cfg, opts)
		updates += n
	}

	// Phase 3: pr_open → merged
	if hasGH {
		fmt.Println("Phase 3: Check merge state (pr_open → merged)")
		n = reconcileMergeState(s, cfg, opts)
		updates += n
	}

	// Phase 4: merged → deployed_dev (AWS orchestrator check)
	hasAWS := opts.toolCheck()("aws")
	if hasAWS {
		fmt.Println("Phase 4: Check deployment state (merged → deployed_dev)")
		n = reconcileDeployState(s, cfg, opts)
		updates += n
	} else {
		fmt.Println("Phase 4: (aws not available — skipping deployment check)")
	}

	// Phase 5: Move completed items to archive
	fmt.Println("Phase 5: Archive completed items")
	var archivedPaths []string
	n, archivedPaths = reconcileArchive(s, cfg, opts)
	updates += n
	movedPaths = append(movedPaths, archivedPaths...)

	// Phase 5b: Heal epic-status drift (I-1641) — reactivate any epic marked
	// archived/completed that still holds a non-archived sprint, so the epic
	// status can't silently lie after a sprint was added to an archived epic.
	fmt.Println("Phase 5b: Epic status heal")
	n = reconcileEpicStatus(s, cfg, opts)
	updates += n

	// Phase 6: Drop terminal items from the work queue
	fmt.Println("Phase 6: Queue cleanup (drop terminal items)")
	n = reconcileQueueCleanup(s, cfg, opts)
	updates += n

	// Phase 7: Drop terminal items from the agent stack
	fmt.Println("Phase 7: Stack cleanup (drop terminal items)")
	n = reconcileStackCleanup(s, cfg, opts)
	updates += n

	// Phase 8: Release stuck-active items with no worktree / open PR / recent touch
	fmt.Println("Phase 8: Release stale-active items")
	n = reconcileStaleActive(s, cfg, opts)
	updates += n

	// Phase 9: Sprint-inheritance drift (I-681) — informational only, no
	// mutation. Surfaces follow-ups worked off their in-progress sprint
	// (the I-676 → T-203 case) so whoever owns the item can `st sprint
	// add` it. Reconcile must not auto-move items here: membership change
	// edits the item, which may belong to a peer agent.
	fmt.Println("Phase 9: Sprint-inheritance drift (informational)")
	reconcileSprintDrift(s, cfg, opts)

	// Phase 10: Regenerate index
	fmt.Println("Phase 10: Regenerate index")
	if !opts.DryRun {
		Index(s, cfg)
	}

	// Phase 11: Prune orphan worktrees (terminal items with worktrees on disk)
	fmt.Println("Phase 11: Orphan worktree cleanup")
	n = reconcileOrphanWorktrees(s, cfg, opts)
	updates += n

	// Summary
	if opts.DryRun {
		fmt.Printf("\nreconcile dry run: %d updates detected\n", updates)
	} else {
		fmt.Printf("\nreconcile: %d updates applied\n", updates)
		// I-1718: pass every archived item's post-Move path explicitly — see
		// reconcileArchive's doc comment for why git add -u can't stage it alone.
		if err := autoSync(s, fmt.Sprintf("st reconcile: %d updates", updates), movedPaths...); err != nil {
			return 1
		}
	}

	return 0
}

// reconcileSprintDrift prints (does not fix) every I-681 sprint-inheritance
// drift: a non-terminal item that blocks an active-sprint member but has no
// sprint of its own. Read-only by design — fixing it edits the item, which
// may be owned by another agent; the owner resolves it via `st sprint add`
// (or `st start --add-to-sprint`).
// opts is accepted for signature parity with every other reconcile phase
// helper (so a future mutating variant needs no signature change); this
// phase is read-only so DryRun has no effect — drift output is identical
// in dry-run and live, which is correct for a purely informational phase.
func reconcileSprintDrift(s *store.Store, cfg *config.Config, opts ReconcileOpts) {
	_ = opts
	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		return
	}
	g := deps.Build(s.All(), cfg)
	for _, e := range sprintinherit.Drift(s.All(), g, reg, cfg) {
		fmt.Printf("  warning: %s\n", e)
	}
}

// reconcileEpicStatus heals epic-status drift (I-1641): any epic marked
// archived/completed that still holds at least one non-archived sprint is
// reactivated to "active". Returns the number of epics healed. On registry
// load failure it returns 0 (no-op), matching reconcileSprintDrift's
// silent-skip; the registry is saved only when a heal occurred and not in
// DryRun mode. The store arg `s` is unused but kept for signature parity with
// every other reconcile phase helper.
func reconcileEpicStatus(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	_ = s
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		// Surface the skip — a silent no-op would let epic-status drift persist
		// across reconcile runs with no operator signal (I-1641 review).
		fmt.Fprintf(os.Stderr, "  warning: epic-status heal skipped — cannot load registry: %v\n", err)
		return 0
	}
	healed := r.ReconcileEpicStatuses()
	if len(healed) == 0 {
		return 0
	}
	for _, id := range healed {
		fmt.Printf("  %s: reactivated (has active sprint)\n", id)
	}
	if opts.DryRun {
		return len(healed)
	}
	// Persist BEFORE counting the heal as applied: if Save fails the on-disk
	// status is unchanged, so reporting it as a healed update would itself be a
	// lie — the exact failure mode this phase exists to prevent (I-1641 review).
	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "  error: epic-status heal computed %d fix(es) but registry Save failed — NOT applied: %v\n", len(healed), err)
		return 0
	}
	return len(healed)
}

// reconcileActiveStageDrift sweeps all active items with a recorded branch and
// advances stage directly to pr_open or merged based on live GitHub PR state.
// Unlike reconcileBranchPush, this makes no local git call — it catches items
// whose branch+PR were opened in a parallel session that synced to remote after
// this session's last git pull (the I-530 / I-876 failure mode).
// Only callable when gh is available. Uses forward-only advanceDeliveryStage so
// it is safe to run on items already at pr_open or beyond — no regression.
func reconcileActiveStageDrift(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	updates := 0
	for _, item := range s.List() {
		tc, ok := cfg.Types[item.Type]
		if !ok {
			continue
		}
		if item.Status != tc.ActiveStatus {
			continue
		}

		branch, _ := getNestedField(item, "work_tracking", "branch")
		if branch == "" || branch == "null" {
			continue
		}

		// Skip items already at or past the furthest stage Phase 0 can reach
		// (merged=4). No prFetch call can produce an advancement for them.
		current, _ := getNestedField(item, "delivery", "stage")
		if stageIndex(current) >= stageIndex("merged") {
			continue
		}

		prState, prURLs := opts.prFetch()(cfg, branch)

		var target string
		switch prState {
		case "OPEN":
			target = "pr_open"
		case "MERGED":
			target = "merged"
		default:
			continue
		}

		if stageIndex(target) <= stageIndex(current) {
			continue
		}

		updates++
		fmt.Printf("  %s: %s → %s (PR %s detected on branch %s)\n", item.ID, current, target, prState, branch)
		if !opts.DryRun {
			itemID := item.ID
			capturedTarget := target
			capturedURLs := prURLs
			if err := s.Mutate(itemID, func(item *model.Item) error {
				advanceDeliveryStage(item, capturedTarget)
				if len(capturedURLs) > 0 {
					storePRURLs(item, capturedURLs)
				}
				return nil
			}); err != nil {
				fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", itemID, err)
			}
		}
	}
	return updates
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
// reconcileArchive returns the update count and the post-Move paths of every
// item it archived. I-1718: s.Move renames the file across status
// directories, which git sees as delete-old + untracked-new — GitSync's
// `git add -u` only stages the deletion, never the new path (I-1715/I-442).
// Callers must pass the returned paths explicitly into autoSync.
func reconcileArchive(s *store.Store, cfg *config.Config, opts ReconcileOpts) (int, []string) {
	updates := 0
	var movedPaths []string
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
			} else if newPath, ok := s.Path(item.ID); ok {
				movedPaths = append(movedPaths, newPath)
			}
		}
	}
	return updates, movedPaths
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
	// Check all configured repo directories.
	// I-778: RepoParent() resolves per-agent repo parent via
	// .as/agent-workspace.yaml and also fixes the missing
	// filepath.IsAbs(ParentDir) handling here.
	root := cfg.Root()
	if cfg.Worktree != nil && cfg.Worktree.Enabled {
		root = cfg.RepoParent()
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

// reconcileQueueCleanup drops queue entries pointing at terminal items
// (closed, abandoned, etc.) so `st queue show` doesn't carry forward
// resolved work between sessions. I-232.
func reconcileQueueCleanup(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	entries := LoadQueue(cfg)
	if len(entries) == 0 {
		return 0
	}
	var kept []QueueEntry
	updates := 0
	for _, e := range entries {
		item, ok := s.Get(e.ID)
		if !ok {
			kept = append(kept, e)
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			updates++
			fmt.Printf("  %s: dropped from queue (status: %s)\n", e.ID, item.Status)
			continue
		}
		kept = append(kept, e)
	}
	if updates == 0 {
		return 0
	}
	if !opts.DryRun {
		if err := SaveQueue(cfg, kept); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: save queue: %v\n", err)
		}
	}
	return updates
}

// reconcileStackCleanup drops stack entries pointing at terminal items.
// LoadStack already resolves the legacy-vs-per-agent path, so this
// piggybacks on that. I-232.
func reconcileStackCleanup(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	entries := LoadStack(cfg)
	if len(entries) == 0 {
		return 0
	}
	var kept []StackEntry
	updates := 0
	for _, e := range entries {
		item, ok := s.Get(e.ID)
		if !ok {
			kept = append(kept, e)
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			updates++
			fmt.Printf("  %s: dropped from stack (status: %s)\n", e.ID, item.Status)
			continue
		}
		kept = append(kept, e)
	}
	if updates == 0 {
		return 0
	}
	if !opts.DryRun {
		if err := SaveStack(cfg, kept); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: save stack: %v\n", err)
		}
	}
	return updates
}

// reconcileStaleActive releases items whose status is the type's
// ActiveStatus AND:
//   - no changelog activity for the item in the last N hours (default 6h,
//     configurable via sprints.stale_active_hours / ST_STALE_ACTIVE_HOURS /
//     ST_STALE_ACTIVE_DAYS). Falls back to last_touched when no changelog. I-874.
//   - no worktree exists at either the per-agent or legacy locations
//   - no open PR is associated with the recorded branch
//   - assigned agent (if set) has no live PID registration (safety gate). I-874.
//
// Calls Release() to reset the item back to its StartStatus. I-232.
func reconcileStaleActive(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	threshold := time.Duration(cfg.StaleActiveHours()) * time.Hour
	now := time.Now()

	// Build live-agent set once for the whole pass. Errors are non-fatal:
	// if we can't list registrations we skip the liveness gate (safe-fail
	// keeps active; no false positives).
	liveAgents := map[string]bool{}
	if regs, err := agent.ListRegistrations(cfg); err == nil {
		for _, r := range regs {
			if agent.IsPIDLive(r.PID) {
				liveAgents[r.AgentID] = true
			}
		}
	}

	updates := 0
	for _, item := range s.List() {
		tc, ok := cfg.Types[item.Type]
		if !ok {
			continue
		}
		// I-1439: a goal's "active" is a lifecycle state (managed by
		// `st goal activate/drop/mark-met`), NOT a work-item active claim.
		// Goals never have a worktree or open PR, so without this skip the
		// stale-active sweep released every active goal back to draft on
		// each session-start reconcile — silently reverting the operator's
		// goal activations.
		if item.Type == "goal" {
			continue
		}
		if item.Status != tc.ActiveStatus {
			continue
		}
		// I-874 safety gate: if the assigned agent has a live process, it may
		// be deep in a compile or test cycle. Don't yank from under it.
		if item.AssignedTo != "" && liveAgents[item.AssignedTo] {
			continue
		}
		// Use changelog latest-entry timestamp as the activity signal; fall back
		// to last_touched when no changelog exists for this item.
		activityAt, ok := changelogLatestTimestamp(cfg, item.ID)
		if !ok {
			activityAt = item.LastTouched
		}
		if !activityAt.IsZero() && now.Sub(activityAt) < threshold {
			continue
		}
		// Worktree present — owner has on-disk state. The owning session is NOT live
		// (live owners were skipped above), so a CLEAN worktree is just a husk from a
		// dead session and must not pin the item active forever (I-1485 — the I-1470
		// case: active/assigned, no live session, work never finished, looked owned).
		// Keep the item active ONLY if a worktree holds unsaved work (uncommitted changes
		// or unpushed commits); surface that loudly for operator review rather than
		// resetting it. A clean (or absent) worktree falls through to Release below.
		dirtyWT := ""
		for _, wt := range []string{filepath.Join(cfg.WorktreeBase(), item.ID), filepath.Join(cfg.WorktreeBaseLegacy(), item.ID)} {
			if wt == "" {
				continue
			}
			if _, err := os.Stat(wt); err != nil {
				continue
			}
			if opts.worktreeUnsaved()(cfg, wt) {
				dirtyWT = wt
				break
			}
		}
		if dirtyWT != "" {
			fmt.Printf("  %s: orphaned active (owner %q not live) with UNSAVED work in %s — keeping active for operator review (I-1485)\n",
				item.ID, item.AssignedTo, dirtyWT)
			continue
		}
		// Open PR — owner is mid-review.
		if branch, _ := getNestedField(item, "work_tracking", "branch"); branch != "" && branch != "null" {
			prState, _ := opts.prFetch()(cfg, branch)
			if prState == "OPEN" || prState == "PR_OPEN" {
				continue
			}
		}
		staleAge := now.Sub(activityAt)
		updates++
		fmt.Printf("  %s: releasing stale-active (no changelog activity for %s)\n",
			item.ID, formatStaleAge(staleAge))
		if !opts.DryRun {
			Release(s, cfg, item.ID)
		}
	}
	return updates
}

// worktreeHasUnsavedWork reports whether any repo checkout under a worktree dir holds
// work a reset would strand: uncommitted changes (tracked, staged, or untracked) or
// unpushed commits. Conservative — any ambiguity (e.g. a branch with no upstream)
// counts as unsaved so genuine work is never silently discarded (mirrors finish.go).
func worktreeHasUnsavedWork(cfg *config.Config, wtDir string) bool {
	var repos []string
	if cfg.Worktree != nil {
		repos = cfg.Worktree.Repos
	}
	for _, repo := range repos {
		repoDir := filepath.Join(wtDir, repo)
		if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
			continue // no such repo checkout in this worktree
		}
		// Uncommitted (tracked/staged/untracked).
		if out, err := runGit(repoDir, "status", "--porcelain"); err == nil && strings.TrimSpace(out) != "" {
			return true
		}
		// Unpushed commits — or no upstream at all (err) → treat as unsaved.
		if out, err := runGit(repoDir, "log", "--oneline", "@{u}..HEAD"); err != nil || strings.TrimSpace(out) != "" {
			return true
		}
	}
	return false
}

// changelogLatestTimestamp returns the timestamp of the most recent changelog
// entry for the given item, reading only the last line of the JSONL log for
// efficiency. Returns (zero, false) when the file doesn't exist or the last
// line is unparseable. I-874.
func changelogLatestTimestamp(cfg *config.Config, id string) (time.Time, bool) {
	path := filepath.Join(cfg.ChangelogDir(), id+".log")
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()

	// Scan all lines to find the last non-empty one.
	var lastLine string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lastLine = line
		}
	}
	if lastLine == "" {
		return time.Time{}, false
	}

	var entry struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(lastLine), &entry); err != nil || entry.Timestamp == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, entry.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// formatStaleAge renders a duration as the simplest human form for the
// reconcile log line ("12d" / "36h" / "240h0m0s" → "10d"). Doesn't try
// to be precise — operators are looking at orders of magnitude here.
func formatStaleAge(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days >= 1 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// reconcileOrphanWorktrees scans cfg.WorktreeBase() and cfg.WorktreeBaseLegacy()
// for subdirectories whose name matches an item ID that is already terminal.
// For each match, attempts TryAutoFinishWorktree. If the worktree is retained
// (uncommitted or unpushed work), prints a warning so the operator can handle
// it manually. Returns the number of worktrees cleaned.
func reconcileOrphanWorktrees(s *store.Store, cfg *config.Config, opts ReconcileOpts) int {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled {
		return 0
	}

	// Scan both the current worktree base and the legacy pre-I-407 location
	// so stranded worktrees from before the per-agent worktree move are caught.
	var baseDirs []string
	if base := cfg.WorktreeBase(); base != "" {
		baseDirs = append(baseDirs, base)
	}
	if legacy := cfg.WorktreeBaseLegacy(); legacy != "" && legacy != cfg.WorktreeBase() {
		baseDirs = append(baseDirs, legacy)
	}

	cleaned := 0
	for _, baseDir := range baseDirs {
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			fmt.Fprintf(os.Stderr, "reconcile: reading worktree base %s: %v\n", baseDir, err)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			id := entry.Name()
			item, ok := s.Get(id)
			if !ok {
				continue
			}
			if !cfg.IsTerminalStatus(item.Type, item.Status) {
				continue
			}
			if opts.DryRun {
				fmt.Printf("  [dry-run] would prune orphan worktree: %s (status: %s)\n", id, item.Status)
				cleaned++
				continue
			}
			if wtCleaned, retained := TryAutoFinishWorktree(cfg, id); wtCleaned {
				fmt.Printf("  pruned orphan worktree: %s (status: %s)\n", id, item.Status)
				cleaned++
			} else if retained {
				fmt.Printf("  warning: orphan worktree %s retained — uncommitted/unpushed work; run `st finish %s --force` to force-clean\n", id, id)
			}
		}
	}
	return cleaned
}
