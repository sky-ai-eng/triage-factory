package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/review"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// LocalClient is the in-process implementation of Client. Holds a
// resolved RunInfo (set at construction by either AutoDetect's env
// path or by the daemon's per-socket handler) and the db.Stores
// bundle the binary is wired against. Every write branches on
// info.IsEventTriggered to choose admin-pool `...System` calls vs
// the synthetic-claims tx wrap — the same branch the pre-agenthost
// subcommand bodies inlined verbatim, now hoisted up one level.
//
// Concurrency: not safe for concurrent calls from independent
// goroutines on a single instance. cmd/exec subcommands are single-
// goroutine; the daemon constructs a fresh LocalClient per request
// (it's cheap — just two pointers) so cross-request isolation is
// trivially preserved without serializing.
type LocalClient struct {
	stores db.Stores
	info   RunInfo

	// ghResolver overrides the GitHub credential resolver. Nil in
	// production (githubResolver builds the real one from stores); set by
	// tests to inject a fake resolver covering both the App-installation and
	// PAT tiers without standing up the full App-mint plumbing.
	ghResolver ghclient.Resolver

	// ghClients memoizes the per-repo scoped GitHub client (+ identity) built
	// for this run, so several gh calls in one exec subcommand (e.g. pr
	// start-review's GetPR + GetPRDiff + GetPRFiles) mint the repo-scoped token
	// once rather than per call. Multi mode only; keyed lower("owner/repo").
	// LocalClient is single-goroutine (see the type doc), so no lock.
	ghClients map[string]repoClient

	// proxyCreds, when set (TF_ROLE=executor), routes gh/jira verbs through the
	// run's credential-sidecar REST proxies instead of resolving real tokens.
	// Set by the daemon's dispatch from the Server's own coordinates; nil on
	// all/local where the resolver path is used.
	proxyCreds *ProxyCredentials
}

// NewLocal builds a LocalClient bound to the given stores + identity.
// Callers that resolve identity from env (AutoDetect's local branch)
// hand the resolved RunInfo here; the daemon hands the per-socket
// run info here at request dispatch.
func NewLocal(stores db.Stores, info RunInfo) *LocalClient {
	return &LocalClient{stores: stores, info: info}
}

func (c *LocalClient) LookupRun(_ context.Context) (RunInfo, error) {
	// Empty RunID at this stage means AutoDetect's env probe was
	// bypassed (test seam) or the caller constructed a LocalClient
	// directly with a zero-value RunInfo. Surface the same sentinel
	// the runident path does so subcommand helpers can translate it
	// to their package-local "missing run id" sentinel without
	// having to distinguish callers.
	if c.info.RunID == "" {
		return RunInfo{}, runident.ErrRunIdentityMissing
	}
	return c.info, nil
}

func (c *LocalClient) Close() error { return nil }

// withWrite picks the per-run routing strategy: event-triggered runs
// route the write through the admin-pool `...System` variant of the
// store call (no JWT claims available — the trigger fired in a
// background goroutine); manual runs wrap the call in
// SyntheticClaimsWithTx with the kicking-off human's identity so RLS
// policies see the right (orgID, userID) pair. The two-arg shape lets
// each call site pick its own admin-pool function and tx-pool closure
// without duplicating the if/else everywhere. Thin wrapper over
// withWriteInfo (record.go) — the free-function form an ee extension
// handler (no LocalClient) also uses.
func (c *LocalClient) withWrite(
	ctx context.Context,
	system func() error,
	user func(ts db.TxStores) error,
) error {
	return withWriteInfo(ctx, c.stores, c.info, system, user)
}

// --- review draft finalization ---

// runReviewArtifact locates the pending review draft this run's add/finalize op
// is acting on. A run can hold several review drafts at once (run-scoped dedup,
// one per PR — TFAC-494), so it resolves by the handle the agent passes (the
// artifact id) rather than just the first pending review, which could be a
// different PR's draft. An empty handle (defensive — exec always passes one)
// falls back to the run's sole pending review. Returns an actionable error when
// no matching pending review exists.
func (c *LocalClient) runReviewArtifact(ctx context.Context, reviewID string) (*domain.Artifact, error) {
	arts, err := c.listArtifactsByRun(ctx)
	if err != nil {
		return nil, err
	}
	if reviewID != "" {
		if art := domain.PendingReviewArtifactByID(arts, reviewID); art != nil {
			return art, nil
		}
		return nil, fmt.Errorf("review %s is not a pending review for this run — run `gh pr start-review` first", reviewID)
	}
	art := domain.FirstPendingReviewArtifact(arts)
	if art == nil {
		return nil, fmt.Errorf("no pending review for this run — run `gh pr start-review` first")
	}
	return art, nil
}

// FinalizeReviewDraft is the host side of `gh pr finalize-review`: it finalizes a
// fully TF-side review draft for human approval and makes NO GitHub write (the
// atomic create+submit happens at approval). It locates the run's review
// artifact, gates on comment freshness vs. the PR's current head (TFAC-499),
// stages the body + event, snapshots the agent's draft (body + event + the
// locally staged inline comments) into details.proposed as the approve-time
// human-feedback baseline, and sets the ready sentinel (details_json.review_event)
// that marks the review awaiting approval.
//
// reviewID is the local review handle the agent passes through (the artifact id);
// it's validated against the artifact's id so a stale handle fails loudly rather
// than finalizing the wrong review. The TFAC-358 anti-double-submit guard fires
// here: a ready sentinel that's already set returns ErrReviewAlreadyFinalized.
//
// Freshness gate (TFAC-499): a review submits to GitHub atomically under one
// commit_id, so every staged comment must be coherent against one head — but each
// comment was anchored to whatever the agent's worktree HEAD was when it was added
// (ReviewArtifactComment.CommitSHA), and the PR may have advanced since. Finalize
// is the last point the agent can still act, so it reconciles each comment forward
// to the PR's current head: a pure shift (content identical, line moved) auto-
// remaps the stored line silently; an outdated comment (line content changed or
// deleted) fails finalize with a per-comment list so the agent re-reads the new
// diff, edits/deletes/re-adds against the new head, and retries. On success every
// comment shares the current head, so approval pins that single commit_id.
func (c *LocalClient) FinalizeReviewDraft(ctx context.Context, reviewID, event, body string) error {
	art, err := c.runReviewArtifact(ctx, reviewID)
	if err != nil {
		return err
	}

	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		return fmt.Errorf("parse review artifact details: %w", err)
	}
	// Anti-double-submit (TFAC-358): the ready sentinel is already set, so the
	// agent already finalized this review. Hard error so it stops looping.
	if details.ReviewEvent != "" {
		return ErrReviewAlreadyFinalized
	}

	// A comment / request_changes review must carry something actionable: a body,
	// inline comments, or both. An approve needs neither (the approval is the
	// signal). The comments are the locally staged set the agent added.
	if body == "" && event != "APPROVE" && len(details.StagedComments) == 0 {
		return fmt.Errorf("a %s review needs --body/--body-file or at least one inline comment", strings.ToLower(event))
	}

	// Freshness gate (TFAC-499): reconcile the staged comments to the PR's current
	// head before snapshotting + arming the ready sentinel. A pure shift auto-
	// remaps in place (mutating details.StagedComments); an outdated comment
	// returns a per-comment error here, before anything is persisted, so the agent
	// can still fix it. Skipped when there are no comments (a pure approve / body-
	// only review has no inline anchor to reconcile).
	//
	// finalizeBase/finalizeHead are the PR base+head this review is finalized
	// against — head is the commit every comment was reconciled to. Both are
	// stamped onto details (TFAC-500): head anchors the commits-since-finalize
	// count, and base+head together reproduce the finalize-time PR diff the human
	// overlay renders (FinalizedBaseSHA...FinalizedHeadSHA). A review WITH comments
	// learns both from the reconcile (which already fetched the PR); a comment-less
	// review (pure approve / body-only) makes no GitHub call here, so head falls
	// back to the start-review head and base stays empty (the overlay then falls
	// back to the live diff — a comment-less review has no inline anchors anyway).
	finalizeHead := details.HeadSHA
	finalizeBase := ""
	if len(details.StagedComments) > 0 {
		base, head, err := c.reconcileStagedCommentsToHead(ctx, art, &details)
		if err != nil {
			return err
		}
		finalizeHead = head
		finalizeBase = base
	}

	// Snapshot the agent's draft as the write-once Proposed baseline — a copy of
	// the staged comments as they stand now (post-reconcile, so the baseline is
	// coherent against the same head the review pins to), so a later human edit to
	// StagedComments still diffs against what the agent originally wrote.
	proposed := domain.ReviewArtifactProposed{Body: body, Event: event}
	proposed.Comments = append(proposed.Comments, details.StagedComments...)
	details.ReviewBody = body
	details.ReviewEvent = event // the ready sentinel
	details.FinalizedHeadSHA = finalizeHead
	details.FinalizedBaseSHA = finalizeBase
	details.Proposed = proposed

	updated := *art
	updated.DetailsJSON = domain.MarshalReviewArtifactDetails(details)
	if _, err := c.UpsertArtifact(ctx, updated); err != nil {
		return fmt.Errorf("snapshot review draft into artifact: %w", err)
	}
	return nil
}

