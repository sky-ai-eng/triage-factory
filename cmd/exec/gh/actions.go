package gh

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/github"
)

// Size caps for GitHub Actions log archive downloads. Three layers of
// defense because zip compression ratios can be extreme and any single
// cap in isolation is bypassable.
//
// maxArchiveBytes is the compressed-download cap — above this, refuse to
// download at all. 500 MB is deliberately generous; realistic log
// archives are a few MB at most, and a PR with 500 MB of compressed CI
// logs has bigger problems than this command.
//
// maxPerFileBytes is the per-entry uncompressed cap. Even a modestly-
// sized archive containing one huge entry (e.g., a 10 GB single file
// padded with zeros) is rejected at the entry level. 100 MB per file is
// generous for log files.
//
// maxTotalUncompressedBytes is the total-uncompressed cap across every
// extracted entry. Without this layer, a highly-compressible archive
// containing many small entries (1000 × 1 MB each = 1 GB uncompressed,
// maybe 10 MB compressed after DEFLATE) could stay under both of the
// other caps yet still expand to multiple gigabytes on disk. 2 GB here
// is enough for any realistic log archive — a matrix build with 20
// jobs × 50 MB of verbose logs each is only 1 GB — and catches
// pathological zip bombs before they fill the disk.
const (
	maxArchiveBytes           int64 = 500 * 1024 * 1024      // 500 MB
	maxPerFileBytes           int64 = 100 * 1024 * 1024      // 100 MB
	maxTotalUncompressedBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GB
)

// handleActions dispatches gh actions subcommands. The GitHub adapter is
// built inside each action once owner/repo resolve, since the raw transport
// (Get / DownloadArtifact) needs them for the host's credential resolution.
func handleActions(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: triagefactory exec gh actions <action> [flags]")
	}

	action := args[0]
	flags := args[1:]

	switch action {
	case "download-logs":
		actionsDownloadLogs(host, flags)
	case "list-runs":
		actionsListRuns(host, flags)
	default:
		exitErr(fmt.Sprintf("unknown actions action: %s", action))
	}
}

// maxListedRuns caps the per-request run list. Agents asking "what runs are
// on this PR" almost always want the newest few — hundreds of runs on a
// long-lived PR would bloat the prompt context and make the output harder
// to reason about. Keeping the cap tight also bounds the API page size.
const maxListedRuns = 20

// listRunsResult is the JSON envelope emitted on success by
// `gh actions list-runs`. Field names mirror GitHub's API where practical
// so agents reading the docs can cross-reference.
type listRunsResult struct {
	Owner   string        `json:"owner"`
	Repo    string        `json:"repo"`
	Filter  listRunFilter `json:"filter"`
	Runs    []listRun     `json:"runs"`
	HeadSHA string        `json:"head_sha,omitempty"` // resolved head SHA when --pr was used
}

type listRunFilter struct {
	PR  int    `json:"pr,omitempty"`
	SHA string `json:"sha,omitempty"`
}

