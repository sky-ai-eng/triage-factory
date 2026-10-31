package agenthost

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// refuseDeliverableWrites makes every deliverable insert, an upsert's included,
// fail in the database itself, so the refusal reaches both write paths: the
// admin-pool write of an event-triggered run and the claims transaction of a
// manual one.
func refuseDeliverableWrites(t *testing.T, conn *sql.DB) {
	t.Helper()
	if _, err := conn.Exec(`
		CREATE TRIGGER refuse_deliverable_writes BEFORE INSERT ON deliverables
		BEGIN SELECT RAISE(ABORT, 'deliverable write refused'); END
	`); err != nil {
		t.Fatalf("install deliverable refusal: %v", err)
	}
}

// TestRecordExternalWrite_FailedDeliverableKeepsTheLedgerRow pins that the ledger
// row never depends on the deliverable written beside it: when the deliverable write
// fails, the action is still recorded, on both trigger types.
func TestRecordExternalWrite_FailedDeliverableKeepsTheLedgerRow(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			conn, stores, info := newCaptureStoresConn(t, eventTriggered)
			refuseDeliverableWrites(t, conn)

			a := &domain.Deliverable{
				Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindComment,
				Target: "octo/repo#1", ExternalID: "7", State: domain.DeliverableStateCommentPosted,
				DedupKey: domain.GitHubCommentKey(domain.GitHubIssueComment, 7),
			}
			act := &domain.ExternalAction{
				Provider: domain.ProviderGitHub, Action: domain.ActionCommentPosted,
				Target: "octo/repo#1", ExternalID: "7", Credential: domain.CredentialGitHubApp,
			}
			RecordExternalWrite(context.Background(), stores, info, a, act)

			if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
				t.Errorf("deliverables = %+v, want none: the write was refused", arts)
			}
			acts := listExternalActions(t, stores)
			if len(acts) != 1 || acts[0].Action != domain.ActionCommentPosted || acts[0].ConversationID != info.ConversationID {
				t.Errorf("ledger rows = %+v, want the one comment_posted row despite the refused deliverable", acts)
			}
		})
	}
}

// TestUpsertDeliverable_FailedBranchDeliverableKeepsThePushRow pins the same for the
// branch push, which composes its ledger row inside the deliverable upsert: the
// deliverable's error still reaches the caller, and the push row is recorded.
func TestUpsertDeliverable_FailedBranchDeliverableKeepsThePushRow(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			conn, stores, info := newCaptureStoresConn(t, eventTriggered)
			refuseDeliverableWrites(t, conn)

			branch, ok := domain.NewBranchDeliverable("octo/repo", "refs/heads/feat", "abc123", true)
			if !ok {
				t.Fatal("NewBranchDeliverable refused the fixture branch")
			}
			if _, err := newDirectRuntime(stores, info).UpsertDeliverable(context.Background(), branch); err == nil {
				t.Fatal("UpsertDeliverable = nil error, want the refused deliverable's error")
			}
			acts := listExternalActions(t, stores)
			if len(acts) != 1 || acts[0].Action != domain.ActionBranchPushed {
				t.Errorf("ledger rows = %+v, want the one branch_pushed row despite the refused deliverable", acts)
			}
		})
	}
}
