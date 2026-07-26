package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// lookupRun is the per-subcommand entry point for routing-sensitive
// state access. The result carries OrgID / UserID / RunID + the
// IsEventTriggered discriminator. Errors surface via the same
// exitErr/os.Exit shape the rest of the file uses so the agent sees a
// clear message and the subcommand exits non-zero.
//
// In local mode this resolves identity from TRIAGE_FACTORY_CONVERSATION_ID at
// AutoDetect time; in sandbox mode the daemon's per-socket map
// determines identity and LookupRun just round-trips. Either way the
// subcommand body reads the routing-relevant fields from a single
// in-process value.
func lookupRun(host agenthost.Client) agenthost.RunInfo {
	info, err := host.LookupRun(context.Background())
	if err != nil {
		exitErr(err.Error())
	}
	return info
}

// agentMemoryFile returns the absolute path the delegated agent must write its
// run-memory file to, composed from the run-scoped env vars the spawner exports
// into the agent's environment (TRIAGE_FACTORY_CONVERSATION_ROOT,
// TRIAGE_FACTORY_BLUEPRINT_RUN_ID, and TRIAGE_FACTORY_CONVERSATION_ID — see
// internal/delegate/run.go). This mirrors the path the completion gate reads
// (cwd/_scratch/entity-memory/<blueprint_run_id>/<run_id>.md), so the "do not
// retry, finish by writing ..." messages below can point the agent at the file
// concretely.
//
// The bare env-var reference these messages used to carry would be written
// verbatim by the agent's Write tool — which does no shell expansion — so the
// file landed at a literal "$TRIAGE_FACTORY_CONVERSATION_ROOT/..." path the gate never
// found. Inside the sandbox TRIAGE_FACTORY_CONVERSATION_ROOT is already translated to
// /work, so reading it here yields a path the agent can actually reach. Falls
// back to the env-var-reference form if any piece is unset (subcommand invoked
// outside a delegated run) so the message still reads coherently.
func agentMemoryFile() string {
	root := os.Getenv("TRIAGE_FACTORY_CONVERSATION_ROOT")
	ns := os.Getenv("TRIAGE_FACTORY_BLUEPRINT_RUN_ID")
	runID := os.Getenv("TRIAGE_FACTORY_CONVERSATION_ID")
	if root == "" || ns == "" || runID == "" {
		return "$TRIAGE_FACTORY_CONVERSATION_ROOT/_scratch/entity-memory/$TRIAGE_FACTORY_BLUEPRINT_RUN_ID/$TRIAGE_FACTORY_CONVERSATION_ID.md"
	}
	return filepath.Join(root, "_scratch", "entity-memory", ns, runID+".md")
}

func handlePR(ctx context.Context, host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: triagefactory exec gh pr <action> [flags]")
	}

	action := args[0]
	flags := args[1:]

	// Every GitHub API call routes through the host. owner/repo are empty
	// here because the PR verbs all resolve their own and pass them
	// explicitly; the adapter only needs them for the raw actions transport.
	api := newHostAPI(host, "", "")

	switch action {
	case "create":
		prCreate(ctx, api, flags)
	case "view":
		prView(ctx, api, flags)
	case "diff":
		prDiff(ctx, host, flags)
	case "files":
		prFiles(ctx, api, flags)
	case "thread-view":
		prThreadView(ctx, api, host, flags)
	case "review-view":
		prReviewView(ctx, api, flags)
	case "review-dismiss":
		prReviewDismiss(ctx, api, flags)
	case "start-review":
		prStartReview(ctx, api, host, flags)
	case "add-review-comment":
		prAddReviewComment(ctx, host, flags)
	case "finalize-review":
		prFinalizeReview(ctx, host, flags)
	case "add-comment":
		prAddComment(ctx, api, flags)
	case "comment-reply":
		prCommentReply(ctx, api, flags)
	case "comment-react":
		prCommentReact(ctx, api, flags)
	case "comment-update":
		prCommentUpdate(ctx, api, host, flags)
	case "comment-delete":
		prCommentDelete(ctx, api, host, flags)
	default:
		exitErr(fmt.Sprintf("unknown pr action: %s", action))
	}
}

func prView(ctx context.Context, client ghAPI, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	verbose := hasFlag(args, "-v") || hasFlag(args, "--verbose")
	pr, err := client.GetPR(ctx, owner, repo, number, verbose)
	exitOnErr(err)
	printJSON(pr)
}

// diffManifest is the JSON envelope `pr diff` prints to stdout (and writes
// to manifest.json) instead of dumping the whole diff into the agent's
// context. Dir / FullDiffPath are absolute so the agent can Read/Grep the
// persisted diff directly without reasoning about cwd. Truncated flags the
// HTTP-406 fallback path where full.diff was reassembled from per-file
// patches rather than fetched verbatim.
//
// Source records which frame full.diff is in: "local_checkout" means it was
// produced from the run's worktree HEAD (the commit the agent is actually
// looking at, and the commit add-review-comment anchors to), "github_api"
// means the live-head diff from the API fallback. HeadSHA is the commit that
// frame is against; BaseSHA is the base it's diffed against — the PR's recorded
// base.sha on the local_checkout path (TFAC-505), so a reader can confirm the
// diff was framed against the true base and not a stale base ref. When the local
// checkout has fallen behind the live PR head, RemoteHeadSHA / BehindBy / Stale
// describe the gap and Warning tells the agent to `git pull` for the current code.
type diffManifest struct {
	Owner         string        `json:"owner"`
	Repo          string        `json:"repo"`
	Number        int           `json:"number"`
	HeadSHA       string        `json:"head_sha"`
	BaseRef       string        `json:"base_ref"`
	BaseSHA       string        `json:"base_sha,omitempty"`
	Source        string        `json:"source"`
	RemoteHeadSHA string        `json:"remote_head_sha,omitempty"`
	BehindBy      int           `json:"behind_by,omitempty"`
	Stale         bool          `json:"stale,omitempty"`
	Warning       string        `json:"warning,omitempty"`
	ChangedFiles  int           `json:"changed_files"`
	Additions     int           `json:"additions"`
	Deletions     int           `json:"deletions"`
	Dir           string        `json:"dir"`
	FullDiffPath  string        `json:"full_diff_path"`
	Truncated     bool          `json:"truncated"`
	Files         []fileSummary `json:"files"`
}