// reconcileStagedCommentsToHead forward-maps every staged comment from the commit
// it was anchored against to the PR's current head (TFAC-499), mutating
// details.StagedComments in place. A comment whose code survived — identical
// content, at the same line (LineMapUnchanged) or a shifted/renamed location
// (LineMapMoved) — is auto-remapped to its new-side line/path and re-anchored to
// the current head. A comment whose anchored code changed or was deleted
// (LineMapOutdated) is collected; if any are outdated the whole finalize fails
// with a per-comment error and NOTHING is persisted (the caller returns before the
// upsert), so the agent re-reads the new diff, edits/deletes/re-adds, and retries.
//
// The forward line-map is computed from the anchor→head compare diff and cached
// per distinct anchor commit, so comments staged against the same checkout share
// one GitHub read rather than fetching the same compare once per comment. A
// comment already anchored at the current head needs no map (and no read).
//
// Returns the PR's base+head SHA at finalize — head is the commit every comment
// was reconciled to, base is the PR base branch tip — so the caller can stamp both
// onto details (TFAC-500): head anchors the commits-since-finalize count, and
// base+head reproduce the finalize-time diff the human overlay renders.
//
// The classify/remap transform is shared with the human-facing freshness +
// Refresh paths via internal/review; agenthost supplies the GitHub fetch (the
// anchor→head compare line-map) and applies the finalize-gate POLICY: a transient
// map error aborts (never silently arm a review against an unverified head), and
// any outdated comment fails the whole finalize with the actionable per-comment
// listing.
func (c *LocalClient) reconcileStagedCommentsToHead(ctx context.Context, art *domain.Artifact, details *domain.ReviewArtifactDetails) (baseSHA, headSHA string, err error) {
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return "", "", fmt.Errorf("review artifact has a malformed target %q", art.Target)
	}
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return "", "", err
	}
	pr, err := client.GetPRBasic(ctx, owner, repo, number)
	if err != nil {
		return "", "", fmt.Errorf("fetch PR #%d to reconcile the review against its current head: %w", number, err)
	}
	head := pr.HeadSHA
	if head == "" {
		return "", "", fmt.Errorf("PR #%d has no head commit SHA; cannot reconcile the review's comments against the current head", number)
	}

	outcomes := review.ReconcileToHead(details.StagedComments, head, details.HeadSHA,
		func(anchor string) (ghclient.LineMap, error) {
			return hostCompareLineMap(ctx, client, owner, repo, anchor, head)
		})

	var stale []staleReviewComment
	remapped := make([]domain.ReviewArtifactComment, len(outcomes))
	for i, o := range outcomes {
		// A transient compare failure must abort finalize, not silently arm a review
		// against an unverified head.
		if o.MapErr != nil {
			return "", "", fmt.Errorf("reconcile review comment on %s (head %s): %w",
				o.Comment.Path, shortCommit(head), o.MapErr)
		}
		if o.Outdated() {
			stale = append(stale, staleReviewComment{
				id:   o.Comment.ID,
				path: o.Comment.Path,
				line: derefInt(o.Comment.Line),
				body: o.Comment.Body,
			})
		}
		remapped[i] = o.Comment
	}
	if len(stale) > 0 {
		return "", "", staleReviewCommentsError(number, head, stale)
	}
	// Persist the survivors' remap (ReconcileToHead returns copies). Done only on
	// success, so a failed finalize leaves the staged set untouched for the retry.
	details.StagedComments = remapped
	return pr.BaseSHA, head, nil
}

// derefInt returns the pointed-to int, or 0 for nil — only the stale-comment
// listing uses it, and an outdated comment is always positioned, so the 0 is never
// actually hit.
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// staleReviewComment is one outdated staged comment, captured for the finalize
// failure listing: its TF-local id (the edit/delete address), path + line (where
// it no longer anchors), and body (snippeted for the message).
type staleReviewComment struct {
	id   string
	path string
	line int
	body string
}

// staleReviewCommentsError renders the actionable finalize failure: which comments
// went stale against the current head and the concrete verbs to fix each, so the
// agent can re-read the new diff, edit/delete the stale comment, re-add it against
// the new code, and retry finalize. Editing/deleting staged comments is the
// TFAC-383 affordance this gate depends on — without it the failure would be a
// dead end.
func staleReviewCommentsError(number int, head string, stale []staleReviewComment) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d staged review comment(s) are outdated against PR #%d's current head (%s): the code each anchors to has changed or been deleted since you wrote it. A review submits atomically under one commit, so every comment must match the current head before this review can be finalized.\n",
		len(stale), number, shortCommit(head))
	for _, s := range stale {
		fmt.Fprintf(&b, "  - %s:%d (comment %s): %q\n", s.path, s.line, s.id, commentBodySnippet(s.body))
	}
	fmt.Fprintf(&b, "\nFor each comment above: re-read the current diff (`gh pr diff %d`), then delete it (`gh pr comment-delete <comment_id>`) or edit it (`gh pr comment-update <comment_id> ...`) and re-add it against the new code (`gh pr add-review-comment ...`). Then run finalize-review again.", number)
	return errors.New(b.String())
}

// commentBodySnippet trims a staged comment's body to a short, single-line
// reference for the stale-comment listing. The severity badge is stripped (it's
// baked into the body for GitHub, noise here) and the body collapsed to its first
// line; rune-aware so a multibyte char isn't split.
func commentBodySnippet(body string) string {
	_, clean := domain.ParseSeverityBadge(body)
	clean = strings.TrimSpace(clean)
	if i := strings.IndexAny(clean, "\r\n"); i >= 0 {
		clean = strings.TrimSpace(clean[:i])
	}
	const max = 120
	r := []rune(clean)
	if len(r) <= max {
		return clean
	}
	return string(r[:max]) + "…"
}

// shortCommit abbreviates a commit SHA to 7 chars for agent-facing messages,
// leaving anything already shorter (a test stub SHA, an empty fallback) untouched.
func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// ResetReviewDraft is the host side of `gh pr start-review --fresh`: a pure local
// reset of this run's review draft for owner/repo#number, with zero GitHub calls.
// It clears the staged comments, the body/event ready sentinel, and the
// write-once Proposed snapshot back to an empty pending draft, keeping the
// artifact row, its handle, and its pinned head SHA. Returns the draft's handle
// and that pinned head SHA, or ("", "") when the run has no draft for that PR (the
// caller falls through to a normal start-review).
//
// Post-494 the whole review lives in details_json, so the reset is a single
// artifact rewrite — there is no GitHub pending review to delete and no identity
// branch (the multi-tenant collision the GitHub-native model had is gone).
//
// NOTE: HeadSHA is deliberately PRESERVED, not refreshed — refreshing it would
// require a GetPR read, and --fresh is contractually zero-GitHub-calls. This is
// safe for the use case that motivates --fresh (you pulled new commits mid-review
// and need to redo the inline comments): a review WITH comments pins the atomic
// submit's commit_id to its comments' own CommitSHA anchors (set per comment at
// add-review-comment, so the re-added comments anchor to the new HEAD), not to
// this HeadSHA — HeadSHA is only the fallback commit_id for a comment-LESS review
// (a pure approve / body-only request-changes). The one stale-SHA edge — pulling
// past HeadSHA and then finalizing a comment-less review — can't arise from the
// --fresh flow, since a comment-less review has no staged comments to clear in
// the first place. If a future flow needs the head re-pinned, refresh it on the
// normal (non-fresh) start-review path, which already reads the head.
func (c *LocalClient) ResetReviewDraft(ctx context.Context, owner, repo string, number int) (reviewID, commitSHA string, err error) {
	arts, err := c.listArtifactsByRun(ctx)
	if err != nil {
		return "", "", err
	}
	target := domain.ReviewTarget(owner+"/"+repo, number)
	var art *domain.Artifact
	for i := range arts {
		if arts[i].Kind == domain.ArtifactKindReview && arts[i].State == domain.ArtifactStateReviewPending && arts[i].Target == target {
			art = &arts[i]
			break
		}
	}
	if art == nil {
		// No draft to reset — the caller starts a normal one. Not an error: --fresh
		// as the first start-review is a valid (if degenerate) clean slate.
		return "", "", nil
	}

	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		return "", "", fmt.Errorf("parse review artifact details: %w", err)
	}
	// Clear the staged set + ready sentinel + frozen Proposed baseline, keeping
	// Number/HeadSHA (see NOTE above) so the restarted review still pins to the
	// same reviewed commit.
	details.StagedComments = nil
	details.ReviewBody = ""
	details.ReviewEvent = ""
	details.Proposed = domain.ReviewArtifactProposed{}

	updated := *art
	updated.DetailsJSON = domain.MarshalReviewArtifactDetails(details)
	if _, err := c.UpsertArtifact(ctx, updated); err != nil {
		return "", "", fmt.Errorf("reset review draft: %w", err)
	}
	return art.ID, details.HeadSHA, nil
}

// listArtifactsByRun reads the calling run's artifacts respecting the pool split:
// event-triggered runs read admin-pool (no JWT claims); manual runs read under
// the kicking-off user's synthetic claims. Mirrors withWrite for the read side.
func (c *LocalClient) listArtifactsByRun(ctx context.Context) ([]domain.Artifact, error) {
	if c.info.IsEventTriggered {
		return c.stores.Artifacts.ListByRunSystem(ctx, c.info.OrgID, c.info.RunID)
	}
	var out []domain.Artifact
	err := c.stores.Tx.SyntheticClaimsWithTx(ctx, c.info.OrgID, c.info.UserID, func(ts db.TxStores) error {
		a, e := ts.Artifacts.ListByRun(ctx, c.info.OrgID, c.info.RunID)
		out = a
		return e
	})
	return out, err
}

// --- workspace ---

func (c *LocalClient) GetAgentRun(ctx context.Context) (*domain.AgentRun, error) {
	return c.stores.AgentRuns.GetSystem(ctx, c.info.OrgID, c.info.RunID)
}

