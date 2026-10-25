package server

import (
	"net/http"
	"reflect"
	"testing"
)

// The optional in_review rule on a Linear team, from the write side. The
// poller and the claim path read none of it, so what these pin is that it is
// checked like the other write targets, that it round-trips, and that mapping
// or clearing it neither arms nor disarms a team nor re-dues a poll.

func linearInReview() map[string]any {
	return map[string]any{"member_ids": []string{linearStateReview}, "canonical_id": linearStateReview}
}

// TestLinearTeamsPut_InReviewOnAnArmedTeam: an element naming only in_review
// on an armed team stores the rule, carries the other three as stored, and
// leaves the team armed. In Review is also an in_progress member of
// armedLinearTeam, so the state sits in both rules. Clearing it again keeps
// the team armed. Neither write re-dues the poll.
func TestLinearTeamsPut_InReviewOnAnArmedTeam(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)
	armed := mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng))[0]
	kicked := linearKicks(t, s)

	got := mustPutLinearTeams(t, s, map[string]any{"id": linearTeamEng, "in_review": linearInReview()})
	if len(got) != 1 {
		t.Fatalf("echo = %+v, want one team", got)
	}
	eng := got[0]
	if !eng.Armed {
		t.Errorf("mapping in_review disarmed the team: %+v", eng)
	}
	want := linearStateRefJSON{ID: linearStateReview, Name: "In Review", Type: "started"}
	if eng.InReview.Canonical == nil || *eng.InReview.Canonical != want {
		t.Errorf("in_review canonical = %+v, want %+v", eng.InReview.Canonical, want)
	}
	if !reflect.DeepEqual(eng.InReview.Members, []linearStateRefJSON{want}) {
		t.Errorf("in_review members = %+v, want [%+v]", eng.InReview.Members, want)
	}
	if !reflect.DeepEqual(eng.Pickup, armed.Pickup) || !reflect.DeepEqual(eng.InProgress, armed.InProgress) || !reflect.DeepEqual(eng.Done, armed.Done) {
		t.Errorf("mapping in_review moved another rule:\n%+v\nwant those of\n%+v", eng, armed)
	}
	if read := storedLinearTeamsRead(t, s); !reflect.DeepEqual(read, got) {
		t.Errorf("settings read after the write =\n%+v\nwant the echo\n%+v", read, got)
	}
	if kicked() {
		t.Error("mapping in_review re-dued Linear polling")
	}

	cleared := mustPutLinearTeams(t, s, map[string]any{
		"id": linearTeamEng, "in_review": map[string]any{"member_ids": []string{}, "canonical_id": ""},
	})
	if !reflect.DeepEqual(cleared, []linearTeamSettings{armed}) {
		t.Errorf("echo after clearing in_review =\n%+v\nwant the armed team as first stored\n%+v", cleared, armed)
	}
	if kicked() {
		t.Error("clearing in_review re-dued Linear polling")
	}
}

// TestLinearTeamsPut_InReviewAloneArmsNothing: a watched team that maps only
// in_review is stored, and reads as unarmed.
func TestLinearTeamsPut_InReviewAloneArmsNothing(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)
	kicked := linearKicks(t, s)

	got := mustPutLinearTeams(t, s, map[string]any{"id": linearTeamEng, "in_review": linearInReview()})
	if len(got) != 1 || got[0].Armed || got[0].InReview.Canonical == nil {
		t.Errorf("echo = %+v, want one unarmed team with in_review mapped", got)
	}
	if kicked() {
		t.Error("mapping in_review alone re-dued Linear polling")
	}
}

// TestLinearTeamsPut_InReviewRuleFaults: optional is not unchecked. in_review
// is members and a canonical together, the canonical one of the members,
// decided before Linear is asked; and its states must be the team's own.
func TestLinearTeamsPut_InReviewRuleFaults(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	for _, c := range []struct {
		name  string
		rule  map[string]any
		field string
	}{
		{"members, no canonical", map[string]any{"member_ids": []string{linearStateReview}}, "linear_teams[0].in_review.canonical_id"},
		{"canonical outside members", map[string]any{"member_ids": []string{linearStateReview}, "canonical_id": linearStateDoing}, "linear_teams[0].in_review.canonical_id"},
		{"canonical, no members", map[string]any{"member_ids": []string{}, "canonical_id": linearStateReview}, "linear_teams[0].in_review.member_ids"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := armedLinearTeam(linearTeamEng)
			e["in_review"] = c.rule
			assertOneFault(t, putLinearTeams(t, s, e), http.StatusUnprocessableEntity, "INVALID_FIELD", c.field)
		})
	}
	if fake.Calls() != 0 {
		t.Errorf("an in_review rule fault reached Linear %d times", fake.Calls())
	}

	e := armedLinearTeam(linearTeamEng)
	e["in_review"] = map[string]any{"member_ids": []string{linearStateUnknown}, "canonical_id": linearStateUnknown}
	assertOneFault(t, putLinearTeams(t, s, e), http.StatusUnprocessableEntity, "INVALID_FIELD", "linear_teams[0].in_review.member_ids")

	if got := storedLinearTeamsRead(t, s); len(got) != 0 {
		t.Errorf("a refused write stored %+v", got)
	}
}
