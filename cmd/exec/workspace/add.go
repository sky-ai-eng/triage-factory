package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// addDeps abstracts the side-effecting collaborators of materializeWorkspace
// so tests can stub the worktree mutations without invoking real git or
// touching the filesystem. Production wiring uses defaultAddDeps which
// delegates to the worktree package.
//
// Only the worktree-mutating surface is injectable. The agenthost calls
// go through the supplied Client directly because tests construct a
// LocalClient over an in-memory SQLite db.Stores; mocking the IPC
// layer would test less of the actual code than exercising LocalClient
// end-to-end.
type addDeps struct {
	createWorktree func(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot string) (string, error)
	removeWorktree func(path, runID string) error
	statPath       func(path string) (os.FileInfo, error)
	now            func() time.Time
}

func defaultAddDeps() addDeps {
	return addDeps{
		createWorktree: worktree.CreateForBranchInRoot,
		removeWorktree: worktree.RemoveAt,
		statPath:       os.Stat,
		now:            time.Now,
	}
}

// staleReservationAge is the grace window during which a row whose
// on-disk path doesn't exist yet is treated as an in-flight winner
// rather than a stale reservation. Sized to outlast the slowest
// legitimate create — a fresh bare clone of a multi-GB monorepo can
// take a couple of minutes; 5 minutes gives that ~3x headroom while
// still un-jamming runs whose `workspace add` was killed mid-create
// (process kill, SIGTERM at server stop, machine restart) before the
// row was either updated or released.
//
// Concurrency context: this complements the PK-based reservation. A
// loser arriving during the genuine in-flight window (created_at <
// staleReservationAge ago) returns the winner's path and lets the
// agent's `cd` succeed once the create lands. A loser arriving long
// after the row was abandoned reclaims the slot.
const staleReservationAge = 5 * time.Minute

// validation errors returned by materializeWorkspace. Callers translate
// these into stderr messages + non-zero exit; tests assert on identity.
var (
	errMissingRunID        = errors.New("workspace add: TRIAGE_FACTORY_RUN_ID not set; this command must be invoked by the delegated agent")
	errInvalidOwnerRepo    = errors.New("workspace add: invalid owner/repo")
	errRunNotFound         = errors.New("workspace add: run not found")
	errTaskNotFound        = errors.New("workspace add: task not found")
	errNotJiraRun          = errors.New("workspace add: only supported for Jira runs; GitHub PR runs have an eagerly-materialized worktree")
	errRepoNotConfigured   = errors.New("workspace add: repo is not configured in Triage Factory; add it on the Settings page first")
	errRepoNotTracked      = errors.New("workspace add: repo is not tracked by this team; add it to the team on the Settings page first")
	errRepoMissingCloneURL = errors.New("workspace add: repo has no clone URL on its profile; try re-profiling from the Settings page")
	errInvalidEntityKey    = errors.New("workspace add: task entity key contains characters disallowed for git refs")
)

