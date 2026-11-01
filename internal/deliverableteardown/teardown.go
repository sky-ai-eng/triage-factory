// Package deliverableteardown retires every unresolved deliverable a task's
// conversations hold: each draft PR closed (TF-side and on GitHub), each
// pending review dismissed. Pushed branches are kept — retention is separate.
//
// It is the TASK-END gesture and nothing else. A requeue, a claim and a
// re-delegation all leave the deliverables standing, so the next attempt inherits
// the draft PR it is going to push to rather than starting from nothing.
//
// The body lives in its own package because both ends of a task's life run it:
// a person closing the card, through the server's tx-bound app-pool adapter
// with that person as the actor, and the event cascade closing it for them,
// through the spawner's admin-pool adapter with no actor at all. Only the pool,
// the actor and who the audit rows name as performing the close differ; the
// decision about which deliverables are unresolved and what retiring one means
// is the same decision, so it is written once.
package deliverableteardown

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Stores is the store surface one teardown batch reads and writes. Each method
// mirrors a db.ConversationStore / db.DeliverableStore / db.ExternalActionStore
// method one-for-one; the adapter behind it decides which pool answers and
// whether a transaction binds the batch.
type Stores interface {
	// ConversationsForTask lists every conversation the task holds — the
	// blueprint's step conversations and any standalone one. Keying the
	// teardown on the task's conversations rather than on a conversation
	// status is what makes it find a deliverable a long-terminal run left.
	ConversationsForTask(ctx context.Context, orgID, taskID string) ([]domain.Conversation, error)
	// DeliverablesForConversation lists one conversation's deliverables.
	DeliverablesForConversation(ctx context.Context, orgID, conversationID string) ([]domain.Deliverable, error)
	// UpsertDeliverable writes back one deliverable whose state this teardown flipped.
	UpsertDeliverable(ctx context.Context, orgID string, a domain.Deliverable) error
	// RecordExternalAction appends the audit row for a GitHub write this
	// teardown is about to make.
	RecordExternalAction(ctx context.Context, orgID string, act domain.ExternalAction) error
	// Entity and Repository are the point reads (db.EntityStore.Get,
	// db.RepositoryStore.Get) a draft PR's pull request is resolved through:
	// the entity it is about, and the registry row that entity references.
	Entity(ctx context.Context, orgID, id string) (*domain.Entity, error)
	Repository(ctx context.Context, orgID, id string) (*domain.Repository, error)
}

// Deps is what Teardown needs from the process running it.
type Deps interface {
	// Batch runs fn over the store surface. The server's adapter binds one
	// app-pool transaction, so the flips and their audit rows commit or roll
	// back together and a failed batch leaves the deliverables unresolved for the
	// next attempt — the flips re-target the same predicate set, so a retry is
	// safe. The System adapter has no transaction runner (the admin pool's
	// `...System` methods are per-call), so its batch is all-or-nothing only
	// per write; the same retry repairs a partial one.
	//
	// Teardown calls it twice: once to read what the task holds, and once —
	// after the credential classification, which can reach GitHub and must not
	// run inside a write transaction — to flip and audit.
	Batch(ctx context.Context, orgID string, fn func(Stores) error) error
	// GitHub is the per-repo client source the draft-PR closes are made
	// through, and the same source their credential attribution is classified
	// against. Read exactly once per teardown, so an adapter is free to take a
	// lock or otherwise pay for the read. Nil is tolerated: the closes are
	// skipped and every audit row records the App, exactly as an
	// unclassifiable repo does.
	GitHub() ghclient.Resolver
	// SettlePullRequest settles pull request deliverable a when it is still
	// pending (reconcile.Reconciler.SettleDeliverable), reading whether its
	// run opened the pull request, so a draft the run did open is closed on
	// GitHub rather than TF-side only. Called outside any batch, since a
	// settle can reach GitHub. Best-effort: an adapter with nothing to settle
	// through does nothing, and the draft is then closed TF-side only.
	SettlePullRequest(ctx context.Context, orgID string, a domain.Deliverable)
}