type listRun struct {
	RunID        int64  `json:"run_id"`
	WorkflowName string `json:"workflow_name"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HeadSHA      string `json:"head_sha"`
	HeadBranch   string `json:"head_branch"`
	CreatedAt    string `json:"created_at"`
	URL          string `json:"url"`
}

// actionsListRuns implements `gh actions list-runs --pr N | --sha SHA`.
//
// Discovery fallback for CI-fix prompts: the primary event carries a
// WorkflowRunID when the failing check is Actions-backed and the poller
// saw it, but third-party CI (Supabase, Circle) lacks that field, and
// force-pushes can stale-invalidate the stored ID. This command lets the
// agent list runs for a PR or SHA and pick the right one.
//
// Exactly one of --pr / --sha is required. --pr takes a PR number and
// resolves the head SHA before calling the Actions list endpoint; --sha
// hits the endpoint directly.
func actionsListRuns(host agenthost.Client, args []string) {
	prStr := flagVal(args, "--pr")
	sha := flagVal(args, "--sha")

	if (prStr == "") == (sha == "") {
		exitErr("usage: triagefactory exec gh actions list-runs (--pr <N> | --sha <SHA>) [--repo owner/repo]")
	}

	owner, repo, err := resolveRepo(args)
	if err != nil {
		exitErr(err.Error())
	}
	client := newHostAPI(host, owner, repo)

	if prStr != "" {
		prNum, err := strconv.Atoi(prStr)
		if err != nil || prNum <= 0 {
			exitErr(fmt.Sprintf("invalid --pr value %q: expected a positive integer", prStr))
		}
		// Compact fetch — GetPR is heavier than we need (pulls reviews and
		// comments) but it's the existing path that correctly handles auth,
		// retries, and error shapes. The extra calls are a few hundred
		// milliseconds on a PR with normal activity — acceptable for a
		// discovery command agents run at most once per task.
		pr, err := client.GetPR(owner, repo, prNum, false)
		if err != nil {
			exitErr(fmt.Sprintf("fetch PR #%d: %v", prNum, err))
		}
		if pr.HeadSHA == "" {
			exitErr(fmt.Sprintf("PR #%d has no head SHA — can't list runs for it", prNum))
		}
		sha = pr.HeadSHA
	}

	runs, err := fetchRunsForSHA(client, owner, repo, sha)
	if err != nil {
		exitErr(err.Error())
	}

	filter := listRunFilter{SHA: sha}
	if prStr != "" {
		// strconv.Atoi above already validated
		n, _ := strconv.Atoi(prStr)
		filter.PR = n
	}

	printJSON(listRunsResult{
		Owner:   owner,
		Repo:    repo,
		Filter:  filter,
		Runs:    runs,
		HeadSHA: sha,
	})
}

// fetchRunsForSHA queries GET /repos/{o}/{r}/actions/runs?head_sha=<sha>
// and flattens the response to the subset of fields the agent needs.
// Runs come back newest-first per GitHub's default ordering.
func fetchRunsForSHA(client ghAPI, owner, repo, sha string) ([]listRun, error) {
	q := url.Values{
		"head_sha":              []string{sha},
		"per_page":              []string{strconv.Itoa(maxListedRuns)},
		"exclude_pull_requests": []string{"true"},
	}
	apiPath := fmt.Sprintf("/repos/%s/%s/actions/runs?%s", owner, repo, q.Encode())

	data, err := client.Get(apiPath)
	if err != nil {
		return nil, fmt.Errorf("list workflow runs: %w", err)
	}

	var resp struct {
		WorkflowRuns []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Event      string `json:"event"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HeadSHA    string `json:"head_sha"`
			HeadBranch string `json:"head_branch"`
			CreatedAt  string `json:"created_at"`
			HTMLURL    string `json:"html_url"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse workflow runs: %w", err)
	}

	out := make([]listRun, 0, len(resp.WorkflowRuns))
	for _, r := range resp.WorkflowRuns {
		out = append(out, listRun{
			RunID:        r.ID,
			WorkflowName: r.Name,
			Event:        r.Event,
			Status:       r.Status,
			Conclusion:   r.Conclusion,
			HeadSHA:      r.HeadSHA,
			HeadBranch:   r.HeadBranch,
			CreatedAt:    r.CreatedAt,
			URL:          r.HTMLURL,
		})
	}
	return out, nil
}

// downloadLogsResult is the JSON envelope emitted on success by
// `gh actions download-logs`. Structured output matches the `exec gh`
// contract that every command produces JSON on stdout, so downstream
// tooling (agents, jq, shell pipelines) can parse without scraping
// human-readable text.
//
// Two response shapes are possible depending on which path served the
// request:
//
//   - Run-level archive succeeded (run is fully completed): Entries is the
//     top-level extracted dir listing, FallbackUsed=false, and Jobs[]
//     entries have empty LogPath fields (the agent reads files via DestDir
//     using the standard zip layout).
//   - Per-job fallback (run is still in_progress; archive 404s): Entries is
//     omitted, FallbackUsed=true, and each completed job in Jobs[] has its
//     LogPath populated with the absolute path to a plain-text log file.
//     Queued/in_progress jobs are stubs (empty LogPath).
//
// Jobs[] is always present so the agent sees the full job set in either
// shape. RunStatus is derived from job statuses (any non-completed job →
// run is in_progress) which avoids a second API round-trip.
type downloadLogsResult struct {
	RunID           int64     `json:"run_id"`
	Owner           string    `json:"owner"`
	Repo            string    `json:"repo"`
	DestDir         string    `json:"dest_dir"`
	BytesDownloaded int64     `json:"bytes_downloaded"`
	RunStatus       string    `json:"run_status"`
	FallbackUsed    bool      `json:"fallback_used"`
	Entries         []string  `json:"entries,omitempty"`
	Jobs            []jobInfo `json:"jobs"`
}

// jobInfo is one row of the Jobs[] array on downloadLogsResult. Field names
// mirror GitHub's Actions API where possible so an agent reading the docs
// can cross-reference. LogPath is the only field we synthesize ourselves
// (absolute path on disk, populated only when the per-job fallback wrote a
// log file for this job).
type jobInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
	LogPath     string `json:"log_path,omitempty"`
}

// actionsDownloadLogs implements `gh actions download-logs <run_id>`.
//
// Fetches workflow run logs into <cwd>/_scratch/ci-logs/<run_id>/ and
// prints a structured JSON result on stdout so agents can parse it the
// same way they parse every other exec gh command. Errors go to stderr
// with a non-zero exit.
//
// Two paths exist because GitHub's run-level archive endpoint
// (/actions/runs/{id}/logs) returns 404 until the entire run completes —
// so when one job fails fast in a long-running matrix build, the archive
// is unavailable for the rest of the run. The fallback hits the per-job
// endpoint (/actions/jobs/{id}/logs) for each completed job individually,
// which works the moment a job finishes regardless of run state. Either
// way the response always includes Jobs[] so the agent sees the full job
// set with stubs for anything still running.
//
// The resource-owning work (temp file + destination directory) lives in
// downloadAndExtractLogs / downloadPerJobLogsToDir so defers actually fire
// on error — exitErr calls os.Exit, which skips defers, so inlining the
// logic here would leak the temp zip (and leave a half-extracted destDir)
// on every failure path.
func actionsDownloadLogs(host agenthost.Client, args []string) {
	// Validate the positional arg first. If the user forgot <run_id>
	// entirely, we want the usage message, not a confusing "could not
	// resolve repo" error that happens to surface because they're
	// running from a non-checkout dir. Usage errors take precedence
	// over resolution errors.
	runIDStr := firstPositional(args)
	if runIDStr == "" {
		exitErr("usage: triagefactory exec gh actions download-logs <run_id> [--repo owner/repo]")
	}
	runID, err := strconv.ParseInt(runIDStr, 10, 64)
	if err != nil || runID <= 0 {
		exitErr(fmt.Sprintf("invalid run_id %q: expected a positive integer", runIDStr))
	}

	owner, repo, err := resolveRepo(args)
	if err != nil {
		exitErr(err.Error())
	}
	client := newHostAPI(host, owner, repo)

	// Destination: <cwd>/_scratch/ci-logs/<run_id>/. Resolving to absolute
	// so the success output gives the agent a path it can use directly
	// without needing to reason about cwd. safeDestDirForRun also walks
	// each path component to reject pre-existing symlinks — otherwise a
	// symlinked `_scratch` (accidental or malicious) would let our
	// RemoveAll / MkdirAll / zip extraction escape the working directory.
	cwd, err := os.Getwd()
	if err != nil {
		exitErr(fmt.Sprintf("resolve cwd: %v", err))
	}
	destDir, err := safeDestDirForRun(cwd, runID)
	if err != nil {
		exitErr(err.Error())
	}

	// Always fetch the job list up front. Cheap (single JSON GET) and the
	// response shape is consistent in either path — agents don't need to
	// branch on whether Jobs[] is present.
	jobs, err := fetchJobsForRun(client, owner, repo, runID)
	if err != nil {
		exitErr(err.Error())
	}
	runStatus := deriveRunStatus(jobs)

	// Try the run-level archive first. This is the happy path for any
	// completed run and produces the same response shape we've always
	// emitted (Entries listing + extracted files on disk).
	bytesDownloaded, err := downloadAndExtractLogs(client, owner, repo, runID, destDir)
	if err == nil {
		entries, listErr := topLevelEntries(destDir)
		if listErr != nil {
			exitErr(fmt.Sprintf("list extracted entries: %v", listErr))
		}
		if entries == nil {
			entries = []string{} // JSON null would be misleading; empty archive is an empty list
		}
		printJSON(downloadLogsResult{
			RunID:           runID,
			Owner:           owner,
			Repo:            repo,
			DestDir:         destDir,
			BytesDownloaded: bytesDownloaded,
			RunStatus:       runStatus,
			FallbackUsed:    false,
			Entries:         entries,
			Jobs:            jobs,
		})
		return
	}

	// Fall back to per-job download iff the archive 404'd. Every other
	// error (auth failure, network blip, size cap exceeded, etc.) is a
	// real problem and should propagate so the agent sees it.
	var he *github.HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
		exitErr(err.Error())
	}

	bytesDownloaded, jobs, err = downloadPerJobLogsToDir(client, owner, repo, destDir, jobs)
	if err != nil {
		exitErr(err.Error())
	}

	printJSON(downloadLogsResult{
		RunID:           runID,
		Owner:           owner,
		Repo:            repo,
		DestDir:         destDir,
		BytesDownloaded: bytesDownloaded,
		RunStatus:       runStatus,
		FallbackUsed:    true,
		Entries:         nil, // omitempty — archive wasn't extracted on this path
		Jobs:            jobs,
	})
}

// fetchJobsForRun queries GET /repos/{o}/{r}/actions/runs/{id}/jobs and
// flattens the response to the subset of fields the agent + fallback path
// need. Jobs come back in the order GitHub returns them (roughly start
// order, which matches what the user sees in the Actions UI).
//
// Pagination: capped at 100 (one page). A workflow with >100 jobs is
// already extreme; if we ever see truncation we log a warning to stderr
// rather than silently dropping data, but real pagination is deferred
// until something actually hits the cap.
func fetchJobsForRun(client ghAPI, owner, repo string, runID int64) ([]jobInfo, error) {
	apiPath := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?per_page=100", owner, repo, runID)
	data, err := client.Get(apiPath)
	if err != nil {
		return nil, fmt.Errorf("list jobs for run %d: %w", runID, err)
	}
	var resp struct {
		TotalCount int `json:"total_count"`
		Jobs       []struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Status      string `json:"status"`
			Conclusion  string `json:"conclusion"`
			StartedAt   string `json:"started_at"`
			CompletedAt string `json:"completed_at"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse jobs response: %w", err)
	}
	if resp.TotalCount > len(resp.Jobs) {
		fmt.Fprintf(os.Stderr,
			"warning: run %d has %d jobs but only the first %d were returned (pagination not implemented)\n",
			runID, resp.TotalCount, len(resp.Jobs))
	}
	out := make([]jobInfo, 0, len(resp.Jobs))
	for _, j := range resp.Jobs {
		out = append(out, jobInfo{
			ID:          j.ID,
			Name:        j.Name,
			Status:      j.Status,
			Conclusion:  j.Conclusion,
			StartedAt:   j.StartedAt,
			CompletedAt: j.CompletedAt,
		})
	}
	return out, nil
}

