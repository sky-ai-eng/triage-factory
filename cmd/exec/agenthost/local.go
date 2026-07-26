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

	// rt is the seam every DB-backed effect routes through: directRuntime over
	// db.Stores in all/local (and when the orchestrator serves a relayed op),
	// or relayRuntime over the supervision channel in the capless sidecar. The
	// resolver-path fields below (ghResolver, and the Jira resolver built inline)
	// still read c.stores directly, but they are never reached when proxyCreds
	// is set — the executor sidecar builds a LocalClient with nil stores + a
	// relayRuntime + proxyCreds, so no store field is dereferenced there.
	rt Runtime

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

	// gateWired reports whether the exec-gh repo authorization gate can run —
	// true whenever the runtime can serve the team-tracks + conversation_worktrees reads
	// (always, on the sidecar's relay runtime and a fully-wired all/local
	// LocalClient). A partial test wiring (nil stores) leaves it false so the
	// gate no-ops rather than dereferencing an absent store. This replaces the
	// former `c.stores.X == nil` skip, which would have silently DISABLED the
	// gate on the sidecar (whose LocalClient holds nil stores by design).
	gateWired bool
}

// NewLocal builds a LocalClient bound to the given stores + identity, with an
// in-process directRuntime over those stores. Callers that resolve identity
// from env (AutoDetect's local branch) hand the resolved RunInfo here; the
// daemon hands the per-socket run info here at request dispatch.
func NewLocal(stores db.Stores, info RunInfo) *LocalClient {
	return &LocalClient{
		stores:    stores,
		info:      info,
		rt:        newDirectRuntime(stores, info),
		gateWired: stores.TeamGitHubRepos != nil && stores.RunWorktrees != nil,
	}
}

// SetGitHubResolver overrides the credential resolver this client resolves gh
// verbs through, AND the one the in-process runtime's review-posture identity
// probe uses. They must be the same object: two resolvers would mean the posture
// decision classified a different credential than the one that goes on to make
// the call. Used by the local-mode wiring and by tests injecting a fake tier.
func (c *LocalClient) SetGitHubResolver(r ghclient.Resolver) {
	c.ghResolver = r
	if dr, ok := c.rt.(*directRuntime); ok {
		dr.ghResolver = r
	}
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

// FinalizeReviewDraft is the host side of `gh pr finalize-review`: it locates the
// run's review artifact, gates on comment freshness vs. the PR's current head,
// stages the body + event, snapshots the agent's draft (body + event + the
// locally staged inline comments) into details.proposed as the approve-time
// human-feedback baseline, and sets the ready sentinel
// (details_json.review_event) that marks the draft finished — the guard against
// a second finalize, whether the first one staged the review or posted it.
//
// Whether the review then goes to GitHub is the team's posture,
// resolved here and nowhere else: this is the only place holding the DB handle,
// the team settings, and the credential resolver — the in-sandbox verb has none
// of them and must not learn about posture. Under a staging posture nothing
// reaches GitHub (the atomic create+submit happens at approval, exactly as
// before); under a posting posture the review is submitted through the shared
// publish path the human-approval handler uses. The result reports which.
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
func (c *LocalClient) FinalizeReviewDraft(ctx context.Context, reviewID, event, body string) (ReviewFinalizeResult, error) {
	staged := ReviewFinalizeResult{}
	art, err := c.runReviewArtifact(ctx, reviewID)
	if err != nil {
		return staged, err
	}

	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		return staged, fmt.Errorf("parse review artifact details: %w", err)
	}
	// Anti-double-submit (TFAC-358): the ready sentinel is already set, so the
	// agent already finalized this review. Hard error so it stops looping. This
	// holds under every posture — the sentinel is armed before any GitHub write,
	// so a second call is refused whether the first one posted or staged.
	if details.ReviewEvent != "" {
		return staged, ErrReviewAlreadyFinalized
	}

	// A comment / request_changes review must carry something actionable: a body,
	// inline comments, or both. An approve needs neither (the approval is the
	// signal). The comments are the locally staged set the agent added.
	if body == "" && event != "APPROVE" && len(details.StagedComments) == 0 {
		return staged, fmt.Errorf("a %s review needs --body/--body-file or at least one inline comment", strings.ToLower(event))
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
			return staged, err
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

	if err := c.saveReviewDraftDetails(ctx, art.ID, details); err != nil {
		return staged, fmt.Errorf("snapshot review draft into artifact: %w", err)
	}

	// Posture decision AFTER the snapshot, never before: the guarded write is
	// what arms the anti-double-submit sentinel, so ordering it first makes the
	// auto-post at-most-once. A crash (or a GitHub failure) between here and the
	// submit leaves a finalized draft a human can still approve — the inverse
	// order would leave a posted review that finalize would happily post again.
	if !c.shouldPostReview(ctx, art, details) {
		return staged, nil
	}
	return c.postFinalizedReview(ctx, art, details), nil
}

// shouldPostReview resolves the team's review posture and answers whether this
// finalized draft goes straight to GitHub. Both inputs (the team setting, the
// acting credential's identity) come from the runtime, so the answer is the same
// in-process on all/local and relayed from the capless sidecar.
//
// A resolution failure stages. The posture setting exists to let a team opt INTO
// posting; a backend blip must not be the thing that decides to publish, and the
// staged draft loses nothing — a human can approve it.
func (c *LocalClient) shouldPostReview(ctx context.Context, art *domain.Artifact, details domain.ReviewArtifactDetails) bool {
	owner, repo, _, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return false
	}
	res, err := c.rt.ReviewPosture(ctx, owner, repo)
	if err != nil {
		agenthostLog.Warn("review posture unresolved; staging the review for approval",
			"run", c.info.RunID, "artifact", art.ID, "error", err)
		return false
	}
	return review.PostureSubmits(res.Posture, res.Identity, details.ReviewEvent, details.StagedComments)
}

