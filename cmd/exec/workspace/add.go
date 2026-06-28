package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
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
	// checkout materializes a detached worktree at the fresh tip of an
	// existing branch (ref==""→the repo's default branch). The default and
	// --ref paths.
	checkout func(ctx context.Context, owner, repo, cloneURL, ref, runID, runRoot string) (string, error)
	// checkoutPR materializes a worktree on a PR head (fork-aware, via the
	// upstream's refs/pull/<n>/head). The --pr path.
	checkoutPR func(ctx context.Context, owner, repo, upstreamCloneURL, headCloneURL, headBranch string, prNumber int, runID, runRoot string) (string, error)
	// fetchPR resolves a PR's head ref + clone URLs (host-side) for the --pr
	// path. Nil in defaultAddDeps; materializeWorkspace wires it to
	// host.GithubGetPR when unset, so production routes through the agenthost
	// seam while tests stub a canned PRView without standing up GitHub.
	fetchPR        func(ctx context.Context, owner, repo string, number int) (*ghclient.PRView, error)
	removeWorktree func(path, runID string) error
	statPath       func(path string) (os.FileInfo, error)
	now            func() time.Time
}

func defaultAddDeps() addDeps {
	return addDeps{
		checkout:       worktree.CreateForCheckoutInRoot,
		checkoutPR:     worktree.CreateForPRInRoot,
		removeWorktree: worktree.RemoveAt,
		statPath:       os.Stat,
		now:            time.Now,
	}
}

// checkoutSpec is the parsed shape of what `workspace add` should materialize.
// The default (zero value) checks out the repo's default branch; --ref names an
// existing branch; --pr names a pull request whose head is checked out. ref and
// pr are mutually exclusive (enforced at parse time).
type checkoutSpec struct {
	ref string // --ref <branch>; "" = the repo's default branch
	pr  int    // --pr <N>; 0 = not a PR checkout
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
	errRepoNotConfigured   = errors.New("workspace add: repo is not configured in Triage Factory; add it on the Settings page first")
	errRepoNotTracked      = errors.New("workspace add: repo is not tracked by this team; add it to the team on the Settings page first")
	errRepoMissingCloneURL = errors.New("workspace add: repo has no clone URL on its profile; try re-profiling from the Settings page")
	errInvalidRef          = errors.New("workspace add: --ref contains characters disallowed for git refs")
	errRefAndPR            = errors.New("workspace add: --ref and --pr are mutually exclusive")
	errInvalidPR           = errors.New("workspace add: --pr requires a positive integer PR number")
	errMissingOwnerRepo    = errors.New("workspace add: missing argument; expected owner/repo")
)

