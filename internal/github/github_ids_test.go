package github

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The snapshot records GitHub's id beside every handle a poll reads one for:
// the PR author, each review's author, and each requested user and team. An
// actor GitHub returns without one (a deleted account) records none.
func TestBuildSnapshot_RecordsGitHubIDs(t *testing.T) {
	const body = `{
	  "number": 9,
	  "repository": {"nameWithOwner": "octo/repo"},
	  "author": {"__typename": "User", "login": "alice", "databaseId": 583231},
	  "reviewRequests": {"nodes": [
	    {"requestedReviewer": {"login": "bob", "databaseId": 77}},
	    {"requestedReviewer": {"slug": "core", "databaseId": 11, "organization": {"login": "octo"}}},
	    {"requestedReviewer": {"login": "ghostly"}}
	  ]},
	  "latestReviews": {"nodes": [
	    {"id": "PRR_1", "author": {"__typename": "Bot", "login": "renovate", "databaseId": "2740"}, "state": "COMMENTED", "submittedAt": "2026-10-01T10:00:00Z"},
	    {"id": "PRR_2", "author": {"__typename": "Mannequin", "login": "imported"}, "state": "APPROVED", "submittedAt": "2026-10-01T11:00:00Z"}
	  ]}
	}`
	var pr gqlPR
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	snap := pr.toSnapshot()
	if snap.AuthorID != "583231" {
		t.Errorf("AuthorID = %q, want 583231", snap.AuthorID)
	}
	wantRequests := []domain.RequestedReviewer{
		{Handle: "bob", ID: "77"},
		{Handle: "octo/core", ID: "11", Team: true},
		{Handle: "ghostly"},
	}
	if got := snap.RequestedReviewers(); !reflect.DeepEqual(got, wantRequests) {
		t.Errorf("RequestedReviewers = %+v, want %+v", got, wantRequests)
	}
	if snap.Reviews[0].AuthorID != "2740" || snap.Reviews[1].AuthorID != "" {
		t.Errorf("review author ids = %q, %q; want 2740 and none", snap.Reviews[0].AuthorID, snap.Reviews[1].AuthorID)
	}
}

// The REST listing of open pull requests carries the same ids, on the author
// and on every requested user and team.
func TestRESTDiscoverySnapshot_RecordsGitHubIDs(t *testing.T) {
	const body = `[{
	  "number": 4, "node_id": "PR_4", "state": "open",
	  "user": {"login": "alice", "id": 583231},
	  "requested_reviewers": [{"login": "bob", "id": 77}],
	  "requested_teams": [{"slug": "core", "id": 11}]
	}]`
	prs, err := parseOpenPRs("octo", "repo", []byte(body))
	if err != nil {
		t.Fatalf("parseOpenPRs: %v", err)
	}
	snap := prs[0].Snapshot
	if snap.AuthorID != "583231" {
		t.Errorf("AuthorID = %q, want 583231", snap.AuthorID)
	}
	want := []domain.RequestedReviewer{{Handle: "bob", ID: "77"}, {Handle: "octo/core", ID: "11", Team: true}}
	if got := snap.RequestedReviewers(); !reflect.DeepEqual(got, want) {
		t.Errorf("RequestedReviewers = %+v, want %+v", got, want)
	}
}
