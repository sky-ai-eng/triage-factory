package agenthost

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A pull request create records the run's deliverable and links it to the
// pull request's entity, and attributes nothing: the owner and commissioner
// are the deliverable reconciler's to stamp, once GitHub's answer shows the
// run opened it, so neither a create recorded here nor one relayed from a
// sidecar changes an org-wide entity beyond linking it.

// botOpenedPR is the deliverable the recording funnel receives when the agent
// opens a PR through the exec verb (recordGithubPR), the one door that mints
// one.
func botOpenedPR() domain.Deliverable {
	return domain.NewPullRequestDeliverable(
		"octo/repo", 42, "PR_kwDOABCD", "feature/x", "main",
		"https://github.com/octo/repo/pull/42", "Fix the thing", "Body", true,
	)
}

// botOpenedPRIdentity is what GitHub answers for botOpenedPR's pull request and
// repository, which the PR create reads beside its response.
var botOpenedPRIdentity = domain.GitHubIdentity{
	RepositoryID: "1", Repository: "octo/repo", NodeID: "PR_kwDOABCD", Kind: domain.EntityKindPullRequest,
}

func ownerOf(t *testing.T, stores db.Stores, entityID string) string {
	t.Helper()
	team, err := stores.Entities.OwningTeamForEntitySystem(
		context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("OwningTeamForEntitySystem: %v", err)
	}
	return team
}

// commissionerOf reads entities.commissioned_by_user_id. Raw SQL because
// nothing in production reads that column by id: the pull-request list reads
// it as a predicate.
func commissionerOf(t *testing.T, conn *sql.DB, entityID string) string {
	t.Helper()
	var got sql.NullString
	if err := conn.QueryRow(
		`SELECT commissioned_by_user_id FROM entities WHERE id = ?`, entityID,
	).Scan(&got); err != nil {
		t.Fatalf("read commissioned_by_user_id for %s: %v", entityID, err)
	}
	return got.String
}

func prEntity(t *testing.T, stores db.Stores) *domain.Entity {
	t.Helper()
	ent, err := stores.Entities.GetBySource(
		context.Background(), runmode.LocalDefaultOrgID, "github", "https://github.com", "octo/repo#42")
	if err != nil {
		t.Fatalf("GetBySource: %v", err)
	}
	return ent
}

// assertLinkedAndUnattributed checks that the run holds one pull request
// deliverable, linked to the pull request's entity, and that the entity has
// no owner and no commissioner.
func assertLinkedAndUnattributed(t *testing.T, conn *sql.DB, stores db.Stores, info ConversationInfo) {
	t.Helper()
	ent := prEntity(t, stores)
	if ent == nil {
		t.Fatal("no entity for the opened PR")
	}
	mine := listConversationDeliverables(t, stores, info.ConversationID)
	if len(mine) != 1 || mine[0].EntityID != ent.ID {
		t.Errorf("the run's deliverables = %+v, want one linked to entity %s", mine, ent.ID)
	}
	if got := ownerOf(t, stores, ent.ID); got != "" {
		t.Errorf("owning team = %q, want none", got)
	}
	if got := commissionerOf(t, conn, ent.ID); got != "" {
		t.Errorf("commissioned by = %q, want none", got)
	}
}

// TestRecordPRCreate_LinksAndAttributesNothing covers both orderings against
// the poller: the run opens the PR before any poll has seen it, and the funnel
// mints the entity; or a poll discovered it first, and the funnel links that
// row rather than creating a second.
func TestRecordPRCreate_LinksAndAttributesNothing(t *testing.T) {
	for _, tc := range []struct {
		name           string
		eventTriggered bool
		discovered     bool
	}{
		{"manual run, deliverable first", false, false},
		{"event-triggered run, deliverable first", true, false},
		{"manual run, entity first", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			conn, stores, info := newCaptureStoresConn(t, tc.eventTriggered)
			var discovered *domain.Entity
			if tc.discovered {
				var err error
				discovered, _, err = stores.Entities.FindOrCreateSystem(
					ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "octo/repo#42", "", "pr", "Fix the thing", "")
				if err != nil {
					t.Fatalf("seed discovered entity: %v", err)
				}
			}

			a := botOpenedPR()
			act := &domain.ExternalAction{
				Provider: domain.ProviderGitHub, Action: domain.ActionPRCreated, Target: a.Target, URL: a.URL,
				NodeID: "PR_kwDOABCD",
			}
			act.SetObject(domain.ObjectKindPullRequest, "")
			recordExternalWrite(ctx, stores, info, &a, act, domain.EntityContainer{}, botOpenedPRIdentity)

			assertLinkedAndUnattributed(t, conn, stores, info)
			if discovered != nil {
				if ent := prEntity(t, stores); ent == nil || ent.ID != discovered.ID {
					t.Errorf("entity = %+v, want the discovered %s linked rather than a second", ent, discovered.ID)
				}
			}
			if ent := prEntity(t, stores); ent != nil {
				if role := touchRole(t, conn, info.ConversationID, ent.ID); role != domain.MemoryRoleProduced {
					t.Errorf("link role = %q, want %q", role, domain.MemoryRoleProduced)
				}
			}
		})
	}
}

// TestRelay_PRCreateNamingAnUnrecordedPullRequestAttributesNothing is the
// relayed create the sidecar alone vouches for: one naming a pull request no
// TF row holds yet. The run gains its deliverable, linked, and the entity
// gains no owner and no commissioner, so a pull request someone else opened
// does not route its events to the sending run's team.
func TestRelay_PRCreateNamingAnUnrecordedPullRequestAttributesNothing(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		t.Run(fmt.Sprintf("event_triggered=%v", eventTriggered), func(t *testing.T) {
			conn, stores, info := newCaptureStoresConn(t, eventTriggered)
			rt := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)

			a := botOpenedPR()
			act := &domain.ExternalAction{
				Provider: domain.ProviderGitHub, Action: domain.ActionPRCreated, Target: a.Target, URL: a.URL,
				NodeID: "PR_kwDOABCD", Credential: domain.CredentialGitHubApp,
			}
			act.SetObject(domain.ObjectKindPullRequest, "")
			rt.Record(context.Background(), &a, act, domain.EntityContainer{}, botOpenedPRIdentity)

			assertLinkedAndUnattributed(t, conn, stores, info)
		})
	}
}