const (
	diffSourceLocal = "local_checkout"
	diffSourceAPI   = "github_api"
)

// fileSummary is the per-file overview shared by `pr diff`'s manifest and
// `pr files`: path, change status, line counts, a binary hint, and the
// pre-rename path. Deliberately omits the patch — diff content is served by
// `pr diff`'s full.diff, never inlined into these listings.
type fileSummary struct {
	Path             string `json:"path"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Binary           bool   `json:"binary"`
	PreviousFilename string `json:"previous_filename,omitempty"`
}

const (
	fullDiffFilename = "full.diff"
	manifestFilename = "manifest.json"
)

// prDiff persists the PR diff under _scratch/ and prints a manifest, rather
// than dumping the whole diff to stdout (which lands the entire thing in the
// delegated agent's context in one shot — unbounded and not navigable with
// Read/Grep/Glob).
//
// The diff is framed against the run's WORKTREE HEAD — the commit the agent's
// checkout is actually on, which is also the commit add-review-comment anchors
// comments to. Reading and commenting against the same frame is what keeps a
// line the agent saw mapped to the line GitHub anchors the comment to. If the
// live PR head has moved past the local checkout, the manifest carries a
// staleness warning pointing the agent at `git pull`; the diff still reflects
// the code the agent has in hand. Outside a worktree (no checkout to anchor
// against) it falls back to the live-head diff from the GitHub API.
//
// Two escape hatches keep the inline behavior available:
//   - --file <path>: targeted single-file diff stays inline (bounded). No
//     file written.
//   - --stdout: whole-diff-to-stdout, for scripts/pipelines. No file written.
//
// Both inline paths prefer the local-checkout diff and fall back to the API
// (including persistPRDiff's HTTP-406 "diff too large" reassembly from per-file
// patches) when there's no worktree.
func prDiff(ctx context.Context, host agenthost.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	// Repo-scoped adapter so the raw compare Get used for the staleness check
	// resolves the org's credential tier (the bare PR dispatch builds an
	// owner/repo-less client; the compare endpoint needs both).
	client := newHostAPI(host, owner, repo)
	file := flagVal(args, "--file")
	stdout := hasFlag(args, "--stdout")

	cwd, err := os.Getwd()
	if err != nil {
		exitErr(fmt.Sprintf("resolve cwd: %v", err))
	}
	checkout := resolveLocalCheckout(cwd)

	if file != "" || stdout {
		fmt.Print(inlineDiff(ctx, client, checkout, owner, repo, number, file))
		return
	}

	manifest, err := persistPRDiff(ctx, client, checkout, cwd, owner, repo, number)
	exitOnErr(err)
	printJSON(manifest)
}

// inlineDiff returns the diff text for the --file / --stdout escape hatches.
// Prefers the worktree-HEAD diff (same frame as the persisted manifest and the
// comment anchors); falls back to the live-head API diff — with the HTTP-406
// per-file reassembly — when there's no local checkout.
func inlineDiff(ctx context.Context, client ghAPI, checkout localCheckout, owner, repo string, number int, file string) string {
	if checkout.ok {
		if pr, err := client.GetPR(ctx, owner, repo, number, false); err == nil {
			if base, ok := resolveDiffBase(checkout.cwd, pr.BaseSHA, pr.BaseRef); ok {
				if diff, err := localUnifiedDiff(checkout.cwd, base, file); err == nil {
					if file != "" && strings.TrimSpace(diff) == "" {
						exitErr(fmt.Sprintf("file %q is not part of PR #%d's diff", file, number))
					}
					return diff
				}
			}
		}
	}

	diff, err := client.GetPRDiff(ctx, owner, repo, number, file)
	if err != nil {
		if !ghclient.IsHTTP406(err) {
			exitOnErr(err)
		}
		files, ferr := client.GetPRFiles(ctx, owner, repo, number)
		exitOnErr(ferr)
		if file != "" {
			diff = ghclient.SingleFileDiff(files, file)
			if diff == "" {
				exitErr(fmt.Sprintf("file %q is not part of PR #%d's diff", file, number))
			}
		} else {
			diff = ghclient.ReassemblePRDiff(files)
		}
	}
	return diff
}

// persistPRDiff writes full.diff and manifest.json into a per-PR directory
// under _scratch/ and returns the manifest. The directory is keyed by (owner,
// repo, number) — the task's identity, which the agent always knows — so a
// later blueprint step can locate an earlier capture without first looking up
// the head SHA. Each `pr diff` overwrites the capture in place.
//
// When the run has a local checkout, full.diff is the worktree-HEAD diff and
// manifest.head_sha is that worktree HEAD — the same frame add-review-comment
// anchors against. The live PR head + a behind-by count are recorded alongside
// so a reader can detect (and the agent can act on) the checkout having fallen
// behind. Outside a worktree it falls back to the live-head API diff.
func persistPRDiff(ctx context.Context, client ghAPI, checkout localCheckout, cwd, owner, repo string, number int) (diffManifest, error) {
	pr, err := client.GetPR(ctx, owner, repo, number, false)
	if err != nil {
		return diffManifest{}, fmt.Errorf("fetch PR #%d: %w", number, err)
	}

	manifest := diffManifest{
		Owner:         owner,
		Repo:          repo,
		Number:        number,
		BaseRef:       pr.BaseRef,
		RemoteHeadSHA: pr.HeadSHA,
	}

	var fullDiff string
	if local, ok := buildLocalDiffManifest(ctx, client, checkout, owner, repo, &manifest, pr); ok {
		fullDiff = local
	} else {
		fullDiff, err = buildAPIDiffManifest(ctx, client, owner, repo, number, &manifest, pr)
		if err != nil {
			return diffManifest{}, err
		}
	}

	// Key on the task identity (owner, repo, number) the agent always knows,
	// so it can locate an earlier capture without first resolving the head SHA.
	dirKey := owner + "__" + repo + "__" + strconv.Itoa(number)
	destDir, err := safeScratchSubdir(cwd, "_scratch", "pr-diffs", dirKey)
	if err != nil {
		return diffManifest{}, err
	}

	// Overwrite any prior capture for this PR so a re-diff is deterministic —
	// stale files from an older fetch (a renamed-away file, a now-current
	// hunk) would otherwise sit alongside the fresh ones and mislead a reader.
	if err := os.RemoveAll(destDir); err != nil {
		return diffManifest{}, fmt.Errorf("clear stale diff directory: %w", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return diffManifest{}, fmt.Errorf("create diff directory: %w", err)
	}

	fullDiffPath := filepath.Join(destDir, fullDiffFilename)
	if err := os.WriteFile(fullDiffPath, []byte(fullDiff), 0o644); err != nil {
		return diffManifest{}, fmt.Errorf("write %s: %w", fullDiffFilename, err)
	}
	manifest.Dir = destDir
	manifest.FullDiffPath = fullDiffPath

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return diffManifest{}, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, manifestFilename), manifestBytes, 0o644); err != nil {
		return diffManifest{}, fmt.Errorf("write %s: %w", manifestFilename, err)
	}

	return manifest, nil
}

// buildLocalDiffManifest fills the manifest from the run's worktree-HEAD diff
// and returns the full diff text + ok=true. Returns ok=false (caller falls back
// to the API) when there's no checkout or the base ref can't be resolved
// locally. Populates head_sha = worktree HEAD, the per-file rows from the diff
// itself, and the staleness fields when the live head has moved ahead.
func buildLocalDiffManifest(ctx context.Context, client ghAPI, checkout localCheckout, owner, repo string, manifest *diffManifest, pr *ghclient.PRView) (string, bool) {
	if !checkout.ok {
		return "", false
	}
	base, ok := resolveDiffBase(checkout.cwd, pr.BaseSHA, pr.BaseRef)
	if !ok {
		return "", false
	}
	diff, err := localUnifiedDiff(checkout.cwd, base, "")
	if err != nil {
		return "", false
	}

	manifest.Source = diffSourceLocal
	manifest.HeadSHA = checkout.headSHA
	manifest.BaseSHA = base
	manifest.Files = parseDiffSummaries(diff)
	manifest.ChangedFiles = len(manifest.Files)
	for _, f := range manifest.Files {
		manifest.Additions += f.Additions
		manifest.Deletions += f.Deletions
	}

	if behind := remoteAheadBy(ctx, client, owner, repo, checkout.headSHA, pr.HeadSHA); behind > 0 {
		manifest.Stale = true
		manifest.BehindBy = behind
		manifest.Warning = fmt.Sprintf(
			"This diff reflects your local checkout (HEAD %s), which is %d commit(s) behind the live PR head (%s). "+
				"New commits have landed on the PR branch that you do not have. Run `git pull` to update your checkout "+
				"before reviewing or commenting if you want the current code — review comments anchor to the commit you have checked out.",
			shortSHA(checkout.headSHA), behind, shortSHA(pr.HeadSHA),
		)
	} else if pr.HeadSHA != "" && checkout.headSHA != pr.HeadSHA {
		// SHAs differ but compare reported no commits ahead (force-push that
		// rewrote rather than appended, or a compare that couldn't be fetched).
		// Flag the divergence without a precise count.
		manifest.Stale = true
		manifest.Warning = fmt.Sprintf(
			"Your local checkout (HEAD %s) differs from the live PR head (%s). The PR branch may have been "+
				"force-pushed. Run `git pull` to reconcile if you want the current code.",
			shortSHA(checkout.headSHA), shortSHA(pr.HeadSHA),
		)
	}
	return diff, true
}

// buildAPIDiffManifest fills the manifest from the live-head GitHub diff (the
// no-worktree fallback). head_sha is the live PR head; per-file rows come from
// GetPRFiles, which doubles as the HTTP-406 reassembly source.
func buildAPIDiffManifest(ctx context.Context, client ghAPI, owner, repo string, number int, manifest *diffManifest, pr *ghclient.PRView) (string, error) {
	files, err := client.GetPRFiles(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("list PR files: %w", err)
	}
	fullDiff, truncated, err := fetchFullDiff(ctx, client, owner, repo, number, files)
	if err != nil {
		return "", err
	}

	manifest.Source = diffSourceAPI
	manifest.HeadSHA = pr.HeadSHA
	manifest.Truncated = truncated
	manifest.ChangedFiles = pr.ChangedFiles
	manifest.Additions = pr.Additions
	manifest.Deletions = pr.Deletions
	manifest.Files = make([]fileSummary, 0, len(files))
	// Some hosts don't populate the PR-level counts; fall back to the file
	// list length so changed_files is never a misleading zero.
	if manifest.ChangedFiles == 0 {
		manifest.ChangedFiles = len(files)
	}
	for _, f := range files {
		manifest.Files = append(manifest.Files, fileSummary{
			Path:             f.Filename,
			Status:           f.Status,
			Additions:        f.Additions,
			Deletions:        f.Deletions,
			Binary:           ghclient.IsBinaryFile(f),
			PreviousFilename: f.PreviousFilename,
		})
	}
	return fullDiff, nil
}

// fetchFullDiff returns the PR's unified diff and whether it was reassembled
// from per-file patches. The happy path is the verbatim diff media type; on
// HTTP 406 ("diff too large") GitHub refuses it, so we reconstruct an
// approximate unified diff from the already-fetched per-file patches and flag
// the result truncated. Any other error propagates.
func fetchFullDiff(ctx context.Context, client ghAPI, owner, repo string, number int, files []ghclient.PRFile) (string, bool, error) {
	diff, err := client.GetPRDiff(ctx, owner, repo, number, "")
	if err == nil {
		return diff, false, nil
	}
	if !ghclient.IsHTTP406(err) {
		return "", false, fmt.Errorf("fetch PR diff: %w", err)
	}
	return ghclient.ReassemblePRDiff(files), true, nil
}

// shortSHA abbreviates a commit SHA to 7 chars for human/agent-facing messages,
// leaving anything already shorter untouched.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// prFilesResult is the slim overview `pr files` prints: one summary row per
// changed file, no patch content. PR-level sizing (changed_files / additions /
// deletions) is deliberately NOT here — those live on the PR object and are
// served accurately by `pr view`. Summing them from this list would silently
// undercount any PR past the GetPRFiles cap, exactly the huge PRs where sizing
// matters most. Truncated flags that the listing itself hit that cap, so the
// agent knows to fall back to `pr diff` for the files beyond it. The diff
// content is served by `pr diff`; `pr files` is the cheapest "what changed"
// look (one API call, no full-diff fetch, no disk write).
type prFilesResult struct {
	Truncated bool          `json:"truncated"`
	Files     []fileSummary `json:"files"`
}

// buildPRFilesResult assembles the slim envelope from the raw file list,
// dropping the patch. Truncated is set when the list reached the GetPRFiles
// cap (and so may be missing files); it's intentionally conservative — a PR
// with exactly MaxPRFiles complete files reads as truncated, which is the safe
// direction (worst case the agent double-checks with `pr diff`).
func buildPRFilesResult(files []ghclient.PRFile) prFilesResult {
	result := prFilesResult{
		Truncated: len(files) >= ghclient.MaxPRFiles,
		Files:     make([]fileSummary, 0, len(files)),
	}
	for _, f := range files {
		result.Files = append(result.Files, fileSummary{
			Path:             f.Filename,
			Status:           f.Status,
			Additions:        f.Additions,
			Deletions:        f.Deletions,
			Binary:           ghclient.IsBinaryFile(f),
			PreviousFilename: f.PreviousFilename,
		})
	}
	return result
}

func prFiles(ctx context.Context, client ghAPI, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	files, err := client.GetPRFiles(ctx, owner, repo, number)
	exitOnErr(err)
	printJSON(buildPRFilesResult(files))
}

func prThreadView(ctx context.Context, client ghAPI, host agenthost.Client, args []string) {
	if len(args) < 2 {
		exitErr("usage: gh pr thread-view <pr_number> <comment_id> [--page N]")
	}
	// Resolve owner/repo from the FULL args so an explicit --repo is honored
	// (resolveRepo scans for it); firstPositional already skips flags, so the
	// number still binds to the leading <pr_number> positional. Slicing to
	// args[:1] here would hide --repo and silently resolve the wrong repo.
	owner, repo, number := parseRepoAndNumber(args)
	commentID := mustInt(args[1], "comment_id")
	page := 1
	if v := flagVal(args, "--page"); v != "" {
		page = mustInt(v, "page")
	}
	thread, err := client.GetCommentThread(ctx, owner, repo, commentID, page)
	exitOnErr(err)
	// The PR is the addressed entity; the comment thread is an attribute. The
	// ghAPI seam (a mirror of *github.Client, which fetches a thread by comment
	// id) can't carry the PR number, so the touch is recorded here from the CLI
	// positional the agent supplied, after the read succeeds. Best-effort.
	host.RecordReadTouch(ctx, domain.ArtifactProviderGitHub, domain.PullRequestTarget(owner+"/"+repo, number), "")
	printJSON(thread)
}

func prReviewView(ctx context.Context, client ghAPI, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr review-view <review_id> --pr <pr_number> [-v]")
	}
	reviewID := mustInt(args[0], "review_id")
	// Explicit --pr-missing message: the most common mistake here is
	// the agent extrapolating the `pr view <pr_number>` shape and
	// writing `pr review-view <pr_number>` (omitting --pr). The
	// generic "pr_number is required" mustInt error doesn't point at
	// the asymmetry; spelling it out gets the agent to the corrected
	// shape on the next attempt.
	prFlag := flagVal(args, "--pr")
	if prFlag == "" {
		exitErr(fmt.Sprintf(
			"review-view requires --pr <pr_number>. The positional argument %d is the review_id, not the PR number — they are different ids (review ids come from `gh pr view <pr_number> -v` -> reviews[].id). Canonical shape: gh pr review-view %d --pr <pr_number> [-v]",
			reviewID, reviewID,
		))
	}
	owner, repo := ownerRepo(args)
	prNumber := mustInt(prFlag, "pr_number")
	verbose := hasFlag(args, "-v") || hasFlag(args, "--verbose")
	detail, err := client.GetReviewDetail(ctx, owner, repo, prNumber, reviewID, verbose)
	exitOnErr(err)
	printJSON(detail)
}

func prReviewDismiss(ctx context.Context, client ghAPI, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr review-dismiss <review_id> --pr <number> --body <reason>")
	}
	reviewID := mustInt(args[0], "review_id")
	owner, repo := ownerRepo(args)
	prNumber := mustInt(flagVal(args, "--pr"), "pr_number")
	body := flagVal(args, "--body")
	if body == "" {
		body = "Dismissed"
	}
	err := client.DismissReview(ctx, owner, repo, prNumber, reviewID, body)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "review_id": reviewID, "status": "dismissed"})
}

// --- Review lifecycle (local staging) ---

// prStartReview records a fully TF-side review draft and prints its local handle.
// Zero GitHub writes (TFAC-494): the review is staged entirely in the artifact's
// details_json and created+submitted atomically only at approval, so concurrent
// runs on one PR never collide on GitHub's one-per-identity pending-review slot.
// It reads the PR head SHA (a read, not a write) so the review pins to the
// reviewed commit at submit. The durable run-scoped `review` artifact is recorded
// at the host choke point (LocalClient.GithubCreatePendingReview); the returned
// handle (the artifact id) is what add-review-comment / finalize-review speak.
//
// --fresh resets an in-progress draft to a clean slate so the agent can restart
// a review it has botched: it clears the run's staged comments + the body/event
// ready sentinel in place, keeping the same handle and pinned head SHA. Post-494
// this is a pure local artifact reset — zero GitHub calls, no identity / no
// anti-hijack logic (the GitHub pending-review model that needed those is gone).
// On a clean slate (no prior start-review in this run) --fresh has nothing to
// reset and falls through to a normal start.
func prStartReview(ctx context.Context, client ghAPI, host agenthost.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	_ = lookupRun(host) // validates identity is present; routing happens inside host

	if hasFlag(args, "--fresh") {
		// Pure-local reset of this run's existing draft for the PR — no GitHub
		// call. An empty handle means there was no draft to reset, so we fall
		// through to a normal start below (which reads the head SHA and creates
		// one), keeping --fresh usable even as the first start-review. commit_sha
		// is the draft's preserved head SHA, echoed so the reset output matches the
		// shape a normal start-review prints.
		handle, commitSHA, err := host.ResetReviewDraft(ctx, owner, repo, number)
		exitOnErr(err)
		if handle != "" {
			printJSON(map[string]any{
				"review_id":  handle,
				"pr_number":  number,
				"commit_sha": commitSHA,
				"status":     "pending",
				"reset":      true,
			})
			return
		}
	}

	// Read the head SHA so the review pins to the reviewed commit at submit.
	pr, err := client.GetPR(ctx, owner, repo, number, false)
	exitOnErr(err)
	// Fail early on a missing head SHA: it's the commit the atomic submit pins to,
	// so staging it empty just defers an opaque GitHub 422 (surfaced as a 502) to
	// approval time. Bail here with an actionable message instead.
	if pr.HeadSHA == "" {
		exitErr(fmt.Sprintf("PR #%d has no head commit SHA — cannot start a review (push a commit to the PR branch first)", number))
	}

	// Record the local review-draft artifact through the host. No GitHub write,
	// no collision check — the draft is run-scoped, so two runs reviewing the same
	// PR never share a row. Seed no inline comments (the agent adds them via
	// add-review-comment).
	reviewID, err := host.GithubCreatePendingReview(ctx, owner, repo, number, pr.HeadSHA, nil)
	exitOnErr(err)

	printJSON(map[string]any{
		"review_id":  reviewID,
		"pr_number":  number,
		"commit_sha": pr.HeadSHA,
		"status":     "pending",
	})
}

// prAddReviewComment bakes the severity badge into the comment body, then stages
// it on this run's review draft (→ GithubAddPendingReviewComment, a local write).
//
// The comment is anchored to — and validated against — the run's WORKTREE HEAD:
// the commit the agent's checkout is on, which is the frame the agent read the
// diff in (`pr diff` shows that same frame). Validating the (path, line,
// start_line) against the local checkout's diff and anchoring the submitted
// comment to that same commit is what keeps a line the agent saw mapped to the
// line GitHub anchors to — sourcing either from the live PR head would re-open
// the skew when the checkout has fallen behind. Outside a worktree it hands an
// empty anchor to the host, which falls back to live-head validation.
//
// Severity lives only in the comment body (the overlay parses it back out for
// the chip).
func prAddReviewComment(ctx context.Context, host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr add-review-comment <review_id> --file <path> --line <N> (--body <text> | --body-file <path>) [--start-line <N>] [--severity <blocker|major|minor|clean>]")
	}
	reviewID := args[0]
	file := flagVal(args, "--file")
	line := mustInt(flagVal(args, "--line"), "line")
	body := flagVal(args, "--body")
	bodyFile := flagVal(args, "--body-file")

	// --body-file mirrors `pr create`: read the comment body from a file (or
	// "-" for stdin) so a multi-line body — diagnosis prose plus a ```suggestion
	// block whose backticks would otherwise need shell escaping — can be passed
	// without a fragile heredoc. The blueprint review aggregator relies on this
	// to relay each reviewer's verbatim body straight from its findings file.
	if body != "" && bodyFile != "" {
		exitErr("--body and --body-file are mutually exclusive; pass one or the other")
	}
	if bodyFile != "" {
		var data []byte
		var err error
		if bodyFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(bodyFile)
		}
		if err != nil {
			exitErr("read --body-file: " + err.Error())
		}
		body = string(data)
	}

	if file == "" || body == "" {
		exitErr("--file and --body (or --body-file) are required")
	}

	// --severity tags the finding's level; baked into the comment body as a
	// shields.io badge (the overlay parses it back out to render a chip).
	// Optional and case-insensitive — third-party review skills that omit it
	// just get an un-badged comment.
	severity, err := domain.NormalizeSeverity(flagVal(args, "--severity"))
	if err != nil {
		exitErr(err.Error())
	}

	var startLine *int
	if sl := flagVal(args, "--start-line"); sl != "" {
		v := mustInt(sl, "start-line")
		startLine = &v
	}

	owner, repo := ownerRepo(args)
	_ = lookupRun(host)

	// Resolve the anchor: the reviewed PR's worktree HEAD — the commit the
	// agent's checkout is on, which is the frame it read the diff in. The host
	// validates the range against this commit's diff (compare base...HEAD) and
	// pins the submitted comment to it. Empty when there's no checkout, in which
	// case the host falls back to live-head validation + anchoring.
	//
	// Prefer the PR's worktree resolved from the run's (run, repo, ref) registry
	// over cwd: in a multi-PR run cwd may sit in a DIFFERENT PR's checkout
	// (TFAC-494/TFAC-502), which would anchor this comment to the wrong commit.
	// Falls back to cwd HEAD when the registry has no unambiguous PR worktree for
	// this repo (single-PR runs, or a path the CLI can't rev-parse), preserving
	// prior behavior.
	cwd, err := os.Getwd()
	if err != nil {
		exitErr(fmt.Sprintf("resolve cwd: %v", err))
	}
	anchorSHA := resolveLocalCheckout(cwd).headSHA
	if wt := reviewedPRWorktreePath(host, owner, repo); wt != "" {
		if sha := resolveLocalCheckout(wt).headSHA; sha != "" {
			anchorSHA = sha
		}
	}

	// Bake the badge in, then stage onto this run's review draft. The host resolves
	// the run's review artifact, validates the range against the anchor commit's
	// diff, and appends the staged comment — returning a TF-local comment id.
	badgedBody := domain.SeverityBadgeMarkdown(severity) + body
	commentID, err := host.GithubAddPendingReviewComment(ctx, owner, repo, reviewID, file, badgedBody, line, startLine, anchorSHA)
	exitOnErr(err)

	printJSON(map[string]any{
		"comment_id": commentID,
		"review_id":  reviewID,
		"severity":   severity,
		"status":     "pending",
	})
}