func (c *LocalClient) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	return c.stores.Tasks.GetSystem(ctx, c.info.OrgID, taskID)
}

func (c *LocalClient) ListRepos(ctx context.Context) ([]domain.RepoProfile, error) {
	return c.stores.Repos.ListSystem(ctx, c.info.OrgID)
}

func (c *LocalClient) GetRepo(ctx context.Context, repoID string) (*domain.RepoProfile, error) {
	return c.stores.Repos.GetSystem(ctx, c.info.OrgID, repoID)
}

func (c *LocalClient) TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error) {
	return c.stores.TeamGitHubRepos.TracksRepoSystem(ctx, c.info.TeamID, owner, repo)
}

func (c *LocalClient) GetRunWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.RunWorktree, error) {
	return c.stores.RunWorktrees.GetByRepoRefSystem(ctx, c.info.OrgID, c.info.RunID, repoID, ref)
}

func (c *LocalClient) ListRunWorktrees(ctx context.Context) ([]domain.RunWorktree, error) {
	return c.stores.RunWorktrees.ListSystem(ctx, c.info.OrgID, c.info.RunID)
}

func (c *LocalClient) InsertRunWorktree(ctx context.Context, row domain.RunWorktree) (bool, string, error) {
	if c.info.IsEventTriggered {
		return c.stores.RunWorktrees.InsertSystem(ctx, c.info.OrgID, row)
	}
	var (
		inserted    bool
		winningPath string
	)
	err := c.stores.Tx.SyntheticClaimsWithTx(ctx, c.info.OrgID, c.info.UserID, func(ts db.TxStores) error {
		i, w, ierr := ts.RunWorktrees.Insert(ctx, c.info.OrgID, row)
		inserted = i
		winningPath = w
		return ierr
	})
	return inserted, winningPath, err
}

func (c *LocalClient) DeleteRunWorktreeByRepoRef(ctx context.Context, repoID, ref string) error {
	return c.withWrite(ctx,
		func() error {
			return c.stores.RunWorktrees.DeleteByRepoRefSystem(ctx, c.info.OrgID, c.info.RunID, repoID, ref)
		},
		func(ts db.TxStores) error {
			return ts.RunWorktrees.DeleteByRepoRef(ctx, c.info.OrgID, c.info.RunID, repoID, ref)
		},
	)
}

// --- agent run footer ---

func (c *LocalClient) BuildAgentRunFooter(_ context.Context, kind string) (string, error) {
	return agentmeta.Build(c.stores.AgentRuns, c.info.OrgID, c.info.RunID, kind), nil
}

// --- artifacts ---

// UpsertArtifact stamps the run identity (run_id, org_id, team_id) onto the
// caller's polymorphic artifact and upserts it. Event-triggered runs route
// admin-pool (no JWT claims to bind); manual runs wrap the app-pool write in
// the kicking-off user's synthetic claims — the same branch the pending-PR
// writer uses. Returns the stored row. See TFAC-460.
func (c *LocalClient) UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error) {
	a.OrgID = c.info.OrgID
	a.TeamID = c.info.TeamID
	a.RunID = c.info.RunID
	// A branch push is also an external action (ActionBranchPushed) — record it in
	// the same write as the artifact so the audit log of record and the artifact
	// agree (TFAC-483). nil for any other kind (the review-draft snapshot this
	// method also serves is not a fresh external write).
	act := c.branchPushAction(a)
	if c.info.IsEventTriggered {
		stored, err := c.stores.Artifacts.UpsertSystem(ctx, c.info.OrgID, a)
		if err != nil {
			return domain.Artifact{}, err
		}
		// Event path: no admin tx to compose with, so the action is a second
		// best-effort admin write — log a failure, never lose the stored artifact.
		if rerr := c.recordActionSystem(ctx, act); rerr != nil {
			agenthostLog.Warn("branch external-action recording failed (push already applied)",
				"run", c.info.RunID, "target", a.Target, "error", rerr)
		}
		return stored, nil
	}
	var out domain.Artifact
	err := c.stores.Tx.SyntheticClaimsWithTx(ctx, c.info.OrgID, c.info.UserID, func(ts db.TxStores) error {
		stored, uerr := ts.Artifacts.Upsert(ctx, c.info.OrgID, a)
		if uerr != nil {
			return uerr
		}
		out = stored
		return recordActionTx(ctx, ts, c.info.OrgID, act)
	})
	return out, err
}

// --- jira ---
//
// jiraSystemClient builds the org's bot-attributed Jira client. On
// TF_ROLE=executor, when the daemon attached a sealed run bundle to ctx
// (server.dispatch), it builds straight from the bundle's Jira credential —
// no secret-store read, since the secret store is disabled on an executor.
// Every other role/mode falls through to the ForSystem resolver: local mode
// reads the OS keychain on the user's own machine, TF_ROLE=all reads the
// live (Vault-backed) secret store. Either way the agent process never holds
// the token.
//
// A missing credential maps to a clear "not configured" message (the
// guidance exec printed before handing off); every other resolver error
// (a transient vault/keychain outage) propagates so it isn't misreported
// as "absent". Over IPC the message crosses as the response Error string,
// so the agent reads the identical text across every mode/role.
func (c *LocalClient) jiraSystemClient(ctx context.Context) (*jiraclient.Client, error) {
	if c.proxyCreds != nil {
		return proxyJiraClient(c.proxyCreds)
	}
	client, err := jiraclient.NewResolver(c.stores.Secrets, c.stores.Orgs).ForSystem(ctx, c.info.OrgID)
	if errors.Is(err, jiraclient.ErrNoJiraSystemCredential) {
		return nil, errors.New("no Jira credential configured; run triagefactory and complete setup first")
	}
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (c *LocalClient) JiraGetIssue(ctx context.Context, key string) (*jiraclient.Issue, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetIssue(ctx, key)
}

func (c *LocalClient) JiraTransitionTo(ctx context.Context, key, status string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.TransitionTo(ctx, key, status); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ActionIssueTransitioned, domain.ArtifactStateIssueUpdated, status, jiraDetailsJSON(map[string]any{"status": status}))
	return nil
}

func (c *LocalClient) JiraGetTransitions(ctx context.Context, key string) ([]jiraclient.Transition, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetTransitions(ctx, key)
}

func (c *LocalClient) JiraAddComment(ctx context.Context, key, body string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	commentID, err := client.AddComment(ctx, key, body)
	if err != nil {
		return err
	}
	c.recordJiraComment(ctx, key, commentID, body)
	return nil
}

func (c *LocalClient) JiraAssignToSelf(ctx context.Context, key string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.AssignToSelf(ctx, key); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ActionIssueAssigned, domain.ArtifactStateIssueUpdated, "", jiraDetailsJSON(map[string]any{"assignee": "self"}))
	return nil
}

func (c *LocalClient) JiraUnassign(ctx context.Context, key string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.Unassign(ctx, key); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ActionIssueUpdated, domain.ArtifactStateIssueUpdated, "", jiraDetailsJSON(map[string]any{"assignee": nil}))
	return nil
}

func (c *LocalClient) JiraCreateIssue(ctx context.Context, project, issueType, summary, description, parentKey, priority string) (string, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return "", err
	}
	key, err := client.CreateIssue(ctx, project, issueType, summary, description, parentKey, priority)
	if err != nil {
		return "", err
	}
	c.recordJiraIssue(ctx, key, domain.ActionIssueCreated, domain.ArtifactStateIssueCreated, "", "")
	return key, nil
}

func (c *LocalClient) JiraUpdateIssue(ctx context.Context, key string, fields jiraclient.UpdateIssueFields) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.UpdateIssue(ctx, key, fields); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ActionIssueUpdated, domain.ArtifactStateIssueUpdated, "", jiraDetailsJSON(map[string]any{"fields": updatedFieldNames(fields)}))
	return nil
}

func (c *LocalClient) JiraSetParent(ctx context.Context, key, parentKey string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.SetParent(ctx, key, parentKey); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ActionIssueUpdated, domain.ArtifactStateIssueUpdated, "", jiraDetailsJSON(map[string]any{"parent": parentKey}))
	return nil
}

func (c *LocalClient) JiraGetChildIssues(ctx context.Context, key string) ([]jiraclient.Issue, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetChildIssues(ctx, key)
}

func (c *LocalClient) JiraSearchIssues(ctx context.Context, jql string, fields []string, maxResults int) ([]jiraclient.Issue, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.SearchIssues(ctx, jql, fields, maxResults)
}

func (c *LocalClient) JiraListPriorities(ctx context.Context) ([]jiraclient.Priority, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListPriorities(ctx)
}

func (c *LocalClient) JiraSetPriority(ctx context.Context, key, priority string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.SetPriority(ctx, key, priority); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ActionIssueUpdated, domain.ArtifactStateIssueUpdated, "", jiraDetailsJSON(map[string]any{"priority": priority}))
	return nil
}

func (c *LocalClient) JiraListIssueTypes(ctx context.Context, project string) ([]jiraclient.IssueType, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListIssueTypes(ctx, project)
}

// --- jira artifact recording (TFAC-459) ---
//
// Record one durable `artifacts` row per successful Jira mutation the agent
// performs, attributed to this run. Jira writes are not preview-gated — they
// execute immediately — so this is straight record-on-success: the call above
// already took effect, and recording is best-effort, never able to fail or
// roll back the action it observed.
//
// Recording happens here on the LocalClient because the multi daemon
// dispatches every host-routed Jira call through NewLocal (server.go), so this
// one seam covers both the sandbox (daemon) and the local-mode CLI, with the
// full RunInfo + Stores in scope.
//
// Every mediated Jira *mutation* is captured: issue create, and the in-place
// edits — transition, assign/unassign, update-fields, set-parent, set-priority
// — plus comments. Pure reads (get / search / list-*) record nothing. This is
// the full write surface of `exec jira`, so an agent can't mutate an issue
// without leaving an audit trail.

