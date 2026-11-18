package slack

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// twoTeamsWatching sets up an org whose two teams both watch C1, team1 first
// and so its primary, and whose workspace also lists C2, which neither
// watches.
func twoTeamsWatching(t *testing.T, r *slackChannelsRig, slug string) (orgID, owner, team1, team2 string) {
	t.Helper()
	orgID, owner, team1 = pgtest.SeedOrgWithUser(t, r.h, slug)
	r.listChannels(orgID, owner, "C1", "C2")
	team2 = pgtest.SeedTeam(t, r.h, orgID, "team2")
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, owner, team2)
	for _, teamID := range []string{team1, team2} {
		rec := httptest.NewRecorder()
		r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/teams/"+teamID+"/slack-channels", owner, orgID, teamID,
			map[string]any{"channel_ids": []string{"C1"}}))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT(%s) status = %d (body=%s)", teamID, rec.Code, rec.Body.String())
		}
	}
	return orgID, owner, team1, team2
}

// primaryOf reads teamID's row of C1 and reports whether teamID is its
// primary.
func primaryOf(t *testing.T, r *slackChannelsRig, owner, orgID, teamID string) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	r.hdl.handleGet(rec, r.reqGet(owner, orgID, teamID, "C1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET C1 for %s status = %d (body=%s)", teamID, rec.Code, rec.Body.String())
	}
	return decodeBody[channelView](t, rec).IsPrimary
}

// TestChannelsHandler_Patch_IsPrimaryField: true moves the primary to the
// team; false and null are 422 on is_primary and change nothing; a value that
// is not a boolean is 400; an empty body changes nothing and answers the row.
func TestChannelsHandler_Patch_IsPrimaryField(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, team1, team2 := twoTeamsWatching(t, r, "chan-patch-field")

	for _, body := range []string{`{"is_primary":false}`, `{"is_primary":null}`} {
		rec := httptest.NewRecorder()
		r.hdl.handlePatch(rec, r.reqPatch(owner, orgID, team1, "C1", rawJSON(body)))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s status = %d, want 422 (body=%s)", body, rec.Code, rec.Body.String())
		}
		errs := decodeErrors(t, rec)
		if len(errs) != 1 || errs[0].Reason != httpx.ReasonInvalidField || errs[0].Field != "is_primary" {
			t.Errorf("%s errors = %+v; want one INVALID_FIELD on is_primary", body, errs)
		}
	}
	if !primaryOf(t, r, owner, orgID, team1) {
		t.Fatal("team1 lost the primary to a refused PATCH")
	}

	bad := httptest.NewRecorder()
	r.hdl.handlePatch(bad, r.reqPatch(owner, orgID, team2, "C1", rawJSON(`{"is_primary":"yes"}`)))
	if bad.Code != http.StatusBadRequest {
		t.Errorf("a string is_primary status = %d, want 400 (body=%s)", bad.Code, bad.Body.String())
	}
	unknown := httptest.NewRecorder()
	r.hdl.handlePatch(unknown, r.reqPatch(owner, orgID, team2, "C1", rawJSON(`{"team_id":"x"}`)))
	if unknown.Code != http.StatusBadRequest {
		t.Errorf("an unknown field status = %d, want 400 (body=%s)", unknown.Code, unknown.Body.String())
	}

	empty := httptest.NewRecorder()
	r.hdl.handlePatch(empty, r.reqPatch(owner, orgID, team2, "C1", map[string]any{}))
	if empty.Code != http.StatusOK {
		t.Fatalf("empty PATCH status = %d, want 200 (body=%s)", empty.Code, empty.Body.String())
	}
	if view := decodeBody[channelView](t, empty); view.ChannelID != "C1" || view.IsPrimary || !view.Tracked {
		t.Fatalf("empty PATCH answer = %+v; want team2's row of C1, watched and not primary", view)
	}
	if !primaryOf(t, r, owner, orgID, team1) {
		t.Fatal("an empty PATCH moved the primary")
	}

	moved := httptest.NewRecorder()
	r.hdl.handlePatch(moved, r.reqPatch(owner, orgID, team2, "C1", map[string]any{"is_primary": true}))
	if moved.Code != http.StatusOK {
		t.Fatalf("PATCH true status = %d, want 200 (body=%s)", moved.Code, moved.Body.String())
	}
	if !decodeBody[channelView](t, moved).IsPrimary || primaryOf(t, r, owner, orgID, team1) || !primaryOf(t, r, owner, orgID, team2) {
		t.Fatal("PATCH true did not move the primary from team1 to team2")
	}
}

