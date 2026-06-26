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
// In local mode this resolves identity from TRIAGE_FACTORY_RUN_ID at
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
// into the agent's environment (TRIAGE_FACTORY_RUN_ROOT,
// TRIAGE_FACTORY_BLUEPRINT_RUN_ID, and TRIAGE_FACTORY_RUN_ID — see
// internal/delegate/run.go). This mirrors the path the completion gate reads
// (cwd/_scratch/entity-memory/<blueprint_run_id>/<run_id>.md), so the "do not
// retry, finish by writing ..." messages below can point the agent at the file
// concretely.
//
// The bare env-var reference these messages used to carry would be written
// verbatim by the agent's Write tool — which does no shell expansion — so the
// file landed at a literal "$TRIAGE_FACTORY_RUN_ROOT/..." path the gate never
// found. Inside the sandbox TRIAGE_FACTORY_RUN_ROOT is already translated to
// /work, so reading it here yields a path the agent can actually reach. Falls
// back to the env-var-reference form if any piece is unset (subcommand invoked
// outside a delegated run) so the message still reads coherently.
func agentMemoryFile() string {
	root := os.Getenv("TRIAGE_FACTORY_RUN_ROOT")
	ns := os.Getenv("TRIAGE_FACTORY_BLUEPRINT_RUN_ID")
	runID := os.Getenv("TRIAGE_FACTORY_RUN_ID")
	if root == "" || ns == "" || runID == "" {
		return "$TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/$TRIAGE_FACTORY_BLUEPRINT_RUN_ID/$TRIAGE_FACTORY_RUN_ID.md"
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
		prDiff(ctx, api, flags)
	case "files":
		prFiles(ctx, api, flags)
	case "thread-view":
		prThreadView(ctx, api, flags)
	case "review-view":
		prReviewView(ctx, api, flags)
	case "review-dismiss":
		prReviewDismiss(ctx, api, flags)
	case "start-review":
		prStartReview(ctx, api, host, flags)
	case "add-review-comment":
		prAddReviewComment(ctx, host, flags)
	case "submit-review":
		prSubmitReview(ctx, host, flags)
	case "add-comment":
		prAddComment(ctx, api, flags)
	case "comment-reply":
		prCommentReply(ctx, api, flags)
	case "comment-react":
		prCommentReact(ctx, api, flags)
	case "comment-update":
		prCommentUpdate(ctx, api, flags)
	case "comment-delete":
		prCommentDelete(ctx, api, flags)
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
type diffManifest struct {
	Owner        string        `json:"owner"`
	Repo         string        `json:"repo"`
	Number       int           `json:"number"`
	HeadSHA      string        `json:"head_sha"`
	BaseRef      string        `json:"base_ref"`
	ChangedFiles int           `json:"changed_files"`
	Additions    int           `json:"additions"`
	Deletions    int           `json:"deletions"`
	Dir          string        `json:"dir"`
	FullDiffPath string        `json:"full_diff_path"`
	Truncated    bool          `json:"truncated"`
	Files        []fileSummary `json:"files"`
}

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
// Two escape hatches keep the old inline behavior available:
//   - --file <path>: targeted single-file diff stays inline (bounded). No
//     file written.
//   - --stdout: whole-diff-to-stdout, for scripts/pipelines. No file written.
//
// Both inline paths share persistPRDiff's HTTP-406 ("diff too large")
// fallback: GitHub refuses the diff media type, so we reassemble from
// per-file patches — the whole diff for --stdout, or just the requested
// file for --file — instead of erroring.
func prDiff(ctx context.Context, client ghAPI, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	file := flagVal(args, "--file")
	stdout := hasFlag(args, "--stdout")

	if file != "" || stdout {
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
		fmt.Print(diff)
		return
	}

	cwd, err := os.Getwd()
	if err != nil {
		exitErr(fmt.Sprintf("resolve cwd: %v", err))
	}
	manifest, err := persistPRDiff(ctx, client, cwd, owner, repo, number)
	exitOnErr(err)
	printJSON(manifest)
}

// persistPRDiff fetches the PR's diff + metadata, writes full.diff and
// manifest.json into a per-PR directory under _scratch/, and returns the
// manifest. The directory is keyed by (owner, repo, number) — the task's
// identity, which the agent always knows — so a later blueprint step can
// locate an earlier capture without first looking up the head SHA. Each
// `pr diff` overwrites the capture in place; the manifest records head_sha so
// a reader that cares about freshness can compare it to the live head.
func persistPRDiff(ctx context.Context, client ghAPI, cwd, owner, repo string, number int) (diffManifest, error) {
	// Three separate REST calls follow (GetPR → GetPRFiles → GetPRDiff): the
	// API has no transactional snapshot, so a force-push mid-sequence could
	// leave manifest.head_sha (from GetPR) describing a different commit than
	// full.diff. We accept that race — head_sha is recorded precisely so a
	// reader can detect the drift — rather than adding fragile re-fetch loops.
	pr, err := client.GetPR(ctx, owner, repo, number, false)
	if err != nil {
		return diffManifest{}, fmt.Errorf("fetch PR #%d: %w", number, err)
	}

	// GetPRFiles is needed for the per-file manifest rows and doubles as the
	// reassembly source if the full diff is too large (HTTP 406).
	files, err := client.GetPRFiles(ctx, owner, repo, number)
	if err != nil {
		return diffManifest{}, fmt.Errorf("list PR files: %w", err)
	}

	fullDiff, truncated, err := fetchFullDiff(ctx, client, owner, repo, number, files)
	if err != nil {
		return diffManifest{}, err
	}

	// Key on the task identity (owner, repo, number) the agent always knows,
	// so it can locate an earlier capture without first resolving the head
	// SHA. head_sha goes in the manifest for freshness comparison.
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

	manifest := diffManifest{
		Owner:        owner,
		Repo:         repo,
		Number:       number,
		HeadSHA:      pr.HeadSHA,
		BaseRef:      pr.BaseRef,
		ChangedFiles: pr.ChangedFiles,
		Additions:    pr.Additions,
		Deletions:    pr.Deletions,
		Dir:          destDir,
		FullDiffPath: fullDiffPath,
		Truncated:    truncated,
		Files:        make([]fileSummary, 0, len(files)),
	}
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

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return diffManifest{}, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, manifestFilename), manifestBytes, 0o644); err != nil {
		return diffManifest{}, fmt.Errorf("write %s: %w", manifestFilename, err)
	}

	return manifest, nil
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

func prThreadView(ctx context.Context, client ghAPI, args []string) {
	if len(args) < 2 {
		exitErr("usage: gh pr thread-view <pr_number> <comment_id> [--page N]")
	}
	owner, repo, _ := parseRepoAndNumber(args[:1])
	commentID := mustInt(args[1], "comment_id")
	page := 1
	if v := flagVal(args, "--page"); v != "" {
		page = mustInt(v, "page")
	}
	thread, err := client.GetCommentThread(ctx, owner, repo, commentID, page)
	exitOnErr(err)
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

// --- Review lifecycle (local state) ---

// prStartReview creates a real GitHub *pending* review (private to the bot until
// submit) and prints its review id. The durable `review` artifact is recorded at
// the host choke point (LocalClient.GithubCreatePendingReview), which also runs
// the identity-aware collision check. No local staging: the review now lives on
// GitHub, edited 1:1.
func prStartReview(ctx context.Context, client ghAPI, host agenthost.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	_ = lookupRun(host) // validates identity is present; routing happens inside host

	// Fetch the head SHA so the pending review pins to the reviewed commit.
	pr, err := client.GetPR(ctx, owner, repo, number, false)
	exitOnErr(err)

	// Create the pending review through the host (→ GithubCreatePendingReview):
	// it folds in the collision check and records the artifact. Seed no inline
	// comments — the agent adds them one at a time via add-review-comment.
	reviewID, err := client.CreatePendingReview(ctx, owner, repo, number, pr.HeadSHA, nil)
	if err != nil {
		if errors.Is(err, agenthost.ErrPendingReviewCollision) {
			exitErr(fmt.Sprintf(
				"a pending review already exists for this identity on PR #%d. Use a GitHub App / service-account credential so the bot manages its own review — a real-user PAT's in-progress review must never be hijacked.",
				number,
			))
		}
		exitOnErr(err)
	}

	printJSON(map[string]any{
		"review_id":  reviewID,
		"pr_number":  number,
		"commit_sha": pr.HeadSHA,
		"status":     "pending",
	})
}

// prAddReviewComment bakes the severity badge into the comment body, then adds it
// to the real GitHub pending review (→ GithubAddPendingReviewComment). Severity
// lives only in the GitHub comment body now (no local column); the overlay parses
// it back out for the chip. No local diff-shape pre-check — the live PR is the
// source of truth, so GitHub's own out-of-diff line rejection surfaces instead.
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

	// Bake the badge in, then add to the GitHub pending review. The add op keys
	// off the review node id, but the host needs owner/repo to resolve the repo's
	// credential — so build a repo-scoped adapter (the shared PR dispatch adapter
	// is unscoped).
	badgedBody := domain.SeverityBadgeMarkdown(severity) + body
	client := newHostAPI(host, owner, repo)
	commentID, err := client.AddPendingReviewComment(ctx, reviewID, ghclient.SubmitReviewComment{
		Path:      file,
		Line:      line,
		StartLine: startLine,
		Body:      badgedBody,
	})
	exitOnErr(err)

	printJSON(map[string]any{
		"comment_id": commentID,
		"review_id":  reviewID,
		"severity":   severity,
		"status":     "pending",
	})
}

// prSubmitReview hands the finished review off for human approval — it does NOT
// submit to GitHub (approval does that). The host snapshots the agent's draft
// (body + event + the live inline comments) into the run's review artifact and
// sets the ready sentinel that parks the run. The TFAC-358 anti-double-submit
// guard fires host-side: a second call gets a hard error.
func prSubmitReview(ctx context.Context, host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr submit-review <review_id> --event <approve|comment|request_changes> (--body <text> | --body-file <path>)")
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

	// Snapshot the draft into the run's review artifact and set the ready
	// sentinel. No GitHub submit — approval applies body + event atomically. The
	// "meaningful review" check (body or inline comments for a non-approve) runs
	// host-side, where the live comments are known.
	err := host.FinalizeReviewDraft(ctx, reviewID, ghEvent, body)
	if errors.Is(err, agenthost.ErrReviewAlreadyFinalized) {
		exitErr(fmt.Sprintf(
			"review %s has already been finalized for human approval. Do not call submit-review again — your work on this review is complete. Finish the run by writing %s and returning your completion JSON.",
			reviewID, agentMemoryFile(),
		))
	}
	exitOnErr(err)

	printJSON(map[string]any{
		// drafted_awaiting_approval makes the contract explicit: the review is
		// handed off to the human approval queue and the agent's work on this PR
		// is done. next_step spells out the wrap-up so even agents that aren't
		// reading the prompt closely get the signal directly from the tool result.
		"status":    "drafted_awaiting_approval",
		"review_id": reviewID,
		"event":     ghEvent,
		"next_step": "Review is drafted and awaiting human approval. Do not call submit-review again. Finish the run by writing " + agentMemoryFile() + " and returning your completion JSON.",
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

// prCommentUpdate edits a real GitHub comment by its numeric id (an issue comment
// or a submitted review comment). Pending-review comments are edited human-side
// through the overlay (server-direct), never here — the agent flow is add-only.
func prCommentUpdate(ctx context.Context, client ghAPI, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-update <comment_id> --body <text>")
	}
	owner, repo := ownerRepo(args)
	id := mustInt(args[0], "comment_id")
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	exitOnErr(client.UpdateComment(ctx, owner, repo, id, body))
	printJSON(map[string]any{"ok": true})
}

func prCommentDelete(ctx context.Context, client ghAPI, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-delete <comment_id>")
	}
	owner, repo := ownerRepo(args)
	id := mustInt(args[0], "comment_id")
	exitOnErr(client.DeleteComment(ctx, owner, repo, id))
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
