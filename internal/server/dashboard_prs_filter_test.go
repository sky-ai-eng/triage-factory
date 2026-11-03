package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// POST /api/dashboard/prs/list: the viewer's pull requests, their states
// filter and their page token.

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
	repoID := dbseed.GitHubEntityRepository(t, s.db, runmode.LocalDefaultOrgID, "https://github.com", sourceID)
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, state,
		                      commissioned_by_user_id, created_at, last_polled_at, scope, repository_id)
		VALUES (?, ?, 'github', ?, 'pr', ?, ?, ?, ?, ?, ?, ?, 'https://github.com', ?)
	`, "ent-"+sourceID, runmode.LocalDefaultOrgID, sourceID, snap.Title, snap.URL, string(blob),
		entityState, commissioner, now, now, repoID); err != nil {
		t.Fatalf("seed entity %s: %v", sourceID, err)
	}
}

// TestDashboardPRs_IncludesWhatICommissioned: "mine" is what my GitHub login
// authored plus what a run I commissioned opened under the bot's login, which
// maps to no TF user. The commissioned leg needs no bound login, and another
// person's pull requests, authored or commissioned, are never mine.
func TestDashboardPRs_IncludesWhatICommissioned(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

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
	seedCommissionedPR(t, s, domain.PRSnapshot{
		Number: 4, Repo: "acme/widget", Author: "someone-else", State: "OPEN", Title: "Not mine",
	}, "", "")

	mine := func() map[int]domain.PRSummaryRow {
		t.Helper()
		out := httpxtest.DecodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{}))
		if out.Total() != len(out.Items) {
			t.Errorf("total %d, page %d rows", out.Total(), len(out.Items))
		}
		got := map[int]domain.PRSummaryRow{}
		for _, p := range out.Items {
			got[p.Number] = p
		}
		return got
	}

	if got := mine(); len(got) != 1 || got[2].Number != 2 {
		t.Errorf("with no bound login, returned %v, want exactly #2 (commissioned)", got)
	}

	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	got := mine()
	if len(got) != 2 || got[1].Number != 1 || got[2].Number != 2 {
		t.Fatalf("returned %v, want #1 (authored) and #2 (commissioned)", got)
	}
	if got[1].Repo != "acme/widget" || got[1].Author != "octocat" {
		t.Errorf("#1 = %+v, want acme/widget by octocat", got[1])
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
		paged := httpxtest.DecodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{
			"states": tc.states,
		}))
		if len(paged.Items) != tc.want || paged.Total() != tc.want {
			t.Errorf("states=%v: %d rows / total %d, want %d", tc.states, len(paged.Items), paged.Total(), tc.want)
		}
		counted := httpxtest.DecodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{
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

	first := httpxtest.DecodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{
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

// prNumbers reads one PR list page and returns its numbers in served order,
// checking the total agrees with the page.
func prNumbers(t *testing.T, s *Server, path string, body map[string]any) []int {
	t.Helper()
	out := httpxtest.DecodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", path, body))
	got := make([]int, len(out.Items))
	for i, p := range out.Items {
		got[i] = p.Number
	}
	if out.Total() != len(got) {
		t.Errorf("%s %v: total %d, page %d", path, body, out.Total(), len(got))
	}
	return got
}

// TestPRLists_DraftFilterAndUpdatedAtSort pins the board's column reads on
// both routes: each column is states plus draft, newest activity first, and
// the narrowing and the order are the server's.
func TestPRLists_DraftFilterAndUpdatedAtSort(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	for _, p := range []struct {
		n       int
		draft   bool
		updated string
	}{
		{30, false, "2026-10-01T09:00:00Z"},
		{31, true, "2026-10-02T09:00:00Z"},
		{32, false, "2026-10-04T09:00:00Z"},
		{33, true, "2026-10-03T09:00:00Z"},
	} {
		seedCommissionedPR(t, s, domain.PRSnapshot{
			Number: p.n, Repo: "acme/widget", Author: "octocat", State: "OPEN", IsDraft: p.draft, UpdatedAt: p.updated,
		}, "", "")
	}

	for _, tc := range []struct {
		body map[string]any
		want []int
	}{
		{map[string]any{"states": []string{"open"}, "draft": true, "sort_key": "updated_at"}, []int{33, 31}},
		{map[string]any{"states": []string{"open"}, "draft": false, "sort_key": "updated_at"}, []int{32, 30}},
		{map[string]any{"sort_key": "updated_at", "sort_dir": "asc"}, []int{30, 31, 33, 32}},
		{map[string]any{"draft": false, "sort_key": "updated_at", "sort_dir": "desc"}, []int{32, 30}},
	} {
		for _, path := range []string{"/api/dashboard/prs/list", teamPRsPath} {
			if got := prNumbers(t, s, path, tc.body); !slices.Equal(got, tc.want) {
				t.Errorf("%s %v = %v, want %v", path, tc.body, got, tc.want)
			}
		}
	}
}

// TestPRLists_DraftAndSortAreStrict: a draft that is not a boolean, an
// unknown sort key or direction, and a direction with no key are each a 400
// naming the field, on both routes; an order spelled any other way is an
// unknown field.
func TestPRLists_DraftAndSortAreStrict(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for _, tc := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"draft": "yes"}, "draft"},
		{map[string]any{"draft": 1}, "draft"},
		{map[string]any{"sort_key": "title"}, "sort_key"},
		{map[string]any{"sort_key": "updated_at", "sort_dir": "newest"}, "sort_dir"},
		{map[string]any{"sort_dir": "desc"}, "sort_dir"},
		{map[string]any{"order": "updated_at"}, "order"},
	} {
		for _, path := range []string{"/api/dashboard/prs/list", teamPRsPath} {
			rec := doJSON(t, s, "POST", path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s %v: status = %d, want 400; body=%s", path, tc.body, rec.Code, rec.Body.String())
				continue
			}
			assertFieldError(t, rec, tc.field)
		}
	}
}

// TestPRLists_PageTokenIsBoundToDraftAndSort: an offset addresses a position
// in one filtered ordering, so a token minted under one draft value or order
// is refused under another.
func TestPRLists_PageTokenIsBoundToDraftAndSort(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	for i := range 3 {
		seedCommissionedPR(t, s, domain.PRSnapshot{
			Number: 40 + i, Repo: "acme/widget", Author: "octocat", State: "OPEN", IsDraft: true,
		}, "", "")
	}

	for _, tc := range []struct{ mint, reuse map[string]any }{
		{map[string]any{"draft": true}, map[string]any{"draft": false}},
		{map[string]any{"draft": true}, map[string]any{}},
		{map[string]any{}, map[string]any{"sort_key": "updated_at"}},
		{map[string]any{"sort_key": "updated_at"}, map[string]any{"sort_key": "updated_at", "sort_dir": "asc"}},
	} {
		for _, path := range []string{"/api/dashboard/prs/list", teamPRsPath} {
			mint := map[string]any{"page_size": 2}
			maps.Copy(mint, tc.mint)
			first := httpxtest.DecodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", path, mint))
			if first.NextPageToken == "" {
				t.Fatalf("%s %v: no next_page_token on a page that leaves rows behind", path, tc.mint)
			}
			reuse := map[string]any{"page_size": 2, "page_token": first.NextPageToken}
			maps.Copy(reuse, tc.reuse)
			rec := doJSON(t, s, "POST", path, reuse)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: token minted under %v reused under %v = %d, want 400; body=%s",
					path, tc.mint, tc.reuse, rec.Code, rec.Body.String())
				continue
			}
			assertFieldError(t, rec, "page_token")
		}
	}
}
