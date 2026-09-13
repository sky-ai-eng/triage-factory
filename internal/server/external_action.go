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
// drafting conversation is conversation_id (who proposed the change); the approving/dragging human
// is actor_user_id (who authorized it).
//
// Two shapes, by whether the action also flips an artifact's state:
//
//   - Where the action flips an artifact in a WithTx (approve → open, submit →
//     submitted, close → closed, dismiss → dismissed), the caller composes the
//     Record INTO that same tx (tx.ExternalActions.Record(...)) via
//     domain.ArtifactAction, so the audit row commits or rolls back atomically
//     with the flip and the two can't disagree.
//   - Where the action is a pure GitHub mutation with no critical flip (the PR
//     title/body edit, the dashboard board-drag draft toggle — their only DB
//     write is a best-effort snapshot refresh), the caller uses
//     recordExternalActionBestEffort, which records in its own claims-set tx
//     AFTER the pessimistic GitHub write succeeds, so it always fires (even when
//     the snapshot path early-returns) and never fails the user's applied edit.

// githubCredentialForArtifact is ghclient.CredentialForRepo keyed off the repo
// an artifact's own target names — the coordinates every approval-lifecycle
// write addresses.
func githubCredentialForArtifact(ctx context.Context, resolver ghclient.Resolver, orgID string, art *domain.Artifact) string {
	owner, repo, _, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return domain.CredentialGitHubApp
	}
	return ghclient.CredentialForRepo(ctx, resolver, orgID, owner, repo)
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
