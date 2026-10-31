package domain

import "testing"

func TestArtifactDeliverable(t *testing.T) {
	const host = "https://github.com"
	pr := Deliverable{ID: "d-pr", Kind: DeliverableKindPullRequest, Scope: host, EntityID: "e1"}
	review := Deliverable{ID: "d-review", Kind: DeliverableKindReview, Scope: host, ExternalID: "4242"}
	unsent := Deliverable{ID: "d-unsent", Kind: DeliverableKindReview, Scope: host, ExternalID: "0"}
	branch := Deliverable{ID: "d-branch", Kind: DeliverableKindBranch, Scope: host, EntityID: "e1"}
	all := []Deliverable{branch, pr, review, unsent}

	cases := []struct {
		name     string
		artifact Artifact
		want     string
	}{
		{"pull request on its entity", Artifact{ObjectKind: ObjectKindPullRequest, Scope: host, EntityID: "e1"}, "d-pr"},
		{"pull request on another entity", Artifact{ObjectKind: ObjectKindPullRequest, Scope: host, EntityID: "e2"}, ""},
		{"posted review", Artifact{ObjectKind: ObjectKindReview, Scope: host, ObjectKey: GitHubReviewObjectKey("4242")}, "d-review"},
		{"review on another host", Artifact{ObjectKind: ObjectKindReview, Scope: "https://ghe.example", ObjectKey: GitHubReviewObjectKey("4242")}, ""},
		{"review no deliverable carries", Artifact{ObjectKind: ObjectKindReview, Scope: host, ObjectKey: GitHubReviewObjectKey("1")}, ""},
		{"branch, which no deliverable matches", Artifact{ObjectKind: ObjectKindBranch, Scope: host, EntityID: "e1"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ArtifactDeliverable(c.artifact, all)
			if ok != (c.want != "") || got.ID != c.want {
				t.Errorf("ArtifactDeliverable = (%q, %v), want %q", got.ID, ok, c.want)
			}
		})
	}
}

func TestIsObjectID(t *testing.T) {
	for s, want := range map[string]bool{
		"branch|repo|refs/heads/x": true,
		"pull_request|e1":          true,
		"widget|x":                 false,
		"branch|":                  false,
		"branch":                   false,
		"":                         false,
	} {
		if got := IsObjectID(s); got != want {
			t.Errorf("IsObjectID(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestArtifactDeliverableKeys(t *testing.T) {
	entityIDs, reviewIDs := ArtifactDeliverableKeys([]Artifact{
		{ObjectKind: ObjectKindPullRequest, EntityID: "e2"},
		{ObjectKind: ObjectKindPullRequest, EntityID: "e1"},
		{ObjectKind: ObjectKindPullRequest, EntityID: "e2"},
		{ObjectKind: ObjectKindPullRequest},
		{ObjectKind: ObjectKindReview, ObjectKey: GitHubReviewObjectKey("77")},
		{ObjectKind: ObjectKindReview, ObjectKey: GitHubReviewObjectKey("5")},
		// Keys that name no review id contribute none.
		{ObjectKind: ObjectKindReview, ObjectKey: "github:review:0"},
		{ObjectKind: ObjectKindReview, ObjectKey: "github:review:abc"},
		{ObjectKind: ObjectKindReview, ObjectKey: "github:review:007"},
		{ObjectKind: ObjectKindReview, ObjectKey: "jira:review:9"},
		// Kinds no deliverable matches contribute nothing, whatever they carry.
		{ObjectKind: ObjectKindBranch, EntityID: "e3", ObjectKey: GitHubReviewObjectKey("8")},
		{ObjectKind: ObjectKindIssue, EntityID: "e4"},
	})
	if len(entityIDs) != 2 || entityIDs[0] != "e1" || entityIDs[1] != "e2" {
		t.Errorf("entity ids = %v, want [e1 e2]", entityIDs)
	}
	if len(reviewIDs) != 2 || reviewIDs[0] != "5" || reviewIDs[1] != "77" {
		t.Errorf("review ids = %v, want [5 77]", reviewIDs)
	}
	if e, r := ArtifactDeliverableKeys(nil); len(e) != 0 || len(r) != 0 {
		t.Errorf("no artifacts = (%v, %v), want none", e, r)
	}
}
