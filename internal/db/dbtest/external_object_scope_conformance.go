package dbtest

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ExternalObjectScopeFactory hands RunExternalObjectScopeConformance the store
// bundle, the org every call is for, and a team the artifacts can hang off
// (artifacts.team_id is required). Each call is a fresh, empty org.
type ExternalObjectScopeFactory func(t *testing.T) (s db.Stores, orgID, teamID string)

// RunExternalObjectScopeConformance covers the scope column artifacts and the
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
	prArtifact := func(teamID, scope, state string) domain.Artifact {
		return domain.Artifact{
			TeamID: teamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
			Scope: scope, Target: domain.PullRequestTarget("octo/api", 7), ExternalID: "7",
			URL:    scope + "/octo/api/pull/7",
			NodeID: "PR_kw7", State: state, DedupKey: domain.PullRequestDedupKey("PR_kw7"),
		}
	}

	t.Run("Artifact_upsert_persists_and_returns_the_scope", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		got, err := s.Artifacts.UpsertSystem(ctx, orgID, prArtifact(teamID, hostB, domain.ArtifactStatePROpen))
		if err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		if got.Scope != hostB {
			t.Errorf("returned scope = %q, want %q", got.Scope, hostB)
		}
		AssertWriteReturnedStoredRow(t, "UpsertSystem", got, func() (*domain.Artifact, error) {
			return s.Artifacts.Get(ctx, orgID, got.ID)
		})
		listed, err := s.Artifacts.ListNonTerminalBySystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListNonTerminalBySystem: %v", err)
		}
		if len(listed) != 1 || listed[0].Scope != hostB {
			t.Errorf("listed = %+v, want the one row carrying scope %q", listed, hostB)
		}
	})

	t.Run("The_same_dedup_key_in_two_scopes_is_two_artifacts", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		a, err := s.Artifacts.UpsertSystem(ctx, orgID, prArtifact(teamID, hostA, domain.ArtifactStatePROpen))
		if err != nil {
			t.Fatalf("UpsertSystem(%s): %v", hostA, err)
		}
		b, err := s.Artifacts.UpsertSystem(ctx, orgID, prArtifact(teamID, hostB, domain.ArtifactStatePRDraft))
		if err != nil {
			t.Fatalf("UpsertSystem(%s): %v", hostB, err)
		}
		if a.ID == b.ID {
			t.Fatalf("both scopes landed on row %s: the dedup key is unique only within a scope", a.ID)
		}

		// A second write in one scope updates that scope's row and only it.
		again, err := s.Artifacts.UpsertSystem(ctx, orgID, prArtifact(teamID, hostA, domain.ArtifactStatePRMerged))
		if err != nil {
			t.Fatalf("UpsertSystem(%s) again: %v", hostA, err)
		}
		if again.ID != a.ID || again.State != domain.ArtifactStatePRMerged {
			t.Errorf("second %s write = %+v, want row %s moved to merged", hostA, again, a.ID)
		}
		other, err := s.Artifacts.Get(ctx, orgID, b.ID)
		if err != nil || other == nil {
			t.Fatalf("Get(%s): %v, %v", b.ID, other, err)
		}
		if other.State != domain.ArtifactStatePRDraft || other.Scope != hostB || other.URL != hostB+"/octo/api/pull/7" {
			t.Errorf("%s row after a %s write = %+v, want it untouched", hostB, hostA, other)
		}

		// The insert-if-absent backstop keys on the scope as well: absent in a
		// third scope, present in its own.
		inserted, err := s.Artifacts.InsertArtifactIfAbsentSystem(ctx, orgID, prArtifact(teamID, "https://ghe.third.example", domain.ArtifactStatePROpen))
		if err != nil || !inserted {
			t.Errorf("InsertArtifactIfAbsentSystem in a new scope = %v, %v; want an insert", inserted, err)
		}
		inserted, err = s.Artifacts.InsertArtifactIfAbsentSystem(ctx, orgID, prArtifact(teamID, hostB, domain.ArtifactStatePROpen))
		if err != nil || inserted {
			t.Errorf("InsertArtifactIfAbsentSystem in a scope that holds the key = %v, %v; want no insert", inserted, err)
		}
		if got, _ := s.Artifacts.Get(ctx, orgID, b.ID); got == nil || got.State != domain.ArtifactStatePRDraft {
			t.Errorf("%s row after insert-if-absent = %+v, want its state kept", hostB, got)
		}
	})

	t.Run("An_artifact_without_a_scope_is_refused", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		unscoped := prArtifact(teamID, "", domain.ArtifactStatePROpen)
		if _, err := s.Artifacts.UpsertSystem(ctx, orgID, unscoped); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("UpsertSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
		if _, err := s.Artifacts.Upsert(ctx, orgID, unscoped); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("Upsert with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
		if _, err := s.Artifacts.InsertArtifactIfAbsentSystem(ctx, orgID, unscoped); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("InsertArtifactIfAbsentSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
		_, total, err := s.Artifacts.ListByOrgSystem(ctx, orgID, db.ArtifactListOpts{CountOnly: true})
		if err != nil {
			t.Fatalf("ListByOrgSystem: %v", err)
		}
		if total != 0 {
			t.Errorf("artifacts after refused writes = %d, want 0", total)
		}
	})

	t.Run("Pending_reviews_are_read_on_one_scope", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		review := func(scope, repoPath, nodeID, conversationID string) domain.Artifact {
			a := domain.NewReviewArtifact(repoPath, 7, nodeID, "abc123", conversationID)
			a.TeamID, a.Scope = teamID, scope
			return a
		}
		own, err := s.Artifacts.UpsertSystem(ctx, orgID, review(hostA, "octo/api", "PR_kw7", "conv-a"))
		if err != nil {
			t.Fatalf("seed review on %s: %v", hostA, err)
		}
		// The same pull request under another spelling of its repository's
		// name is still its draft: the read matches the key, not the target.
		renamed, err := s.Artifacts.UpsertSystem(ctx, orgID, review(hostA, "Octo/old-api", "PR_kw7", "conv-b"))
		if err != nil {
			t.Fatalf("seed review under another name on %s: %v", hostA, err)
		}
		// Another pull request #7 — another repository under the same name —
		// is not, and neither is the same node id on another host.
		if _, err := s.Artifacts.UpsertSystem(ctx, orgID, review(hostA, "octo/api", "PR_kw7_other", "conv-c")); err != nil {
			t.Fatalf("seed review of another PR on %s: %v", hostA, err)
		}
		if _, err := s.Artifacts.UpsertSystem(ctx, orgID, review(hostB, "octo/api", "PR_kw7", "conv-a")); err != nil {
			t.Fatalf("seed review on %s: %v", hostB, err)
		}
		got, err := s.Artifacts.ListPendingReviewsByPullRequestSystem(ctx, orgID, hostA, "PR_kw7")
		if err != nil {
			t.Fatalf("ListPendingReviewsByPullRequestSystem: %v", err)
		}
		ids := map[string]bool{}
		for _, a := range got {
			ids[a.ID] = true
		}
		if len(got) != 2 || !ids[own.ID] || !ids[renamed.ID] {
			t.Errorf("pending reviews on %s = %+v, want exactly rows %s and %s", hostA, got, own.ID, renamed.ID)
		}
		if got, err := s.Artifacts.ListPendingReviewsByPullRequestSystem(ctx, orgID, hostA, ""); err != nil || len(got) != 0 {
			t.Errorf("ListPendingReviewsByPullRequestSystem with no node id = %+v, %v; want nothing", got, err)
		}
		if _, err := s.Artifacts.ListPendingReviewsByPullRequestSystem(ctx, orgID, "", "PR_kw7"); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("ListPendingReviewsByPullRequestSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
	})

	t.Run("Present_dedup_keys_are_read_on_one_scope", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		comment := func(scope, commentID string) string {
			key := domain.ArtifactDedupKey(domain.ArtifactProviderLinear, domain.ArtifactKindComment, "uuid-1", commentID)
			if _, err := s.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
				TeamID: teamID, Provider: domain.ArtifactProviderLinear, Kind: domain.ArtifactKindComment,
				Scope: scope, Target: "ENG-1", ExternalID: commentID,
				State: domain.ArtifactStateCommentPosted, DedupKey: key,
			}); err != nil {
				t.Fatalf("seed comment %s on %s: %v", commentID, scope, err)
			}
			return key
		}
		both := comment(hostA, "c-1")
		comment(hostB, "c-1")
		onlyA := comment(hostA, "c-2")
		onlyB := comment(hostB, "c-3")
		absent := domain.ArtifactDedupKey(domain.ArtifactProviderLinear, domain.ArtifactKindComment, "uuid-1", "c-4")

		got, err := s.Artifacts.PresentDedupKeysSystem(ctx, orgID, hostA, []string{both, onlyA, onlyB, absent})
		if err != nil {
			t.Fatalf("PresentDedupKeysSystem: %v", err)
		}
		if want := map[string]bool{both: true, onlyA: true}; !maps.Equal(got, want) {
			t.Errorf("present on %s = %v, want %v", hostA, got, want)
		}
		got, err = s.Artifacts.PresentDedupKeysSystem(ctx, orgID, hostA, nil)
		if err != nil || len(got) != 0 {
			t.Errorf("PresentDedupKeysSystem with no keys = %v, %v; want an empty set", got, err)
		}
		if _, err := s.Artifacts.PresentDedupKeysSystem(ctx, orgID, "", []string{both}); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("PresentDedupKeysSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
	})

	t.Run("External_actions_persist_the_scope_and_dedup_within_it", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		push := func(scope string) domain.ExternalAction {
			return domain.ExternalAction{
				TeamID: teamID, Provider: domain.ArtifactProviderGitHub, Scope: scope,
				Action: domain.ActionBranchPushed, Target: "octo/api", ExternalID: "refs/heads/fix",
				Credential: domain.CredentialGitHubApp,
				DedupKey:   domain.BranchPushDedupKey("conv-1", "refs/heads/fix", "abc123"),
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
			TeamID: teamID, Provider: domain.ArtifactProviderJira, Action: domain.ActionIssueTransitioned,
			Target: "ENG-1", Credential: domain.CredentialJiraOrg,
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
// exec funnel stamps on a write. For fixtures that seed artifacts straight
// through a store.
func InOrgScope(t *testing.T, orgs db.OrgsStore, orgID string, a domain.Artifact) domain.Artifact {
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
