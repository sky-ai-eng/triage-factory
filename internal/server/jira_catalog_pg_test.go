package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// seedJiraCredential makes baseURL the org's Jira site and stores a Data
// Center Jira service credential for it, written as userID the way the
// credential route writes them.
func (r *authRig) seedJiraCredential(orgID, userID uuid.UUID, baseURL string) {
	r.t.Helper()
	ctx := context.Background()
	if err := r.srv.tx.WithTx(ctx, orgID.String(), userID.String(), func(tx db.TxStores) error {
		if _, err := tx.Orgs.SetSourceBaseURL(ctx, orgID.String(), eventsource.KindJira, baseURL); err != nil {
			return err
		}
		return integrations.Save(ctx, tx.Secrets, orgID.String(),
			auth.Credentials{JiraPAT: "org-pat"})
	}); err != nil {
		r.t.Fatalf("seed jira credential: %v", err)
	}
}

// decodeJiraPage reads a list response, failing unless it is a 200.
func decodeJiraPage[T any](t *testing.T, resp *http.Response) httpxtest.ListEnvelope[T] {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
	var out httpxtest.ListEnvelope[T]
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v; body=%s", err, raw)
	}
	return out
}

// TestJiraCatalog_Postgres_AddressedAndGatedByOrg: the catalog routes answer for
// the org in the path, and a token from one org does not page another's. A member
// removed while their session still names the org gets 404s and Jira is not asked.
func TestJiraCatalog_Postgres_AddressedAndGatedByOrg(t *testing.T) {
	rig := newAuthRig(t)
	alice := rig.seedUser()
	org, team := rig.seedOrg(alice, "jira-a-"+uuid.NewString()[:8])
	other, _ := rig.seedOrg(alice, "jira-b-"+uuid.NewString()[:8])
	fake := newJiraCatalogFake(t, "SKY", "OPS")
	otherFake := newJiraCatalogFake(t, "DESK", "SKY")
	rig.seedJiraCredential(org, alice, fake.URL)
	rig.seedJiraCredential(other, alice, otherFake.URL)

	sid := rig.signIn(alice)
	rig.setActiveOrg(t, sid, org)
	base := "/api/orgs/" + org.String() + "/jira/projects"
	otherBase := "/api/orgs/" + other.String() + "/jira/projects"

	got := decodeJiraPage[jiraProjectJSON](t, rig.postJSONWithSid(http.MethodPost, otherBase+"/list", sid, map[string]any{}))
	if keys := projectKeysOf(got.Items); keys != "DESK,SKY" {
		t.Fatalf("the second org's projects = %s, want DESK,SKY from its own Jira", keys)
	}

	for name, paths := range map[string][2]string{
		"projects list": {base + "/list", otherBase + "/list"},
		"statuses list": {base + "/" + fakeJiraProjectID("SKY") + "/statuses/list", otherBase + "/" + fakeJiraProjectID("SKY") + "/statuses/list"},
	} {
		first := decodeJiraPage[json.RawMessage](t, rig.postJSONWithSid(http.MethodPost, paths[0], sid, map[string]any{"page_size": 1}))
		if first.NextPageToken == "" {
			t.Fatalf("%s: page 1 carried no next_page_token", name)
		}
		resp := rig.postJSONWithSid(http.MethodPost, paths[1], sid,
			map[string]any{"page_size": 1, "page_token": first.NextPageToken})
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: another org's token = %d %s, want 400", name, resp.StatusCode, raw)
		}
	}

	bob := rig.seedUser()
	pgtest.AddOrgMember(t, rig.h, bob.String(), org.String(), team.String(), "member", "member")
	bobSid := rig.signIn(bob)
	rig.setActiveOrg(t, bobSid, org)
	if got := decodeJiraPage[jiraProjectJSON](t, rig.postJSONWithSid(http.MethodPost, base+"/list", bobSid, map[string]any{})); len(got.Items) != 2 {
		t.Fatalf("a member's read = %+v, want both projects", got.Items)
	}

	pgtest.MustExec(t, rig.h.AdminDB, `DELETE FROM memberships WHERE user_id = $1`, bob)
	pgtest.MustExec(t, rig.h.AdminDB, `DELETE FROM org_memberships WHERE user_id = $1`, bob)
	before := fake.Calls()
	for name, resp := range map[string]*http.Response{
		"projects list": rig.postJSONWithSid(http.MethodPost, base+"/list", bobSid, map[string]any{}),
		"project":       rig.requestWithSid(http.MethodGet, base+"/"+fakeJiraProjectID("SKY"), bobSid),
		"statuses list": rig.postJSONWithSid(http.MethodPost, base+"/"+fakeJiraProjectID("SKY")+"/statuses/list", bobSid, map[string]any{}),
		"status":        rig.requestWithSid(http.MethodGet, base+"/"+fakeJiraProjectID("SKY")+"/statuses/"+statusDoneID, bobSid),
	} {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s after removal = %d, want 404", name, resp.StatusCode)
		}
	}
	if got := fake.Calls() - before; got != 0 {
		t.Errorf("a removed member's reads reached Jira %d times", got)
	}
}
