package server

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// External-action audit recording for the human-authorized, org-executed GitHub
// approval lifecycle (TFAC-483). These are writes a human triggers from the
// approval UI / board but that TF performs under the ORG GitHub credential —
// so they belong in the audit log of record alongside the bot's own writes. The
// drafting run is run_id (who proposed the change); the approving/dragging human
// is actor_user_id (who authorized it).
//
// Two shapes, by whether the action also flips an artifact's state:
//
//   - Where the action flips an artifact in a WithTx (approve → open, submit →
//     submitted, close → closed, dismiss → dismissed), the caller composes the
//     Record INTO that same tx (tx.ExternalActions.Record(...)) via
//     githubApprovalAction, so the audit row commits or rolls back atomically
//     with the flip and the two can't disagree.
//   - Where the action is a pure GitHub mutation with no critical flip (the PR
//     title/body edit, the dashboard board-drag draft toggle — their only DB
//     write is a best-effort snapshot refresh), the caller uses
//     recordExternalActionBestEffort, which records in its own claims-set tx
//     AFTER the pessimistic GitHub write succeeds, so it always fires (even when
//     the snapshot path early-returns) and never fails the user's applied edit.

// githubCredentialFor names the org credential a GitHub write on owner/repo is
// made under (a domain.Credential* value), for the audit row's credential
// column. It asks the same resolver the write's own client comes from, because
// that is the only thing that knows: an installation token and a PAT are
// indistinguishable strings once minted, so the tier has to come from the
// decision that picked one. The resolver shares its coverage probe and token
// cache with that resolution, so at a site that has already built a client this
// is a cache read rather than a second round trip.
//
// Anything unresolved reports the App — a resolver that doesn't classify, a
// malformed target, a credential backend that just failed. The row is about a
// GitHub write that happened or is about to; the App is what this column has
// said since it existed, and asserting a PAT nobody observed would be worse
// than the status quo it replaces.
//
// Call it BEFORE opening the transaction the row is composed into: it can reach
// GitHub, and a credential probe inside a write tx would hold that tx open for
// a network round trip.
func githubCredentialFor(ctx context.Context, resolver ghclient.Resolver, orgID, owner, repo string) string {
	ir, ok := resolver.(ghclient.RepoIdentityResolver)
	if !ok {
		return domain.CredentialGitHubApp
	}
	_, identity, err := ir.ClientForRepoWithIdentity(ctx, orgID, owner, repo)
	if err != nil {
		externalActionLog.Warn("classify github credential for audit row failed; recording as the app",
			"org", orgID, "owner", owner, "repo", repo, "error", err)
		return domain.CredentialGitHubApp
	}
	return identity.Credential()
}

// githubCredentialForArtifact is githubCredentialFor keyed off the repo an
// artifact's own target names — the coordinates every approval-lifecycle write
// addresses.
func githubCredentialForArtifact(ctx context.Context, resolver ghclient.Resolver, orgID string, art *domain.Artifact) string {
	owner, repo, _, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return domain.CredentialGitHubApp
	}
	return githubCredentialFor(ctx, resolver, orgID, owner, repo)
}

// githubApprovalAction builds the external_actions row for an artifact-backed
// GitHub approval-lifecycle write. run_id is the drafting run (art.ConversationID),
// actor_user_id is the approving human (userID), team_id is the artifact's team,
// and the object coordinates come from the artifact. from/to carry the lifecycle
// transition (draft→open, pending→submitted, …). credential is the org
// credential the write is made under, classified by the caller before it opens
// whatever transaction this row lands in.
func githubApprovalAction(art *domain.Artifact, userID, action, from, to, credential string) domain.ExternalAction {
	// A PR artifact carries its html_url; a review artifact carries none (its
	// target is the PR), so fall back to the PR web URL from the target — the audit
	// row links somewhere rather than render non-clickable.
	url := art.URL
	if url == "" {
		if owner, repo, number, ok := domain.ParsePRTarget(art.Target); ok {
			url = domain.GitHubPullURL(owner+"/"+repo, number)
		}
	}
	return domain.ExternalAction{
		TeamID:         art.TeamID,
		Provider:       domain.ArtifactProviderGitHub,
		Action:         action,
		Target:         art.Target,
		ExternalID:     art.ExternalID,
		URL:            url,
		FromState:      from,
		ToState:        to,
		ConversationID: art.ConversationID,
		ActorUserID:    userID,
		Credential:     credential,
	}
}

// recordExternalActionBestEffort records act in its own claims-set tx (app pool),
// logging-and-swallowing any failure. For the approval/board sites whose action
// is a pure GitHub mutation that already landed pessimistically — the audit row
// is recorded after success and must never fail the user's applied write.
func recordExternalActionBestEffort(ctx context.Context, tx db.TxRunner, orgID, userID string, act domain.ExternalAction) {
	if err := tx.WithTx(ctx, orgID, userID, func(t db.TxStores) error {
		return t.ExternalActions.Record(ctx, orgID, act)
	}); err != nil {
		externalActionLog.Warn("external-action recording failed (GitHub write already applied)",
			"action", act.Action, "target", act.Target, "error", err)
	}
}
