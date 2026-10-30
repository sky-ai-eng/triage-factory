package sqlite_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestLinearTeamRulesStore_SQLite runs the shared conformance suite against
// the SQLite impl. Local mode is one org; the second team the suite needs is
// inserted straight into teams beside the seeded default team.
func TestLinearTeamRulesStore_SQLite(t *testing.T) {
	dbtest.RunLinearTeamRulesConformance(t, func(t *testing.T) dbtest.LinearTeamRulesFixture {
		t.Helper()
		conn := openSQLiteForTest(t)
		const otherTeamID = "00000000-0000-4000-8000-0000000000b2"
		if _, err := conn.Exec(
			`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
			otherTeamID, runmode.LocalDefaultOrgID, "other", "Other",
		); err != nil {
			t.Fatalf("seed second team: %v", err)
		}
		return dbtest.LinearTeamRulesFixture{
			Store:       sqlitestore.New(conn).LinearTeamRules,
			OrgID:       runmode.LocalDefaultOrgID,
			TeamID:      runmode.LocalDefaultTeamID,
			OtherTeamID: otherTeamID,
			ArchiveTeam: func(t *testing.T, teamID string) {
				if _, err := conn.Exec(`UPDATE teams SET deleted_at = ? WHERE id = ?`, time.Now().UTC(), teamID); err != nil {
					t.Fatalf("archive team: %v", err)
				}
			},
		}
	})
}

// TestLinearTeamRules_SQLite_ChecksRefuseMalformedRows: the schema itself,
// not only the Go writer, refuses a rule column of the wrong JSON shape and a
// write-target rule with members and no canonical, or the reverse, while each
// rule may be mapped or empty independently of the others. Without the shape
// checks a '{}' or 'null' members column would read as an empty rule, since
// json_array_length counts either as 0. in_review is checked on its own
// insert, beside the other three rules mapped.
func TestLinearTeamRules_SQLite_ChecksRefuseMalformedRows(t *testing.T) {
	conn := openSQLiteForTest(t)
	const ref = `{"id":"s1","name":"Todo","type":"unstarted"}`
	const arr = "[" + ref + "]"
	insert := func(pickup, inProgress string, inProgressCanonical any, done string, doneCanonical any) error {
		_, err := conn.Exec(`
			INSERT INTO linear_team_rules (team_id, linear_workspace_id, linear_team_id, linear_team_key,
				pickup_members, in_progress_members, in_progress_canonical, done_members, done_canonical)
			VALUES (?, 'ws-test', ?, 'KEY', ?, ?, ?, ?, ?)`,
			runmode.LocalDefaultTeamID, uuid.NewString(), pickup, inProgress, inProgressCanonical, done, doneCanonical)
		return err
	}

	if err := insert("[]", "[]", nil, "[]", nil); err != nil {
		t.Fatalf("an unarmed row was refused: %v", err)
	}
	if err := insert(arr, arr, ref, arr, ref); err != nil {
		t.Fatalf("an armed row was refused: %v", err)
	}
	if err := insert(arr, "[]", nil, "[]", nil); err != nil {
		t.Fatalf("a row mapping pickup alone was refused: %v", err)
	}
	if err := insert("[]", arr, ref, arr, ref); err != nil {
		t.Fatalf("a row mapping the write targets alone was refused: %v", err)
	}
	for name, row := range map[string]struct {
		pickup, inProgress, done           string
		inProgressCanonical, doneCanonical any
	}{
		"object as members":  {"{}", arr, arr, ref, ref},
		"null as members":    {"null", arr, arr, ref, ref},
		"members not json":   {"[", arr, arr, ref, ref},
		"array as canonical": {arr, arr, arr, "[]", ref},
		"empty canonical":    {arr, arr, arr, "", ref},
		"members, no target": {arr, arr, arr, nil, ref},
		"target, no members": {arr, arr, "[]", ref, ref},
	} {
		if err := insert(row.pickup, row.inProgress, row.inProgressCanonical, row.done, row.doneCanonical); err == nil {
			t.Errorf("%s: the row was accepted", name)
		}
	}

	insertReview := func(inReview string, inReviewCanonical any) error {
		_, err := conn.Exec(`
			INSERT INTO linear_team_rules (team_id, linear_workspace_id, linear_team_id, linear_team_key,
				pickup_members, in_progress_members, in_progress_canonical,
				in_review_members, in_review_canonical, done_members, done_canonical)
			VALUES (?, 'ws-test', ?, 'KEY', ?, ?, ?, ?, ?, ?, ?)`,
			runmode.LocalDefaultTeamID, uuid.NewString(), arr, arr, ref, inReview, inReviewCanonical, arr, ref)
		return err
	}
	if err := insertReview(arr, ref); err != nil {
		t.Fatalf("a row mapping in_review was refused: %v", err)
	}
	for name, row := range map[string]struct {
		members   string
		canonical any
	}{
		"in_review object as members":  {"{}", ref},
		"in_review array as canonical": {arr, "[]"},
		"in_review members, no target": {arr, nil},
		"in_review target, no members": {"[]", ref},
	} {
		if err := insertReview(row.members, row.canonical); err == nil {
			t.Errorf("%s: the row was accepted", name)
		}
	}
}