// postFinalizedReview submits an already-finalized draft to GitHub through the
// shared publish path (internal/review.SubmitStaged — the same function the
// human-approval handler calls, so comment anchoring, the commit pin, and the
// composed deep link behave identically), then records the artifact as submitted
// with the posted review's id + URL and appends the review_submitted audit row.
//
// A submit failure is not fatal: the draft stays finalized and pending, which is
// precisely the state a human approves from, so the run reports the staged
// outcome and the review is recoverable in the UI. It is logged host-side —
// stdout belongs to the verb's JSON.
func (c *LocalClient) postFinalizedReview(ctx context.Context, art *domain.Artifact, details domain.ReviewArtifactDetails) ReviewFinalizeResult {
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return ReviewFinalizeResult{}
	}
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		agenthostLog.Warn("auto-post review: no GitHub client; leaving the review staged for approval",
			"run", c.info.RunID, "artifact", art.ID, "error", err)
		return ReviewFinalizeResult{}
	}
	// The footer is the same run-attribution block the approval path appends, so
	// an auto-posted review carries the identical provenance line.
	footer, ferr := c.rt.AgentFooter(ctx, "review")
	if ferr != nil {
		agenthostLog.Warn("auto-post review: agent footer unavailable; posting without it",
			"run", c.info.RunID, "artifact", art.ID, "error", ferr)
		footer = ""
	}
	res, err := review.SubmitStaged(ctx, client, review.SubmitInput{
		Owner:   owner,
		Repo:    repo,
		Number:  number,
		Details: details,
		Footer:  footer,
		WebBase: func() string {
			base, berr := c.githubResolver().BaseURLFor(ctx, c.info.OrgID)
			if berr != nil {
				agenthostLog.Warn("auto-post review: github base unresolved; using default host",
					"run", c.info.RunID, "artifact", art.ID, "error", berr)
				return ""
			}
			return base
		},
	})
	if err != nil {
		agenthostLog.Warn("auto-post review: submit failed; the review stays staged for human approval",
			"run", c.info.RunID, "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		return ReviewFinalizeResult{}
	}

	// Record what landed: the event GitHub was asked for, the review's id + URL,
	// and the pending → submitted flip, composed with the audit row in one write.
	// Unlike the approval path there is no CAS claim to release — the run owns
	// this draft and the submit already happened, so the row is brought in line
	// with GitHub best-effort (the recording funnel logs and swallows failures
	// rather than unwinding an applied external write).
	details.ReviewEvent = res.Event
	submitted := *art
	submitted.State = domain.ArtifactStateReviewSubmitted
	submitted.ExternalID = strconv.Itoa(res.ReviewID)
	submitted.URL = res.URL
	submitted.DetailsJSON = domain.MarshalReviewArtifactDetails(details)
	c.upsertGithubArtifact(ctx, submitted, githubAction(submitted, domain.ActionReviewSubmitted,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted))

	return ReviewFinalizeResult{Posted: true, URL: res.URL}
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

	if err := c.saveReviewDraftDetails(ctx, art.ID, details); err != nil {
		return "", "", fmt.Errorf("reset review draft: %w", err)
	}
	return art.ID, details.HeadSHA, nil
}