// reviewedPRWorktreePath returns the absolute worktree path of the PR being
// reviewed in owner/repo, resolved from the run's (run, repo, ref) worktree
// registry: the unique conversation_worktrees row whose repo matches and whose ref is a
// PR ref ("pr-<N>"). This is what lets add-review-comment anchor to the RIGHT
// PR's worktree HEAD in a multi-PR run instead of trusting cwd (TFAC-502).
//
// Returns "" when there is no such row, or more than one (two PRs reviewed in
// the SAME repo within one run is ambiguous without the PR number, so the
// caller falls back to cwd — the agent is expected to have cd'd into the
// reviewed PR's worktree). A list error is non-fatal: anchoring degrades to
// cwd, never blocking the comment.
func reviewedPRWorktreePath(host agenthost.Client, owner, repo string) string {
	rows, err := host.ListRunWorktrees(context.Background())
	if err != nil {
		return ""
	}
	repoID := owner + "/" + repo
	match := ""
	for _, w := range rows {
		if !strings.EqualFold(w.RepoID, repoID) || !strings.HasPrefix(w.Ref, "pr-") {
			continue
		}
		if match != "" {
			// Two PR worktrees in one repo for this run — we can't tell which PR
			// this comment targets from owner/repo alone, so fall back to cwd
			// HEAD. Warn (stderr, so the success JSON on stdout stays clean) so a
			// "comment anchored to the wrong commit" symptom is diagnosable: the
			// agent must run add-review-comment from inside the reviewed PR's
			// worktree in that case.
			fmt.Fprintf(os.Stderr, "gh pr add-review-comment: %s has multiple PR worktrees this run; anchoring to cwd HEAD — run this from the reviewed PR's worktree\n", repoID)
			return ""
		}
		match = w.Path
	}
	return match
}

