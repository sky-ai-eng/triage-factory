// Package artifactteardown retires every unresolved artifact a task's
// conversations hold: each draft PR closed (TF-side and on GitHub), each
// pending review dismissed. Pushed branches are kept — retention is separate.
//
// It is the TASK-END gesture and nothing else. A requeue, a claim and a
// re-delegation all leave the artifacts standing, so the next attempt inherits
// the draft PR it is going to push to rather than starting from nothing.
//
// The body lives in its own package because both ends of a task's life run it:
// a person closing the card, through the server's tx-bound app-pool adapter
// with that person as the actor, and the event cascade closing it for them,
// through the spawner's admin-pool adapter with no actor at all. Only the pool
// and the actor differ; the decision about which artifacts are unresolved and
// what retiring one means is the same decision, so it is written once.
package artifactteardown

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Stores is the store surface one teardown batch reads and writes. Each method
// mirrors a db.ConversationStore / db.ArtifactStore / db.ExternalActionStore
// method one-for-one; the adapter behind it decides which pool answers and
// whether a transaction binds the batch.
type Stores interface {
	// ConversationsForTask lists every conversation the task holds — the
	// blueprint's step conversations and any standalone one. Keying the
	// teardown on the task's conversations rather than on a conversation
	// status is what makes it find an artifact a long-terminal run left.
	ConversationsForTask(ctx context.Context, orgID, taskID string) ([]domain.Conversation, error)
	// ArtifactsForConversation lists one conversation's artifacts.
	ArtifactsForConversation(ctx context.Context, orgID, conversationID string) ([]domain.Artifact, error)
	// UpsertArtifact writes back one artifact whose state this teardown flipped.
	UpsertArtifact(ctx context.Context, orgID string, a domain.Artifact) error
	// RecordExternalAction appends the audit row for a GitHub write this
	// teardown is about to make.
	RecordExternalAction(ctx context.Context, orgID string, act domain.ExternalAction) error
}

// Deps is what Teardown needs from the process running it.
type Deps interface {
	// Batch runs fn over the store surface. The server's adapter binds one
	// app-pool transaction, so the flips and their audit rows commit or roll
	// back together and a failed batch leaves the artifacts unresolved for the
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
	// against. Nil is tolerated: the closes are skipped and every audit row
	// records the App, exactly as an unclassifiable repo does.
	GitHub() ghclient.Resolver
}

