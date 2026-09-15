package github

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestPRBody_CarriedByBothFetchPathsAndKeptOutOfSnapshotJSON pins the
// contract the tracker's description mirror rests on: the REST open-PR
// listing and both GraphQL fragments surface the PR body on the snapshot,
// and the persisted snapshot form drops it.
func TestPRBody_CarriedByBothFetchPathsAndKeptOutOfSnapshotJSON(t *testing.T) {
	const body = "## Problem\n\nThe widget frobnicates twice."

	rest, err := parseOpenPRs("octo", "repo", []byte(`[{
		"number": 1, "node_id": "PR_1", "title": "t", "state": "open",
		"body": `+strconv.Quote(body)+`,
		"user": {"login": "bob"}, "head": {"sha": "s", "ref": "f"}, "base": {"ref": "main"}
	}]`))
	if err != nil {
		t.Fatalf("parseOpenPRs: %v", err)
	}
	if len(rest) != 1 || rest[0].Snapshot.Body != body {
		t.Fatalf("REST discovery snapshot body = %q; want the listing's body", rest[0].Snapshot.Body)
	}

	var pr gqlPR
	if err := json.Unmarshal([]byte(`{"id":"PR_1","number":1,"title":"t","body":`+strconv.Quote(body)+`}`), &pr); err != nil {
		t.Fatalf("unmarshal gqlPR: %v", err)
	}
	if got := pr.toSnapshot().Body; got != body {
		t.Errorf("full-fragment snapshot body = %q; want %q", got, body)
	}
	if got := pr.toDiscoverySnapshot().Body; got != body {
		t.Errorf("discovery-fragment snapshot body = %q; want %q", got, body)
	}
	if !strings.Contains(prBaseFields, "\n\tbody\n") {
		t.Errorf("prBaseFields does not request body; both fragments must, or the mirror flips between cycles")
	}

	raw, err := json.Marshal(rest[0].Snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(raw), "frobnicates") {
		t.Errorf("snapshot_json carries the body; it must stay in-memory only: %s", raw)
	}
}