// listArtifactsByRun reads the calling run's artifacts through the runtime
// (directRuntime respects the event-vs-manual pool split; relayRuntime relays).
func (c *LocalClient) listArtifactsByRun(ctx context.Context) ([]domain.Artifact, error) {
	return c.rt.ListRunArtifacts(ctx)
}

// saveReviewDraftDetails persists a mutated review draft's details_json through
// the state-guarded write (pending only). Every draft mutation goes through
// this instead of UpsertArtifact: the writers read the draft, mutate details in
// memory, and write back — an unconditional upsert landing after a concurrent
// human approve/dismiss would write the stale pending state back over the
// resolved row, making a review that already posted to GitHub approvable (and
// submittable) again. A lost race surfaces as an actionable error.
func (c *LocalClient) saveReviewDraftDetails(ctx context.Context, artifactID string, details domain.ReviewArtifactDetails) error {
	updated, err := c.rt.UpdateReviewDetailsIfPending(ctx, artifactID, domain.MarshalReviewArtifactDetails(details))
	if err != nil {
		return err
	}
	if !updated {
		return fmt.Errorf("this review is no longer pending (it was submitted or dismissed while you were editing) — nothing was changed")
	}
	return nil
}

// --- workspace ---

func (c *LocalClient) GetConversation(ctx context.Context) (*domain.Conversation, error) {
	return c.rt.GetConversation(ctx)
}

func (c *LocalClient) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	return c.rt.GetTask(ctx, taskID)
}

func (c *LocalClient) ListRepos(ctx context.Context) ([]domain.RepoProfile, error) {
	return c.rt.ListRepos(ctx)
}

func (c *LocalClient) GetRepo(ctx context.Context, repoID string) (*domain.RepoProfile, error) {
	return c.rt.GetRepo(ctx, repoID)
}

func (c *LocalClient) TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error) {
	return c.rt.TeamTracksRepo(ctx, owner, repo)
}

func (c *LocalClient) GetRunWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.RunWorktree, error) {
	return c.rt.GetRunWorktreeByRepoRef(ctx, repoID, ref)
}

func (c *LocalClient) ListRunWorktrees(ctx context.Context) ([]domain.RunWorktree, error) {
	return c.rt.ListRunWorktrees(ctx)
}

func (c *LocalClient) InsertRunWorktree(ctx context.Context, row domain.RunWorktree) (bool, string, error) {
	return c.rt.InsertRunWorktree(ctx, row)
}

func (c *LocalClient) DeleteRunWorktreeByRepoRef(ctx context.Context, repoID, ref string) error {
	return c.rt.DeleteRunWorktree(ctx, repoID, ref)
}

// --- agent run footer ---

func (c *LocalClient) BuildAgentFooter(ctx context.Context, kind string) (string, error) {
	return c.rt.AgentFooter(ctx, kind)
}

// --- artifacts ---

// UpsertArtifact stamps the run identity onto the caller's polymorphic artifact
// and upserts it, composing the branch-push external action into the same write
// (TFAC-483). Routes through the runtime — directRuntime does the event-vs-manual
// pool/tx branch in-process; relayRuntime relays it as one op so the tx stays
// orchestrator-side. Returns the stored row. See TFAC-460.
func (c *LocalClient) UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error) {
	a = c.hostAnchorBranchURL(ctx, a)
	return c.rt.UpsertArtifact(ctx, a)
}

