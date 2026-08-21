package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func orgSourcePath(kind string) string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/sources/" + kind
}

// decodeSource reads the single-source resource off a response.
func decodeSource(t *testing.T, raw []byte) eventsource.Source {
	t.Helper()
	var got eventsource.Source
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode source: %v", err)
	}
	return got
}

// TestOrgSource_PatchTogglesAndReadReflectsIt walks the pair end to end: the
// PATCH answers with the derived state rather than echoing the boolean it was
// handed, the GET beside it agrees, and the list the whole authoring surface
// reads from agrees too.
func TestOrgSource_PatchTogglesAndReadReflectsIt(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)

	if got := readOrgSources(t, s)[eventsource.KindJira]; got != string(eventsource.StateAvailable) {
		t.Fatalf("precondition: jira = %q, want available", got)
	}

	rec := doJSON(t, s, http.MethodPatch, orgSourcePath("jira"), map[string]any{"disabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeSource(t, rec.Body.Bytes()); got.State != eventsource.StateDisabled || got.Kind != "jira" {
		t.Errorf("PATCH answered %+v, want jira/disabled", got)
	}

	read := doJSON(t, s, http.MethodGet, orgSourcePath("jira"), nil)
	if read.Code != http.StatusOK {
		t.Fatalf("GET: %d: %s", read.Code, read.Body.String())
	}
	if got := decodeSource(t, read.Body.Bytes()); got.State != eventsource.StateDisabled {
		t.Errorf("GET answered %+v, want disabled", got)
	}
	list := readOrgSources(t, s)
	if list[eventsource.KindJira] != string(eventsource.StateDisabled) {
		t.Errorf("list says jira = %q, want disabled — the element and the collection are one derivation", list[eventsource.KindJira])
	}
	if list[eventsource.KindGitHub] != string(eventsource.StateAvailable) {
		t.Errorf("pausing jira moved github to %q, want available", list[eventsource.KindGitHub])
	}

	// And back. The credential was never touched, so the source returns to
	// available rather than to unconfigured — which is the whole point of the
	// route existing beside the credential unbind.
	back := doJSON(t, s, http.MethodPatch, orgSourcePath("jira"), map[string]any{"disabled": false})
	if back.Code != http.StatusOK {
		t.Fatalf("PATCH back: %d: %s", back.Code, back.Body.String())
	}
	if got := decodeSource(t, back.Body.Bytes()); got.State != eventsource.StateAvailable {
		t.Errorf("re-enabled jira = %q, want available — the credential stayed bound", got.State)
	}
}

// TestOrgSource_PatchClearsSnapshotsOnDisableOnly pins the write's other half:
// disabling clears the tracker snapshots that make a re-enable safe, and
// enabling does not (there is nothing to protect against, and clearing would
// throw away a cycle of state for nothing).
func TestOrgSource_PatchClearsSnapshotsOnDisableOnly(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	// The server exposes no store bundle; a handler test that has to look at
	// the rows a request wrote opens its own against the same connection.
	st := sqlitestore.New(s.db)

	ent, _, err := st.Entities.FindOrCreate(ctx, org, "jira", "SKY-pause-1", "issue", "Ticket", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := st.Entities.UpdateSnapshot(ctx, org, ent.ID, `{"key":"SKY-pause-1"}`); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	if rec := doJSON(t, s, http.MethodPatch, orgSourcePath("jira"), map[string]any{"disabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH disable: %d: %s", rec.Code, rec.Body.String())
	}
	got, err := st.Entities.GetBySource(ctx, org, "jira", "SKY-pause-1")
	if err != nil || got == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", got, err)
	}
	if got.SnapshotJSON != "" {
		t.Errorf("snapshot = %q after disable, want cleared", got.SnapshotJSON)
	}

	if _, err := st.Entities.UpdateSnapshot(ctx, org, ent.ID, `{"key":"SKY-pause-1"}`); err != nil {
		t.Fatalf("re-seed snapshot: %v", err)
	}
	if rec := doJSON(t, s, http.MethodPatch, orgSourcePath("jira"), map[string]any{"disabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH enable: %d: %s", rec.Code, rec.Body.String())
	}
	got, err = st.Entities.GetBySource(ctx, org, "jira", "SKY-pause-1")
	if err != nil || got == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", got, err)
	}
	if got.SnapshotJSON == "" {
		t.Error("enabling cleared the snapshot; only disabling has a burst to prevent")
	}
}

// TestOrgSource_PatchAudits pins that the pause reads as what it is in the
// access log — not as a credential removal, which is the wrong story for "we
// paused Jira for a sprint" and the reason this route exists.
func TestOrgSource_PatchAudits(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)

	if rec := doJSON(t, s, http.MethodPatch, orgSourcePath("jira"), map[string]any{"disabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d: %s", rec.Code, rec.Body.String())
	}
	rows, _, err := sqlitestore.New(s.db).AccessChangeLog.ListByOrg(context.Background(), runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{Limit: 10})
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(rows) == 0 || rows[0].Action != domain.AccessActionEventSourceDisabled {
		t.Fatalf("newest audit row = %+v, want %q", rows, domain.AccessActionEventSourceDisabled)
	}
	if rows[0].DetailJSON != domain.AccessDetailEventSource("jira") {
		t.Errorf("detail = %q, want %q", rows[0].DetailJSON, domain.AccessDetailEventSource("jira"))
	}
}