// prFinalizeReview hands the finished review to the host, which snapshots the
// agent's draft (body + event + the locally staged inline comments) into the
// run's review artifact and sets the ready sentinel. The TFAC-358
// anti-double-submit guard fires host-side: a second call gets a hard error.
//
// Whether the review is then staged for a human or submitted to GitHub is the
// team's review posture (TFAC-680) — resolved host-side, where the settings and
// the credential live. This verb knows nothing about posture; it reports which
// of the two happened, from what the host returns. That split is the standing
// rule the response contract rests on: the PROMPT describes the shape of the
// call, the TOOL RESPONSE reports what actually happened. Making the prompt text
// posture-dependent would fragment the cached system-prompt prefix per team
// config, and hardcoding one outcome makes it wrong for every team that changes
// the setting.
//
// The verb is "finalize-review", not "submit-review": the agent's act is
// finishing the review, not choosing to publish it — the posture decides that —
// and "submit" would imply the agent is calling submitPullRequestReview itself.
// "finalize" matches the internal vocabulary (FinalizeReviewDraft, the ready
// sentinel).
func prFinalizeReview(ctx context.Context, host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr finalize-review <review_id> --event <approve|comment|request_changes> (--body <text> | --body-file <path>)")
	}
	reviewID := args[0]
	event := flagVal(args, "--event")
	body := flagVal(args, "--body")
	bodyFile := flagVal(args, "--body-file")

	// --body-file mirrors `pr create` / `add-review-comment`: read the summary
	// body from a file (or "-" for stdin), so the blueprint review aggregator
	// can assemble a multi-line summary without shell escaping.
	if body != "" && bodyFile != "" {
		exitErr("--body and --body-file are mutually exclusive; pass one or the other")
	}
	if bodyFile != "" {
		var data []byte
		var err error
		if bodyFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(bodyFile)
		}
		if err != nil {
			exitErr("read --body-file: " + err.Error())
		}
		body = string(data)
	}

	if event == "" {
		exitErr("--event is required (approve, comment, request_changes)")
	}

	eventMap := map[string]string{
		"approve":         "APPROVE",
		"comment":         "COMMENT",
		"request_changes": "REQUEST_CHANGES",
	}
	ghEvent, ok := eventMap[event]
	if !ok {
		ghEvent = event
	}

	_ = lookupRun(host)

	// Snapshot the draft into the run's review artifact, set the ready sentinel,
	// and — under a posting posture — submit it. The "meaningful review" check
	// (body or inline comments for a non-approve) runs host-side, where the live
	// comments are known.
	res, err := host.FinalizeReviewDraft(ctx, reviewID, ghEvent, body)
	if errors.Is(err, agenthost.ErrReviewAlreadyFinalized) {
		exitErr(fmt.Sprintf(
			"review %s has already been finalized. Do not call finalize-review again — your work on this review is complete. Finish the run by writing %s and returning your completion JSON.",
			reviewID, agentMemoryFile(),
		))
	}
	exitOnErr(err)

	// Two shapes, one per outcome. Both carry next_step spelling out the same
	// wrap-up, so an agent that isn't reading the prompt closely still gets the
	// "your review work is done" signal directly from the tool result.
	if res.Posted {
		printJSON(map[string]any{
			"status":    "submitted",
			"review_id": reviewID,
			"event":     ghEvent,
			"url":       res.URL,
			"next_step": "Review is submitted to GitHub. Do not call finalize-review again. Finish the run by writing " + agentMemoryFile() + " and returning your completion JSON.",
		})
		return
	}
	printJSON(map[string]any{
		"status":    "drafted_awaiting_approval",
		"review_id": reviewID,
		"event":     ghEvent,
		"next_step": "Review is drafted and awaiting human approval. Do not call finalize-review again. Finish the run by writing " + agentMemoryFile() + " and returning your completion JSON.",
	})
}