// hostAnchorBranchURL re-anchors a branch artifact's web URL to the org's own
// GitHub host — github.com, a GHES host, or a GHEC data-residency host. A branch
// artifact's URL is synthesized where the host isn't knowable (the sandbox
// pre-push hook holds no credentials; the domain constructor has no org
// context), so it is composed against github.com and corrected here. This is the
// one host-side choke point every branch write funnels through — the local
// mode hook's in-process client and the multi-mode git-proxy push recorder both
// upsert through this LocalClient — and it holds the credential resolver, so the
// link is fixed before the row is written.
//
// Branch is the only artifact kind with a synthesized URL: a PR carries GitHub's
// own html_url and a review's link is resolved server-side, so both already name
// the right host and pass through untouched. A base-resolve failure keeps the
// default github.com URL (the link degrades; the record does not), and a
// non-branch ref leaves the URL as-is rather than clobber it.
func (c *LocalClient) hostAnchorBranchURL(ctx context.Context, a domain.Artifact) domain.Artifact {
	if a.Kind != domain.ArtifactKindBranch {
		return a
	}
	base, err := c.githubResolver().BaseURLFor(ctx, c.info.OrgID)
	if err != nil {
		agenthostLog.Warn("resolve github base for branch URL failed; keeping default host",
			"run", c.info.RunID, "target", a.Target, "ref", a.ExternalID, "error", err)
		return a
	}
	if u, ok := domain.BranchArtifactWebURL(base, a.Target, a.ExternalID); ok {
		a.URL = u
	}
	return a
}

// --- jira ---
//
// jiraSystemClient builds the org's bot-attributed Jira client. In multi
// mode the daemon lives in the run's credential sidecar and proxyCreds is
// always set, so every Jira verb routes through the sidecar's REST proxy —
// no secret-store read anywhere in multi (the executor's store is disabled,
// and the fused single process that once read the live store is gone).
// Local mode falls through to the ForSystem resolver over the OS keychain
// on the user's own machine. Either way the agent process never holds the
// token.
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
	issue, err := client.GetIssue(ctx, key)
	if err != nil {
		return nil, err
	}
	// The addressed issue is the touched entity — best-effort, relayed on the
	// sidecar. URL is left empty; the poll cycle owns stub enrichment.
	c.rt.RecordReadTouch(ctx, domain.ArtifactProviderJira, key, "")
	return issue, nil
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
	if key == "" {
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
	c.rt.Record(ctx, &a, act)
}

// jiraSiteBase returns the org's configured Jira site URL, trailing slash
// trimmed, or "" if it's unreadable or unset. Best-effort (a read error maps to
// ""), routed through the runtime so it works whether the stores are in-process
// or the read relays. This is the human-facing site (e.g.
// https://acme.atlassian.net), not the API gateway base the Jira client talks
// to — the same source the poller stamps entity URLs from.
func (c *LocalClient) jiraSiteBase(ctx context.Context) string {
	base, err := c.rt.OrgJiraBaseURL(ctx)
	if err != nil {
		return ""
	}
	return base
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
		// Through SetGitHubResolver so the in-process runtime's review-posture
		// identity probe shares this one cache rather than building a second.
		c.SetGitHubResolver(ghclient.NewResolver(c.stores.Secrets, c.stores.GitHubApps, c.stores.Orgs, c.stores.Agents, nil))
	}
	return c.ghResolver
}

