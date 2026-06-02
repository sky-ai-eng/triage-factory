package agenthost

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
// without duplicating the if/else everywhere.
func (c *LocalClient) withWrite(
	ctx context.Context,
	system func() error,
	user func(ts db.TxStores) error,
) error {
	if c.info.IsEventTriggered {
		return system()
	}
	return c.stores.Tx.SyntheticClaimsWithTx(ctx, c.info.OrgID, c.info.UserID, user)
}

// --- pending reviews ---

func (c *LocalClient) GetPendingReview(ctx context.Context, reviewID string) (*domain.PendingReview, error) {
	return c.stores.Reviews.GetSystem(ctx, c.info.OrgID, reviewID)
}

func (c *LocalClient) CreatePendingReview(ctx context.Context, r domain.PendingReview) error {
	return c.withWrite(ctx,
		func() error { return c.stores.Reviews.CreateSystem(ctx, c.info.OrgID, r) },
		func(ts db.TxStores) error { return ts.Reviews.Create(ctx, c.info.OrgID, r) },
	)
}

func (c *LocalClient) DeletePendingReview(ctx context.Context, reviewID string) error {
	return c.withWrite(ctx,
		func() error { return c.stores.Reviews.DeleteSystem(ctx, c.info.OrgID, reviewID) },
		func(ts db.TxStores) error { return ts.Reviews.Delete(ctx, c.info.OrgID, reviewID) },
	)
}

func (c *LocalClient) LockReviewSubmission(ctx context.Context, reviewID, body, event string) error {
	return c.withWrite(ctx,
		func() error {
			return c.stores.Reviews.LockSubmissionSystem(ctx, c.info.OrgID, reviewID, body, event)
		},
		func(ts db.TxStores) error {
			return ts.Reviews.LockSubmission(ctx, c.info.OrgID, reviewID, body, event)
		},
	)
}

func (c *LocalClient) AddPendingReviewComment(ctx context.Context, comment domain.PendingReviewComment) error {
	return c.withWrite(ctx,
		func() error { return c.stores.Reviews.AddCommentSystem(ctx, c.info.OrgID, comment) },
		func(ts db.TxStores) error { return ts.Reviews.AddComment(ctx, c.info.OrgID, comment) },
	)
}

func (c *LocalClient) UpdatePendingReviewComment(ctx context.Context, commentID, body string) error {
	return c.withWrite(ctx,
		func() error {
			return c.stores.Reviews.UpdateCommentSystem(ctx, c.info.OrgID, commentID, body)
		},
		func(ts db.TxStores) error {
			return ts.Reviews.UpdateComment(ctx, c.info.OrgID, commentID, body)
		},
	)
}

func (c *LocalClient) DeletePendingReviewComment(ctx context.Context, commentID string) error {
	return c.withWrite(ctx,
		func() error { return c.stores.Reviews.DeleteCommentSystem(ctx, c.info.OrgID, commentID) },
		func(ts db.TxStores) error { return ts.Reviews.DeleteComment(ctx, c.info.OrgID, commentID) },
	)
}

func (c *LocalClient) ListPendingReviewComments(ctx context.Context, reviewID string) ([]domain.PendingReviewComment, error) {
	return c.stores.Reviews.ListCommentsSystem(ctx, c.info.OrgID, reviewID)
}

// --- pending PRs ---

func (c *LocalClient) GetPendingPRByRunID(ctx context.Context) (*domain.PendingPR, error) {
	return c.stores.PendingPRs.ByRunIDSystem(ctx, c.info.OrgID, c.info.RunID)
}

// CreateAndLockPendingPR collapses the old Create + Lock pair into a
// single agenthost call. The manual path runs both inside one
// synthetic-claims tx (atomic — a crash between Create and Lock used
// to strand an unlocked row, see the TODO removed in this refactor).
// The event-triggered path still does them as two admin-pool calls
// because there's no shared tx surface across the admin pool's
// statements; the second-layer Lock is still load-bearing for the
// rare insert-but-no-lock race two concurrent `pr create` invocations
// can produce.
func (c *LocalClient) CreateAndLockPendingPR(ctx context.Context, row domain.PendingPR) error {
	return c.withWrite(ctx,
		func() error {
			if err := c.stores.PendingPRs.CreateSystem(ctx, c.info.OrgID, row); err != nil {
				return err
			}
			return c.stores.PendingPRs.LockSystem(ctx, c.info.OrgID, row.ID, row.Title, row.Body)
		},
		func(ts db.TxStores) error {
			if err := ts.PendingPRs.Create(ctx, c.info.OrgID, row); err != nil {
				return err
			}
			return ts.PendingPRs.Lock(ctx, c.info.OrgID, row.ID, row.Title, row.Body)
		},
	)
}

func (c *LocalClient) LockPendingPR(ctx context.Context, id, title, body string) error {
	return c.withWrite(ctx,
		func() error { return c.stores.PendingPRs.LockSystem(ctx, c.info.OrgID, id, title, body) },
		func(ts db.TxStores) error { return ts.PendingPRs.Lock(ctx, c.info.OrgID, id, title, body) },
	)
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

func (c *LocalClient) GetRunWorktreeByRepo(ctx context.Context, repoID string) (*domain.RunWorktree, error) {
	return c.stores.RunWorktrees.GetByRepoSystem(ctx, c.info.OrgID, c.info.RunID, repoID)
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

func (c *LocalClient) DeleteRunWorktreeByRepo(ctx context.Context, repoID string) error {
	return c.withWrite(ctx,
		func() error {
			return c.stores.RunWorktrees.DeleteByRepoSystem(ctx, c.info.OrgID, c.info.RunID, repoID)
		},
		func(ts db.TxStores) error {
			return ts.RunWorktrees.DeleteByRepo(ctx, c.info.OrgID, c.info.RunID, repoID)
		},
	)
}

// --- chain ---

func (c *LocalClient) GetChainRunForRun(ctx context.Context) (*domain.BlueprintRun, *int, error) {
	return c.stores.Blueprints.GetRunForRunSystem(ctx, c.info.OrgID, c.info.RunID)
}

func (c *LocalClient) BuildAgentRunFooter(_ context.Context, kind string) (string, error) {
	return agentmeta.Build(c.stores.AgentRuns, c.info.OrgID, c.info.RunID, kind), nil
}

func (c *LocalClient) InsertChainVerdict(ctx context.Context, payload string) error {
	return c.withWrite(ctx,
		func() error {
			return c.stores.Blueprints.InsertVerdictSystem(ctx, c.info.OrgID, c.info.RunID, payload)
		},
		func(ts db.TxStores) error {
			return ts.Blueprints.InsertVerdict(ctx, c.info.OrgID, c.info.RunID, payload)
		},
	)
}
