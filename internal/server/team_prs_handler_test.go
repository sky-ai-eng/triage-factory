package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// POST /api/teams/{team_id}/prs/list — the team arm of the pull-request list,
// and the Overview's OPEN PRS figure as its count-only read.
//
// Local mode is where the route's body contract is exercised: N=1 has one team
// and one member, so the population's two legs, the states filter, and the
// token's filter binding all read here. The disclosure rule is multi's —
// localMembership answers "yes" to every gate — and lives in the Postgres
// handler test below.

const teamPRsPath = "/api/teams/default/prs/list"

// seedTeamPR inserts a github pull-request entity carrying snap, with the two
// columns the read consults beside snapshot_json.
func seedTeamPR(t *testing.T, s *Server, snap domain.PRSnapshot, owningTeam, entityState string) {
	t.Helper()
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if entityState == "" {
		entityState = "active"
	}
	var owning any
	if owningTeam != "" {
		owning = owningTeam
	}
	now := time.Now().UTC()
	sourceID := fmt.Sprintf("%s#%d", snap.Repo, snap.Number)
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, state,
		                      owning_team_id, created_at, last_polled_at)
		VALUES (?, ?, 'github', ?, 'pr', ?, ?, ?, ?, ?, ?, ?)
	`, "ent-"+sourceID, runmode.LocalDefaultOrgID, sourceID, snap.Title, snap.URL, string(blob),
		entityState, owning, now, now); err != nil {
		t.Fatalf("seed entity %s: %v", sourceID, err)
	}
}

// TestTeamPRList_MemberSeesBothLegs is the route's reason for existing: a
// member's own pull requests and the ones TF opened for the team, in one list,
// and nothing that is neither.
func TestTeamPRList_MemberSeesBothLegs(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}

	seedTeamPR(t, s, domain.PRSnapshot{
		Number: 1, Repo: "acme/widget", Author: "octocat", State: "OPEN", Title: "Mine",
	}, "", "")
	seedTeamPR(t, s, domain.PRSnapshot{
		Number: 2, Repo: "acme/widget", Author: "tf-bot", State: "OPEN", Title: "Opened for us",
	}, runmode.LocalDefaultTeamID, "")
	seedTeamPR(t, s, domain.PRSnapshot{
		Number: 3, Repo: "acme/widget", Author: "stranger", State: "OPEN", Title: "Not ours",
	}, "", "")

	out := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", teamPRsPath, map[string]any{}))
	if len(out.Items) != 2 || out.Total() != 2 {
		t.Fatalf("got %d rows / total %d, want 2/2; body items = %+v", len(out.Items), out.Total(), out.Items)
	}
	got := map[int]bool{}
	for _, p := range out.Items {
		got[p.Number] = true
	}
	if !got[1] || !got[2] {
		t.Errorf("returned %v, want #1 (member-authored) and #2 (team-owned)", got)
	}
	if got[3] {
		t.Error("returned #3 — a stranger's pull request is not the team's")
	}
}

// TestTeamPRList_OpenCountIsTheOverviewFigure pins the shape the Overview
// actually calls: states=[open] with page_size 0, reading total_count. The
// figure has to be the number the list itself would report, or the page and
// the tile disagree in front of the user.
func TestTeamPRList_OpenCountIsTheOverviewFigure(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	seedTeamPR(t, s, domain.PRSnapshot{Number: 10, Repo: "acme/widget", Author: "octocat", State: "OPEN"}, "", "")
	seedTeamPR(t, s, domain.PRSnapshot{Number: 11, Repo: "acme/widget", Author: "octocat", State: "OPEN"}, "", "")
	seedTeamPR(t, s, domain.PRSnapshot{
		Number: 12, Repo: "acme/widget", Author: "octocat", State: "MERGED", Merged: true,
	}, "", "closed")

	figure := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", teamPRsPath, map[string]any{
		"states": []string{"open"}, "page_size": 0,
	}))
	if len(figure.Items) != 0 {
		t.Errorf("count-only read returned %d rows, want none", len(figure.Items))
	}
	if figure.Total() != 2 {
		t.Fatalf("OPEN PRS = %d, want 2", figure.Total())
	}

	paged := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", teamPRsPath, map[string]any{
		"states": []string{"open"},
	}))
	if paged.Total() != figure.Total() || len(paged.Items) != 2 {
		t.Errorf("paged read = %d rows / total %d, disagrees with the figure %d",
			len(paged.Items), paged.Total(), figure.Total())
	}
}

// TestTeamPRList_MalformedStateIs400 pins the strict-filter rule: an unknown
// state is a named 400, never an empty page and never a silent widening back
// to every state.
func TestTeamPRList_MalformedStateIs400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", teamPRsPath, map[string]any{"states": []string{"abandoned"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "states")
}

// TestTeamPRList_PageTokenIsBoundToItsFilters pins the token contract: page 2
// of one query cannot be requested with another query's token, because the
// offset would address a different result set.
func TestTeamPRList_PageTokenIsBoundToItsFilters(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	for i := range 3 {
		seedTeamPR(t, s, domain.PRSnapshot{
			Number: 20 + i, Repo: "acme/widget", Author: "octocat", State: "OPEN",
		}, "", "")
	}

	first := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", teamPRsPath, map[string]any{
		"page_size": 2,
	}))
	if first.NextPageToken == "" {
		t.Fatal("no next_page_token on a page that leaves rows behind")
	}
	// The same token against a narrowed query: refused, not silently applied.
	rec := doJSON(t, s, "POST", teamPRsPath, map[string]any{
		"page_size": 2, "page_token": first.NextPageToken, "states": []string{"open"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a token minted under other filters; body=%s", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "page_token")

	// And the token IS good for the query that minted it.
	second := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", teamPRsPath, map[string]any{
		"page_size": 2, "page_token": first.NextPageToken,
	}))
	if len(second.Items) != 1 || second.Total() != 3 {
		t.Fatalf("page 2 = %d rows / total %d, want 1/3", len(second.Items), second.Total())
	}
}

// TestTeamPRList_UnknownTeamIs404 covers the addressable half of the
// disclosure rule that local mode can state: a team_id naming nothing is a
// 404, never a 400 that confirms the shape of ids worth guessing.
func TestTeamPRList_UnknownTeamIs404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", "/api/teams/not-a-team/prs/list", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamPRList_StoreReadsThroughTheClaimsTx is a wiring pin rather than a
// behaviour one: the route must read the team arm through TxStores, where the
// tracked-set semi-join runs under the caller's claims. A nil binding is the
// failure mode a refactor introduces silently.
func TestTeamPRList_StoreReadsThroughTheClaimsTx(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	if err := s.tx.WithTx(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID,
		func(tx db.TxStores) error {
			if tx.TeamPRs == nil {
				t.Error("TxStores.TeamPRs is not wired")
			}
			return nil
		}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}
