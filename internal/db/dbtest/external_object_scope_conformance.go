package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ExternalObjectScopeFactory hands RunExternalObjectScopeConformance the store
// bundle, the org every call is for, and a team the deliverables can hang off
// (deliverables.team_id is required). Each call is a fresh, empty org.
type ExternalObjectScopeFactory func(t *testing.T) (s db.Stores, orgID, teamID string)

// RunExternalObjectScopeConformance covers the scope column deliverables and the
// audit ledger carry: a write persists it and a read returns it, a write
// without one is refused and leaves nothing behind, and the dedup key is
// unique only within a scope — the same key in two scopes is two objects, so
// two rows, and a write in one scope never lands on the other's row. Reads
// keyed by an object's coordinates take the scope too.
func RunExternalObjectScopeConformance(t *testing.T, mk ExternalObjectScopeFactory) {
	t.Helper()
	ctx := context.Background()
	const (
		hostA = TestGitHubHost
		hostB = TestOtherGitHubHost
	)
	prDeliverable := func(teamID, scope, state string) domain.Deliverable {
		return domain.Deliverable{
			TeamID: teamID, Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindPullRequest,
			Scope: scope, Target: domain.PullRequestTarget("octo/api", 7), ExternalID: "7",
			URL:    scope + "/octo/api/pull/7",
			NodeID: "PR_kw7", State: state, DedupKey: domain.PullRequestDedupKey("PR_kw7"),
		}
	}

	t.Run("Deliverable_upsert_persists_and_returns_the_scope", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		got, err := s.Deliverables.UpsertSystem(ctx, orgID, prDeliverable(teamID, hostB, domain.DeliverableStatePROpen))
		if err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		if got.Scope != hostB {
			t.Errorf("returned scope = %q, want %q", got.Scope, hostB)
		}
		AssertWriteReturnedStoredRow(t, "UpsertSystem", got, func() (*domain.Deliverable, error) {
			return s.Deliverables.Get(ctx, orgID, got.ID)
		})
		listed, err := s.Deliverables.ListNonTerminalBySystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListNonTerminalBySystem: %v", err)
		}
		if len(listed) != 1 || listed[0].Scope != hostB {
			t.Errorf("listed = %+v, want the one row carrying scope %q", listed, hostB)
		}
	})

	t.Run("The_same_dedup_key_in_two_scopes_is_two_deliverables", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		a, err := s.Deliverables.UpsertSystem(ctx, orgID, prDeliverable(teamID, hostA, domain.DeliverableStatePROpen))
		if err != nil {
			t.Fatalf("UpsertSystem(%s): %v", hostA, err)
		}
		b, err := s.Deliverables.UpsertSystem(ctx, orgID, prDeliverable(teamID, hostB, domain.DeliverableStatePRDraft))
		if err != nil {
			t.Fatalf("UpsertSystem(%s): %v", hostB, err)
		}
		if a.ID == b.ID {
			t.Fatalf("both scopes landed on row %s: the dedup key is unique only within a scope", a.ID)
		}

		// A second write in one scope updates that scope's row and only it.
		again, err := s.Deliverables.UpsertSystem(ctx, orgID, prDeliverable(teamID, hostA, domain.DeliverableStatePRMerged))
		if err != nil {
			t.Fatalf("UpsertSystem(%s) again: %v", hostA, err)
		}
		if again.ID != a.ID || again.State != domain.DeliverableStatePRMerged {
			t.Errorf("second %s write = %+v, want row %s moved to merged", hostA, again, a.ID)
		}
		other, err := s.Deliverables.Get(ctx, orgID, b.ID)
		if err != nil || other == nil {
			t.Fatalf("Get(%s): %v, %v", b.ID, other, err)
		}
		if other.State != domain.DeliverableStatePRDraft || other.Scope != hostB || other.URL != hostB+"/octo/api/pull/7" {
			t.Errorf("%s row after a %s write = %+v, want it untouched", hostB, hostA, other)
		}

		// The insert-if-absent backstop keys on the scope as well: absent in a
		// third scope, present in its own.
		inserted, err := s.Deliverables.InsertDeliverableIfAbsentSystem(ctx, orgID, prDeliverable(teamID, "https://ghe.third.example", domain.DeliverableStatePROpen))
		if err != nil || !inserted {
			t.Errorf("InsertDeliverableIfAbsentSystem in a new scope = %v, %v; want an insert", inserted, err)
		}
		inserted, err = s.Deliverables.InsertDeliverableIfAbsentSystem(ctx, orgID, prDeliverable(teamID, hostB, domain.DeliverableStatePROpen))
		if err != nil || inserted {
			t.Errorf("InsertDeliverableIfAbsentSystem in a scope that holds the key = %v, %v; want no insert", inserted, err)
		}
		if got, _ := s.Deliverables.Get(ctx, orgID, b.ID); got == nil || got.State != domain.DeliverableStatePRDraft {
			t.Errorf("%s row after insert-if-absent = %+v, want its state kept", hostB, got)
		}
	})

	t.Run("A_deliverable_without_a_scope_is_refused", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		unscoped := prDeliverable(teamID, "", domain.DeliverableStatePROpen)
		if _, err := s.Deliverables.UpsertSystem(ctx, orgID, unscoped); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("UpsertSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
		if _, err := s.Deliverables.Upsert(ctx, orgID, unscoped); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("Upsert with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
		if _, err := s.Deliverables.InsertDeliverableIfAbsentSystem(ctx, orgID, unscoped); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("InsertDeliverableIfAbsentSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
		// The refused row is an open pull request, which the non-terminal
		// read would return had any write landed.
		stored, err := s.Deliverables.ListNonTerminalBySystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListNonTerminalBySystem: %v", err)
		}
		if len(stored) != 0 {
			t.Errorf("deliverables after refused writes = %d, want 0", len(stored))
		}
	})

	t.Run("Pending_reviews_are_read_by_entity", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		pr := func(key, nodeID string) string {
			t.Helper()
			e, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, domain.ProviderGitHub, hostA, key, nodeID, "pr", "t", "")
			if err != nil {
				t.Fatalf("seed entity %s: %v", key, err)
			}
			return e.ID
		}
		entityID, otherID := pr("octo/api#7", "PR_kw7"), pr("octo/other#7", "PR_kw7_other")
		review := func(repoPath, nodeID, entity, conversationID string) domain.Deliverable {
			t.Helper()
			a := domain.NewReviewDeliverable(repoPath, 7, nodeID, "abc123", conversationID)
			a.TeamID, a.Scope, a.EntityID = teamID, hostA, entity
			stored, err := s.Deliverables.UpsertSystem(ctx, orgID, a)
			if err != nil {
				t.Fatalf("seed review %s: %v", conversationID, err)
			}
			return stored
		}
		own := review("octo/api", "PR_kw7", entityID, "conv-a")
		// The same pull request under another spelling of its repository's
		// name is still its draft: the read matches the entity, not the target.
		renamed := review("Octo/old-api", "PR_kw7", entityID, "conv-b")
		review("octo/other", "PR_kw7_other", otherID, "conv-c")
		got, err := s.Deliverables.ListPendingReviewsByEntitySystem(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListPendingReviewsByEntitySystem: %v", err)
		}
		ids := map[string]bool{}
		for _, a := range got {
			ids[a.ID] = true
		}
		if len(got) != 2 || !ids[own.ID] || !ids[renamed.ID] {
			t.Errorf("pending reviews of %s = %+v, want exactly rows %s and %s", entityID, got, own.ID, renamed.ID)
		}
		if got, err := s.Deliverables.ListPendingReviewsByEntitySystem(ctx, orgID, ""); err != nil || len(got) != 0 {
			t.Errorf("ListPendingReviewsByEntitySystem with no entity = %+v, %v; want nothing", got, err)
		}
	})

	t.Run("External_actions_persist_the_scope_and_dedup_within_it", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		push := func(scope string) domain.ExternalAction {
			return domain.ExternalAction{
				TeamID: teamID, Provider: domain.ProviderGitHub, Scope: scope,
				Action: domain.ActionBranchPushed, Target: "octo/api", ExternalID: "refs/heads/fix",
				Credential:  domain.CredentialGitHubApp,
				DedupKey:    domain.BranchPushDedupKey("conv-1", "repo-1", "refs/heads/fix", "abc123"),
				PerformedBy: domain.PerformedByAgent,
			}
		}
		for _, scope := range []string{hostA, hostB, hostA} {
			if err := s.ExternalActions.RecordSystem(ctx, orgID, push(scope)); err != nil {
				t.Fatalf("RecordSystem(%s): %v", scope, err)
			}
		}
		if err := s.ExternalActions.Record(ctx, orgID, push(hostB)); err != nil {
			t.Fatalf("Record(%s): %v", hostB, err)
		}
		actions, _, err := s.ExternalActions.ListByOrgSystem(ctx, orgID, domain.ExternalActionListOpts{})
		if err != nil {
			t.Fatalf("ListByOrgSystem: %v", err)
		}
		perScope := map[string]int{}
		for _, a := range actions {
			perScope[a.Scope]++
		}
		if len(actions) != 2 || perScope[hostA] != 1 || perScope[hostB] != 1 {
			t.Errorf("actions = %+v, want one row per scope: the twin collapses within a scope, never across", actions)
		}
	})

	t.Run("An_external_action_without_a_scope_is_refused", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		unscoped := domain.ExternalAction{
			TeamID: teamID, Provider: domain.ProviderJira, Action: domain.ActionIssueTransitioned,
			Target: "ENG-1", Credential: domain.CredentialJiraOrg,
			PerformedBy: domain.PerformedByAgent,
		}
		if err := s.ExternalActions.RecordSystem(ctx, orgID, unscoped); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("RecordSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
		if err := s.ExternalActions.Record(ctx, orgID, unscoped); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("Record with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
		_, total, err := s.ExternalActions.ListByOrgSystem(ctx, orgID, domain.ExternalActionListOpts{CountOnly: true})
		if err != nil {
			t.Fatalf("ListByOrgSystem: %v", err)
		}
		if total != 0 {
			t.Errorf("actions after refused writes = %d, want 0", total)
		}
	})
}

// InOrgScope returns a with its Scope set to orgID's current scope for its
// provider (domain.ExternalObjectScope) when it names none — the scope the
// exec funnel stamps on a write. For fixtures that seed deliverables straight
// through a store.
func InOrgScope(t *testing.T, orgs db.OrgsStore, orgID string, a domain.Deliverable) domain.Deliverable {
	t.Helper()
	if a.Scope == "" {
		a.Scope = orgScopeFor(t, orgs, orgID, a.Provider)
	}
	return a
}

// ActionInOrgScope is InOrgScope for an audit-ledger row.
func ActionInOrgScope(t *testing.T, orgs db.OrgsStore, orgID string, act domain.ExternalAction) domain.ExternalAction {
	t.Helper()
	if act.Scope == "" {
		act.Scope = orgScopeFor(t, orgs, orgID, act.Provider)
	}
	return act
}

func orgScopeFor(t *testing.T, orgs db.OrgsStore, orgID, provider string) string {
	t.Helper()
	set, err := orgs.GetSettingsSystem(context.Background(), orgID)
	if err != nil {
		t.Fatalf("read org settings for an external object's scope: %v", err)
	}
	return domain.ExternalObjectScope(provider, set)
}