// prCreate opens a real GitHub draft PR immediately and prints
// {status:"created", url, number, draft:true}. The caller is responsible for
// having pushed the head branch upstream first; that's the contract documented
// in the agent prompts, and the ls-remote preflight below enforces it early.
//
// The PR is always created as a draft — repo-visible, but not "ready for
// review" — and stays a draft until a human approves it in the UI (which marks
// it ready for review and appends the agentmeta footer). No footer is applied
// here: the proposed body is what the agent drafted. The durable pull_request
// artifact is recorded at the host choke point (LocalClient.GithubCreatePR),
// deduped on the PR number — which is also the idempotency guard against a
// repeated `pr create`, replacing the retired pending_prs one-per-run lock.
func prCreate(ctx context.Context, client ghAPI, args []string) {
	title := flagVal(args, "--title")
	body := flagVal(args, "--body")
	bodyFile := flagVal(args, "--body-file")
	base := flagVal(args, "--base")
	head := flagVal(args, "--head")

	if title == "" {
		exitErr("usage: gh pr create --title <T> (--body <B> | --body-file <path>) --base <branch> [--head <branch>] [--draft] [--repo owner/repo]\n--title is required")
	}
	if base == "" {
		exitErr("--base is required (the branch to merge into, e.g. main)")
	}

	// --body and --body-file are mutually exclusive: with both set,
	// the agent's intent is ambiguous and silently picking one risks
	// dropping the longer/more-recent draft. Force a clean choice.
	if body != "" && bodyFile != "" {
		exitErr("--body and --body-file are mutually exclusive; pass one or the other")
	}
	if bodyFile != "" {
		// "-" means stdin, matching gh's convention. The file path is
		// read directly (no glob expansion, no relative-path
		// surprises — just os.ReadFile from cwd).
		var data []byte
		var err error
		if bodyFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(bodyFile)
		}
		if err != nil {
			exitErr("read --body-file: " + err.Error())
		}
		body = string(data)
	}

	// Strip Claude Code's auto-appended citation line. Claude Code
	// (the agent harness) routinely tacks "🤖 Generated with [Claude
	// Code](https://claude.com/claude-code)" onto every PR body it
	// produces. Triage Factory's footer (added by the server at
	// submit time) already attributes the work — letting the
	// upstream Claude Code citation through would visually crowd
	// out the TF citation and double-bill the PR. Strip before
	// queuing so the user never sees it in the preview, even if the
	// agent forgot to remove it.
	body = stripClaudeCodeCitation(body)

	owner, repo := ownerRepo(args)

	// If --head wasn't supplied, derive from the current branch. The
	// agent's cwd inside a materialized worktree is `feature/<KEY>`
	// after `cd "$(triagefactory exec workspace add ...)"` so this
	// resolves cleanly. exitErr if we can't determine — would otherwise
	// silently submit the wrong branch to GitHub.
	if head == "" {
		out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err != nil {
			exitErr("could not determine current branch via git rev-parse; pass --head <branch> explicitly. err: " + err.Error())
		}
		head = strings.TrimSpace(string(out))
		if head == "" || head == "HEAD" {
			exitErr("current branch is detached or empty; pass --head <branch> explicitly")
		}
	}

	// Pre-flight: verify the head branch actually exists on origin.
	// pr create's contract requires `git push` first, but agents
	// occasionally skip that step (e.g. a Jira agent that decided
	// the changes already lived on the remote and went straight to
	// `pr create`). Without this check, the row queues fine, the
	// human approval overlay can't fetch the diff, the user sees a
	// 502, and the run isn't recoverable without manual DB surgery.
	// `ls-remote --exit-code` returns 2 specifically when nothing
	// matched, so we hard-fail with a clear "did you forget to
	// push?" message that teaches the agent how to retry — cheaper
	// than letting the agent finish the run with a broken row.
	//
	// However, this preflight relies on the current working directory's
	// git configuration (`origin`). When the caller targets a repository
	// explicitly (for example via `--repo` and `--head`) from outside a
	// local checkout, there is no worktree to query and we should let the
	// request proceed to the GitHub API instead of failing early here.
	inWorkTree := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	if err := inWorkTree.Run(); err == nil {
		lsRemote := exec.Command("git", "ls-remote", "--exit-code", "--heads", "origin", head)
		var lsStderr strings.Builder
		lsRemote.Stderr = &lsStderr
		if err := lsRemote.Run(); err != nil {
			exitErr(fmt.Sprintf(
				"head branch '%s' is not on origin. Run `git push origin %s` first, then retry `pr create`. `pr create` requires the head branch to exist on the upstream — without it the diff cannot be rendered for human approval. (git ls-remote stderr: %s)",
				head, head, strings.TrimSpace(lsStderr.String()),
			))
		}
	}

	// Always open a real draft PR. node_id (3rd return) is recorded onto the
	// pull_request artifact at the host choke point (LocalClient.GithubCreatePR),
	// so it's discarded here.
	number, htmlURL, _, err := client.CreatePR(ctx, owner, repo, head, base, title, body, true)
	exitOnErr(err)

	printJSON(map[string]any{
		"status": "created",
		"number": number,
		"url":    htmlURL,
		"draft":  true,
	})
}