// TestOrgSource_RejectsBadRequests covers the addresses and bodies the route
// must refuse: a kind this deployment does not carry is not a resource, and a
// PATCH that names no field wrote nothing so it must not answer "updated".
func TestOrgSource_RejectsBadRequests(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)

	cases := []struct {
		name   string
		method string
		kind   string
		body   any
		want   int
	}{
		{"unknown kind is not an address", http.MethodPatch, "gerrit", map[string]any{"disabled": true}, http.StatusNotFound},
		{"unknown kind is unreadable too", http.MethodGet, "gerrit", nil, http.StatusNotFound},
		{"system is not a source", http.MethodPatch, "system", map[string]any{"disabled": true}, http.StatusNotFound},
		{"no field named", http.MethodPatch, "jira", map[string]any{}, http.StatusBadRequest},
		{"disabled is not a boolean", http.MethodPatch, "jira", map[string]any{"disabled": "yes"}, http.StatusBadRequest},
		{"unknown field", http.MethodPatch, "jira", map[string]any{"disabled": true, "reason": "sprint"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, s, tc.method, orgSourcePath(tc.kind), tc.body)
			if rec.Code != tc.want {
				t.Errorf("%s %s = %d, want %d: %s", tc.method, tc.kind, rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// A refused write stored nothing — the vocabulary check is what keeps an
	// unresolvable row out of the table, not a later reader's tolerance of it.
	if row, err := sqlitestore.New(s.db).OrgEventSources.Get(context.Background(), runmode.LocalDefaultOrgID, "gerrit"); err != nil || row != nil {
		t.Errorf("Get(gerrit) = %+v, err=%v; want no row", row, err)
	}
}

// TestOrgSource_WriteIsOrgAdminReadIsMember pins D3's split gate, and the
// disclosure rule with it: a plain member reads (they have to be able to render
// why their own handler is inert) but cannot write, and a caller outside the org
// is told nothing at all.
func TestOrgSource_WriteIsOrgAdminReadIsMember(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	founder := r.seedUser()
	orgA, teamA := r.seedOrg(founder, "acme")
	member := r.seedUser()
	pgtest.AddOrgMember(t, r.h, member.String(), orgA.String(), teamA.String(), "member", "member")
	outsider := r.seedUser()
	orgB, _ := r.seedOrg(outsider, "other-co")

	adminResp, _ := r.driveCallback(founder)
	adminSid := r.sidFromResp(adminResp)
	memberResp, _ := r.driveCallback(member)
	memberSid := r.sidFromResp(memberResp)

	cases := []struct {
		name      string
		sid       string
		org       string
		wantRead  int
		wantPatch int
	}{
		{"org admin", adminSid, orgA.String(), http.StatusOK, http.StatusOK},
		// 403, not 404: this org IS in the member's own list, so a 404 here
		// would read as a bug rather than as a permission boundary.
		{"plain member", memberSid, orgA.String(), http.StatusOK, http.StatusForbidden},
		// 404 on both, and for the same reason: disclose nothing about an org
		// the caller cannot see.
		{"outside the org", memberSid, orgB.String(), http.StatusNotFound, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/orgs/" + tc.org + "/sources/jira"
			if got := r.requestWithSid("GET", path, tc.sid).StatusCode; got != tc.wantRead {
				t.Errorf("GET = %d, want %d", got, tc.wantRead)
			}
			got := r.postJSONWithSid("PATCH", path, tc.sid, map[string]any{"disabled": true}).StatusCode
			if got != tc.wantPatch {
				t.Errorf("PATCH = %d, want %d", got, tc.wantPatch)
			}
		})
	}
}

// TestEventHandlerCreate_RefusesDisabledSource pins the create-time gate's new
// arm. It is the same 422 INVALID_FIELD the unconfigured case answers, because
// both are references the caller can act on — and it carries its own message,
// since "go connect Jira" is useless advice for a source an admin turned off.
func TestEventHandlerCreate_RefusesDisabledSource(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	if rec := doJSON(t, s, http.MethodPatch, orgSourcePath("jira"), map[string]any{"disabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d: %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"name":       "paused rule",
		"event_type": domain.EventJiraIssueAssigned,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Errors []struct {
			Reason, Message, Field string
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode errors: %v", err)
	}
	if len(body.Errors) != 1 || body.Errors[0].Field != "event_type" {
		t.Fatalf("errors = %+v, want one fault on event_type", body.Errors)
	}
	if body.Errors[0].Message != "jira is turned off for this organization by an org admin" {
		t.Errorf("message = %q, want the disabled wording rather than the unconfigured one", body.Errors[0].Message)
	}
}
