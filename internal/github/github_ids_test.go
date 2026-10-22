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

// The snapshot names who merged a pull request, and who closed it while it
// stands closed without merging, each with GitHub's id beside the login, on
// both fragments. A reopened pull request and a merged one keep their
// ClosedEvent, and neither names a closer.
func TestBuildSnapshot_RecordsMergerAndCloser(t *testing.T) {
	const closedEvents = `"closedEvents": {"nodes": [{"actor": {"__typename": "Bot", "login": "stale", "databaseId": 2740}}]}`
	for _, c := range []struct {
		name, body                                 string
		mergedBy, mergedByID, closedBy, closedByID string
	}{
		{"merged", `{"number": 1, "state": "MERGED", "merged": true, ` + closedEvents + `,
		  "mergedBy": {"__typename": "User", "login": "alice", "databaseId": 583231}}`,
			"alice", "583231", "", ""},
		{"merged by a merge queue", `{"number": 2, "state": "MERGED", "merged": true,
		  "mergedBy": {"__typename": "Bot", "login": "github-merge-queue", "databaseId": "9919"}}`,
			"github-merge-queue[bot]", "9919", "", ""},
		{"closed by a bot", `{"number": 3, "state": "CLOSED", "mergedBy": null, ` + closedEvents + `}`,
			"", "", "stale[bot]", "2740"},
		{"closed by an account read without an id", `{"number": 4, "state": "CLOSED", "closedEvents": {"nodes": [{"actor": {"__typename": "Mannequin", "login": "imported"}}]}}`,
			"", "", "imported", ""},
		{"reopened", `{"number": 5, "state": "OPEN", ` + closedEvents + `}`, "", "", "", ""},
		{"closed with no event read", `{"number": 6, "state": "CLOSED", "closedEvents": {"nodes": []}}`, "", "", "", ""},
	} {
		var pr gqlPR
		if err := json.Unmarshal([]byte(c.body), &pr); err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		for fragment, snap := range map[string]domain.PRSnapshot{"full": pr.toSnapshot(), "discovery": pr.toDiscoverySnapshot()} {
			got := [4]string{snap.MergedBy, snap.MergedByID, snap.ClosedBy, snap.ClosedByID}
			if want := [4]string{c.mergedBy, c.mergedByID, c.closedBy, c.closedByID}; got != want {
				t.Errorf("%s (%s): merged by %s/%s, closed by %s/%s; want %s/%s, %s/%s",
					c.name, fragment, got[0], got[1], got[2], got[3], want[0], want[1], want[2], want[3])
			}
		}
	}
}