// gitRefPattern restricts a user-supplied --ref to a conservative refname
// alphabet before it's interpolated into a fetch refspec and passed to git.
// Uppercase is permitted (branch names routinely carry ticket keys like
// SKY-220).
//
// Blocks: leading dash (interpreted as a git CLI flag), whitespace, shell
// metacharacters (`;`, `|`, backticks, `$`), refname-illegal characters
// (`:`, `?`, `*`, `[`, `~`, `^`, `\`, control bytes). The `..` substring is
// rejected separately — it's lexically allowed by the char class but git
// refnames forbid it (and it enables path traversal in the worktree dir).
var gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,128}$`)

func validateGitRef(ref string) error {
	if !gitRefPattern.MatchString(ref) || strings.Contains(ref, "..") {
		return fmt.Errorf("%w: %q", errInvalidRef, ref)
	}
	return nil
}

// parseAddArgs splits `workspace add` argv into the owner/repo positional and
// the checkout spec. Exactly one positional owner/repo is required; --ref and
// --pr are optional and mutually exclusive. Hand-rolled (rather than flag.Parse)
// because the positional comes first and Go's flag package stops at the first
// non-flag argument.
func parseAddArgs(args []string) (ownerRepo string, spec checkoutSpec, err error) {
	var positional []string
	var prStr string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--ref":
			i++
			if i >= len(args) {
				return "", spec, fmt.Errorf("workspace add: --ref requires a branch name")
			}
			spec.ref = args[i]
		case strings.HasPrefix(a, "--ref="):
			spec.ref = strings.TrimPrefix(a, "--ref=")
		case a == "--pr":
			i++
			if i >= len(args) {
				return "", spec, fmt.Errorf("workspace add: --pr requires a PR number")
			}
			prStr = args[i]
		case strings.HasPrefix(a, "--pr="):
			prStr = strings.TrimPrefix(a, "--pr=")
		case strings.HasPrefix(a, "-"):
			return "", spec, fmt.Errorf("workspace add: unknown flag %q", a)
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return "", spec, errMissingOwnerRepo
	}
	if len(positional) > 1 {
		return "", spec, fmt.Errorf("%w: %q", errInvalidOwnerRepo, strings.Join(positional, " "))
	}
	ownerRepo = positional[0]
	if spec.ref != "" && prStr != "" {
		return "", spec, errRefAndPR
	}
	if prStr != "" {
		n, perr := strconv.Atoi(prStr)
		if perr != nil || n <= 0 {
			return "", spec, fmt.Errorf("%w: %q", errInvalidPR, prStr)
		}
		spec.pr = n
	}
	return ownerRepo, spec, nil
}

// materializeWorkspace is the orchestration body of `workspace add`,
// extracted from runAdd so it returns errors instead of os.Exit-ing.
// Returns the absolute worktree path the agent should cd into.
//
// Generalized (TFAC-498): works for ANY run — Jira, GitHub, or taskless — and
// no longer prescribes a feature branch. By default it checks out the repo's
// default branch (detached); --ref checks out a named branch; --pr checks out a
// PR head (fork-aware). The agent then drives git itself (`git checkout -b ...`)
// and the push gate authorizes whatever branch the worktree lands on.
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
//  1. Run validation (the run must exist; no Jira/task gate).
//  2. Idempotent re-add check: if a row exists AND its path is a
//     live directory, return it. If the row exists but the path is
//     missing/not-a-dir (e.g. wiped by startup orphan sweep), drop
//     the stale row so the reservation step below can re-reserve.
//  3. Repo profile lookup (clone URL required) + team-tracking gate.
//  4. Reserve the run_worktrees row with the deterministic path
//     {runRoot}/{owner}/{repo}. PK conflict picks the winner.
//  5. Loser path: return winner's path immediately.
//  6. Winner path: materialize the checkout on disk. On failure, release
//     the reservation so the next attempt can retry.
func materializeWorkspace(host agenthost.Client, ownerRepoArg string, spec checkoutSpec, deps addDeps) (string, error) {
	owner, repo, ok := splitOwnerRepo(ownerRepoArg)
	if !ok {
		return "", fmt.Errorf("%w: %q", errInvalidOwnerRepo, ownerRepoArg)
	}
	repoID := owner + "/" + repo

	if spec.ref != "" {
		if err := validateGitRef(spec.ref); err != nil {
			return "", err
		}
	}

	ctx := context.Background()
	info, err := host.LookupRun(ctx)
	if err != nil {
		// runID is empty at this point — LookupRun is what would have
		// produced it. Only the ErrRunIdentityMissing branch fires here, and
		// it ignores the runID argument.
		return "", translateLookupErr("workspace add", "", err)
	}

	// The run must exist (the reservation FKs it; a clear error beats an
	// opaque FK failure later). No task is loaded — `workspace add` is now
	// run-agnostic and serves taskless runs too.
	run, err := host.GetAgentRun(ctx)
	if err != nil {
		return "", fmt.Errorf("workspace add: load run: %w", err)
	}
	if run == nil {
		return "", fmt.Errorf("%w: %s", errRunNotFound, info.RunID)
	}

	// Idempotent re-add. If a row exists for this (run, repo), prefer
	// its path — the row is the authoritative reservation.
	//
	// Two scenarios where the on-disk path may NOT exist when the row
	// does:
	//
	//  - In-flight winner: another `workspace add` invocation reserved
	//    the row moments ago and its create is still running. Dropping the
	//    row here would defeat the PK serialization and let both processes
	//    create the same target dir. We tolerate this by returning the
	//    reserved path; the agent's `cd` succeeds once the winner's create
	//    lands.
	//
	//  - Killed mid-create: the original creator was killed (SIGTERM,
	//    process supervisor reaping, machine restart) after
	//    InsertRunWorktree returned but before the create completed. The row
	//    has no live owner; subsequent retries looping forever on a
	//    never-realized path is the wrong answer.
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

	runRoot := worktree.RunRoot(info.RunID)
	// Path is deterministic from the InRoot create contract:
	// filepath.Join(runRoot, owner, repo). Compute it here so we can
	// reserve the row BEFORE the create runs.
	wtPath := filepath.Join(runRoot, profile.Owner, profile.Repo)

	// Reserve. Two concurrent processes that both reach this point
	// race at the PK; the loser short-circuits before touching git.
	// FeatureBranch is now informational only (the push gate reads the
	// worktree's live current branch, not this column) — store the checkout
	// intent so `workspace list` stays legible.
	row := domain.RunWorktree{
		RunID:         info.RunID,
		RepoID:        repoID,
		Path:          wtPath,
		FeatureBranch: reservedBranchFor(spec),
	}
	inserted, winningPath, err := host.InsertRunWorktree(ctx, row)
	if err != nil {
		return "", fmt.Errorf("workspace add: reserve worktree row: %w", err)
	}
	if !inserted {
		// Lost the reservation race. Return the winner's path.
		return winningPath, nil
	}

	// Wire the host-side PR fetch for the --pr path unless a test stubbed it.
	if deps.fetchPR == nil {
		deps.fetchPR = func(ctx context.Context, owner, repo string, number int) (*ghclient.PRView, error) {
			return host.GithubGetPR(ctx, owner, repo, number, false)
		}
	}

	// We won. Materialize the checkout.
	gotPath, err := createCheckout(ctx, deps, profile, spec, info.RunID, runRoot)
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
		// The InRoot create contract is to land at
		// filepath.Join(runRoot, owner, repo); a divergence means the
		// worktree library changed and our reservation path no longer
		// matches reality. Surface loudly rather than silently storing
		// the wrong path.
		workspaceLog.Warn("created path diverges from reserved; investigate", "got_path", gotPath, "reserved_path", wtPath, "run_id", info.RunID, "repo", repoID)
	}

	return wtPath, nil
}

// createCheckout dispatches to the right worktree-materialization path for the
// spec. For --pr it fetches the PR (host-side) to learn the head branch + fork
// URL, then routes to the fork-aware CreateForPRInRoot; otherwise it checks out
// the named ref (or the default branch) detached. Returns raw errors; the
// caller wraps them with the "create worktree" context and releases the
// reservation.
func createCheckout(ctx context.Context, deps addDeps, profile *domain.RepoProfile, spec checkoutSpec, runID, runRoot string) (string, error) {
	if spec.pr > 0 {
		pr, err := deps.fetchPR(ctx, profile.Owner, profile.Repo, spec.pr)
		if err != nil {
			return "", fmt.Errorf("fetch PR #%d on %s: %w", spec.pr, profile.ID, err)
		}
		if pr == nil {
			return "", fmt.Errorf("PR #%d not found on %s", spec.pr, profile.ID)
		}
		upstream, head := prCloneURLs(profile.CloneURL, pr)
		return deps.checkoutPR(ctx, profile.Owner, profile.Repo, upstream, head, pr.HeadRef, spec.pr, runID, runRoot)
	}
	return deps.checkout(ctx, profile.Owner, profile.Repo, profile.CloneURL, spec.ref, runID, runRoot)
}

// prCloneURLs derives the (upstream, head) clone URLs to hand CreateForPRInRoot,
// keeping both in the SAME protocol as the bare's origin (profile.CloneURL) so
// CreateForPR's own-repo-vs-fork comparison (head != upstream) stays honest and
// repairOriginURL never flips the bare between SSH and HTTPS. The upstream is
// always the configured repo's own clone URL — the PR's base is owner/repo by
// construction. The head is the fork's URL in the matching protocol, or "" for
// a deleted-fork PR (head.repo == null), which CreateForPR materializes
// read-only (still reviewable via the upstream's refs/pull/<n>/head).
func prCloneURLs(originURL string, pr *ghclient.PRView) (upstream, head string) {
	upstream = originURL
	if strings.HasPrefix(originURL, "https://") {
		return upstream, pr.CloneURL
	}
	// SSH (or any non-HTTPS) origin: match with the SSH head form.
	return upstream, pr.SSHURL
}

// reservedBranchFor computes the informational FeatureBranch stored on the
// reservation row for `workspace list` display. The push gate no longer reads
// it (it reads the worktree's live current branch), so this is purely the
// checkout intent: the PR ref, the named --ref, or empty for a detached
// default-branch checkout (no branch claimed until the agent makes one).
func reservedBranchFor(spec checkoutSpec) string {
	switch {
	case spec.pr > 0:
		return fmt.Sprintf("pull/%d", spec.pr)
	case spec.ref != "":
		return spec.ref
	default:
		return ""
	}
}

// runAdd is the CLI entrypoint: argv → materializeWorkspace → stdout/stderr.
// All errors translate into exitErr so the caller process gets a non-zero
// exit and the agent sees a clear message on stderr. Successful resolution
// (first add or idempotent re-add) prints the absolute worktree path on
// stdout for `cd "$(... workspace add owner/repo)"`.
func runAdd(host agenthost.Client, args []string) {
	ownerRepo, spec, err := parseAddArgs(args)
	if err != nil {
		exitErr(err.Error())
	}

	path, err := materializeWorkspace(host, strings.TrimSpace(ownerRepo), spec, defaultAddDeps())
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
