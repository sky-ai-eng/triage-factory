package agenthost

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// refuseArtifactWrites makes every artifact insert, an upsert's included,
// fail in the database itself, so the refusal reaches both write paths: the
// admin-pool write of an event-triggered run and the claims transaction of a
// manual one.
func refuseArtifactWrites(t *testing.T, conn *sql.DB) {
	t.Helper()
	if _, err := conn.Exec(`
		CREATE TRIGGER refuse_artifact_writes BEFORE INSERT ON artifacts
		BEGIN SELECT RAISE(ABORT, 'artifact write refused'); END
	`); err != nil {
		t.Fatalf("install artifact refusal: %v", err)
	}
}

// TestRecordExternalWrite_FailedArtifactKeepsTheLedgerRow pins that the ledger
// row never depends on the artifact written beside it: when the artifact write
// fails, the action is still recorded, on both trigger types.
func TestRecordExternalWrite_FailedArtifactKeepsTheLedgerRow(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			conn, stores, info := newCaptureStoresConn(t, eventTriggered)
			refuseArtifactWrites(t, conn)

			a := &domain.Artifact{
				Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
				ExternalID: "7", State: domain.ArtifactStateCommentDeleted,
				DedupKey: domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 7),
			}
			act := &domain.ExternalAction{
				Provider: domain.ArtifactProviderGitHub, Action: domain.ActionCommentDeleted,
				Target: "octo/repo", ExternalID: "7", Credential: domain.CredentialGitHubApp,
			}
			RecordExternalWrite(context.Background(), stores, info, a, act)

			if arts := listConversationArtifacts(t, stores, info.ConversationID); len(arts) != 0 {
				t.Errorf("artifacts = %+v, want none: the write was refused", arts)
			}
			acts := listExternalActions(t, stores)
			if len(acts) != 1 || acts[0].Action != domain.ActionCommentDeleted || acts[0].ConversationID != info.ConversationID {
				t.Errorf("ledger rows = %+v, want the one comment_deleted row despite the refused artifact", acts)
			}
		})
	}
}

// TestUpsertArtifact_FailedBranchArtifactKeepsThePushRow pins the same for the
// branch push, which composes its ledger row inside the artifact upsert: the
// artifact's error still reaches the caller, and the push row is recorded.
func TestUpsertArtifact_FailedBranchArtifactKeepsThePushRow(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			conn, stores, info := newCaptureStoresConn(t, eventTriggered)
			refuseArtifactWrites(t, conn)

			branch, ok := domain.NewBranchArtifact("octo/repo", "refs/heads/feat", "abc123", true)
			if !ok {
				t.Fatal("NewBranchArtifact refused the fixture branch")
			}
			if _, err := newDirectRuntime(stores, info).UpsertArtifact(context.Background(), branch); err == nil {
				t.Fatal("UpsertArtifact = nil error, want the refused artifact's error")
			}
			acts := listExternalActions(t, stores)
			if len(acts) != 1 || acts[0].Action != domain.ActionBranchPushed {
				t.Errorf("ledger rows = %+v, want the one branch_pushed row despite the refused artifact", acts)
			}
		})
	}
}