// deriveRunStatus collapses the per-job statuses into a single run-level
// status string. Avoids a second API call to /actions/runs/{id} just to
// read a field we can compute. Mapping:
//   - no jobs visible → "queued" (run hasn't really started yet)
//   - all jobs "queued" → "queued"
//   - all jobs "completed" → "completed"
//   - anything else (mix, or any "in_progress") → "in_progress"
//
// Precise enough for the agent's purposes; it cares about completed vs
// not-completed rather than the GitHub-internal taxonomy.
func deriveRunStatus(jobs []jobInfo) string {
	if len(jobs) == 0 {
		return "queued"
	}
	allCompleted := true
	allQueued := true
	for _, j := range jobs {
		if j.Status != "completed" {
			allCompleted = false
		}
		if j.Status != "queued" {
			allQueued = false
		}
	}
	switch {
	case allCompleted:
		return "completed"
	case allQueued:
		return "queued"
	default:
		return "in_progress"
	}
}

// jobNameSafePattern matches characters that are not safe in filesystem
// paths across the platforms we support. Spaces, slashes, parens, colons,
// and the rest get replaced with underscore so a job named
// "Lint / typecheck (node 20)" maps to "Lint___typecheck__node_20_".
var jobNameSafePattern = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// sanitizeJobNameForFile maps a GitHub job name (which can contain spaces,
// slashes, parens, brackets, etc.) to a filesystem-safe filename component.
// Empty result (job name was all special chars or empty) collapses to
// "job"; the caller appends the job ID anyway, so disambiguation is
// guaranteed regardless of how aggressively the name was sanitized.
func sanitizeJobNameForFile(name string) string {
	cleaned := jobNameSafePattern.ReplaceAllString(name, "_")
	if cleaned == "" {
		return "job"
	}
	return cleaned
}

