package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// TestMemoryGenerationFailing_Postgres_RetryNow: a no_model failure sends the org
// admin an urgent, undismissible row with the settings link and "Retry now",
// sends a member nothing, and Retry now rings the org-wide doorbell, leaving
// the row open.
func TestMemoryGenerationFailing_Postgres_RetryNow(t *testing.T) {
	rig := newAuthRig(t)
	owner := rig.seedUser()
	org, _ := rig.seedOrg(owner, "memory-failing-"+uuid.NewString()[:8])
	member := rig.seedUser()
	pgtest.MustExec(t, rig.h.AdminDB, `INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, member, org)
	ownerSid, memberSid := rig.signIn(owner), rig.signIn(member)
	rig.setActiveOrg(t, ownerSid, org)
	rig.setActiveOrg(t, memberSid, org)

	var rung []string
	rig.srv.SetOnMemoryOwed(func(orgID, conversationID string) { rung = append(rung, orgID+"/"+conversationID) })
	installNotify(t, rig.srv)
	kinds.MemoryGenerationFailing.Sync(context.Background(), org.String(), []kinds.MemoryFailure{{
		ErrorKind: domain.MemoryAttemptErrNoModel, TasksWaiting: 2, ErrorMessage: "the background jobs model setting is empty",
	}})

	openInbox := func(sid string) httpxtest.ListEnvelope[notificationItemWire] {
		t.Helper()
		return httpxtest.DecodeList[notificationItemWire](t, rig.tokensJSON(http.MethodPost, meNotifications+"/list", map[string]any{"status": "open"}, sid, ""))
	}
	if got := openInbox(memberSid); got.Total() != 0 {
		t.Errorf("org member's inbox = %+v, want nothing: the kind is for org admins", got.Items)
	}
	inbox := openInbox(ownerSid)
	if inbox.Total() != 1 {
		t.Fatalf("org admin's inbox = %+v, want the one row", inbox.Items)
	}
	row := inbox.Items[0]
	if row.Kind != kinds.MemoryGenerationFailing.Name() || row.Priority != "urgent" || row.Dismissible || row.Audience.Scope != "org" {
		t.Errorf("row = %+v, want an urgent, undismissible org row", row)
	}
	if acts := row.Actions; len(acts) != 2 || acts[0].ID != "org_settings" || acts[1].ID != "retry" ||
		acts[1].Allowed == nil || !*acts[1].Allowed {
		t.Fatalf("actions = %+v, want the settings link, then Retry now allowed", acts)
	}

	if rec := rig.tokensJSON(http.MethodPost, meNotifications+"/"+row.ID+"/actions/retry", nil, memberSid, ""); rec.Code != http.StatusNotFound {
		t.Errorf("a member's retry = %d, want 404 for a row they can't read: %s", rec.Code, rec.Body.String())
	}
	if len(rung) != 0 {
		t.Fatalf("the refused retry rang the doorbell: %v", rung)
	}

	rec := rig.tokensJSON(http.MethodPost, meNotifications+"/"+row.ID+"/actions/retry", nil, ownerSid, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d: %s", rec.Code, rec.Body.String())
	}
	if it := decodeNotificationItem(t, rec.Body.Bytes()); it.ResolvedAt != nil {
		t.Errorf("invoked row = %+v, want it still open until an attempt succeeds", it)
	}
	if want := org.String() + "/"; len(rung) != 1 || rung[0] != want {
		t.Errorf("doorbell = %v, want the org-wide ring %q", rung, want)
	}
}
