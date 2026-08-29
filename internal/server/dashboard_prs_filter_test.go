package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The two things POST /api/dashboard/prs/list grew: a states filter, and a
// second identity leg — pull requests a run the viewer commissioned opened
// under the bot's login.

// seedCommissionedPR inserts a github pull-request entity carrying snap, with
// the commissioner and entity state the read consults beside snapshot_json.
func seedCommissionedPR(t *testing.T, s *Server, snap domain.PRSnapshot, commissionedBy, entityState string) {
	t.Helper()
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if entityState == "" {
		entityState = "active"
	}
	var commissioner any
	if commissionedBy != "" {
		commissioner = commissionedBy
	}
	now := time.Now().UTC()
	sourceID := fmt.Sprintf("%s#%d", snap.Repo, snap.Number)
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, state,
		                      commissioned_by_user_id, created_at, last_polled_at)
		VALUES (?, ?, 'github', ?, 'pr', ?, ?, ?, ?, ?, ?, ?)
	`, "ent-"+sourceID, runmode.LocalDefaultOrgID, sourceID, snap.Title, snap.URL, string(blob),
		entityState, commissioner, now, now); err != nil {
		t.Fatalf("seed entity %s: %v", sourceID, err)
	}
}

// TestDashboardPRs_IncludesWhatICommissioned is the second leg's reason for
// existing: a pull request TF opened is authored by a bot that maps to no TF
// user, so the author predicate alone never returns work the viewer asked for.
// Someone else's commission stays theirs.
func TestDashboardPRs_IncludesWhatICommissioned(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	const someoneElse = "44444444-4444-4444-4444-444444444444"
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, display_name) VALUES (?, 'Someone Else')`, someoneElse); err != nil {
		t.Fatalf("seed other user: %v", err)
	}

	seedCommissionedPR(t, s, domain.PRSnapshot{
		Number: 1, Repo: "acme/widget", Author: "octocat", State: "OPEN", Title: "Mine by hand",
	}, "", "")
	seedCommissionedPR(t, s, domain.PRSnapshot{
		Number: 2, Repo: "acme/widget", Author: "tf-bot", State: "OPEN", Title: "Mine by delegation",
	}, runmode.LocalDefaultUserID, "")
	seedCommissionedPR(t, s, domain.PRSnapshot{
		Number: 3, Repo: "acme/widget", Author: "tf-bot", State: "OPEN", Title: "Somebody else's ask",
	}, someoneElse, "")

	out := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{}))
	if len(out.Items) != 2 || out.Total() != 2 {
		t.Fatalf("got %d rows / total %d, want 2/2; items=%+v", len(out.Items), out.Total(), out.Items)
	}
	got := map[int]bool{}
	for _, p := range out.Items {
		got[p.Number] = true
	}
	if !got[1] || !got[2] {
		t.Errorf("returned %v, want #1 (authored) and #2 (commissioned)", got)
	}
	if got[3] {
		t.Error("returned #3 — another person's commission is not mine")
	}
}

// TestDashboardPRs_UnboundIdentityStillSeesCommissions is why the handler no
// longer short-circuits on a missing GitHub login: a person who has never
// bound one can still have asked for work, and hiding it behind an unrelated
// missing binding answers the wrong question.
func TestDashboardPRs_UnboundIdentityStillSeesCommissions(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	seedCommissionedPR(t, s, domain.PRSnapshot{
		Number: 5, Repo: "acme/widget", Author: "tf-bot", State: "OPEN", Title: "Mine by delegation",
	}, runmode.LocalDefaultUserID, "")
	seedCommissionedPR(t, s, domain.PRSnapshot{
		Number: 6, Repo: "acme/widget", Author: "octocat", State: "OPEN", Title: "Nobody's, to me",
	}, "", "")

	out := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{}))
	if len(out.Items) != 1 || out.Total() != 1 || out.Items[0].Number != 5 {
		t.Fatalf("got %+v (total %d), want exactly #5 — the commissioned leg needs no bound login",
			out.Items, out.Total())
	}
}

// TestDashboardPRs_StatesFilterAndCountOnly pins the filter and the figure
// beside it: the same query counted equals the same query paged.
func TestDashboardPRs_StatesFilterAndCountOnly(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	seedCommissionedPR(t, s, domain.PRSnapshot{Number: 10, Repo: "acme/widget", Author: "octocat", State: "OPEN"}, "", "")
	seedCommissionedPR(t, s, domain.PRSnapshot{
		Number: 11, Repo: "acme/widget", Author: "octocat", State: "MERGED", Merged: true,
	}, "", "closed")
	seedCommissionedPR(t, s, domain.PRSnapshot{
		Number: 12, Repo: "acme/widget", Author: "octocat", State: "CLOSED",
	}, "", "closed")

	for _, tc := range []struct {
		states []string
		want   int
	}{
		{[]string{"open"}, 1},
		{[]string{"merged"}, 1},
		{[]string{"closed"}, 1},
		{[]string{"merged", "closed"}, 2},
	} {
		paged := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{
			"states": tc.states,
		}))
		if len(paged.Items) != tc.want || paged.Total() != tc.want {
			t.Errorf("states=%v: %d rows / total %d, want %d", tc.states, len(paged.Items), paged.Total(), tc.want)
		}
		counted := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{
			"states": tc.states, "page_size": 0,
		}))
		if len(counted.Items) != 0 {
			t.Errorf("states=%v count-only returned %d rows, want none", tc.states, len(counted.Items))
		}
		if counted.Total() != paged.Total() {
			t.Errorf("states=%v: count-only total %d, paged total %d — the figure and the list disagree",
				tc.states, counted.Total(), paged.Total())
		}
	}
}

// TestDashboardPRs_MalformedStateIs400 pins the strict-filter rule on this arm
// too, from the shared validator both routes call.
func TestDashboardPRs_MalformedStateIs400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{"states": []string{"abandoned"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "states")
}

// TestDashboardPRs_PageTokenIsBoundToItsFilters pins that a token minted for
// one filter set is refused for another — the offset would otherwise address a
// different result set.
func TestDashboardPRs_PageTokenIsBoundToItsFilters(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	for i := range 3 {
		seedCommissionedPR(t, s, domain.PRSnapshot{
			Number: 20 + i, Repo: "acme/widget", Author: "octocat", State: "OPEN",
		}, "", "")
	}

	first := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{
		"page_size": 2,
	}))
	if first.NextPageToken == "" {
		t.Fatal("no next_page_token on a page that leaves rows behind")
	}
	rec := doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{
		"page_size": 2, "page_token": first.NextPageToken, "states": []string{"open"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a token minted under other filters; body=%s", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "page_token")
}