// TestChannelsHandler_Patch_Refusals: a channel the team does not watch is
// 404, and so is an id that is not a channel's; a team admin who is not an org
// admin is refused, and nothing moves.
func TestChannelsHandler_Patch_Refusals(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, team1, team2 := twoTeamsWatching(t, r, "chan-patch-refusals")

	for _, channelID := range []string{"C2", "C9", "not-a-channel"} {
		rec := httptest.NewRecorder()
		r.hdl.handlePatch(rec, r.reqPatch(owner, orgID, team1, channelID, map[string]any{"is_primary": true}))
		if rec.Code != http.StatusNotFound {
			t.Errorf("PATCH %s status = %d, want 404 (body=%s)", channelID, rec.Code, rec.Body.String())
		}
	}

	teamAdmin := pgtest.SeedUser(t, r.h, "chan-patch-teamadmin")
	pgtest.AddOrgMember(t, r.h, teamAdmin, orgID, team2, "member", "admin")
	rec := httptest.NewRecorder()
	r.hdl.handlePatch(rec, r.reqPatch(teamAdmin, orgID, team2, "C1", map[string]any{"is_primary": true}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("team admin PATCH status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if !primaryOf(t, r, owner, orgID, team1) {
		t.Fatal("a refused PATCH moved the primary")
	}
}

// TestChannelsHandler_Patch_OrgAdminOffTheTeam: an org admin who is not on the
// team takes its primary and is answered with the team's row as it now
// stands: watched, and primary.
func TestChannelsHandler_Patch_OrgAdminOffTheTeam(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, team1, team2 := twoTeamsWatching(t, r, "chan-patch-orgadmin")
	orgAdmin := pgtest.SeedUser(t, r.h, "chan-patch-orgadmin-admin")
	pgtest.AddOrgMember(t, r.h, orgAdmin, orgID, team1, "admin", "member")

	rec := httptest.NewRecorder()
	r.hdl.handlePatch(rec, r.reqPatch(orgAdmin, orgID, team2, "C1", map[string]any{"is_primary": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if view := decodeBody[channelView](t, rec); view.ChannelID != "C1" || !view.Tracked || !view.IsPrimary {
		t.Fatalf("answer = %+v; want team2's row of C1, watched and primary", view)
	}
	if !primaryOf(t, r, owner, orgID, team2) || primaryOf(t, r, owner, orgID, team1) {
		t.Fatal("the primary did not move to team2")
	}
}

// TestChannelsHandler_Get_MatchesList: the point read answers the item the
// list answers for a channel, watched or only listed in Slack, and 404 for a
// channel the team could not watch or a caller outside the team.
func TestChannelsHandler_Get_MatchesList(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, team1, _ := twoTeamsWatching(t, r, "chan-get")

	list := httptest.NewRecorder()
	r.hdl.handleList(list, r.listReq(owner, orgID, team1, map[string]any{}))
	items := decodeChannelList(t, list).Items
	if len(items) != 2 {
		t.Fatalf("list = %+v; want C1 and C2", items)
	}
	for _, item := range items {
		rec := httptest.NewRecorder()
		r.hdl.handleGet(rec, r.reqGet(owner, orgID, team1, item.ChannelID))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d (body=%s)", item.ChannelID, rec.Code, rec.Body.String())
		}
		if got := decodeBody[channelView](t, rec); !reflect.DeepEqual(got, item) {
			t.Errorf("GET %s = %+v; want the list's item %+v", item.ChannelID, got, item)
		}
	}

	for _, channelID := range []string{"C9", "not-a-channel"} {
		rec := httptest.NewRecorder()
		r.hdl.handleGet(rec, r.reqGet(owner, orgID, team1, channelID))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404 (body=%s)", channelID, rec.Code, rec.Body.String())
		}
	}

	outsider := pgtest.SeedUser(t, r.h, "chan-get-outsider")
	otherTeam := pgtest.SeedTeam(t, r.h, orgID, "other")
	pgtest.AddOrgMember(t, r.h, outsider, orgID, otherTeam, "member", "member")
	rec := httptest.NewRecorder()
	r.hdl.handleGet(rec, r.reqGet(outsider, orgID, team1, "C1"))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), httpx.ReasonNotFound) {
		t.Errorf("GET as a non-member of the team = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// rawJSON is a request body sent as written, for a value a Go literal cannot
// spell, such as an explicit null.
type rawJSON string

func (j rawJSON) MarshalJSON() ([]byte, error) { return []byte(j), nil }