// recordJiraIssue upserts the single deduped `issue` artifact for KEY. Every
// issue mutation (create / transition / assign / unassign / update / set-parent
// / set-priority) collapses onto one row keyed jira:issue:<KEY>; external_id
// and url are the issue's stable coordinates, populated on every action so a
// later one can fill a URL an earlier one couldn't compute (the store preserves
// them on empty either way — see ArtifactStore.Upsert). state + details_json
// carry the most recent action — by design the row tracks the last action, not
// the first (domain.Artifact).
// action is the audit discriminator (issue_created / issue_transitioned /
// issue_assigned / issue_updated) — finer-grained than the artifact's
// created/updated state, which can't distinguish a transition from an assign from
// a field edit. toState carries a transition's target status (the Jira status the
// agent moved the ticket to), empty otherwise; the agent's exec transition can't
// cheaply know the prior status, so fromState stays empty.
func (c *LocalClient) recordJiraIssue(ctx context.Context, key, action, state, toState, detailsJSON string) {
	if c.stores.Artifacts == nil || key == "" {
		return
	}
	a := domain.Artifact{
		Kind:        domain.ArtifactKindIssue,
		Target:      key,
		ExternalID:  key,
		URL:         c.jiraBrowseURL(ctx, key),
		State:       state,
		DedupKey:    domain.ArtifactDedupKey(domain.ArtifactProviderJira, domain.ArtifactKindIssue, key, ""),
		DetailsJSON: detailsJSON,
	}
	c.upsertJiraArtifact(ctx, a, jiraAction(a, action, "", toState))
}

// recordJiraComment upserts a `comment` artifact keyed jira:comment:<id>. A
// missing comment id (the POST landed but its body didn't parse) means there's
// no stable key to dedup on, so recording is skipped — the comment still
// posted; only its audit row is best-effort dropped. The skip is logged at
// debug so a future Jira response-shape change surfaces as missing rows with a
// breadcrumb, not silently.
func (c *LocalClient) recordJiraComment(ctx context.Context, key, commentID, body string) {
	if c.stores.Artifacts == nil {
		return
	}
	if commentID == "" {
		agenthostLog.Debug("jira comment recorded without an id; skipping artifact",
			"run", c.info.RunID, "issue", key)
		return
	}
	a := domain.Artifact{
		Kind:        domain.ArtifactKindComment,
		Target:      key,
		ExternalID:  commentID,
		URL:         c.jiraCommentURL(ctx, key, commentID),
		State:       domain.ArtifactStateCommentPosted,
		DedupKey:    domain.ArtifactDedupKey(domain.ArtifactProviderJira, domain.ArtifactKindComment, commentID, ""),
		DetailsJSON: jiraDetailsJSON(map[string]any{"body": jiraBodySnippet(body)}),
	}
	c.upsertJiraArtifact(ctx, a, jiraAction(a, domain.ActionIssueCommentPosted, "", ""))
}

// upsertJiraArtifact stamps the provider onto a and writes it best-effort via
// the shared recording funnel (RecordExternalWrite, record.go): a recording
// failure is logged and swallowed so it never fails the agent's already-
// applied Jira action. act is the external-action audit row appended in the
// SAME write as the artifact (TFAC-483), under the org Jira service-account
// credential. See upsertGithubArtifact.
func (c *LocalClient) upsertJiraArtifact(ctx context.Context, a domain.Artifact, act *domain.ExternalAction) {
	a.Provider = domain.ArtifactProviderJira
	RecordExternalWrite(ctx, c.stores, c.info, &a, act)
}

// jiraSiteBase returns the org's configured Jira site URL, trailing slash
// trimmed, or "" if it's unreadable or unset. Best-effort and uses the
// admin-pool settings read so it works without a JWT-claims context, like the
// rest of recording. This is the human-facing site (e.g.
// https://acme.atlassian.net), not the API gateway base the Jira client talks
// to — the same source the poller stamps entity URLs from.
func (c *LocalClient) jiraSiteBase(ctx context.Context) string {
	if c.stores.Orgs == nil {
		return ""
	}
	set, err := c.stores.Orgs.GetSettingsSystem(ctx, c.info.OrgID)
	if err != nil || set.JiraBaseURL == "" {
		return ""
	}
	return strings.TrimRight(set.JiraBaseURL, "/")
}

// jiraBrowseURL builds the issue's human-facing {site}/browse/<KEY> link, or ""
// when the site URL is unavailable (url is an optional artifact field).
func (c *LocalClient) jiraBrowseURL(ctx context.Context, key string) string {
	base := c.jiraSiteBase(ctx)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/browse/%s", base, key)
}

// jiraCommentURL builds a deep link to the comment —
// {site}/browse/<KEY>?focusedCommentId=<id> — which both Cloud and Server/DC
// honor. Empty when the site URL is unavailable.
func (c *LocalClient) jiraCommentURL(ctx context.Context, key, commentID string) string {
	base := c.jiraSiteBase(ctx)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/browse/%s?focusedCommentId=%s", base, key, commentID)
}

// jiraDetailsJSON marshals a small kind-specific payload for an artifact's
// details_json. Returns "" (→ SQL NULL) on an empty map or a marshal error —
// details are descriptive metadata, never load-bearing, so a failure here must
// not break recording.
func jiraDetailsJSON(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// updatedFieldNames lists which issue fields an update touched, for the
// artifact's details_json — the names only, never the (potentially large)
// values. Order is stable so the recorded payload is deterministic.
func updatedFieldNames(f jiraclient.UpdateIssueFields) []string {
	names := []string{}
	if f.Summary != nil {
		names = append(names, "summary")
	}
	if f.Description != nil {
		names = append(names, "description")
	}
	if f.Priority != nil {
		names = append(names, "priority")
	}
	if f.IssueType != nil {
		names = append(names, "issue_type")
	}
	if len(f.AddLabels) > 0 {
		names = append(names, "add_labels")
	}
	if len(f.RemoveLabels) > 0 {
		names = append(names, "remove_labels")
	}
	return names
}

// jiraBodySnippet trims a comment body to a short, stored-once reference for
// the artifact's details_json. Rune-aware so a multibyte char isn't split.
func jiraBodySnippet(body string) string {
	const max = 200
	body = strings.TrimSpace(body)
	r := []rune(body)
	if len(r) <= max {
		return body
	}
	return string(r[:max]) + "…"
}

// --- github ---
//
// githubResolver returns the credential resolver to use. Tests inject one
// via the ghResolver field; production builds the real org-tiered resolver
// (App installation token → org PAT) from this client's stores. The secret
// store backing it differs by mode but the call site doesn't care: local
// mode reads the OS keychain on the user's own machine; the daemon (sandbox
// path) reads the host's Vault-backed store — the host can read the
// credential the sandboxed agent can't. Either way the agent process never
// holds a token.
//
// The resolver is memoized on first build so several GitHub calls within one
// exec subcommand (e.g. pr start-review's GetPR + GetPRDiff + GetPRFiles)
// share one in-memory installation-token cache instead of re-minting per
// call. Safe because LocalClient is single-goroutine (see the type doc). The
// daemon pre-sets ghResolver to the Server's shared resolver, so this build
// path is the local-mode CLI's only. App-or-PAT tier selection lives entirely
// in the resolver — no gh-specific credential logic here.
func (c *LocalClient) githubResolver() ghclient.Resolver {
	if c.ghResolver == nil {
		c.ghResolver = ghclient.NewResolver(c.stores.Secrets, c.stores.GitHubApps, c.stores.Orgs, c.stores.Agents, nil)
	}
	return c.ghResolver
}

// mapGithubResolveErr maps a resolver error to the agent-facing form: a
// missing credential becomes the same "not configured" guidance the gh branch
// printed before this refactor; every other resolver error (a transient
// vault/keychain outage) propagates verbatim so it isn't misreported as
// "absent". Over IPC the message crosses as the response Error string.
func mapGithubResolveErr(err error) error {
	if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
		return errors.New("GitHub not configured; run triagefactory and complete setup first")
	}
	return err
}

// repoClient memoizes one repo's resolved client + acting identity for the
// life of a single exec subcommand (one LocalClient).
type repoClient struct {
	client   *ghclient.Client
	identity ghclient.Identity
}

// githubClientForRepo resolves an authenticated client for owner/repo, gated +
// down-scoped in multi mode (see resolveRepoClient).
func (c *LocalClient) githubClientForRepo(ctx context.Context, owner, repo string) (*ghclient.Client, error) {
	client, _, err := c.resolveRepoClient(ctx, owner, repo)
	return client, err
}

