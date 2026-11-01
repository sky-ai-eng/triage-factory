package agenthost

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// These tests drive the relay the way a compromised sidecar would: every
// deliverable key and id below is one the sidecar chose, naming a row that
// belongs to another run.

// seedOtherConversation inserts a second conversation beside the capture
// stores' run, to own the rows the relayed writes must not reach.
func seedOtherConversation(t *testing.T, conn *sql.DB) string {
	t.Helper()
	const id = "22222222-2222-2222-2222-222222222222"
	if _, err := conn.Exec(`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, id); err != nil {
		t.Fatalf("seed another conversation: %v", err)
	}
	return id
}

// seedOtherReview stores conversation other's pending review draft on the pull
// request whose node id is nodeID.
func seedOtherReview(t *testing.T, stores db.Stores, info ConversationInfo, other, nodeID string) domain.Deliverable {
	t.Helper()
	a := domain.NewReviewDeliverable("octo/repo", 7, nodeID, "head-sha", other)
	a.TeamID, a.ConversationID, a.Scope = info.TeamID, other, testScope("github")
	a.DetailsJSON = domain.MarshalReviewDeliverableDetails(domain.ReviewDeliverableDetails{Number: 7, HeadSHA: "head-sha", ReviewBody: "the other run's draft"})
	stored, err := stores.Deliverables.UpsertSystem(context.Background(), info.OrgID, a)
	if err != nil {
		t.Fatalf("seed the other run's review: %v", err)
	}
	return stored
}

// assertDeliverableUnchanged fails unless the row want.ID still holds want's
// owner, state and details.
func assertDeliverableUnchanged(t *testing.T, stores db.Stores, want domain.Deliverable) {
	t.Helper()
	got, err := stores.Deliverables.Get(context.Background(), want.OrgID, want.ID)
	if err != nil || got == nil {
		t.Fatalf("Get(%s) = %v, %v", want.ID, got, err)
	}
	if got.ConversationID != want.ConversationID || got.State != want.State || got.DetailsJSON != want.DetailsJSON || got.DedupKey != want.DedupKey {
		t.Errorf("the other run's row = %+v, want it unchanged from %+v", got, want)
	}
}

// TestRelay_ReviewKeyNamingAnotherConversationLandsOnTheSendersDraft: a review
// draft relayed under a key naming another conversation is keyed on the
// sending run's conversation, through either door a sidecar can send one by,
// so it lands on a row of the run's own and the other conversation's draft is
// untouched.
func TestRelay_ReviewKeyNamingAnotherConversationLandsOnTheSendersDraft(t *testing.T) {
	for _, door := range []string{"upsert", "record"} {
		t.Run(door, func(t *testing.T) {
			conn, stores, info := newCaptureStoresConn(t, true)
			ctx := context.Background()
			other := seedOtherConversation(t, conn)
			theirs := seedOtherReview(t, stores, info, other, "PR_kw7")
			rt := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)

			forged := domain.NewReviewDeliverable("octo/repo", 7, "PR_kw7", "forged-sha", other)
			forged.State = domain.DeliverableStateReviewSubmitted
			forged.DetailsJSON = domain.MarshalReviewDeliverableDetails(domain.ReviewDeliverableDetails{Number: 7, ReviewBody: "forged"})
			switch door {
			case "upsert":
				if _, err := rt.UpsertDeliverable(ctx, forged, domain.GitHubIdentity{}); err != nil {
					t.Fatalf("UpsertDeliverable: %v", err)
				}
			case "record":
				rt.Record(ctx, &forged, nil, domain.EntityContainer{}, domain.GitHubIdentity{})
			}

			mine := listConversationDeliverables(t, stores, info.ConversationID)
			if len(mine) != 1 || mine[0].ID == theirs.ID || mine[0].DedupKey != domain.ReviewDedupKey("PR_kw7", info.ConversationID) {
				t.Errorf("the run's deliverables = %+v, want one review keyed on its own conversation", mine)
			}
			assertDeliverableUnchanged(t, stores, theirs)
		})
	}
}

// TestRelay_ReviewKeyWithNoNodeIDIsRefused: a review whose key carries no pull
// request node id cannot be keyed on the run's conversation, so it is refused:
// an upsert answers an error, and a record writes its action alone.
func TestRelay_ReviewKeyWithNoNodeIDIsRefused(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()
	rt := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)

	forged := domain.NewReviewDeliverable("octo/repo", 7, "PR_kw7", "head-sha", info.ConversationID)
	forged.DedupKey = "github:review:octo/repo#7:" + info.ConversationID
	if _, err := rt.UpsertDeliverable(ctx, forged, domain.GitHubIdentity{}); err == nil {
		t.Error("UpsertDeliverable of a review with no node id = nil error, want a refusal")
	}
	act := &domain.ExternalAction{Provider: domain.ProviderGitHub, Action: domain.ActionReviewSubmitted, Target: "octo/repo#7", Credential: domain.CredentialGitHubApp}
	rt.Record(ctx, &forged, act, domain.EntityContainer{}, domain.GitHubIdentity{})

	if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
		t.Errorf("deliverables = %+v, want none", arts)
	}
	if acts := listExternalActions(t, stores); len(acts) != 1 || acts[0].Action != domain.ActionReviewSubmitted {
		t.Errorf("ledger rows = %+v, want the one review_submitted row", acts)
	}
}

// TestRelay_ReviewWritesByIDAreScopedToTheRun: a relayed draft rewrite or
// state move naming another conversation's deliverable id is refused with an
// error naming the id, and the row is left as it was.
func TestRelay_ReviewWritesByIDAreScopedToTheRun(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		t.Run(fmt.Sprintf("event_triggered=%v", eventTriggered), func(t *testing.T) {
			conn, stores, info := newCaptureStoresConn(t, eventTriggered)
			ctx := context.Background()
			other := seedOtherConversation(t, conn)
			theirs := seedOtherReview(t, stores, info, other, "PR_kw7")
			rt := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)
			want := fmt.Sprintf("deliverable %s is not one of this conversation's", theirs.ID)

			updated, err := rt.UpdateReviewDetailsIfPending(ctx, theirs.ID,
				domain.MarshalReviewDeliverableDetails(domain.ReviewDeliverableDetails{ReviewBody: "rewritten"}))
			if err == nil || !strings.Contains(err.Error(), want) || updated {
				t.Errorf("UpdateReviewDetailsIfPending = %v, %v; want the refusal naming %s", updated, err, theirs.ID)
			}
			moved, err := rt.TransitionReviewState(ctx, theirs.ID,
				domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "9001", "", "")
			if err == nil || !strings.Contains(err.Error(), want) || moved {
				t.Errorf("TransitionReviewState = %v, %v; want the refusal naming %s", moved, err, theirs.ID)
			}
			assertDeliverableUnchanged(t, stores, theirs)
		})
	}
}

// TestRelay_PRCreateNamingARecordedPullRequestLeavesItsRow: a relayed pull
// request create naming the node id of a pull request another run recorded
// leaves that row's state as it was, attributes nothing to the sending run,
// and still records its pr_created ledger row.
func TestRelay_PRCreateNamingARecordedPullRequestLeavesItsRow(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		t.Run(fmt.Sprintf("event_triggered=%v", eventTriggered), func(t *testing.T) {
			conn, stores, info := newCaptureStoresConn(t, eventTriggered)
			ctx := context.Background()
			other := seedOtherConversation(t, conn)
			theirs := botOpenedPR()
			theirs.TeamID, theirs.ConversationID, theirs.Scope = info.TeamID, other, testScope("github")
			seeded, err := stores.Deliverables.UpsertSystem(ctx, info.OrgID, theirs)
			if err != nil {
				t.Fatalf("seed the other run's pull request: %v", err)
			}
			rt := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)

			forged := domain.NewPullRequestDeliverable("octo/repo", 42, "PR_kwDOABCD", "feature/x", "main",
				"https://github.com/octo/repo/pull/42", "Forged", "Body", false)
			act := &domain.ExternalAction{
				Provider: domain.ProviderGitHub, Action: domain.ActionPRCreated, Target: forged.Target, URL: forged.URL,
				NodeID: "PR_kwDOABCD", Credential: domain.CredentialGitHubApp,
			}
			act.SetObject(domain.ObjectKindPullRequest, "")
			// The upsert op records review drafts only, so it is no way around
			// the create's insert.
			if _, err := rt.UpsertDeliverable(ctx, forged, botOpenedPRIdentity); err == nil {
				t.Error("UpsertDeliverable of a pull request = nil error, want a refusal")
			}
			rt.Record(ctx, &forged, act, domain.EntityContainer{}, botOpenedPRIdentity)

			assertDeliverableUnchanged(t, stores, seeded)
			if mine := listConversationDeliverables(t, stores, info.ConversationID); len(mine) != 0 {
				t.Errorf("the run's deliverables = %+v, want none", mine)
			}
			acts := listExternalActions(t, stores)
			if len(acts) != 1 || acts[0].Action != domain.ActionPRCreated || acts[0].ConversationID != info.ConversationID {
				t.Errorf("ledger rows = %+v, want the run's one pr_created row", acts)
			}
			ent := prEntity(t, stores)
			if ent == nil {
				t.Fatal("no entity for the pull request")
			}
			if got := ownerOf(t, stores, ent.ID); got != "" {
				t.Errorf("owning team = %q, want none: the create did not open this pull request", got)
			}
		})
	}
}

// TestRelay_IssueKeyIsNotFollowedInMultiMode: in multi mode a relayed Jira
// action or read touch names an issue by an id and a key the sidecar chose, so
// the entity the id names is linked as stored and never renamed onto the
// relayed key.
func TestRelay_IssueKeyIsNotFollowedInMultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()
	site := testScope("jira")
	issue, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, info.OrgID, site, "OPS-7", "10007", dbseed.JiraIssueProject("OPS-7"), "An issue", site+"/browse/OPS-7")
	if err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
	rt := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)

	act := &domain.ExternalAction{Provider: domain.ProviderJira, Action: domain.ActionIssueUpdated, Target: "FORGED-1", Credential: domain.CredentialJiraOrg}
	act.SetObject(domain.ObjectKindIssue, domain.IssueObjectKey(domain.ProviderJira, site, "10007"))
	rt.Record(ctx, nil, act, domain.EntityContainer{}, domain.GitHubIdentity{})
	rt.RecordReadTouch(ctx, domain.ProviderJira, "FORGED-2", "10007", domain.EntityContainer{}, domain.GitHubIdentity{}, "")

	if acts := listExternalActions(t, stores); len(acts) != 1 || acts[0].EntityID != issue.ID {
		t.Errorf("ledger rows = %+v, want the one row linked to %s", acts, issue.ID)
	}
	got, err := stores.Entities.GetSystem(ctx, info.OrgID, issue.ID)
	if err != nil || got == nil || got.SourceID != "OPS-7" {
		t.Errorf("issue = %+v, %v; want it still under OPS-7", got, err)
	}
}