// entityKeyPattern restricts task.EntitySourceID to a conservative refname
// alphabet before it's interpolated into a feature branch and passed to git.
// The spec calls for `^[a-z0-9][a-z0-9._/-]{0,128}$`; uppercase is permitted
// here because Jira issue keys are upper-cased by convention (e.g. SKY-220)
// and are already the de-facto branch-name source across the codebase.
//
// Blocks: leading dash (interpreted as a git CLI flag), whitespace, shell
// metacharacters (`;`, `|`, backticks, `$`), refname-illegal characters
// (`:`, `?`, `*`, `[`, `~`, `^`, `\`, control bytes). The `..` substring is
// rejected separately — it's lexically allowed by the char class but git
// refnames forbid it (and it enables path traversal in the worktree dir).
var entityKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,128}$`)

func validateEntityKey(key string) error {
	if !entityKeyPattern.MatchString(key) || strings.Contains(key, "..") {
		return fmt.Errorf("%w: %q", errInvalidEntityKey, key)
	}
	return nil
}

// materializeWorkspace is the orchestration body of `workspace add`,
// extracted from runAdd so it returns errors instead of os.Exit-ing.
// Returns the absolute worktree path the agent should cd into.
//
// Concurrency: the cross-process serialization point is the
// run_worktrees PK insert (`InsertRunWorktree`'s INSERT OR IGNORE),
// hidden behind host.InsertRunWorktree. Two concurrent invocations
// both passing the idempotency precheck race at insert time; the
// loser sees inserted=false and returns the winner's path without
// touching git. Reserving BEFORE the create is load-bearing — if we
// created first, both racing processes would hit `git worktree add`
// against the same deterministic target dir and the second would
// fail on "directory exists" before ever reaching the PK conflict.
//
// Order:
//  1. Run + task validation, Jira-vs-GitHub gate.
//  2. Idempotent re-add check: if a row exists AND its path is a
//     live directory, return it. If the row exists but the path is
//     missing/not-a-dir (e.g. wiped by startup orphan sweep), drop
//     the stale row so the reservation step below can re-reserve.
//  3. Repo profile lookup (clone URL required).
//  4. Reserve the run_worktrees row with the deterministic path
//     {runRoot}/{owner}/{repo}. PK conflict picks the winner.
//  5. Loser path: return winner's path immediately.
//  6. Winner path: create the worktree on disk. On failure, release
//     the reservation so the next attempt can retry.
func materializeWorkspace(host agenthost.Client, ownerRepoArg string, deps addDeps) (string, error) {
	owner, repo, ok := splitOwnerRepo(ownerRepoArg)
	if !ok {
		return "", fmt.Errorf("%w: %q", errInvalidOwnerRepo, ownerRepoArg)
	}
	repoID := owner + "/" + repo

	ctx := context.Background()
	info, err := host.LookupRun(ctx)
	if err != nil {
		// runID is empty at this point — LookupRun is what would have
		// produced it. The NotFound branch of translateLookupErr is
		// unreachable in the LocalClient path (LookupRun only errors
		// when the env-derived runID was empty at construction), so
		// passing "" is correct: only the ErrRunIdentityMissing branch
		// fires here, and it ignores the runID argument.
		return "", translateLookupErr("workspace add", "", err)
	}

	run, err := host.GetAgentRun(ctx)
	if err != nil {
		return "", fmt.Errorf("workspace add: load run: %w", err)
	}
	if run == nil {
		return "", fmt.Errorf("%w: %s", errRunNotFound, info.RunID)
	}
	task, err := host.GetTask(ctx, run.TaskID)
	if err != nil {
		return "", fmt.Errorf("workspace add: load task: %w", err)
	}
	if task == nil {
		return "", fmt.Errorf("%w: %s", errTaskNotFound, run.TaskID)
	}
	if task.EntitySource != "jira" {
		return "", fmt.Errorf("%w (run task source is %q)", errNotJiraRun, task.EntitySource)
	}
	if err := validateEntityKey(task.EntitySourceID); err != nil {
		return "", err
	}

	// Idempotent re-add. If a row exists for this (run, repo), prefer
	// its path — the row is the authoritative reservation.
	//
	// Two scenarios where the on-disk path may NOT exist when the row
	// does:
	//
	//  - In-flight winner: another `workspace add` invocation reserved
	//    the row moments ago and its createWorktree is still running.
	//    Dropping the row here would defeat the PK serialization and
	//    let both processes create the same target dir. We tolerate
	//    this by returning the reserved path; the agent's `cd` succeeds
	//    once the winner's create lands.
	//
	//  - Killed mid-create: the original creator was killed (SIGTERM,
	//    process supervisor reaping, machine restart) after
	//    InsertRunWorktree returned but before CreateForBranchInRoot
	//    completed. The row has no live owner; subsequent retries
	//    looping forever on a never-realized path is the wrong answer.
	//
	// The two are distinguishable by row age: in-flight creates finish
	// inside the `staleReservationAge` window; killed-mid-create rows
	// outlive it. Pre-staleness, trust the row. Past staleness with
	// the path still missing, drop the row and re-reserve.
	existing, err := host.GetRunWorktreeByRepo(ctx, repoID)
	if err != nil {
		return "", fmt.Errorf("workspace add: lookup existing worktree: %w", err)
	}
	if existing != nil {
		_, statErr := deps.statPath(existing.Path)
		switch {
		case statErr == nil:
			// Path exists on disk — live worktree, return it.
			return existing.Path, nil
		case errors.Is(statErr, os.ErrNotExist):
			age := deps.now().Sub(existing.CreatedAt)
			if age < staleReservationAge {
				// In-flight winner: another invocation reserved the row
				// and is currently creating the worktree. Return its
				// path; agent's cd succeeds once the create lands.
				return existing.Path, nil
			}
			// Stale: reservation outlived its creator without a
			// completed worktree. Drop and fall through to re-reserve.
			workspaceLog.Warn("dropping stale reservation; path missing and row age exceeds threshold", "run_id", info.RunID, "repo", repoID, "path", existing.Path, "age", age, "threshold", staleReservationAge)
			if delErr := host.DeleteRunWorktreeByRepo(ctx, repoID); delErr != nil {
				return "", fmt.Errorf("workspace add: delete stale reservation: %w", delErr)
			}
		default:
			// Stat error other than NotExist (permissions, IO error).
			// Surface rather than guess at semantics.
			return "", fmt.Errorf("workspace add: stat reserved worktree path %q: %w", existing.Path, statErr)
		}
	}

	profile, err := host.GetRepo(ctx, repoID)
	if err != nil {
		return "", fmt.Errorf("workspace add: load repo profile: %w", err)
	}
	if profile == nil {
		return "", fmt.Errorf("%w: %s", errRepoNotConfigured, repoID)
	}
	// Team-tracking gate: a run may only materialize a repo its team tracks, so
	// `workspace add` and the git proxy's push gate agree — otherwise the agent
	// could clone a repo the proxy will then refuse to push to. Same
	// TracksRepoSystem predicate the proxy's Authorize uses, keyed by the run's
	// team. (Org-configured ≠ team-tracked: a repo can be configured org-wide
	// yet not attached to this run's team.)
	if tracks, terr := host.TeamTracksRepo(ctx, owner, repo); terr != nil {
		return "", fmt.Errorf("workspace add: check team tracking: %w", terr)
	} else if !tracks {
		return "", fmt.Errorf("%w: %s", errRepoNotTracked, repoID)
	}
	if profile.CloneURL == "" {
		return "", fmt.Errorf("%w: %s", errRepoMissingCloneURL, repoID)
	}

	baseBranch := profile.BaseBranch
	if baseBranch == "" {
		baseBranch = profile.DefaultBranch
	}
	featureBranch := "feature/" + task.EntitySourceID
	runRoot := worktree.RunRoot(info.RunID)
	// Path is deterministic from CreateForBranchInRoot's contract:
	// filepath.Join(runRoot, owner, repo). Compute it here so we can
	// reserve the row BEFORE the create runs.
	wtPath := filepath.Join(runRoot, profile.Owner, profile.Repo)

	// Reserve. Two concurrent processes that both reach this point
	// race at the PK; the loser short-circuits before touching git.
	row := domain.RunWorktree{
		RunID:         info.RunID,
		RepoID:        repoID,
		Path:          wtPath,
		FeatureBranch: featureBranch,
	}
	inserted, winningPath, err := host.InsertRunWorktree(ctx, row)
	if err != nil {
		return "", fmt.Errorf("workspace add: reserve worktree row: %w", err)
	}
	if !inserted {
		// Lost the reservation race. Return the winner's path. There's
		// a tiny window where the winner's createWorktree is still in
		// flight and the path doesn't exist on disk yet; the agent's
		// subsequent `cd` would fail loudly if the create eventually
		// errors (winner deletes the row, next agent retry re-reserves).
		// Polling here would add complexity for negligible gain.
		return winningPath, nil
	}

	// We won. Create the worktree.
	gotPath, err := deps.createWorktree(
		ctx,
		profile.Owner, profile.Repo,
		profile.CloneURL,
		baseBranch, featureBranch,
		info.RunID, runRoot,
	)
	if err != nil {
		// Release the reservation so the next attempt can retry.
		// Delete failures are logged but don't shadow the create error
		// the caller actually needs.
		if delErr := host.DeleteRunWorktreeByRepo(ctx, repoID); delErr != nil {
			workspaceLog.Warn("release reservation after create failure failed", "error", delErr)
		}
		return "", fmt.Errorf("workspace add: create worktree: %w", err)
	}
	if gotPath != wtPath {
		// CreateForBranchInRoot's contract is to land at
		// filepath.Join(runRoot, owner, repo); a divergence means the
		// worktree library changed and our reservation path no longer
		// matches reality. Surface loudly rather than silently storing
		// the wrong path.
		workspaceLog.Warn("created path diverges from reserved; investigate", "got_path", gotPath, "reserved_path", wtPath, "run_id", info.RunID, "repo", repoID)
	}

	return wtPath, nil
}

// runAdd is the CLI entrypoint: argv → materializeWorkspace → stdout/stderr.
// All errors translate into exitErr so the caller process gets a non-zero
// exit and the agent sees a clear message on stderr. Successful resolution
// (first add or idempotent re-add) prints the absolute worktree path on
// stdout for `cd "$(... workspace add owner/repo)"`.
func runAdd(host agenthost.Client, args []string) {
	if len(args) == 0 {
		exitErr("workspace add: missing argument; expected owner/repo")
	}

	path, err := materializeWorkspace(
		host,
		strings.TrimSpace(args[0]),
		defaultAddDeps(),
	)
	if err != nil {
		exitErr(err.Error())
	}
	fmt.Println(path)
}

// splitOwnerRepo splits "owner/repo" — exactly one slash, both halves
// non-empty. Inputs with extra slashes (`owner/repo/extra`) reject at
// parse time rather than surfacing as a misleading "repo is not
// configured" error after the lookup synthesizes a repo ID that no
// configured repo could ever match. Matches the rest of the
// codebase's slug convention.
func splitOwnerRepo(s string) (owner, repo string, ok bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