// resolveRepoClient is the single funnel every exec-gh GitHub call goes
// through. In MULTI mode it enforces the per-run repo gate (the run may only
// touch a repo its team tracks AND that it has materialized — its eagerly-
// cloned task repo, recorded in run_worktrees, or a workspace-add'd one) and
// resolves a per-repo DOWN-SCOPED client (the injected token is narrowed to
// owner/repo), memoized per repo so several calls in one subcommand mint once.
// In LOCAL mode (N=1, unscoped — mirroring the git proxy being nil locally) it
// is the prior behavior verbatim: no gate, no scoping.
func (c *LocalClient) resolveRepoClient(ctx context.Context, owner, repo string) (*ghclient.Client, ghclient.Identity, error) {
	if runmode.Current() != runmode.ModeMulti {
		return c.legacyRepoClient(ctx, owner, repo)
	}
	if err := c.authorizeRepo(ctx, owner, repo); err != nil {
		return nil, ghclient.IdentityUnknown, err
	}
	key := strings.ToLower(owner + "/" + repo)
	if rc, ok := c.ghClients[key]; ok {
		return rc.client, rc.identity, nil
	}
	client, identity, err := c.scopedRepoClient(ctx, owner, repo)
	if err != nil {
		return nil, ghclient.IdentityUnknown, err
	}
	if c.ghClients == nil {
		c.ghClients = make(map[string]repoClient)
	}
	c.ghClients[key] = repoClient{client: client, identity: identity}
	return client, identity, nil
}

// scopedRepoClient builds a per-repo down-scoped client. On TF_ROLE=executor,
// when the daemon attached a sealed run bundle to ctx (server.dispatch), it
// builds straight from the bundle's repo-scoped installation token — no
// secret-store read, since ScopedRepoResolver would otherwise resolve
// against the (disabled, on an executor) secret store. Every other role/mode
// falls through to the resolver's ScopedRepoResolver extension (the
// production resolver), falling back further to the unscoped path when the
// resolver doesn't implement it (test fakes). nil permissions keeps the
// installation's FULL permission set on the single repo — the exec-gh verb
// surface spans pull_requests/issues/contents/checks/actions, so a fixed
// narrow set risks a 422 or a broken verb; repo-scoping is the confinement
// that matters.
func (c *LocalClient) scopedRepoClient(ctx context.Context, owner, repo string) (*ghclient.Client, ghclient.Identity, error) {
	if c.proxyCreds != nil {
		return proxyRepoClient(c.proxyCreds, owner, repo)
	}
	if sr, ok := c.githubResolver().(ghclient.ScopedRepoResolver); ok {
		client, identity, err := sr.ClientForRepoScoped(ctx, c.info.OrgID, owner, repo, nil)
		if err != nil {
			return nil, ghclient.IdentityUnknown, mapGithubResolveErr(err)
		}
		return client, identity, nil
	}
	return c.legacyRepoClient(ctx, owner, repo)
}

// legacyRepoClient is the pre-scoping resolution: the App/PAT client + its
// identity, unscoped. Used in local mode and as the fallback for a resolver
// that doesn't implement ScopedRepoResolver. Mirrors the original
// githubClientAndIdentityForRepo (RepoIdentityResolver, else ClientForRepo +
// IdentityUnknown).
func (c *LocalClient) legacyRepoClient(ctx context.Context, owner, repo string) (*ghclient.Client, ghclient.Identity, error) {
	if ir, ok := c.githubResolver().(ghclient.RepoIdentityResolver); ok {
		client, identity, err := ir.ClientForRepoWithIdentity(ctx, c.info.OrgID, owner, repo)
		if err != nil {
			return nil, ghclient.IdentityUnknown, mapGithubResolveErr(err)
		}
		return client, identity, nil
	}
	client, err := c.githubResolver().ClientForRepo(ctx, c.info.OrgID, owner, repo)
	if err != nil {
		return nil, ghclient.IdentityUnknown, mapGithubResolveErr(err)
	}
	return client, ghclient.IdentityUnknown, nil
}

// authorizeRepo is the exec-gh repo gate (multi mode): the run may only act on
// a repo its team tracks AND that it has materialized in run_worktrees. Same
// predicate as the git proxy's Authorize (run_worktrees holds the run's task
// repo too — recorded at setup — and any workspace-add'd repo). A partial test
// wiring (nil stores) skips the gate. Both a hard deny and a fail-closed
// backend error record a git_denied audit row before returning, so a transient
// DB blip during a denied gh op still leaves an audit trail — symmetric with
// the proxy's "authorize-error" path.
func (c *LocalClient) authorizeRepo(ctx context.Context, owner, repo string) error {
	if c.stores.TeamGitHubRepos == nil || c.stores.RunWorktrees == nil {
		return nil
	}
	tracks, err := c.stores.TeamGitHubRepos.TracksRepoSystem(ctx, c.info.TeamID, owner, repo)
	if err != nil {
		c.RecordGitDenied(ctx, owner, repo, "", "gh", "authorize-error")
		return fmt.Errorf("authorize repo %s/%s: %w", owner, repo, err)
	}
	if tracks {
		rows, lerr := c.stores.RunWorktrees.ListSystem(ctx, c.info.OrgID, c.info.RunID)
		if lerr != nil {
			c.RecordGitDenied(ctx, owner, repo, "", "gh", "authorize-error")
			return fmt.Errorf("authorize repo %s/%s: %w", owner, repo, lerr)
		}
		repoID := owner + "/" + repo
		for _, w := range rows {
			if strings.EqualFold(w.RepoID, repoID) {
				return nil
			}
		}
	}
	c.RecordGitDenied(ctx, owner, repo, "", "gh", "repo-not-authorized")
	return fmt.Errorf("repo %s/%s not authorized for this run", owner, repo)
}

func (c *LocalClient) GithubGetPR(ctx context.Context, owner, repo string, number int, verbose bool) (*ghclient.PRView, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetPR(ctx, owner, repo, number, verbose)
}

func (c *LocalClient) GithubGetPRDiff(ctx context.Context, owner, repo string, number int, file string) (string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return client.GetPRDiff(ctx, owner, repo, number, file)
}

func (c *LocalClient) GithubGetPRFiles(ctx context.Context, owner, repo string, number int) ([]ghclient.PRFile, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetPRFiles(ctx, owner, repo, number)
}

func (c *LocalClient) GithubGetCommentThread(ctx context.Context, owner, repo string, commentID, page int) (*ghclient.CommentThread, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetCommentThread(ctx, owner, repo, commentID, page)
}

func (c *LocalClient) GithubGetReviewDetail(ctx context.Context, owner, repo string, number, reviewID int, verbose bool) (*ghclient.ReviewDetail, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetReviewDetail(ctx, owner, repo, number, reviewID, verbose)
}

func (c *LocalClient) GithubDismissReview(ctx context.Context, owner, repo string, number, reviewID int, message string) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.DismissReview(ctx, owner, repo, number, reviewID, message)
}

func (c *LocalClient) GithubSubmitReview(ctx context.Context, owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, "", err
	}
	return client.SubmitReview(ctx, owner, repo, number, commitSHA, event, body, comments)
}

func (c *LocalClient) GithubCreatePR(ctx context.Context, owner, repo, head, base, title, body string, draft bool) (int, string, string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, "", "", err
	}
	number, htmlURL, nodeID, err := client.CreatePR(ctx, owner, repo, head, base, title, body, draft)
	if err != nil {
		return 0, "", "", err
	}
	c.recordGithubPR(ctx, owner, repo, head, base, title, body, number, nodeID, htmlURL, draft)
	return number, htmlURL, nodeID, nil
}

// GithubCreatePendingReview records this run's review draft — a fully TF-side
// artifact, with zero GitHub writes (TFAC-494). It mints a run-scoped `review`
// artifact (state=pending, no ready sentinel, empty ExternalID; the reviewed
// commitSHA pinned into details for the atomic submit at approval) and returns
// the artifact id as the local review handle the agent passes to add/finalize.
//
// Because the dedup key is run-scoped, two runs (different teams, a re-delegate,
// interactive steering) reviewing the same PR each get their own draft row and
// never collide — the multi-tenant pending-review-slot collision the
// GitHub-native model (TFAC-463) introduced is gone, along with the identity
// branch and ErrPendingReviewCollision. The comments param is unused (start-review
// seeds no inline comments; the agent adds them via add-review-comment).
func (c *LocalClient) GithubCreatePendingReview(ctx context.Context, owner, repo string, number int, commitSHA string, _ []ghclient.SubmitReviewComment) (string, error) {
	a := domain.NewReviewArtifact(owner+"/"+repo, number, commitSHA, c.info.RunID)
	stored, err := c.UpsertArtifact(ctx, a)
	if err != nil {
		return "", fmt.Errorf("record review draft artifact: %w", err)
	}
	return stored.ID, nil
}