// Teardown resolves EVERY unresolved deliverable the task's conversations hold.
// Each draft PR is flipped to closed and then closed on GitHub. A draft TF has
// not yet confirmed its run opened is settled first (Deps.SettlePullRequest),
// and one still not confirmed after that (domain.Deliverable.Actionable) is
// closed TF-side only. Each pending review (finalized or not) is flipped to
// dismissed, which is its whole resolution — a review is staged TF-side, so
// there is no GitHub object to retire and no org-credential write to audit.
//
// performedBy names who performs the close on each audit row: a person
// closing the card, or TF itself for an event-driven close. actorUserID is the
// human who authorized it. Empty is the autonomous spelling: an event-driven
// close has no actor, and the row says so rather than naming whoever happened
// to be nearby.
//
// This never flips conversations.status. The caller stops the task's live runs
// one step ahead of it, which owns that transition; a terminal conversation
// simply stays terminal.
//
// The returned error is the batch's: the deliverables are unresolved and a retry
// is expected. A GitHub close that fails is not an error — the deliverable is
// already marked closed and reconciliation retires the object later.
func Teardown(ctx context.Context, deps Deps, orgID, taskID, performedBy, actorUserID string) error {
	// One resolver for the whole pass, read once and threaded through both
	// halves. An adapter's resolver is swapped under it on a GitHub config
	// change, and the two halves are two views of the same act: the credential
	// each audit row names, and the client the close that row describes is
	// actually made through. Re-reading per repo or per deliverable would let a
	// swap land between them and file a row asserting a tier no close used.
	resolver := deps.GitHub()

	// Which credential each draft PR's close is made under is classified, and
	// each pending draft settled, BEFORE the write batch opens, for the same
	// reason the closes themselves run after it: the audit rows are composed
	// inside that batch, and both can reach GitHub. The batch re-reads the
	// drafts, so it sees each settle's outcome.
	credentials := credentialsForTask(ctx, deps, resolver, orgID, taskID)

	// Draft PRs captured inside the batch (state already flipped), each with
	// the pull request it is about, and closed on GitHub after it lands — a
	// network call must not hold a write transaction open.
	var closes []draftClose
	if err := deps.Batch(ctx, orgID, func(st Stores) error {
		convs, err := st.ConversationsForTask(ctx, orgID, taskID)
		if err != nil {
			return fmt.Errorf("list conversations for task: %w", err)
		}
		for i := range convs {
			conversationID := convs[i].ID
			arts, artErr := st.DeliverablesForConversation(ctx, orgID, conversationID)
			if artErr != nil {
				return fmt.Errorf("deliverables for conversation %s: %w", conversationID, artErr)
			}
			draftPRs := domain.AllDraftPullRequests(arts)
			pendingReviews := domain.AllPendingReviewDeliverables(arts)
			if len(draftPRs) == 0 && len(pendingReviews) == 0 {
				continue
			}

			// Abandon each pending review by flipping its deliverable to
			// dismissed. No GitHub call and no audit row: the review is staged
			// TF-side, so a dismiss is a purely local state change rather than
			// an org-credential write (external_actions records only writes).
			// The proposed snapshot is preserved on the row.
			for j := range pendingReviews {
				dismissed := pendingReviews[j]
				dismissed.State = domain.DeliverableStateReviewDismissed
				if err := st.UpsertDeliverable(ctx, orgID, dismissed); err != nil {
					return fmt.Errorf("upsert dismissed review: %w", err)
				}
			}
			// Abandon each draft PR by flipping its deliverable to closed (the
			// GitHub close runs after the batch). The pushed branch and the
			// proposed snapshot are preserved — abandonment retires the PR
			// object, not the work.
			for j := range draftPRs {
				pr := draftPRs[j]
				closed := pr
				closed.State = domain.DeliverableStatePRClosed
				if err := st.UpsertDeliverable(ctx, orgID, closed); err != nil {
					return fmt.Errorf("upsert closed draft pr: %w", err)
				}
				// A row not yet confirmed as its run's is closed here only:
				// closing its pull request on GitHub would act, under TF's
				// credential, on one the run may not have opened.
				if !pr.Actionable() {
					continue
				}
				act := domain.DeliverableAction(&pr, performedBy, actorUserID, domain.ActionPRClosed,
					domain.DeliverableStatePRDraft, domain.DeliverableStatePRClosed,
					credentialFor(credentials, pr.ID))
				if err := st.RecordExternalAction(ctx, orgID, act); err != nil {
					return fmt.Errorf("record external action for closed draft pr: %w", err)
				}
				// The flip is the resolution; a pull request that cannot be
				// resolved is closed TF-side only, and reconciliation retires
				// the GitHub object later.
				resolved, err := db.ResolveDeliverablePullRequest(ctx, orgID, pr, st.Entity, st.Repository)
				if err != nil {
					teardownLog.Warn("resolve draft PR for its GitHub close failed; skipping the close (deliverable already marked closed)",
						"deliverable", pr.ID, "error", err)
					continue
				}
				closes = append(closes, draftClose{art: pr, pr: resolved})
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Resolve on GitHub (best-effort, outside the batch). The deliverables are
	// already marked closed; a GitHub failure leaves the object for
	// reconciliation to retire later and must never fail the close that
	// triggered this. Branches stay.
	for i := range closes {
		CloseDraftPR(ctx, resolver, orgID, &closes[i].art, closes[i].pr)
	}
	return nil
}

// draftClose is one draft PR the teardown flipped, with the pull request on
// GitHub it closes afterwards.
type draftClose struct {
	art domain.Deliverable
	pr  db.DeliverablePullRequest
}

// credentialsForTask classifies the acting GitHub credential for every draft PR
// the task's unresolved deliverables hold, keyed by deliverable id: the credential
// for the repository the pull request is in (db.ResolveDeliverablePullRequest),
// under the name GitHub serves it now. It exists so the teardown's audit rows
// can name that credential without probing GitHub from inside the write batch
// those rows are composed into — the read pass and the classification both
// finish before that batch opens. Its read pass is also where each pending
// draft is settled (Deps.SettlePullRequest), after the read and before the
// write batch.
//
// Best-effort: a read failure yields an empty map and every row falls back to
// the App, exactly as an unclassifiable repo does; so does a pull request that
// cannot be resolved, or whose repository's registry row is retired. The
// teardown itself is unaffected — it re-reads the deliverables under its own
// batch and is the authority on which ones it resolves.
//
// resolver is passed in rather than read from deps: it must be the same one the
// closes these rows describe are made through (see Teardown).
func credentialsForTask(ctx context.Context, deps Deps, resolver ghclient.Resolver, orgID, taskID string) map[string]string {
	type ownerRepo struct{ owner, repo string }
	repoOf := map[string]ownerRepo{} // deliverable id → repository
	var pending []domain.Deliverable
	if err := deps.Batch(ctx, orgID, func(st Stores) error {
		convs, err := st.ConversationsForTask(ctx, orgID, taskID)
		if err != nil {
			return err
		}
		for i := range convs {
			arts, artErr := st.DeliverablesForConversation(ctx, orgID, convs[i].ID)
			if artErr != nil {
				return artErr
			}
			for _, pr := range domain.AllDraftPullRequests(arts) {
				if pr.Verification == domain.DeliverableVerificationPending {
					pending = append(pending, pr)
				}
				resolved, err := db.ResolveDeliverablePullRequest(ctx, orgID, pr, st.Entity, st.Repository)
				if err != nil || resolved.Retired {
					continue
				}
				repoOf[pr.ID] = ownerRepo{owner: resolved.Owner, repo: resolved.Repo}
			}
		}
		return nil
	}); err != nil {
		teardownLog.Warn("pre-read draft PRs for credential attribution failed; teardown rows will record the app",
			"task", taskID, "error", err)
		return nil
	}
	for _, pr := range pending {
		deps.SettlePullRequest(ctx, orgID, pr)
	}
	// One classification per repository, however many draft PRs it holds.
	byRepo := map[ownerRepo]string{}
	out := make(map[string]string, len(repoOf))
	for deliverableID, or := range repoOf {
		cred, seen := byRepo[or]
		if !seen {
			cred = ghclient.CredentialForRepo(ctx, resolver, orgID, or.owner, or.repo)
			byRepo[or] = cred
		}
		out[deliverableID] = cred
	}
	return out
}

// credentialFor looks a draft PR up in the map credentialsForTask built. A miss
// — a pull request that could not be resolved, or a draft PR that appeared
// between the pre-pass and the batch — reports the App, the same answer an
// unclassifiable repo gets.
func credentialFor(credentials map[string]string, deliverableID string) string {
	if cred, hit := credentials[deliverableID]; hit {
		return cred
	}
	return domain.CredentialGitHubApp
}

// CloseDraftPR closes an abandoned draft PR on GitHub. Best-effort: every
// failure is logged, never returned — the deliverable is already marked closed and
// the task or conversation already resolved, so a GitHub hiccup mustn't unwind
// that. pr is the pull request the deliverable is about, resolved by the caller
// (db.ResolveDeliverablePullRequest): its repository's current name and its
// number. One whose repository's registry row is retired is not closed — GitHub
// serves the name as another repository now. The per-repo client resolves
// App-installation-token → PAT like every other PR mutation. A free function
// taking the resolver so the task-level teardown and the per-deliverable dismiss
// endpoint share one GitHub-resolution path. The pushed branch is never touched
// (retention is a separate concern).
func CloseDraftPR(ctx context.Context, resolver ghclient.Resolver, orgID string, art *domain.Deliverable, pr db.DeliverablePullRequest) {
	if resolver == nil {
		teardownLog.Warn("no github resolver; skipping draft PR close (deliverable already marked closed)", "deliverable", art.ID)
		return
	}
	if pr.Retired {
		teardownLog.Warn("draft PR's repository is retired; skipping GitHub close (deliverable already marked closed)", "deliverable", art.ID, "repository_id", pr.RepositoryID)
		return
	}
	owner, repo, number := pr.Owner, pr.Repo, pr.Number
	gh, err := resolver.ClientForRepo(ctx, orgID, owner, repo)
	if err != nil {
		teardownLog.Warn("resolve github client for draft PR close failed", "deliverable", art.ID, "owner", owner, "repo", repo, "error", err)
		return
	}
	if err := gh.ClosePR(ctx, owner, repo, number); err != nil {
		teardownLog.Warn("close draft PR on github failed (deliverable already marked closed)", "deliverable", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
	}
}