// Teardown resolves EVERY unresolved artifact the task's conversations hold.
// Each draft PR is flipped to closed and then closed on GitHub; each pending
// review (finalized or not) is flipped to dismissed, which is its whole
// resolution — a review is staged TF-side, so there is no GitHub object to
// retire and no org-credential write to audit.
//
// actorUserID is the human who authorized the close, recorded on each audit
// row. Empty is the autonomous spelling: an event-driven close has no actor,
// and the row says so rather than naming whoever happened to be nearby.
//
// This never flips conversations.status. The caller stops the task's live runs
// one step ahead of it, which owns that transition; a terminal conversation
// simply stays terminal.
//
// The returned error is the batch's: the artifacts are unresolved and a retry
// is expected. A GitHub close that fails is not an error — the artifact is
// already marked closed and reconciliation retires the object later.
func Teardown(ctx context.Context, deps Deps, orgID, taskID, actorUserID string) error {
	// Which credential each draft PR's close is made under is classified
	// BEFORE the write batch opens, for the same reason the closes themselves
	// run after it: the audit rows are composed inside that batch, and a
	// classification can reach GitHub.
	credentials := credentialsForTask(ctx, deps, orgID, taskID)

	// Draft PRs captured inside the batch (state already flipped) and closed on
	// GitHub after it lands — a network call must not hold a write transaction
	// open.
	var prArtifacts []domain.Artifact
	if err := deps.Batch(ctx, orgID, func(st Stores) error {
		convs, err := st.ConversationsForTask(ctx, orgID, taskID)
		if err != nil {
			return fmt.Errorf("list conversations for task: %w", err)
		}
		for i := range convs {
			conversationID := convs[i].ID
			arts, artErr := st.ArtifactsForConversation(ctx, orgID, conversationID)
			if artErr != nil {
				return fmt.Errorf("artifacts for conversation %s: %w", conversationID, artErr)
			}
			draftPRs := domain.AllDraftPullRequests(arts)
			pendingReviews := domain.AllPendingReviewArtifacts(arts)
			if len(draftPRs) == 0 && len(pendingReviews) == 0 {
				continue
			}

			// Abandon each pending review by flipping its artifact to
			// dismissed. No GitHub call and no audit row: the review is staged
			// TF-side, so a dismiss is a purely local state change rather than
			// an org-credential write (external_actions records only writes).
			// The proposed snapshot is preserved on the row.
			for j := range pendingReviews {
				dismissed := pendingReviews[j]
				dismissed.State = domain.ArtifactStateReviewDismissed
				if err := st.UpsertArtifact(ctx, orgID, dismissed); err != nil {
					return fmt.Errorf("upsert dismissed review: %w", err)
				}
			}
			// Abandon each draft PR by flipping its artifact to closed (the
			// GitHub close runs after the batch). The pushed branch and the
			// proposed snapshot are preserved — abandonment retires the PR
			// object, not the work.
			for j := range draftPRs {
				pr := draftPRs[j]
				closed := pr
				closed.State = domain.ArtifactStatePRClosed
				if err := st.UpsertArtifact(ctx, orgID, closed); err != nil {
					return fmt.Errorf("upsert closed draft pr: %w", err)
				}
				act := domain.ArtifactAction(&pr, actorUserID, domain.ActionPRClosed,
					domain.ArtifactStatePRDraft, domain.ArtifactStatePRClosed,
					credentialForTarget(credentials, pr.Target))
				if err := st.RecordExternalAction(ctx, orgID, act); err != nil {
					return fmt.Errorf("record external action for closed draft pr: %w", err)
				}
				prArtifacts = append(prArtifacts, pr)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Resolve on GitHub (best-effort, outside the batch). The artifacts are
	// already marked closed; a GitHub failure leaves the object for
	// reconciliation to retire later and must never fail the close that
	// triggered this. Branches stay.
	for i := range prArtifacts {
		CloseDraftPR(ctx, deps.GitHub(), orgID, &prArtifacts[i])
	}
	return nil
}

// credentialsForTask classifies the acting GitHub credential for every repo the
// task's unresolved draft PRs live in, keyed by "owner/repo". It exists so the
// teardown's audit rows can name that credential without probing GitHub from
// inside the write batch those rows are composed into — the read pass and the
// classification both finish before that batch opens.
//
// Best-effort: a read failure yields an empty map and every row falls back to
// the App, exactly as an unclassifiable repo does. The teardown itself is
// unaffected — it re-reads the artifacts under its own batch and is the
// authority on which ones it resolves.
func credentialsForTask(ctx context.Context, deps Deps, orgID, taskID string) map[string]string {
	type ownerRepo struct{ owner, repo string }
	repos := map[string]ownerRepo{}
	if err := deps.Batch(ctx, orgID, func(st Stores) error {
		convs, err := st.ConversationsForTask(ctx, orgID, taskID)
		if err != nil {
			return err
		}
		for i := range convs {
			arts, artErr := st.ArtifactsForConversation(ctx, orgID, convs[i].ID)
			if artErr != nil {
				return artErr
			}
			for _, pr := range domain.AllDraftPullRequests(arts) {
				if owner, repo, _, ok := domain.ParsePRTarget(pr.Target); ok {
					repos[owner+"/"+repo] = ownerRepo{owner: owner, repo: repo}
				}
			}
		}
		return nil
	}); err != nil {
		teardownLog.Warn("pre-read draft PRs for credential attribution failed; teardown rows will record the app",
			"task", taskID, "error", err)
		return nil
	}
	out := make(map[string]string, len(repos))
	for repoID, or := range repos {
		out[repoID] = ghclient.CredentialForRepo(ctx, deps.GitHub(), orgID, or.owner, or.repo)
	}
	return out
}

// credentialForTarget looks a PR target's repo up in the map credentialsForTask
// built. A miss — an unparseable target, or a draft PR that appeared between the
// pre-pass and the batch — reports the App, the same answer an unclassifiable
// repo gets.
func credentialForTarget(credentials map[string]string, target string) string {
	owner, repo, _, ok := domain.ParsePRTarget(target)
	if !ok {
		return domain.CredentialGitHubApp
	}
	if cred, hit := credentials[owner+"/"+repo]; hit {
		return cred
	}
	return domain.CredentialGitHubApp
}

// CloseDraftPR closes an abandoned draft PR on GitHub. Best-effort: every
// failure is logged, never returned — the artifact is already marked closed and
// the task or conversation already resolved, so a GitHub hiccup mustn't unwind
// that. owner/repo/number come from the artifact's target; the per-repo client
// resolves App-installation-token → PAT like every other PR mutation. A free
// function taking the resolver so the task-level teardown and the per-artifact
// dismiss endpoint share one GitHub-resolution path. The pushed branch is never
// touched (retention is a separate concern).
func CloseDraftPR(ctx context.Context, resolver ghclient.Resolver, orgID string, art *domain.Artifact) {
	if resolver == nil {
		teardownLog.Warn("no github resolver; skipping draft PR close (artifact already marked closed)", "artifact", art.ID)
		return
	}
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		teardownLog.Warn("draft PR artifact has a malformed target; skipping GitHub close", "artifact", art.ID, "target", art.Target)
		return
	}
	gh, err := resolver.ClientForRepo(ctx, orgID, owner, repo)
	if err != nil {
		teardownLog.Warn("resolve github client for draft PR close failed", "artifact", art.ID, "owner", owner, "repo", repo, "error", err)
		return
	}
	if err := gh.ClosePR(ctx, owner, repo, number); err != nil {
		teardownLog.Warn("close draft PR on github failed (artifact already marked closed)", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
	}
}
