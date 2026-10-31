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