// GithubAddPendingReviewComment appends one inline comment to this run's review
// draft, staged entirely TF-side (TFAC-494) — no GitHub *write*. It locates the
// run's pending review artifact, validates the local handle, mints a stable
// TF-local comment id, appends it to details.StagedComments, and returns the id.
// body already carries the severity badge baked in (the caller bakes it);
// line>0 with a non-nil startLine makes it a multi-line range.
//
// commitSHA is the anchor: the commit GitHub reads the comment's line in the
// frame of, carried through to the atomic submit at approval. When the CLI
// supplied it (the normal path — the agent has a worktree), it's the worktree
// HEAD, and the comment is validated against THAT commit's diff (compare
// base...HEAD) — the same frame the agent read the code in, so a line it saw
// maps to the line GitHub anchors to. When it's empty (no checkout), the host
// anchors to and validates against the live PR head instead — the pre-TFAC-494
// behavior, internally consistent on its own.
func (c *LocalClient) GithubAddPendingReviewComment(ctx context.Context, owner, repo, reviewID, path, body string, line int, startLine *int, commitSHA string) (string, error) {
	art, err := c.runReviewArtifact(ctx, reviewID)
	if err != nil {
		return "", err
	}
	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		return "", fmt.Errorf("parse review artifact details: %w", err)
	}
	// Staging onto a finalized review (ready sentinel set) would mutate a draft the
	// human is already approving — and the Proposed snapshot is frozen, so the new
	// comment would never enter the verdict diff. Reject it.
	if details.ReviewEvent != "" {
		return "", fmt.Errorf("this review has already been finalized for approval; start a fresh review to add more comments")
	}

	// The PR number comes from the artifact target (the handle the agent passes
	// carries no coordinates). Needed for both the live-head fallback and to
	// resolve the base ref the compare-validation diffs against.
	_, _, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return "", fmt.Errorf("review artifact has a malformed target %q", art.Target)
	}
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	pr, err := client.GetPRBasic(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("fetch PR for comment anchor: %w", err)
	}

	var hunks map[string][]ghclient.Hunk
	anchorSHA := commitSHA
	if anchorSHA != "" {
		// Validate against the diff at the agent's checkout (base...worktree-HEAD),
		// so the commentable lines match what the agent read and the anchor frame.
		if pr.BaseRef == "" {
			return "", fmt.Errorf("PR #%d has no base ref; cannot validate a review comment against commit %s", number, anchorSHA)
		}
		hunks, err = hostCompareDiffHunks(ctx, client, owner, repo, pr.BaseRef, anchorSHA)
		if err != nil {
			return "", fmt.Errorf("fetch diff at commit %s for comment validation: %w", anchorSHA, err)
		}
	} else {
		// No local checkout: anchor + validate against the live head.
		if pr.HeadSHA == "" {
			return "", fmt.Errorf("PR #%d has no head commit SHA; cannot anchor a review comment", number)
		}
		hunks, err = hostDiffHunks(ctx, client, owner, repo, number)
		if err != nil {
			return "", fmt.Errorf("fetch PR diff for comment validation: %w", err)
		}
		anchorSHA = pr.HeadSHA
	}
	if msg := ghclient.ValidateCommentRange(hunks, path, line, startLine); msg != "" {
		return "", fmt.Errorf("%s", msg)
	}

	commentID := uuid.New().String()
	ln := line
	comment := domain.ReviewArtifactComment{
		ID:        commentID,
		Path:      path,
		Line:      &ln,
		StartLine: startLine,
		Body:      body,
		CommitSHA: anchorSHA,
	}
	details.StagedComments = append(details.StagedComments, comment)

	updated := *art
	updated.DetailsJSON = domain.MarshalReviewArtifactDetails(details)
	if _, err := c.UpsertArtifact(ctx, updated); err != nil {
		return "", fmt.Errorf("stage review comment: %w", err)
	}
	return commentID, nil
}

// UpdateStagedReviewComment rewrites the body of one staged comment on this run's
// review draft, addressed by the TF-local id add-review-comment minted. body
// already carries the (re-baked) severity badge — the CLI bakes it, mirroring
// add-review-comment — so the comment submits verbatim at approval. Pure local
// write, no GitHub call. An unknown id is an error.
func (c *LocalClient) UpdateStagedReviewComment(ctx context.Context, commentID, body string) error {
	art, details, idx, err := c.runStagedComment(ctx, commentID)
	if err != nil {
		return err
	}
	if err := errIfFinalized(details); err != nil {
		return err
	}
	details.StagedComments[idx].Body = body

	updated := *art
	updated.DetailsJSON = domain.MarshalReviewArtifactDetails(details)
	if _, err := c.UpsertArtifact(ctx, updated); err != nil {
		return fmt.Errorf("update staged review comment: %w", err)
	}
	return nil
}

// DeleteStagedReviewComment removes one staged comment from this run's review
// draft by its TF-local id. Pure local write, no GitHub call. An unknown id is an
// error.
func (c *LocalClient) DeleteStagedReviewComment(ctx context.Context, commentID string) error {
	art, details, idx, err := c.runStagedComment(ctx, commentID)
	if err != nil {
		return err
	}
	if err := errIfFinalized(details); err != nil {
		return err
	}
	details.StagedComments = append(details.StagedComments[:idx], details.StagedComments[idx+1:]...)

	updated := *art
	updated.DetailsJSON = domain.MarshalReviewArtifactDetails(details)
	if _, err := c.UpsertArtifact(ctx, updated); err != nil {
		return fmt.Errorf("delete staged review comment: %w", err)
	}
	return nil
}

// errIfFinalized rejects a staged-comment mutation once the review has been
// finalized (the ready sentinel details.ReviewEvent is set). After finalize the
// Proposed snapshot is frozen and the review is awaiting human approval, where the
// approval UI diffs Proposed against the live StagedComments to show the human's
// edits — an agent (typically looping after finalize-review) mutating the staged
// set here would corrupt that diff, masquerading as a human edit or masking a real
// one. Mirrors the same guard GithubAddPendingReviewComment applies to staging.
// `start-review --fresh` is the supported way to reopen a finalized draft for more
// changes.
func errIfFinalized(details domain.ReviewArtifactDetails) error {
	if details.ReviewEvent != "" {
		return fmt.Errorf("this review has already been finalized for approval; run `gh pr start-review --fresh` to reopen it before changing comments")
	}
	return nil
}

// runStagedComment locates this run's pending review draft that holds the staged
// comment with the given TF-local id, returning the artifact, its parsed details,
// and the comment's index in details.StagedComments. The id is a UUID minted at
// add-review-comment, unique within the run, so scanning every pending review
// draft (a run can hold one per PR) finds the owning draft. Returns an actionable
// error when no draft carries it — the most likely cause is the agent passing a
// numeric (live GitHub) comment id, which belongs on the REST update/delete path.
func (c *LocalClient) runStagedComment(ctx context.Context, commentID string) (*domain.Artifact, domain.ReviewArtifactDetails, int, error) {
	arts, err := c.listArtifactsByRun(ctx)
	if err != nil {
		return nil, domain.ReviewArtifactDetails{}, 0, err
	}
	for i := range arts {
		if arts[i].Kind != domain.ArtifactKindReview || arts[i].State != domain.ArtifactStateReviewPending {
			continue
		}
		d, derr := domain.ParseReviewArtifactDetails(arts[i].DetailsJSON)
		if derr != nil {
			continue
		}
		for idx := range d.StagedComments {
			if d.StagedComments[idx].ID == commentID {
				return &arts[i], d, idx, nil
			}
		}
	}
	return nil, domain.ReviewArtifactDetails{}, 0, fmt.Errorf(
		"no staged review comment %q for this run — its id comes from add-review-comment (a numeric id is a live GitHub comment, edited via the REST path)", commentID)
}

// hostDiffHunks fetches the PR's diff and returns the per-file hunk ranges
// ValidateCommentRange checks a review comment's (path, line, start_line)
// against. Tries the verbatim diff first; on HTTP 406 ("diff too large") GitHub
// refuses it, so it reassembles hunks from the per-file patches instead.
func hostDiffHunks(ctx context.Context, client *ghclient.Client, owner, repo string, number int) (map[string][]ghclient.Hunk, error) {
	diff, err := client.GetPRDiff(ctx, owner, repo, number, "")
	if err != nil {
		if ghclient.IsHTTP406(err) {
			files, ferr := client.GetPRFiles(ctx, owner, repo, number)
			if ferr != nil {
				return nil, ferr
			}
			return ghclient.DiffHunksFromPatches(files), nil
		}
		return nil, err
	}
	return ghclient.DiffHunks(diff), nil
}

// compareOrFallback fetches the base...head compare diff and parses it with
// fromDiff, falling back to the compare endpoint's per-file patches (parsed with
// fromPatches) when GitHub refuses the diff media type with HTTP 406 ("diff too
// large"). Any other error propagates. This is the shared diff-or-patches control
// flow behind hostCompareDiffHunks (validation hunks) and hostCompareLineMap
// (forward line-map) — the two differ only in how they parse the result, so the
// 406-fallback logic lives in exactly one place.
func compareOrFallback[T any](
	ctx context.Context,
	client *ghclient.Client,
	owner, repo, base, head string,
	fromDiff func(string) T,
	fromPatches func([]ghclient.PRFile) T,
) (T, error) {
	diff, err := client.GetCompareDiff(ctx, owner, repo, base, head)
	if err != nil {
		var zero T
		if ghclient.IsHTTP406(err) {
			files, ferr := client.GetCompareFiles(ctx, owner, repo, base, head)
			if ferr != nil {
				return zero, ferr
			}
			return fromPatches(files), nil
		}
		return zero, err
	}
	return fromDiff(diff), nil
}

// hostCompareDiffHunks returns the per-file hunk ranges for base...head — the
// diff in a specific commit's frame, used to validate a review comment against
// the agent's worktree HEAD rather than the live PR head.
func hostCompareDiffHunks(ctx context.Context, client *ghclient.Client, owner, repo, base, head string) (map[string][]ghclient.Hunk, error) {
	return compareOrFallback(ctx, client, owner, repo, base, head, ghclient.DiffHunks, ghclient.DiffHunksFromPatches)
}

// hostCompareLineMap returns the forward line-map (TFAC-497) for base...head — the
// diff in commit base's frame mapped onto commit head's — used by the finalize
// freshness gate to translate a staged comment's anchored line forward to the
// current head.
func hostCompareLineMap(ctx context.Context, client *ghclient.Client, owner, repo, base, head string) (ghclient.LineMap, error) {
	return compareOrFallback(ctx, client, owner, repo, base, head, ghclient.ParseLineMap, ghclient.ParseLineMapFromPatches)
}

func (c *LocalClient) GithubAddComment(ctx context.Context, owner, repo string, number int, body string) (int, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	id, htmlURL, err := client.AddCommentWithURL(ctx, owner, repo, number, body)
	if err != nil {
		return 0, err
	}
	c.recordGithubComment(ctx, owner, repo, number, id, htmlURL)
	return id, nil
}

