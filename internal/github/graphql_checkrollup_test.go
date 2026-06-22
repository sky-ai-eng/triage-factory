package github

import (
	"encoding/json"
	"testing"
)

// TestToSnapshot_StatusCheckRollup pins the TFAC-440 switch from the
// suite-nested checkSuites query to the flat statusCheckRollup.contexts
// connection. It exercises the three things the new decode path must get
// right against a realistic GraphQL payload:
//
//   - flat CheckRun nodes are extracted (no per-app empty-suite padding to
//     wade through — the very thing the old query truncated on);
//   - StatusContext nodes in the union are dropped (the old suite query only
//     ever surfaced check runs, so legacy commit statuses must not leak in);
//   - WorkflowRunID is recovered from checkSuite.workflowRun.databaseId for
//     Actions-backed runs, and stays 0 for third-party CI.
func TestToSnapshot_StatusCheckRollup(t *testing.T) {
	const body = `{
	  "number": 7,
	  "repository": {"nameWithOwner": "octo/repo"},
	  "commits": {"nodes": [{"commit": {
	    "oid": "abc123",
	    "committedDate": "2026-06-22T10:00:00Z",
	    "statusCheckRollup": {
	      "contexts": {
	        "pageInfo": {"hasNextPage": false},
	        "nodes": [
	          {
	            "__typename": "CheckRun",
	            "databaseId": 1001,
	            "name": "build",
	            "status": "COMPLETED",
	            "conclusion": "SUCCESS",
	            "completedAt": "2026-06-22T10:05:00Z",
	            "detailsUrl": "https://github.com/octo/repo/actions/runs/555/job/1",
	            "checkSuite": {"workflowRun": {"databaseId": 555}}
	          },
	          {
	            "__typename": "CheckRun",
	            "databaseId": 1002,
	            "name": "vercel",
	            "status": "COMPLETED",
	            "conclusion": "FAILURE",
	            "completedAt": "2026-06-22T10:06:00Z",
	            "detailsUrl": "https://vercel.com/deploy/xyz",
	            "checkSuite": {"workflowRun": null}
	          },
	          {
	            "__typename": "StatusContext",
	            "databaseId": 0,
	            "name": "",
	            "status": "",
	            "conclusion": ""
	          }
	        ]
	      }
	    }
	  }}]}
	}`

	var pr gqlPR
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	snap := pr.toSnapshot()

	if snap.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q; want abc123", snap.HeadSHA)
	}
	// Two CheckRun nodes survive; the StatusContext node is dropped.
	if len(snap.CheckRuns) != 2 {
		t.Fatalf("CheckRuns = %+v; want 2 (StatusContext filtered out)", snap.CheckRuns)
	}

	byName := map[string]struct {
		conclusion    string
		workflowRunID int64
	}{}
	for _, cr := range snap.CheckRuns {
		byName[cr.Name] = struct {
			conclusion    string
			workflowRunID int64
		}{cr.Conclusion, cr.WorkflowRunID}
	}

	build, ok := byName["build"]
	if !ok {
		t.Fatalf("missing 'build' check run in %+v", snap.CheckRuns)
	}
	if build.conclusion != "success" {
		t.Errorf("build conclusion = %q; want lowercased 'success'", build.conclusion)
	}
	if build.workflowRunID != 555 {
		t.Errorf("build WorkflowRunID = %d; want 555 from checkSuite.workflowRun", build.workflowRunID)
	}

	vercel, ok := byName["vercel"]
	if !ok {
		t.Fatalf("missing 'vercel' check run in %+v", snap.CheckRuns)
	}
	if vercel.conclusion != "failure" {
		t.Errorf("vercel conclusion = %q; want 'failure'", vercel.conclusion)
	}
	if vercel.workflowRunID != 0 {
		t.Errorf("vercel WorkflowRunID = %d; want 0 (third-party, no workflowRun)", vercel.workflowRunID)
	}
}

// TestToSnapshot_NilRollup covers a head commit with no statusCheckRollup
// (e.g. no CI configured): CheckRuns must be the non-nil empty slice — the
// "polled, nothing here" signal the diff layer relies on — never nil, which
// would read as "unknown prior state" and suppress CI transitions.
func TestToSnapshot_NilRollup(t *testing.T) {
	const body = `{
	  "number": 8,
	  "repository": {"nameWithOwner": "octo/repo"},
	  "commits": {"nodes": [{"commit": {
	    "oid": "def456",
	    "committedDate": "2026-06-22T11:00:00Z",
	    "statusCheckRollup": null
	  }}]}
	}`

	var pr gqlPR
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	snap := pr.toSnapshot()
	if snap.CheckRuns == nil {
		t.Fatal("CheckRuns = nil; want non-nil empty (polled, no CI) on a nil rollup")
	}
	if len(snap.CheckRuns) != 0 {
		t.Errorf("CheckRuns = %+v; want empty", snap.CheckRuns)
	}
}

// TestToSnapshot_EmptyContexts covers the distinct-from-nil case: the rollup
// object exists but no checks have reported yet (contexts.nodes == []). The
// for-loop leaves raw nil, so DedupCheckRunsByName yields the non-nil empty
// slice — same "polled, no CI" signal as a nil rollup, reached via a
// different branch.
func TestToSnapshot_EmptyContexts(t *testing.T) {
	const body = `{
	  "number": 10,
	  "repository": {"nameWithOwner": "octo/repo"},
	  "commits": {"nodes": [{"commit": {
	    "oid": "ccc333",
	    "committedDate": "2026-06-22T13:00:00Z",
	    "statusCheckRollup": {"contexts": {"pageInfo": {"hasNextPage": false}, "nodes": []}}
	  }}]}
	}`

	var pr gqlPR
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	snap := pr.toSnapshot()
	if snap.CheckRuns == nil {
		t.Fatal("CheckRuns = nil; want non-nil empty on a non-nil rollup with no contexts")
	}
	if len(snap.CheckRuns) != 0 {
		t.Errorf("CheckRuns = %+v; want empty", snap.CheckRuns)
	}
}

// TestToDiscoverySnapshot_SkipsCI confirms the discovery path still leaves
// CheckRuns nil ("unknown prior state") regardless of rollup contents, so a
// freshly-discovered PR doesn't spuriously emit CI transitions on first poll.
func TestToDiscoverySnapshot_SkipsCI(t *testing.T) {
	const body = `{
	  "number": 9,
	  "repository": {"nameWithOwner": "octo/repo"},
	  "commits": {"nodes": [{"commit": {
	    "oid": "aaa111",
	    "committedDate": "2026-06-22T12:00:00Z",
	    "statusCheckRollup": {"contexts": {"pageInfo": {"hasNextPage": false}, "nodes": [
	      {"__typename": "CheckRun", "databaseId": 1, "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS"}
	    ]}}
	  }}]}
	}`

	var pr gqlPR
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if snap := pr.toDiscoverySnapshot(); snap.CheckRuns != nil {
		t.Errorf("discovery CheckRuns = %+v; want nil (CI filled by Phase 2)", snap.CheckRuns)
	}
}
