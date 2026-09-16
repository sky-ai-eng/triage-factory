package github

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

func TestPRBodyFingerprint_PresenceAndTransportParity(t *testing.T) {
	for _, field := range []string{"", `,"body":null`, `,"body":""`, `,"body":"first"`, `,"body":"` + strings.Repeat("x", 2500) + `tail"`} {
		t.Run(fmtBodyCase(field), func(t *testing.T) {
			wire := `{"number":1,"id":"PR_1","node_id":"PR_1"` + field + `}`
			var pr gqlPR
			if err := json.Unmarshal([]byte(wire), &pr); err != nil {
				t.Fatal(err)
			}
			rest, err := parseOpenPRs("o", "r", []byte("["+wire+"]"))
			if err != nil || len(rest) != 1 {
				t.Fatalf("parse REST: %v", err)
			}
			for _, snap := range []domain.PRSnapshot{pr.toSnapshot(), pr.toDiscoverySnapshot(), rest[0].Snapshot} {
				if (snap.BodyHash == "") != (field == "") {
					t.Fatalf("absent and cleared body conflated: %+v", snap)
				}
				if snap.BodyHash != rest[0].Snapshot.BodyHash {
					t.Fatal("REST and GraphQL disagree on body revision")
				}
			}
		})
	}
}

func fmtBodyCase(field string) string {
	if len(field) > 40 {
		return "long body"
	}
	return field
}