// downloadPerJobLogsToDir writes one log file per completed job into
// destDir, returning the total bytes written and an updated jobs slice
// with LogPath populated for any job whose log was fetched.
//
// Filenames are "<sanitized_name>-<id>.log". Always including the ID
// guarantees uniqueness without disambiguation logic, even when several
// matrix legs sanitize to the same name. The agent reads LogPath out of
// the JSON response rather than recomputing the filename.
//
// Cleanup semantics mirror downloadAndExtractLogs: destDir is clobbered
// up front, and on any failure (mid-download or partial extract) it's
// removed so a retry sees a clean slate. Queued and in_progress jobs are
// skipped (their LogPath stays empty as a stub).
func downloadPerJobLogsToDir(client ghAPI, owner, repo, destDir string, jobs []jobInfo) (int64, []jobInfo, error) {
	if err := os.RemoveAll(destDir); err != nil {
		return 0, nil, fmt.Errorf("clear stale destination directory: %w", err)
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return 0, nil, fmt.Errorf("create destination directory: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(destDir)
		}
	}()

	out := make([]jobInfo, len(jobs))
	copy(out, jobs)

	var totalBytes int64
	ctx := context.Background()
	for i, j := range out {
		if j.Status != "completed" {
			// Stub for queued / in_progress jobs. Empty LogPath signals
			// "no log available yet" — the agent can re-run download-logs
			// later once the run completes for the full archive.
			continue
		}
		filename := fmt.Sprintf("%s-%d.log", sanitizeJobNameForFile(j.Name), j.ID)
		path := filepath.Join(destDir, filename)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return totalBytes, out, fmt.Errorf("create log file %q: %w", path, err)
		}
		apiPath := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs", owner, repo, j.ID)
		n, downloadErr := client.DownloadArtifact(ctx, apiPath, f, maxArchiveBytes)
		closeErr := f.Close()
		if downloadErr != nil {
			return totalBytes, out, fmt.Errorf("download job %d (%s) logs: %w", j.ID, j.Name, downloadErr)
		}
		if closeErr != nil {
			return totalBytes, out, fmt.Errorf("close log file %q: %w", path, closeErr)
		}
		out[i].LogPath = path
		totalBytes += n
	}

	success = true
	return totalBytes, out, nil
}