func (c *LocalClient) GithubReplyToComment(ctx context.Context, owner, repo string, number, commentID int, body string) (int, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	replyID, err := client.ReplyToComment(ctx, owner, repo, number, commentID, body)
	if err != nil {
		return 0, err
	}
	// A review-thread reply rides the review (no standalone `comment` artifact —
	// the same treatment every review line-comment gets, vs a top-level
	// AddComment), but unlike the staged pending-review comments it posts
	// IMMEDIATELY under the org credential. So the audit log of record captures it
	// as an artifact-less comment action (TFAC-483) — the Actions lens is the one
	// surface that records writes with no artifact. A 2xx with no id (unparseable
	// body) has no stable key, so recording is skipped (the reply still posted).
	if replyID > 0 {
		detail, _ := json.Marshal(map[string]any{"in_reply_to": commentID})
		c.recordBotAction(ctx, &domain.ExternalAction{
			Provider:   domain.ArtifactProviderGitHub,
			Action:     domain.ActionCommentPosted,
			Target:     fmt.Sprintf("%s/%s#%d", owner, repo, number),
			ExternalID: strconv.Itoa(replyID),
			URL:        fmt.Sprintf("%s#discussion_r%d", domain.GitHubPullURL(owner+"/"+repo, number), replyID),
			Credential: domain.CredentialGitHubApp,
			DetailJSON: string(detail),
		})
	}
	return replyID, nil
}

func (c *LocalClient) GithubReactToComment(ctx context.Context, owner, repo string, commentID int, emoji string) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.ReactToComment(ctx, owner, repo, commentID, emoji)
}

func (c *LocalClient) GithubUpdateComment(ctx context.Context, owner, repo string, commentID int, body string) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	isIssueComment, err := client.UpdateCommentScoped(ctx, owner, repo, commentID, body)
	if err != nil {
		return err
	}
	// Standalone comments only. A review line-comment — the pulls/comments
	// fallback inside UpdateCommentScoped — rides the review artifact, not a
	// comment artifact, so it records nothing here. The edit keeps the comment
	// posted; the upsert bumps the existing row (re-attributing it to this run),
	// and the store preserves the target/url an add-comment stamped, which this
	// id-only path can't recompute.
	if isIssueComment {
		c.recordGithubCommentState(ctx, commentID, domain.ArtifactStateCommentPosted)
		return nil
	}
	// A published review line-comment (isIssueComment == false → the PATCH hit
	// /pulls/comments/{id}) is artifact-less like a review-thread reply: it rides
	// the review, not a comment artifact, but is still an immediate org-credential
	// write the audit log of record must capture (TFAC-485). The pending-draft path
	// edits human-side via the server staging path and is unrecorded, so reaching
	// here is published-by-construction — no pending check needed. The id-only call
	// site has no PR number, so Target carries owner/repo (no #N) and URL is left
	// empty; the action + comment id are enough.
	c.recordBotAction(ctx, &domain.ExternalAction{
		Provider:   domain.ArtifactProviderGitHub,
		Action:     domain.ActionReviewCommentEdited,
		Target:     owner + "/" + repo,
		ExternalID: strconv.Itoa(commentID),
		Credential: domain.CredentialGitHubApp,
	})
	return nil
}

func (c *LocalClient) GithubDeleteComment(ctx context.Context, owner, repo string, commentID int) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	isIssueComment, err := client.DeleteCommentScoped(ctx, owner, repo, commentID)
	if err != nil {
		return err
	}
	// Standalone comments only — a review line-comment (the pulls/comments
	// fallback) rides the review artifact. Retire the artifact in place: the row
	// persists for the audit ledger, flipped to the deleted state rather than
	// dropped.
	if isIssueComment {
		c.recordGithubCommentState(ctx, commentID, domain.ArtifactStateCommentDeleted)
		return nil
	}
	// A published review line-comment delete (isIssueComment == false → the DELETE
	// hit /pulls/comments/{id}) is artifact-less: it rides the review, but is an
	// immediate org-credential write the audit log must capture (TFAC-485). Same
	// published-by-construction reasoning as the edit path above; the id-only call
	// site has no PR number, so Target carries owner/repo and URL stays empty.
	c.recordBotAction(ctx, &domain.ExternalAction{
		Provider:   domain.ArtifactProviderGitHub,
		Action:     domain.ActionReviewCommentDeleted,
		Target:     owner + "/" + repo,
		ExternalID: strconv.Itoa(commentID),
		Credential: domain.CredentialGitHubApp,
	})
	return nil
}

func (c *LocalClient) GithubAPIGet(ctx context.Context, owner, repo, path string) ([]byte, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.Get(ctx, path)
}

func (c *LocalClient) GithubDownloadArtifact(ctx context.Context, owner, repo, path string, dst io.Writer, maxBytes int64) (int64, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	return client.DownloadArtifact(ctx, path, dst, maxBytes)
}

// --- github artifact recording (TFAC-466) ---
//
// Record one durable `artifacts` row per successful *standalone* GitHub comment
// the agent posts via `exec gh pr` — a top-level PR/issue comment, not a review
// line-comment (those ride the review artifact, handled separately). Like the
// Jira recording above this lives on the LocalClient, the single seam the multi
// daemon dispatches every host-routed call through, so it covers both the
// sandbox (daemon) and the local-mode CLI; and like it, recording is
// best-effort and on-success-only — the comment already posted, so a store
// failure is logged and swallowed, never able to fail the action.
//
// add → comment-update → comment-delete all key on github:comment:<id>, so the
// three collapse onto one row: add stamps the full coordinates (target
// owner/repo#N, the comment id, its html_url), update bumps it in place, delete
// retires it to the deleted state. update/delete only know the comment id (not
// its PR number), so they leave target/url empty and lean on the store
// preserving them (see ArtifactStore.Upsert).

// recordGithubComment upserts the `comment` artifact for a freshly posted
// standalone comment, with every coordinate the add path knows: target
// owner/repo#N, external_id + dedup key on the comment id, and the html_url
// GitHub returned. A zero id (a 2xx whose body didn't parse) has no stable key
// to dedup on, so recording is skipped with a debug breadcrumb — the comment
// still posted.
func (c *LocalClient) recordGithubComment(ctx context.Context, owner, repo string, number, commentID int, htmlURL string) {
	if c.stores.Artifacts == nil {
		return
	}
	if commentID <= 0 {
		agenthostLog.Debug("github comment recorded without an id; skipping artifact",
			"run", c.info.RunID, "repo", owner+"/"+repo, "pr", number)
		return
	}
	a := domain.Artifact{
		Kind:       domain.ArtifactKindComment,
		Target:     fmt.Sprintf("%s/%s#%d", owner, repo, number),
		ExternalID: strconv.Itoa(commentID),
		URL:        htmlURL,
		State:      domain.ArtifactStateCommentPosted,
		DedupKey:   githubCommentDedupKey(commentID),
	}
	c.upsertGithubArtifact(ctx, a, githubAction(a, domain.ActionCommentPosted, "", ""))
}

// recordGithubCommentState upserts the comment artifact for an edit (state
// posted) or a delete (state deleted) keyed on the same comment id. The caller
// only reaches here for a standalone (top-level issue) comment — review
// line-comments are filtered out upstream. These paths carry only the id — not
// the PR number — so they leave target/url empty; the store preserves whatever
// the add path stamped. If the comment was never recorded (e.g. the agent edits
// a human's top-level comment), the upsert mints a minimal row keyed on the id,
// which still truthfully records that this run touched it.
func (c *LocalClient) recordGithubCommentState(ctx context.Context, commentID int, state string) {
	if c.stores.Artifacts == nil {
		return
	}
	if commentID <= 0 {
		agenthostLog.Debug("github comment state change without an id; skipping artifact",
			"run", c.info.RunID, "state", state)
		return
	}
	// The audit action follows the state: a top-level comment edit re-stamps the
	// row posted (→ comment_edited), a delete flips it deleted (→ comment_deleted).
	action := domain.ActionCommentEdited
	if state == domain.ArtifactStateCommentDeleted {
		action = domain.ActionCommentDeleted
	}
	a := domain.Artifact{
		Kind:       domain.ArtifactKindComment,
		ExternalID: strconv.Itoa(commentID),
		State:      state,
		DedupKey:   githubCommentDedupKey(commentID),
	}
	c.upsertGithubArtifact(ctx, a, githubAction(a, action, "", ""))
}

// --- github PR artifact recording ---
//
// Record the durable `pull_request` artifact when the agent opens a draft PR via
// `exec gh pr create`. Like the comment/Jira recording above this lives on the
// LocalClient — the single seam the multi daemon dispatches every host-routed
// GitHub call through (server.go's dispatch builds NewLocal) — so it covers both
// the sandbox (daemon) and the local-mode CLI. Best-effort and on-success-only:
// the PR is already open on GitHub by the time we get here, so a store failure is
// logged and swallowed, never able to fail the action. The artifact dedup key
// (github:pull_request:owner/repo#<number>) is the new idempotency guard that
// replaces the retired pending_prs one-per-run lock.

