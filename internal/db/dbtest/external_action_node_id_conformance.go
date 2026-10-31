package dbtest

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ExternalActionNodeIDFixture is one fresh, empty org for
// RunExternalActionNodeIDConformance: the store bundle, the org every call is
// for, a team the rows hang off (deliverables.team_id is required), a
// conversation in that team the ledger rows name as the one that opened their
// pull request, and a way to move a ledger row's occurred_at into the past, in
// the shape the column's default writes.
type ExternalActionNodeIDFixture struct {
	Stores                        db.Stores
	OrgID, TeamID, ConversationID string
	BackdateAction                func(t *testing.T, actionID string, age time.Duration)
}

// ExternalActionNodeIDFactory builds a fresh fixture per subtest.
type ExternalActionNodeIDFactory func(t *testing.T) ExternalActionNodeIDFixture

// RunExternalActionNodeIDConformance covers the ledger's node_id column and
// the read the reconciler's PR backstop makes from it. The column round-trips
// through both write pools and every list read. The read returns the
// pr_created rows on one scope inside the window that carry a node id and a
// conversation and have no deliverable with that node id in the same scope, and
// nothing else: not a row whose pull request has a deliverable under another
// name, not another scope's row, not a row outside the window, not a row with
// no node id, not a row with no conversation, and not another action's row.
func RunExternalActionNodeIDConformance(t *testing.T, mk ExternalActionNodeIDFactory) {
	t.Helper()
	ctx := context.Background()
	const (
		hostA = TestGitHubHost
		hostB = TestOtherGitHubHost
	)
	prCreated := func(f ExternalActionNodeIDFixture, scope, repoPath string, number int, nodeID string) domain.ExternalAction {
		return domain.ExternalAction{
			ID:             uuid.NewString(),
			TeamID:         f.TeamID,
			Provider:       domain.ProviderGitHub,
			Scope:          scope,
			Action:         domain.ActionPRCreated,
			Target:         domain.PullRequestTarget(repoPath, number),
			ExternalID:     strconv.Itoa(number),
			URL:            domain.GitHubPullURLBase(scope, repoPath, number),
			ToState:        domain.DeliverableStatePRDraft,
			ConversationID: f.ConversationID,
			Credential:     domain.CredentialGitHubApp,
			NodeID:         nodeID,
		}
	}
	prDeliverable := func(teamID, scope, repoPath string, number int, nodeID string) domain.Deliverable {
		a := domain.NewPullRequestDeliverable(repoPath, number, nodeID, "feature", "main",
			domain.GitHubPullURLBase(scope, repoPath, number), "title", "body", true)
		a.TeamID = teamID
		a.Scope = scope
		return a
	}

	t.Run("Node_id_round_trips_through_both_pools_and_every_list", func(t *testing.T) {
		f := mk(t)
		viaApp := prCreated(f, hostA, "octo/api", 1, "PR_app")
		viaAdmin := prCreated(f, hostA, "octo/api", 2, "PR_admin")
		without := prCreated(f, hostA, "octo/api", 3, "")
		without.Action = domain.ActionPRMarkedReady
		if err := f.Stores.ExternalActions.Record(ctx, f.OrgID, viaApp); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, viaAdmin); err != nil {
			t.Fatalf("RecordSystem: %v", err)
		}
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, without); err != nil {
			t.Fatalf("RecordSystem without a node id: %v", err)
		}
		want := map[string]string{viaApp.ID: "PR_app", viaAdmin.ID: "PR_admin", without.ID: ""}

		check := func(read string, got []domain.ExternalAction) {
			t.Helper()
			if len(got) != len(want) {
				t.Fatalf("%s returned %d rows, want %d", read, len(got), len(want))
			}
			for _, a := range got {
				if nodeID, ok := want[a.ID]; !ok || a.NodeID != nodeID {
					t.Errorf("%s row %s node_id = %q, want %q", read, a.ID, a.NodeID, nodeID)
				}
			}
		}
		byOrg, _, err := f.Stores.ExternalActions.ListByOrgSystem(ctx, f.OrgID, domain.ExternalActionListOpts{})
		if err != nil {
			t.Fatalf("ListByOrgSystem: %v", err)
		}
		check("ListByOrgSystem", byOrg)
		byTeam, _, err := f.Stores.ExternalActions.ListByTeam(ctx, f.OrgID, f.TeamID, domain.ExternalActionListOpts{})
		if err != nil {
			t.Fatalf("ListByTeam: %v", err)
		}
		check("ListByTeam", byTeam)
	})

	t.Run("Lists_only_pr_created_rows_whose_node_id_has_no_deliverable", func(t *testing.T) {
		f := mk(t)
		record := func(act domain.ExternalAction) domain.ExternalAction {
			t.Helper()
			if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, act); err != nil {
				t.Fatalf("RecordSystem %s: %v", act.Target, err)
			}
			return act
		}
		upsert := func(a domain.Deliverable) {
			t.Helper()
			if _, err := f.Stores.Deliverables.UpsertSystem(ctx, f.OrgID, a); err != nil {
				t.Fatalf("UpsertSystem %s on %s: %v", a.Target, a.Scope, err)
			}
		}

		// Returned: no deliverable anywhere carries the node id.
		missing := record(prCreated(f, hostA, "octo/api", 1, "PR_missing"))
		f.BackdateAction(t, missing.ID, time.Hour)
		// Returned: the only deliverable with the node id is in another scope.
		elsewhereDeliverable := record(prCreated(f, hostA, "octo/api", 2, "PR_scoped"))
		f.BackdateAction(t, elsewhereDeliverable.ID, 2*time.Hour)
		upsert(prDeliverable(f.TeamID, hostB, "octo/api", 2, "PR_scoped"))

		// Omitted: the pull request has its deliverable.
		record(prCreated(f, hostA, "octo/api", 3, "PR_present"))
		upsert(prDeliverable(f.TeamID, hostA, "octo/api", 3, "PR_present"))
		// Omitted: its deliverable sits under the repository's new name; the row
		// still names the old one.
		record(prCreated(f, hostA, "octo/api", 4, "PR_renamed"))
		upsert(prDeliverable(f.TeamID, hostA, "octo/platform", 4, "PR_renamed"))
		// Omitted: another scope's row.
		record(prCreated(f, hostB, "octo/api", 5, "PR_other_host"))
		// Omitted: outside the window.
		old := record(prCreated(f, hostA, "octo/api", 6, "PR_old"))
		f.BackdateAction(t, old.ID, 8*24*time.Hour)
		// Omitted: no node id.
		record(prCreated(f, hostA, "octo/api", 7, ""))
		// Omitted: its conversation was purged, so nothing would own a
		// deliverable.
		orphan := prCreated(f, hostA, "octo/api", 9, "PR_orphan")
		orphan.ConversationID = ""
		record(orphan)
		// Omitted: another action on a node id with no deliverable.
		ready := prCreated(f, hostA, "octo/api", 8, "PR_ready")
		ready.Action = domain.ActionPRMarkedReady
		record(ready)

		got, err := f.Stores.ExternalActions.ListPRCreatedWithoutDeliverableSystem(ctx, f.OrgID, hostA,
			time.Now().UTC().Add(-7*24*time.Hour))
		if err != nil {
			t.Fatalf("ListPRCreatedWithoutDeliverableSystem: %v", err)
		}
		ids := make([]string, len(got))
		for i, a := range got {
			ids[i] = a.ID
		}
		// Oldest first.
		if want := []string{elsewhereDeliverable.ID, missing.ID}; !slices.Equal(ids, want) {
			t.Fatalf("returned %v, want %v (the row whose only deliverable is on another scope, then the row with none)", got, want)
		}
		if a := got[1]; a.NodeID != "PR_missing" || a.Target != missing.Target || a.Scope != hostA ||
			a.TeamID != f.TeamID || a.ConversationID != f.ConversationID ||
			a.ToState != domain.DeliverableStatePRDraft || a.Action != domain.ActionPRCreated {
			t.Errorf("returned row = %+v, want the pr_created row for %s as recorded", a, missing.Target)
		}
	})

	t.Run("Refuses_an_empty_scope", func(t *testing.T) {
		f := mk(t)
		if _, err := f.Stores.ExternalActions.ListPRCreatedWithoutDeliverableSystem(ctx, f.OrgID, "", time.Now().UTC()); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("err = %v, want ErrExternalObjectScopeRequired", err)
		}
	})
}