// downloadAndExtractLogs owns every on-disk resource the command needs: the
// destination directory and the temp zip file. It's split out from
// actionsDownloadLogs specifically so that defers actually run on failure —
// the outer function uses exitErr (→ os.Exit), which skips defers, so any
// cleanup logic has to live under a function that can return an error.
//
// Cleanup semantics:
//   - Temp zip: always removed on return, success or failure.
//   - destDir:  kept on success, removed on any failure, so a partial
//     extraction never leaves stale files around that could look like a
//     valid state to the next retry.
//
// Returns the number of bytes downloaded on success.
func downloadAndExtractLogs(client ghAPI, owner, repo string, runID int64, destDir string) (int64, error) {
	// Clobber any previous extraction for the same run_id. The command owns
	// this directory completely (<cwd>/_scratch/ci-logs/<run_id>), so a
	// re-run should produce a clean state — otherwise stale entries from an
	// older extraction (jobs that no longer exist, renamed matrix legs)
	// would sit alongside the current run's files and mislead the agent
	// reading them back.
	if err := os.RemoveAll(destDir); err != nil {
		return 0, fmt.Errorf("clear stale destination directory: %w", err)
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return 0, fmt.Errorf("create destination directory: %w", err)
	}
	// Track success so we can roll back destDir on any failure path. The
	// closure captures this by reference — setting success=true at the end
	// of the happy path is what disarms the cleanup.
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(destDir)
		}
	}()

	// Stream the archive to a temp file. We can't extract from a stream
	// because archive/zip needs ReaderAt for the central directory at the
	// end of the file.
	tmpFile, err := os.CreateTemp("", "triagefactory-ci-logs-*.zip")
	if err != nil {
		return 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close() // idempotent-safe; second close just returns ErrClosed
		_ = os.Remove(tmpPath)
	}()

	// No explicit ctx deadline: DownloadArtifact enforces its own
	// per-request timeout via the shallow-copied http.Client it runs
	// against. Setting another WithTimeout here would just duplicate
	// the same magic number (and let the two drift in a future edit).
	// Using context.Background keeps the door open for signal-driven
	// cancellation without committing to a specific wall-clock ceiling.
	ctx := context.Background()

	apiPath := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/logs", owner, repo, runID)
	bytesDownloaded, err := client.DownloadArtifact(ctx, apiPath, tmpFile, maxArchiveBytes)
	if err != nil {
		return 0, fmt.Errorf("download logs: %w", err)
	}
	// Close before reopening for read — archive/zip needs exclusive read
	// access and goes through the filesystem path, not our handle. The
	// deferred Close above will no-op on this already-closed file.
	if err := tmpFile.Close(); err != nil {
		return 0, fmt.Errorf("close temp file: %w", err)
	}

	if err := extractZip(tmpPath, destDir, maxPerFileBytes, maxTotalUncompressedBytes); err != nil {
		return 0, fmt.Errorf("extract archive: %w", err)
	}

	success = true
	return bytesDownloaded, nil
}