// recordGithubPR upserts the `pull_request` artifact for a freshly opened PR. The
// proposed snapshot {title, body} is the agent's draft exactly as sent to GitHub
// (no footer — that lands at human approval); node_id / head_branch / base ride
// details_json for reconciliation and the server's edit/approve handlers. A zero
// number (a 2xx whose body didn't parse) has no stable key to
// dedup on, so recording is skipped with a debug breadcrumb — the PR still
// opened. The `draft` flag selects the artifact's initial state (draft vs open);
// the new PR path always creates drafts, but recording the param keeps the row
// honest if a standalone caller ever opens a non-draft.
func (c *LocalClient) recordGithubPR(ctx context.Context, owner, repo, head, base, title, body string, number int, nodeID, htmlURL string, draft bool) {
	if c.stores.Artifacts == nil {
		return
	}
	if number <= 0 {
		agenthostLog.Debug("github PR recorded without a number; skipping artifact",
			"run", c.info.RunID, "repo", owner+"/"+repo)
		return
	}
	a := domain.NewPullRequestArtifact(
		owner+"/"+repo, number, nodeID, head, base, htmlURL, title, body, draft,
	)
	// to=the created state (draft/open) so the feed can read "created as draft".
	c.upsertGithubArtifact(ctx, a, githubAction(a, domain.ActionPRCreated, "", a.State))
}

// githubCommentDedupKey is the stable key every comment action upserts on:
// github:comment:<id>.
func githubCommentDedupKey(commentID int) string {
	return domain.ArtifactDedupKey(domain.ArtifactProviderGitHub, domain.ArtifactKindComment, strconv.Itoa(commentID), "")
}

// upsertGithubArtifact stamps the provider onto a and writes it best-effort via
// the shared recording funnel (RecordExternalWrite, record.go): a recording
// failure is logged and swallowed so it never fails the agent's already-
// applied GitHub action. act is the external-action audit row to append in the
// SAME write as the artifact (the audit log of record, TFAC-483) — nil when
// this upsert carries no distinct external action (a review reuse that
// re-records the artifact without a new GitHub write).
func (c *LocalClient) upsertGithubArtifact(ctx context.Context, a domain.Artifact, act *domain.ExternalAction) {
	a.Provider = domain.ArtifactProviderGitHub
	RecordExternalWrite(ctx, c.stores, c.info, &a, act)
}

// --- external-action recording (TFAC-483) ---
//
// The bot funnels compose an append-only external_actions row alongside the
// artifact upsert (the audit log of record for every org-credential write). The
// credential is the org GitHub App / org Jira service account / Slack bot
// token by construction — exec resolves the org's system credential, never a
// user's own — so every bot write qualifies. run_id is this run, actor_user_id
// is the kicking-off user (empty → NULL for an event-triggered run, an
// autonomous system action).
//
// stampActionIdentity / recordActionSystem / resolveTouchedEntity are thin
// LocalClient-bound wrappers over the free functions in record.go — kept here
// so the several other call sites in this file (UpsertArtifact,
// RecordGitDenied, RecordGitPushFailed, branchPushAction) don't have to thread
// c.stores/c.info through by hand. Touch-recording itself
// (recordTouchInfo) has no LocalClient-bound wrapper: RecordExternalWrite
// (record.go) is the only caller, and it already holds (stores, info)
// directly.

// stampActionIdentity fills the run/org/team/actor common to every
// bot-attributed action from this client's RunInfo.
func (c *LocalClient) stampActionIdentity(act *domain.ExternalAction) {
	stampActionIdentityInfo(act, c.info)
}

// recordActionSystem appends act on the admin pool (event-triggered runs). A nil
// act — or a partial test wiring with no ExternalActions store — is a no-op.
func (c *LocalClient) recordActionSystem(ctx context.Context, act *domain.ExternalAction) error {
	return recordActionSystemInfo(ctx, c.stores, c.info, act)
}

// recordBotAction records one STANDALONE external-action audit row for a bot
// write that produces no artifact to compose with — a review-thread reply, a
// Slack reaction. Routes through the shared recording funnel
// (RecordExternalWrite, record.go) with a nil artifact; best-effort like every
// other funnel here.
func (c *LocalClient) recordBotAction(ctx context.Context, act *domain.ExternalAction) {
	RecordExternalWrite(ctx, c.stores, c.info, nil, act)
}

// resolveTouchedEntity turns act's (provider, target) into an entities row —
// see resolveTouchedEntityInfo (record.go) for the full rationale.
func (c *LocalClient) resolveTouchedEntity(ctx context.Context, act *domain.ExternalAction) (string, error) {
	return resolveTouchedEntityInfo(ctx, c.stores, c.info, act)
}

// RecordGitDenied appends a git_denied external-action audit row for a git op
// the per-run least-privilege gate refused — the git proxy (off-repo / off-ref
// / non-git path) or the exec-gh channel (off-repo). Host-side only (not on the
// Client interface): the gate runs on the host, never in the sandbox. ref is
// the offending ref on a ref-level denial (empty otherwise); op/reason are the
// gitproxy discriminators. Best-effort like the rest of recording — a failure
// is logged and swallowed, and a denial is a security signal recorded even for
// a denied read.
func (c *LocalClient) RecordGitDenied(ctx context.Context, owner, repo, ref, op, reason string) {
	if c.stores.ExternalActions == nil {
		return
	}
	detail, _ := json.Marshal(map[string]string{"op": op, "ref": ref, "reason": reason})
	c.recordBotAction(ctx, &domain.ExternalAction{
		Provider:   domain.ArtifactProviderGitHub,
		Action:     domain.ActionGitDenied,
		Target:     owner + "/" + repo,
		ExternalID: ref,
		Credential: domain.CredentialGitHubApp,
		DetailJSON: string(detail),
	})
}

// RecordGitPushFailed records one `branch_push_failed` external-action audit
// row for a branch push the upstream refused (a non-2xx receive-pack response —
// observed by the git proxy's outcome capture). The audit log records every
// attempted external write, success or failure; the branch ARTIFACT is
// deliberately not written here — artifacts track only work that landed, and
// the proxy wiring only upserts one on a 2xx. Host-side only (not on the
// Client interface): like RecordGitDenied, the observer runs on the host, never
// in the sandbox. No dedup key (recordBotAction's store fills a uuid), so
// repeated failed attempts each leave their own row and a later successful push
// of the same (run, ref, sha) can never be swallowed by a prior failure. No URL
// either — nothing landed, so a github.com branch link would dangle.
// Best-effort: a recording failure is logged and swallowed.
func (c *LocalClient) RecordGitPushFailed(ctx context.Context, repoPath, ref, sha string, created bool, httpStatus int) {
	if c.stores.ExternalActions == nil {
		return
	}
	detail, _ := json.Marshal(map[string]any{"sha": sha, "new": created, "http_status": httpStatus})
	c.recordBotAction(ctx, &domain.ExternalAction{
		Provider:   domain.ArtifactProviderGitHub,
		Action:     domain.ActionBranchPushFailed,
		Target:     repoPath,
		ExternalID: ref,
		Credential: domain.CredentialGitHubApp,
		DetailJSON: string(detail),
	})
}

// githubAction builds the external-action row for a GitHub write from the
// artifact's coordinates. detail_json is left empty — the rich payload (PR body,
// review draft) lives on the artifact; the audit row carries who/what/when/from→to.
func githubAction(a domain.Artifact, action, from, to string) *domain.ExternalAction {
	return &domain.ExternalAction{
		Provider:   domain.ArtifactProviderGitHub,
		Action:     action,
		Target:     a.Target,
		ExternalID: a.ExternalID,
		URL:        a.URL,
		FromState:  from,
		ToState:    to,
		Credential: domain.CredentialGitHubApp,
	}
}

// jiraAction builds the external-action row for a Jira write, carrying the
// artifact's concise details (status / assignee / fields / comment snippet) into
// the audit detail_json.
func jiraAction(a domain.Artifact, action, from, to string) *domain.ExternalAction {
	return &domain.ExternalAction{
		Provider:   domain.ArtifactProviderJira,
		Action:     action,
		Target:     a.Target,
		ExternalID: a.ExternalID,
		URL:        a.URL,
		FromState:  from,
		ToState:    to,
		Credential: domain.CredentialJiraOrg,
		DetailJSON: a.DetailsJSON,
	}
}

// branchPushAction builds the ActionBranchPushed row for a branch artifact, or
// nil for any other kind (a review-draft snapshot is not a fresh external write).
// The dedup key is deterministic — branch:<run>:<ref>:<sha> — so the git pre-push
// hook and the git-proxy backstop, which both observe the same push, collapse to
// one row; a force-push (new sha) is recorded distinctly. The push lands on
// GitHub under the org App credential.
func (c *LocalClient) branchPushAction(a domain.Artifact) *domain.ExternalAction {
	if a.Kind != domain.ArtifactKindBranch {
		return nil
	}
	sha := domain.ParseBranchArtifactSHA(a.DetailsJSON)
	act := &domain.ExternalAction{
		Provider:   domain.ArtifactProviderGitHub,
		Action:     domain.ActionBranchPushed,
		Target:     a.Target,
		ExternalID: a.ExternalID, // the ref (refs/heads/...)
		URL:        a.URL,
		ToState:    domain.ArtifactStateBranchPushed,
		Credential: domain.CredentialGitHubApp,
		DedupKey:   domain.BranchPushDedupKey(c.info.RunID, a.ExternalID, sha),
		DetailJSON: a.DetailsJSON, // {"sha":...,"new":...}
	}
	c.stampActionIdentity(act)
	return act
}

// --- extensions ---

// CallExtension is the entitlement-gated dispatch point for ee-registered
// agent-facing CLI verbs (see extension.go). Both LocalClient (local mode,
// and the daemon's per-request NewLocal in server.go) and IPCClient (the
// sandbox transport) route through this one check, so a verb author cannot
// forget it.
func (c *LocalClient) CallExtension(ctx context.Context, namespace, method string, args json.RawMessage) (json.RawMessage, error) {
	return callExtension(ctx, c.info, namespace, method, args)
}
