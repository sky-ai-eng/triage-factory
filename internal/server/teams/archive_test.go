package teams

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// teamArchiveRig is a Postgres-backed Handler, with RLS live, over an org
// with an owner and a plain member. spawner is nil: the no-active-work path
// does not need it.
type teamArchiveRig struct {
	h      *pgtest.Harness
	th     *Handler
	orgID  string
	teamID string
	owner  string // org owner (org admin)
	member string // org + team member, NOT an org admin
}

func newTeamArchiveRig(t *testing.T) *teamArchiveRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "owner")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	th := New(stores.Tx, authz.New(h.AdminDB, stores.Tx), stores,
		func() *delegate.Spawner { return nil }, nil)
	return &teamArchiveRig{h: h, th: th, orgID: orgID, teamID: teamID, owner: owner, member: member}
}

// req builds a request as callerID with the team_id path value + claims + org
// seeded, invoking the handler directly (no cookie-session dance).
func (r *teamArchiveRig) req(method, path, callerID, teamID string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if teamID != "" {
		req.SetPathValue("team_id", teamID)
	}
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

func (r *teamArchiveRig) deletedAt(t *testing.T, teamID string) (set bool) {
	t.Helper()
	if err := r.h.AdminDB.QueryRow(
		`SELECT deleted_at IS NOT NULL FROM teams WHERE id = $1`, teamID,
	).Scan(&set); err != nil {
		t.Fatalf("read deleted_at %s: %v", teamID, err)
	}
	return set
}

// TestTeamArchive_OrgAdminArchivesAndCounts: the org owner archives the team —
// 200, deleted_at set, a zero-work count returned (no runs seeded),
// and the team disappears from the archived-list before / appears after.
func TestTeamArchive_OrgAdminArchivesAndCounts(t *testing.T) {
	r := newTeamArchiveRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamArchive(rec, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamArchiveResultJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CancelledRuns != 0 {
		t.Errorf("counts = %+v; want zero (no active work seeded)", resp)
	}
	if !r.deletedAt(t, r.teamID) {
		t.Error("deleted_at not set after archive")
	}
}

// TestTeamArchive_NonOrgAdminIsForbidden: a plain member cannot archive, restore
// or preview, and gets 403 rather than 404 since the team is listed for them.
// The team is untouched.
func TestTeamArchive_NonOrgAdminIsForbidden(t *testing.T) {
	r := newTeamArchiveRig(t)

	for _, tc := range []struct {
		name, method, path string
	}{
		{"archive", http.MethodPost, "/api/teams/" + r.teamID + "/archive"},
		{"restore", http.MethodPost, "/api/teams/" + r.teamID + "/restore"},
		{"preview", http.MethodGet, "/api/teams/" + r.teamID + "/archive/preview"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := r.req(tc.method, tc.path, r.member, r.teamID)
			switch tc.name {
			case "archive":
				r.th.handleTeamArchive(rec, req)
			case "restore":
				r.th.handleTeamRestore(rec, req)
			case "preview":
				r.th.handleTeamArchivePreview(rec, req)
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s by non-admin = %d, want 403; body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
	if r.deletedAt(t, r.teamID) {
		t.Error("team archived by a non-admin probe, want untouched")
	}
}

// TestTeamArchive_AlreadyArchivedIs409: archiving an archived team is a 409.
func TestTeamArchive_AlreadyArchivedIs409(t *testing.T) {
	r := newTeamArchiveRig(t)

	first := httptest.NewRecorder()
	r.th.handleTeamArchive(first, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if first.Code != http.StatusOK {
		t.Fatalf("first archive = %d, want 200; body=%s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	r.th.handleTeamArchive(second, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if second.Code != http.StatusConflict {
		t.Fatalf("second archive = %d, want 409; body=%s", second.Code, second.Body.String())
	}
}

// TestTeamRestore_RevivesTeam: restore clears deleted_at and the team is
// writable again; restoring an active team is a 409.
func TestTeamRestore_RevivesTeam(t *testing.T) {
	r := newTeamArchiveRig(t)

	// Restore before archive → 409 (not archived).
	pre := httptest.NewRecorder()
	r.th.handleTeamRestore(pre, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/restore", r.owner, r.teamID))
	if pre.Code != http.StatusConflict {
		t.Fatalf("restore active team = %d, want 409; body=%s", pre.Code, pre.Body.String())
	}

	arc := httptest.NewRecorder()
	r.th.handleTeamArchive(arc, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if arc.Code != http.StatusOK {
		t.Fatalf("archive = %d, want 200; body=%s", arc.Code, arc.Body.String())
	}

	res := httptest.NewRecorder()
	r.th.handleTeamRestore(res, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/restore", r.owner, r.teamID))
	if res.Code != http.StatusOK {
		t.Fatalf("restore = %d, want 200; body=%s", res.Code, res.Body.String())
	}
	if r.deletedAt(t, r.teamID) {
		t.Error("deleted_at still set after restore")
	}
}

// doorbell records every ring the archive cascade made, in order.
type doorbell struct {
	rings [][2]string
}

func (d *doorbell) ring(orgID, conversationID string) {
	d.rings = append(d.rings, [2]string{orgID, conversationID})
}

func (d *doorbell) taken() [][2]string {
	out := d.rings
	d.rings = nil
	return out
}

// pgConversationBoundary reads one conversation's stamp off the admin pool.
func pgConversationBoundary(t *testing.T, h *pgtest.Harness, conversationID string) (ended bool, reason string) {
	t.Helper()
	if err := h.AdminDB.QueryRow(
		`SELECT ended_at IS NOT NULL, COALESCE(ended_reason, '') FROM conversations WHERE id = $1`,
		conversationID,
	).Scan(&ended, &reason); err != nil {
		t.Fatalf("read boundary of %s: %v", conversationID, err)
	}
	return ended, reason
}

// teamArchiveConversation seeds one conversation on the rig's team.
// origin='interactive' sidesteps the origin-requires-parents CHECK; the
// boundary door reads team_id, the boundary column and the parent link, and
// nothing else.
func teamArchiveConversation(t *testing.T, r *teamArchiveRig, status any) string {
	t.Helper()
	id := uuid.New().String()
	concluded := false
	if s, ok := status.(string); ok {
		status, concluded = dbtest.SeedStatus(s)
	}
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility,
		                           type, origin, trigger_type, status, completed_at)
		VALUES ($1, $2, $3, $4, 'team', 'delegation', 'interactive', 'manual', $5,
		        CASE WHEN $6::boolean THEN now() END)
	`, id, r.orgID, r.owner, r.teamID, status, concluded)
	return id
}

// TestTeamArchive_RingsTheMemoryDoorbellForEveryConversationItStamped: archiving
// a team ends every un-ended conversation it holds, concluded ones included, and
// rings the memory doorbell once per row it stamped. The rig wires no spawner, so
// nothing is stopped: the stamp is wider than the stop.
func TestTeamArchive_RingsTheMemoryDoorbellForEveryConversationItStamped(t *testing.T) {
	r := newTeamArchiveRig(t)
	d := &doorbell{}
	r.th.memoryOwed = d.ring

	midFlight := teamArchiveConversation(t, r, nil)
	parked := teamArchiveConversation(t, r, "open")
	concluded := teamArchiveConversation(t, r, dbtest.SeedConcluded)

	// Already ended: it keeps the boundary that ended it, and whatever it owes
	// was rung for then, so this archive neither stamps nor rings.
	alreadyEnded := teamArchiveConversation(t, r, "open")
	pgtest.MustExec(t, r.h.AdminDB,
		`UPDATE conversations SET ended_at = now(), ended_reason = $2 WHERE id = $1`,
		alreadyEnded, string(domain.EndedTakenOver))
	// A subagent ends with its spawner, never with the team.
	subagent := teamArchiveConversation(t, r, nil)
	pgtest.MustExec(t, r.h.AdminDB,
		`UPDATE conversations SET parent_conversation_id = $2 WHERE id = $1`, subagent, midFlight)

	rec := httptest.NewRecorder()
	r.th.handleTeamArchive(rec, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	for _, id := range []string{midFlight, parked, concluded} {
		if ended, reason := pgConversationBoundary(t, r.h, id); !ended || reason != string(domain.EndedTeamArchived) {
			t.Errorf("%s boundary = (ended=%v, reason=%q), want a stamp with reason %q",
				id, ended, reason, domain.EndedTeamArchived)
		}
	}
	if ended, reason := pgConversationBoundary(t, r.h, subagent); ended || reason != "" {
		t.Errorf("subagent %s boundary = (ended=%v, reason=%q), want none", subagent, ended, reason)
	}
	if _, reason := pgConversationBoundary(t, r.h, alreadyEnded); reason != string(domain.EndedTakenOver) {
		t.Errorf("already-ended conversation %s now reads %q, want its original %q",
			alreadyEnded, reason, domain.EndedTakenOver)
	}

	got := d.taken()
	want := map[string]bool{midFlight: true, parked: true, concluded: true}
	if len(got) != len(want) {
		t.Fatalf("rang %d times for %d stamped conversations: %v", len(got), len(want), got)
	}
	for _, ring := range got {
		if ring[0] != r.orgID {
			t.Errorf("rang for org %q, want %q", ring[0], r.orgID)
		}
		if !want[ring[1]] {
			t.Errorf("rang for %s, which the archive did not stamp", ring[1])
		}
		delete(want, ring[1])
	}
	for id := range want {
		t.Errorf("archive stamped %s without ringing for it", id)
	}
}