// safeDestDirForRun resolves the <cwd>/_scratch/ci-logs/<run_id> destination
// path for a given workflow run via the shared, symlink-safe scratch
// resolver. See safeScratchSubdir for the safety contract.
func safeDestDirForRun(cwd string, runID int64) (string, error) {
	return safeScratchSubdir(cwd, "_scratch", "ci-logs", strconv.FormatInt(runID, 10))
}

// extractZip safely extracts zipPath into destDir. Rejects any entry whose
// resolved destination escapes destDir (path-traversal guard) and any entry
// whose uncompressed size exceeds maxFileBytes (zip-bomb guard). The cap is
// parameterized so tests can exercise the guard without writing 100 MB of
// real content; production callers pass maxPerFileBytes.
//
// maxFileBytes must be positive. There is no "unlimited" mode — callers
// that want effectively no cap should pass math.MaxInt64 explicitly rather
// than relying on a sentinel. Non-positive values are a programmer error
// and return immediately, because silently treating them as "no cap" once
// led to a subtle bug where the runtime io.LimitReader path turned into
// "reject everything" for negative inputs.
//
// destDir must already exist. Returns the first error encountered, which
// aborts the rest of the extraction — partial extraction left on disk is
// expected and fine, the caller (downloadLogs) fails the whole command.
func extractZip(zipPath, destDir string, maxFileBytes, maxTotalBytes int64) error {
	if maxFileBytes <= 0 {
		return fmt.Errorf("extractZip: maxFileBytes must be positive, got %d", maxFileBytes)
	}
	if maxTotalBytes <= 0 {
		return fmt.Errorf("extractZip: maxTotalBytes must be positive, got %d", maxTotalBytes)
	}

	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer reader.Close()

	// Canonicalize destDir once so prefix checks are against a stable value.
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve destination abs path: %w", err)
	}
	// Trailing separator so "/tmp/foo" doesn't prefix-match "/tmp/foobar".
	absDestWithSep := absDest + string(os.PathSeparator)

	// Track cumulative uncompressed bytes across all entries so a
	// highly-compressible archive with many small entries — each under
	// maxFileBytes but collectively expanding to multiple gigabytes —
	// can't slip past the defenses. Checked after each entry because
	// stopping mid-copy would require threading the remaining budget
	// through extractZipEntry, and the overshoot is bounded by
	// maxFileBytes per entry (negligible compared to maxTotalBytes).
	var totalWritten int64
	for _, entry := range reader.File {
		n, err := extractZipEntry(entry, absDest, absDestWithSep, maxFileBytes)
		if err != nil {
			return err
		}
		totalWritten += n
		if totalWritten > maxTotalBytes {
			return fmt.Errorf("archive total uncompressed size exceeds cap of %d bytes (written %d so far — likely zip bomb)", maxTotalBytes, totalWritten)
		}
	}
	return nil
}