// --- Direct comments (hit GitHub API) ---

func prAddComment(ctx context.Context, client ghAPI, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	commentID, err := client.AddComment(ctx, owner, repo, number, body)
	exitOnErr(err)
	printJSON(map[string]any{"comment_id": commentID})
}

func prCommentReply(ctx context.Context, client ghAPI, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-reply <comment_id> --body <text> --pr <number>")
	}
	commentID := mustInt(args[0], "comment_id")
	owner, repo := ownerRepo(args)
	prNumber := mustInt(flagVal(args, "--pr"), "pr_number")
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	replyID, err := client.ReplyToComment(ctx, owner, repo, prNumber, commentID, body)
	exitOnErr(err)
	printJSON(map[string]any{"reply_id": replyID})
}

func prCommentReact(ctx context.Context, client ghAPI, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-react <comment_id> --emoji <emoji>")
	}
	commentID := mustInt(args[0], "comment_id")
	owner, repo := ownerRepo(args)
	emoji := flagVal(args, "--emoji")
	if emoji == "" {
		exitErr("--emoji is required (+1, -1, laugh, confused, heart, hooray, rocket, eyes)")
	}
	err := client.ReactToComment(ctx, owner, repo, commentID, emoji)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true})
}

// prCommentUpdate edits a GitHub comment, polymorphic on the id space the agent
// passes back from whichever add verb produced it:
//
//   - a numeric id is a live GitHub comment (a top-level issue comment from
//     add-comment, or an already-submitted review-diff comment) → UpdateComment,
//     unchanged.
//   - a non-numeric id is a comment on this run's TF-side review draft (from
//     add-review-comment, staged not yet submitted) → rewrite the staged comment
//     on the review artifact via the host, with the severity badge re-baked so
//     the chip survives the edit. No GitHub call until the review submits.
//
// REST ids are all-digits and staged ids never are, so a successful int parse is
// the discriminator. One verb serves both adds.
func prCommentUpdate(ctx context.Context, client ghAPI, host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-update <comment_id> --body <text> [--severity <blocker|major|minor|clean>]")
	}
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}

	// Numeric → live GitHub comment (REST path, unchanged). --severity is
	// meaningless for a submitted comment (no chip), so it's ignored there.
	if id, err := strconv.Atoi(args[0]); err == nil {
		owner, repo := ownerRepo(args)
		exitOnErr(client.UpdateComment(ctx, owner, repo, id, body))
		printJSON(map[string]any{"ok": true})
		return
	}

	// Non-numeric → staged review comment. Re-bake the severity badge (mirroring
	// add-review-comment) so the chip survives the edit. Strip any badge already
	// on the supplied body FIRST: a staged comment's stored body carries its baked
	// badge, so an agent that edits by copying the current body back in would
	// otherwise stack a second badge — and omitting --severity would leave the old
	// one. Stripping then re-baking makes --severity authoritative: present →
	// exactly that badge, omitted → no badge (chip dropped).
	severity, err := domain.NormalizeSeverity(flagVal(args, "--severity"))
	if err != nil {
		exitErr(err.Error())
	}
	_ = lookupRun(host)
	_, unbadged := domain.ParseSeverityBadge(body)
	badgedBody := domain.SeverityBadgeMarkdown(severity) + unbadged
	exitOnErr(host.UpdateStagedReviewComment(ctx, args[0], badgedBody))
	printJSON(map[string]any{"ok": true, "severity": severity})
}