// GithubWebHostBase returns the org's user-facing GitHub host base (github.com,
// a GHES host, or a GHEC data-residency host) for this run. The pre-push hook's
// branch-capture gate uses it to decide whether a push's remote belongs to the
// org's GitHub before recording a branch artifact — local mode's only capture
// point, where the hook holds this in-process client. Same resolution
// (org_settings → github_url secret → default) hostAnchorBranchURL uses to
// anchor the recorded branch link, so the gate and the corrected URL agree on
// the host.
func (c *LocalClient) GithubWebHostBase(ctx context.Context) (string, error) {
	return c.githubResolver().BaseURLFor(ctx, c.info.OrgID)
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
// cloned task repo, recorded in conversation_worktrees, or a workspace-add'd one) and
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
// a repo its team tracks AND that it has EITHER materialized in conversation_worktrees
// (a delegated run's task repo — recorded at setup — or a workspace-add'd repo)
// OR that appears in the turn's pinned set (a curator turn, which creates no
// conversation_worktrees rows). Same team-tracks predicate as the git proxy's
// Authorize; the second arm is the conversation_worktrees ledger or the pinned set. A
// partial test wiring (nil stores) skips the gate. Both a hard deny and a
// fail-closed backend error record a git_denied audit row before returning, so
// a transient DB blip during a denied gh op still leaves an audit trail —
// symmetric with the proxy's "authorize-error" path.
//
// The three hard-deny outcomes are deliberately distinct, because the agent's
// recovery differs and a bare "not authorized" left it guessing (the common
// case: a Slack-triggered taskless run that hasn't cloned anything yet, so no
// repo is materialized). The git proxy's deny path mirrors these same three —
// keep the wording in sync with internal/delegate's gitDeny* builders:
//
//   - Untracked ("repo-not-tracked") — the repo isn't attached to this run's
//     team, so it never appears in this run's `workspace list` and the agent
//     cannot self-serve. Only a team admin adding it as a tracked repo helps.
//   - Tracked, delegated run, not materialized ("repo-not-materialized") — the
//     repo IS in this run's `workspace list` ("available"), just not persisted
//     into the run's worktrees yet. The agent fixes it with `workspace add`.
//   - Tracked, curator turn, not in the pinned set ("repo-not-attached") — a
//     curator turn authorizes off its fixed pinned set and materializes
//     nothing, so `workspace add` is NOT the fix; the repo is simply outside
//     the project. Point the agent at a repo the project already carries.
func (c *LocalClient) authorizeRepo(ctx context.Context, owner, repo string) error {
	if !c.gateWired {
		return nil
	}
	repoID := owner + "/" + repo
	tracks, err := c.rt.TeamTracksRepo(ctx, owner, repo)
	if err != nil {
		c.RecordGitDenied(ctx, owner, repo, "", "gh", "authorize-error")
		return fmt.Errorf("authorize repo %s/%s: %w", owner, repo, err)
	}
	if !tracks {
		c.RecordGitDenied(ctx, owner, repo, "", "gh", "repo-not-tracked")
		return fmt.Errorf("repo %s is not tracked by this team; a team admin must add it as a tracked repo in Settings before it can be used", repoID)
	}
	// Curator turns carry their authorized set explicitly (no conversation_worktrees
	// ledger); a delegated run passes an empty PinnedRepos so this arm is
	// inert and the ledger check below is the sole gate, unchanged.
	for _, p := range c.info.PinnedRepos {
		if strings.EqualFold(p, repoID) {
			return nil
		}
	}
	// A non-empty pinned set marks a curator turn: it materializes nothing, so
	// a tracked-but-unpinned repo is outside the project, not merely un-cloned.
	if len(c.info.PinnedRepos) > 0 {
		c.RecordGitDenied(ctx, owner, repo, "", "gh", "repo-not-attached")
		return fmt.Errorf("repo %s is not attached to this project, so it cannot be accessed here; use a repo attached to this project instead", repoID)
	}
	rows, lerr := c.rt.ListRunWorktrees(ctx)
	if lerr != nil {
		c.RecordGitDenied(ctx, owner, repo, "", "gh", "authorize-error")
		return fmt.Errorf("authorize repo %s/%s: %w", owner, repo, lerr)
	}
	for _, w := range rows {
		if strings.EqualFold(w.RepoID, repoID) {
			return nil
		}
	}
	c.RecordGitDenied(ctx, owner, repo, "", "gh", "repo-not-materialized")
	return fmt.Errorf("repo %s is tracked by this team but not yet materialized in this run; run 'workspace add %s' to persist it, then retry", repoID, repoID)
}

func (c *LocalClient) GithubGetPR(ctx context.Context, owner, repo string, number int, verbose bool) (*ghclient.PRView, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	pr, err := client.GetPR(ctx, owner, repo, number, verbose)
	if err != nil {
		return nil, err
	}
	c.touchPR(ctx, owner, repo, number)
	return pr, nil
}

func (c *LocalClient) GithubGetPRDiff(ctx context.Context, owner, repo string, number int, file string) (string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	diff, err := client.GetPRDiff(ctx, owner, repo, number, file)
	if err != nil {
		return "", err
	}
	c.touchPR(ctx, owner, repo, number)
	return diff, nil
}

func (c *LocalClient) GithubGetPRFiles(ctx context.Context, owner, repo string, number int) ([]ghclient.PRFile, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	files, err := client.GetPRFiles(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	c.touchPR(ctx, owner, repo, number)
	return files, nil
}

// GithubGetCommentThread fetches a review/issue comment thread by comment id.
// It does NOT touch here: the ghAPI seam (a mirror of *github.Client) carries
// no PR number, so `gh pr thread-view` records the PR touch CLI-side via
// Client.RecordReadTouch after this returns (cmd/exec/gh/pr.go).
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
	detail, err := client.GetReviewDetail(ctx, owner, repo, number, reviewID, verbose)
	if err != nil {
		return nil, err
	}
	// The PR is the addressed entity; the review is an attribute of it.
	c.touchPR(ctx, owner, repo, number)
	return detail, nil
}

// touchPR records a durable run→entity touch for the PR owner/repo#number an
// addressed read just resolved — best-effort via the runtime, so it relays to
// the orchestrator on the sidecar and never fails the read. URL is left empty:
// the poll cycle owns stub enrichment. Set-returning GitHub reads (pr list,
// actions download-logs) never call this — the rule is addressed → touch,
// returned-in-a-set → never.
func (c *LocalClient) touchPR(ctx context.Context, owner, repo string, number int) {
	c.rt.RecordReadTouch(ctx, domain.ArtifactProviderGitHub, domain.PullRequestTarget(owner+"/"+repo, number), "")
}

// RecordReadTouch is the Client-interface escape hatch for an addressed read
// whose host method can't build the entity target itself (gh pr thread-view —
// the PR number is a CLI positional the ghAPI seam doesn't carry). It routes to
// the same best-effort runtime touch the in-method reads use, so the sidecar
// relay and the local write behave identically.
func (c *LocalClient) RecordReadTouch(ctx context.Context, provider, target, url string) {
	c.rt.RecordReadTouch(ctx, provider, target, url)
}

// memoryLoadSources is the set of entity source values `memory load` accepts —
// the sources memory can attach to (internal/domain entity.Source). An
// unrecognized source is a usage error, distinct from a valid-source-but-
// unknown-entity miss the runtime returns as an empty result.
var memoryLoadSources = map[string]bool{
	domain.ArtifactProviderGitHub: true,
	domain.ArtifactProviderJira:   true,
	domain.ArtifactProviderSlack:  true,
}

// MemoryLoad validates the source and delegates to the runtime, which does the
// entity lookup + team-scoped memory read + best-effort touch (all direct-to-
// stores on all/local, relayed to the orchestrator on the executor sidecar
// where this LocalClient holds no stores). Validation lives here — not just in
// the CLI — so every caller of the Client seam gets the same guard, and an
// invalid source fails fast without a relay round-trip.
func (c *LocalClient) MemoryLoad(ctx context.Context, source, sourceID string, limit int) (*MemoryLoadResult, error) {
	if !memoryLoadSources[source] {
		return nil, fmt.Errorf("invalid source %q: expected one of github, jira, slack", source)
	}
	return c.rt.MemoryLoad(ctx, source, sourceID, limit)
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
// Exact (path, line, start_line, body) matches against an already-staged
// comment are deduped: the existing comment's id is returned and nothing new
// is appended. This is for agents, not humans — a delegated agent that
// re-adds the same finding (e.g. retrying after an ambiguous result, or
// re-walking its own findings list) would otherwise stage the identical
// comment twice, and both copies would survive to the submitted review. A
// dedup hit is a no-op success rather than an error, so the agent doesn't
// "fix" it by rewording the duplicate into a near-duplicate. Same line,
// different body is a distinct finding and is never collapsed. The match is
// on content only, NOT commitSHA — a dedup hit still refreshes the existing
// comment's anchor to this call's (already-validated) commitSHA when it
// differs, so a re-add after the checkout advances doesn't leave the comment
// anchored to a stale commit.
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

	// Exact-match dedup: an agent that re-adds the same finding (a retry after an
	// ambiguous tool result, re-walking its own findings list) would otherwise stage
	// a second identical inline comment that survives to the submitted review. Only
	// an exact (path, line, start_line, body) match collapses — same line, different
	// body is a distinct finding and must not be silently dropped. Returning the
	// existing id keeps this a no-op success rather than an error the agent might
	// "fix" by rewording the duplicate into a near-duplicate.
	//
	// A dedup hit refreshes the existing comment's anchor to this call's anchorSHA
	// when it differs (e.g. the agent's checkout advanced and it revalidated the
	// same finding against the new head). anchorSHA has already been validated
	// above against ITS OWN diff, so re-anchoring here is as sound as staging a
	// fresh comment would have been — and it matters: leaving the stale anchor in
	// place risks finalize's forward-reconcile (TFAC-499) working from a commit
	// further behind than necessary, or, worse, re-introducing the exact duplicate
	// this dedup exists to prevent if the stale and fresh anchors ever diverge in
	// how they reconcile forward.
	for i := range details.StagedComments {
		existing := &details.StagedComments[i]
		if existing.Path == path && intPtrEq(existing.Line, &line) && intPtrEq(existing.StartLine, startLine) && existing.Body == body {
			if existing.CommitSHA != anchorSHA {
				existing.CommitSHA = anchorSHA
				if err := c.saveReviewDraftDetails(ctx, art.ID, details); err != nil {
					return "", fmt.Errorf("refresh staged review comment anchor: %w", err)
				}
			}
			return existing.ID, nil
		}
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

	if err := c.saveReviewDraftDetails(ctx, art.ID, details); err != nil {
		return "", fmt.Errorf("stage review comment: %w", err)
	}
	return commentID, nil
}

// intPtrEq reports whether two possibly-nil *int point to equal values —
// nil == nil, and a non-nil pair compares by dereferenced value.
func intPtrEq(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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

	if err := c.saveReviewDraftDetails(ctx, art.ID, details); err != nil {
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

	if err := c.saveReviewDraftDetails(ctx, art.ID, details); err != nil {
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
	c.rt.Record(ctx, &a, act)
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
// The DB effects here route through the runtime (c.rt): RecordGitDenied /
// RecordGitPushFailed build their audit action and hand it to recordBotAction →
// c.rt.Record, so the write hits db.Stores in-process (all/local) or relays to
// the orchestrator (sidecar) without this file threading stores by hand.

// recordBotAction records one STANDALONE external-action audit row for a bot
// write that produces no artifact to compose with — a review-thread reply, a
// Slack reaction. Routes through the shared recording funnel
// (RecordExternalWrite, record.go) with a nil artifact; best-effort like every
// other funnel here.
func (c *LocalClient) recordBotAction(ctx context.Context, act *domain.ExternalAction) {
	c.rt.Record(ctx, nil, act)
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

// branchPushActionInfo builds the ActionBranchPushed row for a branch artifact,
// or nil for any other kind. The dedup key is deterministic —
// branch:<run>:<ref>:<sha> — so the git pre-push hook and the git-proxy
// backstop, which both observe the same push, collapse to one row; a force-push
// (new sha) is recorded distinctly. The push lands on GitHub under the org App
// credential. Free-function form (the counterpart of the LocalClient method)
// so the direct runtime — which owns UpsertArtifact but holds no LocalClient —
// can build it from a RunInfo directly.
func branchPushActionInfo(a domain.Artifact, info RunInfo) *domain.ExternalAction {
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
		DedupKey:   domain.BranchPushDedupKey(info.RunID, a.ExternalID, sha),
		DetailJSON: a.DetailsJSON, // {"sha":...,"new":...}
	}
	stampActionIdentityInfo(act, info)
	return act
}

// --- extensions ---

// CallExtension is the entitlement-gated dispatch point for ee-registered
// agent-facing CLI verbs (see extension.go). Both LocalClient (local mode,
// and the daemon's per-request NewLocal in server.go) and IPCClient (the
// sandbox transport) route through this one check, so a verb author cannot
// forget it.
func (c *LocalClient) CallExtension(ctx context.Context, namespace, method string, args json.RawMessage) (json.RawMessage, error) {
	return callExtension(ctx, c.rt, namespace, method, args)
}