// extractZipEntry writes a single zip entry to disk with all safety checks.
// Split out from extractZip so the defers on the entry reader and the
// output file fire per-entry rather than accumulating across the whole
// archive.
//
// Returns the number of uncompressed bytes written to disk — zero for
// directory entries, at most maxFileBytes for files — so the caller can
// accumulate against a total-uncompressed-bytes cap.
func extractZipEntry(entry *zip.File, absDest, absDestWithSep string, maxFileBytes int64) (int64, error) {
	// Path-traversal rejection. filepath.Join + Clean collapses any
	// "../../" segments in the entry name; we then verify the result still
	// lives under destDir. This is the guard that makes arbitrary zip
	// files safe to extract.
	targetPath := filepath.Join(absDest, entry.Name)
	if targetPath != absDest && !strings.HasPrefix(targetPath, absDestWithSep) {
		return 0, fmt.Errorf("unsafe archive entry %q: resolves outside destination", entry.Name)
	}

	if entry.FileInfo().IsDir() {
		return 0, os.MkdirAll(targetPath, 0755)
	}

	// Header size pre-check. This fires on hand-crafted zips with a lying
	// UncompressedSize64 (adversarial case) — standard zip writers overwrite
	// this field with the real size, so for a well-formed zip the header
	// and content agree and this path is effectively a fast rejection.
	// extractZip guarantees maxFileBytes > 0, so the comparison is always
	// meaningful here.
	if int64(entry.UncompressedSize64) > maxFileBytes {
		return 0, fmt.Errorf("archive entry %q exceeds per-file size cap: %d bytes", entry.Name, entry.UncompressedSize64)
	}

	// Ensure parent directory exists — zip archives don't always include
	// explicit directory entries.
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return 0, fmt.Errorf("mkdir parent for %q: %w", entry.Name, err)
	}

	src, err := entry.Open()
	if err != nil {
		return 0, fmt.Errorf("open entry %q: %w", entry.Name, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return 0, fmt.Errorf("create output file %q: %w", entry.Name, err)
	}
	defer dst.Close()

	// Runtime cap via io.LimitReader — this is the guard that actually
	// fires for real oversized content. +1 so we can detect overflow past
	// the cap rather than silently truncating.
	limited := io.LimitReader(src, maxFileBytes+1)
	n, err := io.Copy(dst, limited)
	if err != nil {
		return n, fmt.Errorf("write entry %q: %w", entry.Name, err)
	}
	if n > maxFileBytes {
		return n, fmt.Errorf("archive entry %q exceeded per-file size cap during extraction (content larger than %d bytes)", entry.Name, maxFileBytes)
	}
	return n, nil
}

// topLevelEntries returns the names of entries directly inside dir, sorted
// alphabetically, with a trailing "/" appended to directories so the output
// distinguishes them from files. Used to print a quick orientation listing
// after extraction.
func topLevelEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