// prCommentDelete deletes a GitHub comment, polymorphic on the id space exactly
// like prCommentUpdate: a numeric id deletes a live GitHub comment (unchanged), a
// non-numeric id removes a staged comment from this run's review draft via the
// host (no GitHub call until the review submits).
func prCommentDelete(ctx context.Context, client ghAPI, host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-delete <comment_id>")
	}

	if id, err := strconv.Atoi(args[0]); err == nil {
		owner, repo := ownerRepo(args)
		exitOnErr(client.DeleteComment(ctx, owner, repo, id))
		printJSON(map[string]any{"ok": true})
		return
	}

	_ = lookupRun(host)
	exitOnErr(host.DeleteStagedReviewComment(ctx, args[0]))
	printJSON(map[string]any{"ok": true})
}

// --- argument parsing helpers ---

func parseRepoAndNumber(args []string) (string, string, int) {
	owner, repo := ownerRepo(args)
	// Find first positional arg (not a flag or flag value)
	num := firstPositional(args)
	if num == "" {
		exitErr("PR number is required")
	}
	number := mustInt(num, "pr_number")
	return owner, repo, number
}

// firstPositional returns the first argument that isn't a flag or a flag's value.
func firstPositional(args []string) string {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--repo" || a == "--file" || a == "--pr" || a == "--body" || a == "--body-file" || a == "--line" || a == "--start-line" || a == "--event" || a == "--status" || a == "--severity" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// ownerRepo resolves the target repo for a PR subcommand. Delegates to the
// shared resolveRepo so --repo flag, TRIAGE_FACTORY_REPO env, and .git/config
// fallback all behave consistently across every gh command. Passes the
// full args slice (not just the flag value) so resolveRepo can detect
// "--repo present but empty" and fail loudly instead of silently falling
// back to env/git resolution.
func ownerRepo(args []string) (string, string) {
	owner, repo, err := resolveRepo(args)
	if err != nil {
		exitErr(err.Error())
	}
	return owner, repo
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func flagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func mustInt(s, name string) int {
	if s == "" {
		exitErr(name + " is required")
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		exitErr(fmt.Sprintf("invalid %s: %s", name, s))
	}
	return v
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

// claudeCodeCitationFragment matches the citation Claude Code
// auto-appends to every PR body it produces. We match on the
// markdown-link substring rather than the full "🤖 Generated with
// ..." prefix so the strip survives the agent reformatting the
// emoji or surrounding text. The link target is stable.
const claudeCodeCitationFragment = "Generated with [Claude Code](https://claude.com/claude-code)"

// stripClaudeCodeCitation drops the trailing line containing the
// Claude Code citation, plus any whitespace separating it from the
// preceding content. Returns body unchanged when the last
// non-whitespace line doesn't contain the citation — including the
// case where the citation appears mid-body, since that's content
// the user wrote intentionally.
func stripClaudeCodeCitation(body string) string {
	trimmed := strings.TrimRight(body, " \t\n\r")
	if trimmed == "" {
		return body
	}
	lastNL := strings.LastIndex(trimmed, "\n")
	var lastLine string
	if lastNL == -1 {
		lastLine = trimmed
	} else {
		lastLine = trimmed[lastNL+1:]
	}
	if !strings.Contains(lastLine, claudeCodeCitationFragment) {
		return body
	}
	if lastNL == -1 {
		return ""
	}
	return strings.TrimRight(trimmed[:lastNL], " \t\n\r")
}
