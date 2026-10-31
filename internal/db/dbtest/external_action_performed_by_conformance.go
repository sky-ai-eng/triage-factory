package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunExternalActionPerformedByConformance covers the ledger's performed_by
// column: each of its three values round-trips through both write pools and
// every list read, and both writes refuse an empty or unknown value with
// db.ErrPerformedByRequired, writing nothing. It runs on the node-id suite's
// fixture: a fresh org with a team and a conversation.
func RunExternalActionPerformedByConformance(t *testing.T, mk ExternalActionNodeIDFactory) {
	t.Helper()
	ctx := context.Background()
	action := func(f ExternalActionNodeIDFixture, performedBy string) domain.ExternalAction {
		return domain.ExternalAction{
			ID:             uuid.NewString(),
			TeamID:         f.TeamID,
			Provider:       domain.ProviderGitHub,
			Scope:          TestGitHubHost,
			Action:         domain.ActionPRMarkedReady,
			Target:         "octo/api#7",
			ConversationID: f.ConversationID,
			Credential:     domain.CredentialGitHubApp,
			PerformedBy:    performedBy,
		}
	}

	t.Run("Performed_by_round_trips_through_both_pools_and_every_list", func(t *testing.T) {
		f := mk(t)
		agent := action(f, domain.PerformedByAgent)
		person := action(f, domain.PerformedByPerson)
		system := action(f, domain.PerformedBySystem)
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, agent); err != nil {
			t.Fatalf("RecordSystem agent: %v", err)
		}
		if err := f.Stores.ExternalActions.Record(ctx, f.OrgID, person); err != nil {
			t.Fatalf("Record person: %v", err)
		}
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, system); err != nil {
			t.Fatalf("RecordSystem system: %v", err)
		}
		want := map[string]string{
			agent.ID:  domain.PerformedByAgent,
			person.ID: domain.PerformedByPerson,
			system.ID: domain.PerformedBySystem,
		}

		check := func(read string, got []domain.ExternalAction) {
			t.Helper()
			if len(got) != len(want) {
				t.Fatalf("%s returned %d rows, want %d", read, len(got), len(want))
			}
			for _, a := range got {
				if performedBy, ok := want[a.ID]; !ok || a.PerformedBy != performedBy {
					t.Errorf("%s row %s performed_by = %q, want %q", read, a.ID, a.PerformedBy, performedBy)
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
		byConversation, _, err := f.Stores.ExternalActions.ListByConversation(ctx, f.OrgID, f.ConversationID, domain.ExternalActionListOpts{})
		if err != nil {
			t.Fatalf("ListByConversation: %v", err)
		}
		check("ListByConversation", byConversation)
	})

	t.Run("Both_writes_refuse_an_empty_or_unknown_performer", func(t *testing.T) {
		f := mk(t)
		for _, performedBy := range []string{"", "bot"} {
			if err := f.Stores.ExternalActions.Record(ctx, f.OrgID, action(f, performedBy)); !errors.Is(err, db.ErrPerformedByRequired) {
				t.Errorf("Record with performed_by %q: err = %v, want ErrPerformedByRequired", performedBy, err)
			}
			if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, action(f, performedBy)); !errors.Is(err, db.ErrPerformedByRequired) {
				t.Errorf("RecordSystem with performed_by %q: err = %v, want ErrPerformedByRequired", performedBy, err)
			}
		}
		_, total, err := f.Stores.ExternalActions.ListByOrgSystem(ctx, f.OrgID, domain.ExternalActionListOpts{CountOnly: true})
		if err != nil {
			t.Fatalf("ListByOrgSystem: %v", err)
		}
		if total != 0 {
			t.Errorf("ledger holds %d rows after refused writes, want 0", total)
		}
	})
}
