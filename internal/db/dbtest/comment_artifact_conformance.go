package dbtest

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunCommentArtifactConformance covers what a GitHub comment's artifact
// carries beyond the common columns: the comment's node id, which a later write
// that knows only the node id finds the row by. It also pins that a
// conversation comment and a review comment with the same number are two rows,
// each found by the lookup that tells TF's comments from people's, and that a
// submitted review is found by its numeric id, the lookup that tells TF's
// reviews from people's. The factory is ExternalObjectScopeFactory's: a fresh,
// empty org and a team per call.
func RunCommentArtifactConformance(t *testing.T, mk ExternalObjectScopeFactory) {
	t.Helper()
	ctx := context.Background()
	const (
		hostA = TestGitHubHost
		hostB = TestOtherGitHubHost
	)
	comment := func(teamID, scope string, space domain.GitHubCommentSpace, id int64) domain.Artifact {
		return domain.Artifact{
			TeamID: teamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
			Scope: scope, State: domain.ArtifactStateCommentPosted,
			DedupKey: domain.GitHubCommentDedupKey(space, id),
		}
	}

	t.Run("The_node_id_persists_and_finds_its_row_on_one_scope", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		posted := comment(teamID, hostA, domain.GitHubIssueComment, 7)
		posted.Target, posted.ExternalID, posted.NodeID = "octo/api#3", "7", "IC_kwDOA7"
		got, err := s.Artifacts.UpsertSystem(ctx, orgID, posted)
		if err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		if got.NodeID != "IC_kwDOA7" {
			t.Errorf("returned row = %+v, want node id IC_kwDOA7", got)
		}
		AssertWriteReturnedStoredRow(t, "UpsertSystem", got, func() (*domain.Artifact, error) {
			return s.Artifacts.Get(ctx, orgID, got.ID)
		})

		// The same node id under another host is another object.
		other := comment(teamID, hostB, domain.GitHubIssueComment, 7)
		other.NodeID = "IC_kwDOA7"
		otherRow, err := s.Artifacts.UpsertSystem(ctx, orgID, other)
		if err != nil {
			t.Fatalf("UpsertSystem(%s): %v", hostB, err)
		}

		found, err := s.Artifacts.GetByNodeIDSystem(ctx, orgID, hostA, "IC_kwDOA7")
		if err != nil {
			t.Fatalf("GetByNodeIDSystem: %v", err)
		}
		if found == nil || found.ID != got.ID {
			t.Fatalf("GetByNodeIDSystem(%s) = %+v, want row %s", hostA, found, got.ID)
		}
		if found, _ := s.Artifacts.GetByNodeIDSystem(ctx, orgID, hostB, "IC_kwDOA7"); found == nil || found.ID != otherRow.ID {
			t.Errorf("GetByNodeIDSystem(%s) = %+v, want row %s", hostB, found, otherRow.ID)
		}
		for _, nodeID := range []string{"IC_kwDOA8", ""} {
			found, err := s.Artifacts.GetByNodeIDSystem(ctx, orgID, hostA, nodeID)
			if err != nil || found != nil {
				t.Errorf("GetByNodeIDSystem(%q) = %+v, %v; want no row", nodeID, found, err)
			}
		}
		if _, err := s.Artifacts.GetByNodeIDSystem(ctx, orgID, "", "IC_kwDOA7"); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("GetByNodeIDSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}

		// A later write that does not know the node id keeps it.
		deleted := comment(teamID, hostA, domain.GitHubIssueComment, 7)
		deleted.State = domain.ArtifactStateCommentDeleted
		after, err := s.Artifacts.UpsertSystem(ctx, orgID, deleted)
		if err != nil {
			t.Fatalf("UpsertSystem (delete): %v", err)
		}
		if after.ID != got.ID || after.NodeID != "IC_kwDOA7" || after.State != domain.ArtifactStateCommentDeleted {
			t.Errorf("row after a write without a node id = %+v, want row %s deleted with its node id kept", after, got.ID)
		}

		// The insert-if-absent path writes the node id too.
		absent := comment(teamID, hostA, domain.GitHubReviewComment, 9)
		absent.NodeID = "PRRC_kwDOA9"
		if inserted, err := s.Artifacts.InsertArtifactIfAbsentSystem(ctx, orgID, absent); err != nil || !inserted {
			t.Fatalf("InsertArtifactIfAbsentSystem = %v, %v; want an insert", inserted, err)
		}
		if found, _ := s.Artifacts.GetByNodeIDSystem(ctx, orgID, hostA, "PRRC_kwDOA9"); found == nil || found.DedupKey != absent.DedupKey {
			t.Errorf("row inserted if absent = %+v, want it found by its node id", found)
		}
	})

	t.Run("Comment_keys_are_read_on_one_scope", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		seed := func(scope string, space domain.GitHubCommentSpace, id int64) string {
			a := comment(teamID, scope, space, id)
			if _, err := s.Artifacts.UpsertSystem(ctx, orgID, a); err != nil {
				t.Fatalf("seed %s comment %d on %s: %v", space, id, scope, err)
			}
			return a.DedupKey
		}
		issue := seed(hostA, domain.GitHubIssueComment, 7)
		review := domain.GitHubCommentDedupKey(domain.GitHubReviewComment, 7)
		elsewhere := seed(hostB, domain.GitHubIssueComment, 8)
		if issue == review {
			t.Fatalf("a conversation comment and a review comment numbered 7 share key %q", issue)
		}

		got, err := s.Artifacts.PresentDedupKeysSystem(ctx, orgID, hostA, []string{issue, review, elsewhere})
		if err != nil {
			t.Fatalf("PresentDedupKeysSystem: %v", err)
		}
		if want := map[string]bool{issue: true}; !maps.Equal(got, want) {
			t.Errorf("present on %s = %v, want %v", hostA, got, want)
		}
		if got, err := s.Artifacts.PresentDedupKeysSystem(ctx, orgID, hostA, nil); err != nil || len(got) != 0 {
			t.Errorf("PresentDedupKeysSystem with no keys = %v, %v; want an empty set", got, err)
		}
		if _, err := s.Artifacts.PresentDedupKeysSystem(ctx, orgID, "", []string{issue}); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("PresentDedupKeysSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
	})

	t.Run("Review_ids_are_read_by_kind_on_one_scope", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		seed := func(a domain.Artifact, scope, externalID string) {
			a.TeamID, a.Scope, a.ExternalID = teamID, scope, externalID
			if _, err := s.Artifacts.UpsertSystem(ctx, orgID, a); err != nil {
				t.Fatalf("seed %s %q on %s: %v", a.Kind, externalID, scope, err)
			}
		}
		submitted := domain.NewReviewArtifact("octo/api", 3, "sha", "conv-a")
		submitted.State = domain.ArtifactStateReviewSubmitted
		seed(submitted, hostA, "6000000001")
		seed(domain.NewReviewArtifact("octo/api", 3, "sha", "conv-b"), hostA, "")
		seed(domain.NewReviewArtifact("octo/api", 3, "sha", "conv-c"), hostB, "6000000003")
		pr := domain.Artifact{
			Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
			Target: "octo/api#4", State: domain.ArtifactStatePROpen, DedupKey: "github:pull_request:octo/api#4",
		}
		seed(pr, hostA, "6000000002")

		got, err := s.Artifacts.PresentExternalIDsSystem(ctx, orgID, hostA, domain.ArtifactProviderGitHub, domain.ArtifactKindReview,
			[]string{"6000000001", "6000000002", "6000000003", "6000000004"})
		if err != nil {
			t.Fatalf("PresentExternalIDsSystem: %v", err)
		}
		if want := map[string]bool{"6000000001": true}; !maps.Equal(got, want) {
			t.Errorf("review ids present on %s = %v, want %v", hostA, got, want)
		}
		if got, err := s.Artifacts.PresentExternalIDsSystem(ctx, orgID, hostA, domain.ArtifactProviderGitHub, domain.ArtifactKindReview, nil); err != nil || len(got) != 0 {
			t.Errorf("PresentExternalIDsSystem with no ids = %v, %v; want an empty set", got, err)
		}
		if _, err := s.Artifacts.PresentExternalIDsSystem(ctx, orgID, "", domain.ArtifactProviderGitHub, domain.ArtifactKindReview, []string{"6000000001"}); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("PresentExternalIDsSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
	})
}
