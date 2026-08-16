package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// addDeps abstracts the side-effecting collaborators of materializeWorkspace
// so tests can stub the git mutation without invoking real git or touching
// the filesystem. Production wiring uses defaultAddDeps.
//
// Only the checkout-creating surface is injectable. The agenthost calls
// go through the supplied Client directly because tests construct a
// LocalClient over an in-memory SQLite db.Stores; mocking the IPC
// layer would test less of the actual code than exercising LocalClient
// end-to-end.
type addDeps struct {
	// create materializes the checkout for spec into the run's HOST run root
	// and returns the created path in host view. Nil in defaultAddDeps;
	// materializeWorkspace wires it to host.CreateWorkspaceCheckout when unset
	// (TFAC-546: the git work runs host-side in both transports — the daemon
	// in sandbox mode, in-process in local mode), so tests stub the git
	// mutation while production routes through the agenthost seam.
	create   func(ctx context.Context, owner, repo string, spec checkoutSpec) (string, error)
	statPath func(path string) (os.FileInfo, error)
	now      func() time.Time
}

func defaultAddDeps() addDeps {
	return addDeps{
		statPath: os.Stat,
		now:      time.Now,
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
	errMissingRunID        = errors.New("workspace add: TRIAGE_FACTORY_CONVERSATION_ID not set; this command must be invoked by the delegated agent")
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

// validateGitRef rejects a --ref value git would refuse (or misparse) at fetch
// time with an opaque error. The rule itself lives in the worktree package
// (ValidateCheckoutRef) — the interpolation point, shared with the agenthost
// RPC surface — this wrapper just maps a violation onto errInvalidRef so the
// CLI's error identity stays errors.Is-able.
func validateGitRef(ref string) error {
	if err := worktree.ValidateCheckoutRef(ref); err != nil {
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
// Two path namespaces (TFAC-546): the git materialization runs HOST-SIDE via
// host.CreateWorkspaceCheckout — in the sandbox that's the agenthost daemon
// building into the real host run root, which this process sees bind-mounted
// at /work. Every durable path (the conversation_worktrees reservation, the row the
// push gate and snapshot read) is therefore recorded in HOST view, while every
// path this process touches (the liveness stat) or returns (the `cd` target on
// stdout) is the AGENT view. host.WorkspaceRoots supplies both roots; in local
// mode they're the same string and the translation is the identity.
//
// Concurrency: the cross-process serialization point is the
// conversation_worktrees PK insert (`InsertRunWorktree`'s INSERT OR IGNORE),
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
//  3. Repository row lookup (clone URL required) + team-tracking gate.
//  4. Reserve the conversation_worktrees row with the deterministic path
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

	// ref is the (run, repo, ref) discriminator: a run can materialize several
	// worktrees in one repo (two PRs, or a PR + the default branch), so every
	// lookup / reservation / path below is keyed on it. It doubles as the
	// worktree path-slug subdirectory.
	ref := refForSpec(spec)

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
	run, err := host.GetConversation(ctx)
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
	existing, err := host.GetRunWorktreeByRepoRef(ctx, repoID, ref)
	if err != nil {
		return "", fmt.Errorf("workspace add: lookup existing worktree: %w", err)
	}
	// Roots for the dual-view translation below. conversation_worktrees rows carry the
	// HOST view; everything this process stats or returns is the AGENT view.
	hostRoot, agentRoot, err := host.WorkspaceRoots(ctx)
	if err != nil {
		return "", fmt.Errorf("workspace add: resolve run root: %w", err)
	}

	if existing != nil {
		_, statErr := deps.statPath(agentViewPath(hostRoot, agentRoot, existing.Path))
		switch {
		case statErr == nil:
			// Path exists on disk — live worktree, return it.
			return agentViewPath(hostRoot, agentRoot, existing.Path), nil
		case errors.Is(statErr, os.ErrNotExist):
			age := deps.now().Sub(existing.CreatedAt)
			if age < staleReservationAge {
				// In-flight winner: another invocation reserved the row
				// and is currently creating the worktree. Return its
				// path; agent's cd succeeds once the create lands.
				return agentViewPath(hostRoot, agentRoot, existing.Path), nil
			}
			// Stale: reservation outlived its creator without a
			// completed worktree. Drop and fall through to re-reserve.
			workspaceLog.Warn("dropping stale reservation; path missing and row age exceeds threshold", "run_id", info.RunID, "repo", repoID, "ref", ref, "path", existing.Path, "age", age, "threshold", staleReservationAge)
			if delErr := host.DeleteRunWorktreeByRepoRef(ctx, repoID, ref); delErr != nil {
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

	// Reserved path is deterministic from the InRoot create contract, in HOST
	// view: filepath.Join(hostRoot, owner, repo, ref-slug). The ref-slug subdir
	// is what lets two PRs (or a PR + a branch) coexist in one repo for one run.
	// Compute it here so we can reserve the row BEFORE the create runs.
	//
	// INVARIANT: this must equal the path host.CreateWorkspaceCheckout will
	// land at — the create derives the same hostRoot via WorkspaceRoots and the
	// same slug via {worktree.CheckoutRefSlug(spec.ref) | worktree.PRRefSlug(spec.pr)},
	// which is exactly what refForSpec(spec) produced for `ref` here. The
	// divergence check after the create (gotPath != wtPath) is the backstop if
	// this ever drifts.
	wtPath := filepath.Join(hostRoot, profile.Owner, profile.Repo, ref)

	// Reserve. Two concurrent processes that both reach this point with the
	// SAME (run, repo, ref) race at the PK; the loser short-circuits before
	// touching git. Ref records the checkout intent — the PK discriminator and
	// the path slug — and is what `workspace list` surfaces; the push gate reads
	// the worktree's live current branch, never this row.
	row := domain.RunWorktree{
		RunID:  info.RunID,
		RepoID: repoID,
		Path:   wtPath,
		Ref:    ref,
	}
	inserted, winningPath, err := host.InsertRunWorktree(ctx, row)
	if err != nil {
		return "", fmt.Errorf("workspace add: reserve worktree row: %w", err)
	}
	if !inserted {
		// Lost the reservation race. Return the winner's path.
		return agentViewPath(hostRoot, agentRoot, winningPath), nil
	}

	// Wire the host-side create unless a test stubbed it. Production routes
	// through the agenthost seam: the daemon in sandbox mode (the git work must
	// run where the shared bare and the real run root live), in-process in
	// local mode.
	if deps.create == nil {
		deps.create = func(ctx context.Context, owner, repo string, spec checkoutSpec) (string, error) {
			return host.CreateWorkspaceCheckout(ctx, owner, repo, spec.ref, spec.pr)
		}
	}

	// We won. Materialize the checkout (host-side; returns the HOST view).
	gotPath, err := deps.create(ctx, profile.Owner, profile.Repo, spec)
	if err != nil {
		// Release the reservation so the next attempt can retry.
		// Delete failures are logged but don't shadow the create error
		// the caller actually needs.
		if delErr := host.DeleteRunWorktreeByRepoRef(ctx, repoID, ref); delErr != nil {
			workspaceLog.Warn("release reservation after create failure failed", "error", delErr)
		}
		return "", fmt.Errorf("workspace add: create worktree: %w", err)
	}
	if gotPath != wtPath {
		// The create contract is to land at filepath.Join(hostRoot, owner,
		// repo, ref-slug); a divergence means the create's derivation and our
		// reservation no longer match. Surface loudly rather than silently
		// storing the wrong path.
		workspaceLog.Warn("created path diverges from reserved; investigate", "got_path", gotPath, "reserved_path", wtPath, "run_id", info.RunID, "repo", repoID, "ref", ref)
	}

	return agentViewPath(hostRoot, agentRoot, wtPath), nil
}

// agentViewPath translates a HOST-view path under hostRoot into the calling
// process's view of the same directory (TFAC-546). In local mode both roots
// are the same string and this is the identity; in the sandbox the host run
// root is bind-mounted at agentRoot (/work), so the host prefix is swapped. A
// path outside hostRoot passes through unchanged — there is nothing to
// translate it against, and returning it verbatim keeps the failure legible
// (a stat/cd on it fails loudly rather than pointing somewhere wrong).
func agentViewPath(hostRoot, agentRoot, p string) string {
	if hostRoot == agentRoot || p == "" {
		return p
	}
	rel, err := filepath.Rel(hostRoot, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.Join(agentRoot, rel)
}

// refForSpec computes the conversation_worktrees ref for a checkout spec — the
// (run, repo, ref) PK discriminator AND the worktree path-slug subdirectory.
// "pr-<N>" for a PR, the slugified branch for --ref, or "@default" for a
// detached default-branch checkout. It uses the same slug helpers the InRoot
// create funcs land the worktree at, so the reserved path and the created path
// agree. The push gate reads the worktree's live current branch, never this
// value, so it carries no authorization — only the checkout intent `workspace
// list` surfaces.
func refForSpec(spec checkoutSpec) string {
	switch {
	case spec.pr > 0:
		return worktree.PRRefSlug(spec.pr)
	case spec.ref != "":
		return worktree.CheckoutRefSlug(spec.ref)
	default:
		return worktree.CheckoutRefSlug("")
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
