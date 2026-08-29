package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The disclosure half of POST /api/teams/{team_id}/prs/list, which only multi
// mode can state: localMembership answers yes to every gate, so the two-valued
// denial — a team outside the caller's org is 404-invisible, a team inside it
// that they are not on answers an empty list rather than its work — needs a
// real org graph and the app pool's RLS. Skips without Docker.

type teamPRsRig struct {
	h        *pgtest.Harness
	s        *Server
	orgID    string
	teamID   string
	member   string // org member AND team member
	outsider string // org member, deliberately NOT on the team
	otherOrg struct {
		orgID  string
		teamID string
	}
}

func newTeamPRsRig(t *testing.T) *teamPRsRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, founder, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	outsider := pgtest.SeedUser(t, h, "outsider")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, outsider, orgID)

	otherOrgID, _, otherTeamID := pgtest.SeedOrgWithUser(t, h, "elsewhere")

	r := &teamPRsRig{h: h, s: s, orgID: orgID, teamID: teamID, member: founder, outsider: outsider}
	r.otherOrg.orgID, r.otherOrg.teamID = otherOrgID, otherTeamID
	return r
}

// req builds the list POST as callerID against teamID, with the claims and
// active org withSession/withOrg would normally supply. The handler is invoked
// directly so the test exercises authz + RLS without the cookie dance.
func (r *teamPRsRig) req(callerID, teamID string, body any) *http.Request {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/teams/"+teamID+"/prs/list", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("team_id", teamID)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

// seedTrackedPR registers owner/repo for the org, attaches it to the team, and
// drops a pull request in it authored by login.
func (r *teamPRsRig) seedTrackedPR(t *testing.T, orgID, teamID, login string, number int) {
	t.Helper()
	owner, repo := "acme", fmt.Sprintf("widget-%s", teamID[:8])
	snap := domain.PRSnapshot{
		Number: number, Repo: owner + "/" + repo, Author: login, State: "OPEN", Title: "A pull request",
	}
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	now := time.Now().UTC()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, last_polled_at)
		VALUES ($1, $2, 'github', $3, 'pr', $4, '', $5::jsonb, $6, $6)
	`, uuid.New().String(), orgID, fmt.Sprintf("%s/%s#%d", owner, repo, number), snap.Title, string(blob), now)
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO repositories (org_id, source, owner, repo) VALUES ($1, 'github', $2, $3)
		ON CONFLICT DO NOTHING
	`, orgID, owner, repo)
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO team_github_repos (team_id, repository_id, org_id)
		VALUES ($1, (SELECT id FROM repositories WHERE org_id = $2 AND owner = $3 AND repo = $4), $2)
		ON CONFLICT (team_id, repository_id) DO NOTHING
	`, teamID, orgID, owner, repo)
}

// bindIdentity binds login for userID on the org's effective GitHub host — the
// key the member leg joins on.
func (r *teamPRsRig) bindIdentity(t *testing.T, userID, login string) {
	t.Helper()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO user_github_identities (user_id, github_base_url, login, source)
		VALUES ($1, 'https://github.com', $2, 'connect_oauth')
	`, userID, login)
}

// TestTeamPRList_MemberReadsTheTeamsPullRequests is the positive control the
// two denials below are measured against: with the same fixture, a member gets
// the row.
func TestTeamPRList_MemberReadsTheTeamsPullRequests(t *testing.T) {
	r := newTeamPRsRig(t)
	r.bindIdentity(t, r.member, "octocat")
	r.seedTrackedPR(t, r.orgID, r.teamID, "octocat", 1)

	rec := httptest.NewRecorder()
	r.s.handleTeamPRList(rec, r.req(r.member, r.teamID, map[string]any{}))
	out := decodeList[domain.PRSummaryRow](t, rec)
	if len(out.Items) != 1 || out.Total() != 1 {
		t.Fatalf("member got %d rows / total %d, want 1/1; body=%s", len(out.Items), out.Total(), rec.Body.String())
	}
}

// TestTeamPRList_NonMemberSeesAnEmptyList pins the in-org denial. The team is
// org-visible, so its existence is not a secret and the route answers 200 —
// but the population is gated by the tracked-set semi-join, which
// team_github_repos RLS bounds to members, so a non-member reads nothing. This
// is the /activity audience line: a read the store scopes, not a gate the
// handler refuses.
func TestTeamPRList_NonMemberSeesAnEmptyList(t *testing.T) {
	r := newTeamPRsRig(t)
	r.bindIdentity(t, r.member, "octocat")
	r.seedTrackedPR(t, r.orgID, r.teamID, "octocat", 1)

	rec := httptest.NewRecorder()
	r.s.handleTeamPRList(rec, r.req(r.outsider, r.teamID, map[string]any{}))
	out := decodeList[domain.PRSummaryRow](t, rec)
	if len(out.Items) != 0 || out.Total() != 0 {
		t.Fatalf("a non-member read %d rows / total %d of the team's work; body=%s",
			len(out.Items), out.Total(), rec.Body.String())
	}
}

// TestTeamPRList_TeamInAnotherOrgIs404 pins the cross-org denial: a team
// outside the caller's active org must not even be confirmed to exist.
func TestTeamPRList_TeamInAnotherOrgIs404(t *testing.T) {
	r := newTeamPRsRig(t)
	r.bindIdentity(t, r.member, "octocat")
	r.seedTrackedPR(t, r.otherOrg.orgID, r.otherOrg.teamID, "octocat", 1)

	rec := httptest.NewRecorder()
	r.s.handleTeamPRList(rec, r.req(r.member, r.otherOrg.teamID, map[string]any{}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a team in another org; body=%s", rec.Code, rec.Body.String())
	}
}
