package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// notificationsRig is one org with three people in it: its owner, who is an
// org admin and no member of the infra team; the infra team's admin, an org
// member; and an org member on no team. teamCondition is open for infra, so
// the org's admins have an org row and infra's admins a team row.
type notificationsRig struct {
	*authRig
	orgID, infra                      uuid.UUID
	ownerSid, infraAdminSid, plainSid string
	owner, infraAdmin                 uuid.UUID
	pings                             *capturedPings
	// untracks is every call the "untrack" handler took; it reports the
	// condition fixed.
	untracks                           []notify.ActionCall[notificationTestParams]
	orgRowID, teamRowID                string
	orgNotifications, infraTeamRowPath string
}

func newNotificationsRig(t *testing.T) *notificationsRig {
	t.Helper()
	r := &notificationsRig{authRig: newAuthRig(t)}
	r.owner = r.seedUser()
	r.orgID, _ = r.seedOrg(r.owner, "notifications-org")
	r.infraAdmin = r.seedUser()
	plain := r.seedUser()
	for _, u := range []uuid.UUID{r.infraAdmin, plain} {
		if _, err := r.h.AdminDB.Exec(
			`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, u, r.orgID); err != nil {
			t.Fatalf("seed org membership: %v", err)
		}
	}
	r.infra = r.seedTeam(r.orgID, "infra", "admin", r.infraAdmin)

	r.ownerSid, r.infraAdminSid, r.plainSid = r.signIn(r.owner), r.signIn(r.infraAdmin), r.signIn(plain)
	for _, sid := range []string{r.ownerSid, r.infraAdminSid, r.plainSid} {
		r.setActiveOrg(t, sid, r.orgID)
	}

	r.pings = installNotify(t, r.srv)
	// Registered before anything reads the rows: an action is allowed only
	// where a handler exists.
	teamCondition.HandleAction("untrack", func(_ context.Context, call notify.ActionCall[notificationTestParams]) (notify.ActionResult, error) {
		r.untracks = append(r.untracks, call)
		return notify.ActionResult{Outcome: "Untracked.", Resolved: domain.NotificationResolvedAudienceRemoved}, nil
	})
	teamCondition.Set(context.Background(), r.orgID.String(), notificationTestParams{ID: "t1", TeamID: r.infra.String()}, true)

	r.orgNotifications = "/api/orgs/" + r.orgID.String() + "/notifications/list"
	for _, it := range r.list(r.ownerSid, "", r.orgNotifications, nil).Items {
		switch it.Audience.Scope {
		case "org":
			r.orgRowID = it.ID
		case "team":
			r.teamRowID = it.ID
		}
	}
	if r.orgRowID == "" || r.teamRowID == "" {
		t.Fatalf("after Set: org row %q, team row %q; want both", r.orgRowID, r.teamRowID)
	}
	r.infraTeamRowPath = meNotifications + "/" + r.teamRowID
	r.pings.take()
	return r
}

func (r *notificationsRig) list(sid, bearer, path string, body map[string]any) httpxtest.ListEnvelope[notificationItemWire] {
	r.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	return httpxtest.DecodeList[notificationItemWire](r.t, r.tokensJSON(http.MethodPost, path, body, sid, bearer))
}

// TestNotifications_Postgres_SentAndReadable pins the two predicates apart:
// the inbox is what a person is sent, so an org admin's holds the org row and
// not the team's; the All-teams list and the single read are what they may
// read, which adds every team's rows.
func TestNotifications_Postgres_SentAndReadable(t *testing.T) {
	r := newNotificationsRig(t)

	owner := r.list(r.ownerSid, "", meNotifications+"/list", nil)
	if owner.Total() != 1 || owner.Items[0].ID != r.orgRowID {
		t.Errorf("org admin's inbox = %+v; want the org row alone", owner.Items)
	}
	if a := owner.Items[0].Audience; a.Role == nil || *a.Role != "admins" || a.TeamID != nil || a.UserID != nil {
		t.Errorf("org row audience = %+v", a)
	}
	if !owner.Items[0].Sent {
		t.Error("the org admin's inbox row reads as not sent")
	}

	infra := r.list(r.infraAdminSid, "", meNotifications+"/list", nil)
	if infra.Total() != 1 || infra.Items[0].ID != r.teamRowID {
		t.Fatalf("infra admin's inbox = %+v; want the team row alone", infra.Items)
	}
	if a := infra.Items[0].Audience; a.Scope != "team" || a.TeamID == nil || *a.TeamID != r.infra.String() {
		t.Errorf("team row audience = %+v", a)
	}
	if acts := infra.Items[0].Actions; len(acts) != 1 || acts[0].ID != "untrack" || acts[0].Allowed == nil || !*acts[0].Allowed {
		t.Errorf("team row actions for its team admin = %+v; want untrack allowed", acts)
	}

	if got := r.list(r.plainSid, "", meNotifications+"/list", nil); got.Total() != 0 {
		t.Errorf("a member of no team was sent %+v", got.Items)
	}

	all := r.list(r.ownerSid, "", r.orgNotifications, nil)
	if all.Total() != 2 {
		t.Fatalf("All teams for the org admin = %d rows, want 2", all.Total())
	}
	for _, it := range all.Items {
		// The org row is the admin's own; the team's is one they read without
		// being sent it.
		if it.Sent != (it.ID == r.orgRowID) {
			t.Errorf("All teams row %s sent = %v", it.ID, it.Sent)
		}
		if it.ID != r.teamRowID {
			continue
		}
		// They may read it, but untracking is the team admin's.
		if acts := it.Actions; len(acts) != 1 || acts[0].Allowed == nil || *acts[0].Allowed {
			t.Errorf("team row actions for an org admin outside the team = %+v; want untrack not allowed", acts)
		}
	}
	// The org spelled in capitals is the same org, and its rows' actions are
	// checked against it.
	upper := r.list(r.ownerSid, "", "/api/orgs/"+strings.ToUpper(r.orgID.String())+"/notifications/list", nil)
	if upper.Total() != 2 {
		t.Errorf("All teams addressed in capitals = %d rows, want 2", upper.Total())
	}
	narrowed := r.list(r.ownerSid, "", r.orgNotifications, map[string]any{"team_id_in": []string{r.infra.String()}})
	if narrowed.Total() != 1 || narrowed.Items[0].ID != r.teamRowID {
		t.Errorf("All teams narrowed to infra = %+v", narrowed.Items)
	}
	if got := r.list(r.ownerSid, "", r.orgNotifications, map[string]any{"scope_in": []string{"org"}, "page_size": 0}); got.Total() != 1 {
		t.Errorf("All teams org-scope count = %d, want 1", got.Total())
	}

	// The single read is "may read": the org admin reaches the team row by id,
	// a member of no team reaches neither row.
	if rec := r.tokensJSON(http.MethodGet, r.infraTeamRowPath, nil, r.ownerSid, ""); rec.Code != http.StatusOK {
		t.Errorf("org admin GET team row = %d: %s", rec.Code, rec.Body.String())
	}
	for _, id := range []string{r.orgRowID, r.teamRowID} {
		assertFault(t, r.tokensJSON(http.MethodGet, meNotifications+"/"+id, nil, r.plainSid, ""), http.StatusNotFound, httpx.ReasonNotFound)
		assertFault(t, r.tokensJSON(http.MethodPatch, meNotifications+"/"+id, map[string]any{"read": true}, r.plainSid, ""), http.StatusNotFound, httpx.ReasonNotFound)
	}
}

// TestNotifications_Postgres_Audience: the audience filters select what the
// by_audience values name, on both lists, and the facets count each list's own
// rows: the org admin's inbox holds no team row, their All-teams list does.
func TestNotifications_Postgres_Audience(t *testing.T) {
	r := newNotificationsRig(t)
	team := "team:" + r.infra.String()

	for _, c := range []struct {
		name, sid, path string
		body            map[string]any
		want            []string
	}{
		{"org admins on All teams", r.ownerSid, r.orgNotifications, map[string]any{"scope_in": []string{"org"}, "role_in": []string{"admins"}}, []string{r.orgRowID}},
		{"admins on All teams", r.ownerSid, r.orgNotifications, map[string]any{"role_in": []string{"admins"}}, []string{r.orgRowID, r.teamRowID}},
		{"members on All teams", r.ownerSid, r.orgNotifications, map[string]any{"role_in": []string{"members"}}, nil},
		{"infra on All teams", r.ownerSid, r.orgNotifications, map[string]any{"team_id_in": []string{r.infra.String()}}, []string{r.teamRowID}},
		{"infra in the team admin's inbox", r.infraAdminSid, meNotifications + "/list", map[string]any{"team_id_in": []string{strings.ToUpper(r.infra.String())}}, []string{r.teamRowID}},
		{"infra in the org admin's inbox", r.ownerSid, meNotifications + "/list", map[string]any{"team_id_in": []string{r.infra.String()}}, nil},
	} {
		got := r.list(c.sid, "", c.path, c.body)
		var ids []string
		for _, it := range got.Items {
			ids = append(ids, it.ID)
		}
		slices.Sort(ids)
		slices.Sort(c.want)
		if !slices.Equal(ids, c.want) || got.Total() != len(c.want) {
			t.Errorf("%s = %v (total %d), want %v", c.name, ids, got.Total(), c.want)
		}
	}

	facets := func(sid, path string) notificationFacetsWire {
		t.Helper()
		rec := r.tokensJSON(http.MethodPost, path, map[string]any{"status": "open"}, sid, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		var out notificationFacetsWire
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode facets: %v", err)
		}
		return out
	}
	orgFacets := "/api/orgs/" + r.orgID.String() + "/notifications/facets"
	for _, c := range []struct {
		name, sid, path string
		want            []httpx.FacetJSON
	}{
		{"org admin's inbox", r.ownerSid, meNotifications + "/facets", []httpx.FacetJSON{{Value: "org:admins", Count: 1}}},
		{"team admin's inbox", r.infraAdminSid, meNotifications + "/facets", []httpx.FacetJSON{{Value: team, Count: 1}}},
		{"All teams", r.ownerSid, orgFacets, []httpx.FacetJSON{{Value: "org:admins", Count: 1}, {Value: team, Count: 1}}},
	} {
		got := facets(c.sid, c.path)
		if !slices.Equal(got.ByAudience, c.want) {
			t.Errorf("%s by_audience = %+v, want %+v", c.name, got.ByAudience, c.want)
		}
		var n int
		for _, p := range got.ByPriority {
			n += p.Count
		}
		if n != len(c.want) {
			t.Errorf("%s by_priority = %+v, want %d rows counted", c.name, got.ByPriority, len(c.want))
		}
	}

	// The All-teams facets are the All-teams list's: authorized against
	// {org_id} the same way.
	assertFault(t, r.tokensJSON(http.MethodPost, orgFacets, map[string]any{}, r.infraAdminSid, ""), http.StatusForbidden, httpx.ReasonForbidden)
	other, _ := r.seedOrg(r.seedUser(), "facets-elsewhere")
	assertFault(t, r.tokensJSON(http.MethodPost, "/api/orgs/"+other.String()+"/notifications/facets", map[string]any{}, r.ownerSid, ""),
		http.StatusNotFound, httpx.ReasonNotFound)
}

// TestNotifications_Postgres_OrgListGate: the All-teams list authorizes
// against {org_id}: a member who is not an org admin is refused with 403, and
// an org the caller is not in is 404.
func TestNotifications_Postgres_OrgListGate(t *testing.T) {
	r := newNotificationsRig(t)

	assertFault(t, r.tokensJSON(http.MethodPost, r.orgNotifications, map[string]any{}, r.infraAdminSid, ""), http.StatusForbidden, httpx.ReasonForbidden)
	assertFault(t, r.tokensJSON(http.MethodPost, r.orgNotifications, map[string]any{}, r.plainSid, ""), http.StatusForbidden, httpx.ReasonForbidden)

	other, _ := r.seedOrg(r.seedUser(), "someone-elses-org")
	assertFault(t, r.tokensJSON(http.MethodPost, "/api/orgs/"+other.String()+"/notifications/list", map[string]any{}, r.ownerSid, ""),
		http.StatusNotFound, httpx.ReasonNotFound)
}

// TestNotifications_Postgres_Token: the /api/me routes answer for the org the
// token carries, with no session cursor involved.
func TestNotifications_Postgres_Token(t *testing.T) {
	r := newNotificationsRig(t)
	tok := r.createToken(r.infraAdminSid, map[string]any{"name": "inbox", "org_id": r.orgID.String()})
	if _, err := r.h.AdminDB.Exec(`UPDATE sessions SET active_org_id = NULL`); err != nil {
		t.Fatalf("clear active org: %v", err)
	}

	badge := r.list("", tok.Token, meNotifications+"/list", map[string]any{
		"status": "open", "dismissed": false, "unread": true, "priority_in": []string{"urgent", "high", "low"}, "page_size": 0,
	})
	if badge.Total() != 1 || len(badge.Items) != 0 {
		t.Errorf("badge count under a token = %d (%d items), want 1 and no items", badge.Total(), len(badge.Items))
	}
	if rec := r.tokensJSON(http.MethodPatch, r.infraTeamRowPath, map[string]any{"read": true}, "", tok.Token); rec.Code != http.StatusOK {
		t.Fatalf("PATCH under a token = %d: %s", rec.Code, rec.Body.String())
	}
	if got := r.pings.take(); len(got) != 1 || got[0].UserID != r.infraAdmin.String() || got[0].OrgID != r.orgID.String() {
		t.Errorf("marks ping = %+v; want one to the acting user", got)
	}
	if got := r.list("", tok.Token, meNotifications+"/list", map[string]any{"unread": true, "page_size": 0}); got.Total() != 0 {
		t.Errorf("unread after the token's mark = %d, want 0", got.Total())
	}
}

// TestNotifications_Postgres_Invoke: the action's permission is checked
// against the caller, so the org admin who may read the team row is refused
// its team-admin action, and the team's admin runs it and resolves the row.
func TestNotifications_Postgres_Invoke(t *testing.T) {
	r := newNotificationsRig(t)
	path := r.infraTeamRowPath + "/actions/untrack"

	assertFault(t, r.tokensJSON(http.MethodPost, path, nil, r.ownerSid, ""), http.StatusForbidden, httpx.ReasonForbidden)
	assertFault(t, r.tokensJSON(http.MethodPost, path, nil, r.plainSid, ""), http.StatusNotFound, httpx.ReasonNotFound)
	if len(r.untracks) != 0 {
		t.Fatalf("refused invokes ran the handler: %+v", r.untracks)
	}

	rec := r.tokensJSON(http.MethodPost, path, nil, r.infraAdminSid, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("team admin invoke = %d: %s", rec.Code, rec.Body.String())
	}
	if got := r.untracks; len(got) != 1 || got[0].UserID != r.infraAdmin.String() || got[0].Recipient.TeamID != r.infra.String() || got[0].Params.TeamID != r.infra.String() {
		t.Errorf("handler calls = %+v", got)
	}
	ans := decodeNotificationAction(t, rec.Body.Bytes())
	if ans.Outcome != "Untracked." || ans.Notification == nil || ans.Notification.ResolvedAt == nil ||
		ans.Notification.Resolution == nil || *ans.Notification.Resolution != "t1 resolved: audience_removed." {
		t.Errorf("invoke answered %+v; want the outcome and the row resolved, saying why", ans)
	}
	// The resolve pings the team row's recipients; the org row waits for the
	// producer's next evaluation.
	if got := r.pings.take(); len(got) != 1 || got[0].UserID != r.infraAdmin.String() {
		t.Errorf("pings after the resolve = %+v; want the team's admin", got)
	}
	if got := r.list(r.ownerSid, "", meNotifications+"/list", map[string]any{"status": "open"}); got.Total() != 1 {
		t.Errorf("org admin's open inbox after the team row resolved = %d, want the org row still open", got.Total())
	}
	assertFault(t, r.tokensJSON(http.MethodPost, path, nil, r.infraAdminSid, ""), http.StatusConflict, httpx.ReasonConflict)
}
