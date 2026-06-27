package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestParseArtifactListOpts is a Docker-free unit test for the activity feed's
// query parser: defaults, the pass-through filters, the time window (RFC3339 /
// YYYY-MM-DD, unbounded by default, inverted rejected), and the limit/offset
// clamps + validation.
func TestParseArtifactListOpts(t *testing.T) {
	// Defaults: empty query → default page, no filters, unbounded window.
	opts, errMsg := parseArtifactListOpts(url.Values{})
	if errMsg != "" {
		t.Fatalf("empty query errored: %q", errMsg)
	}
	if opts.Limit != activityPageDefault || opts.Offset != 0 {
		t.Errorf("default paging = limit %d offset %d, want %d/0", opts.Limit, opts.Offset, activityPageDefault)
	}
	if opts.Provider != "" || opts.Kind != "" || opts.State != "" || !opts.Since.IsZero() || !opts.Until.IsZero() {
		t.Errorf("default opts not empty: %+v", opts)
	}

	// Filters + a bare-date window pass through.
	opts, errMsg = parseArtifactListOpts(url.Values{
		"provider": {"github"}, "kind": {"pull_request"}, "state": {"open"},
		"since": {"2026-06-01"}, "until": {"2026-06-30"}, "limit": {"10"}, "offset": {"20"},
	})
	if errMsg != "" {
		t.Fatalf("valid query errored: %q", errMsg)
	}
	if opts.Provider != "github" || opts.Kind != "pull_request" || opts.State != "open" {
		t.Errorf("filters not parsed: %+v", opts)
	}
	if !opts.Since.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) || !opts.Until.Equal(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("window not parsed: since=%v until=%v", opts.Since, opts.Until)
	}
	if opts.Limit != 10 || opts.Offset != 20 {
		t.Errorf("paging = limit %d offset %d, want 10/20", opts.Limit, opts.Offset)
	}

	// Limit clamps to the max.
	if opts, _ = parseArtifactListOpts(url.Values{"limit": {"100000"}}); opts.Limit != activityPageMax {
		t.Errorf("over-max limit = %d, want clamp to %d", opts.Limit, activityPageMax)
	}

	// Malformed values are rejected.
	for name, q := range map[string]url.Values{
		"bad_since":       {"since": {"yesterday"}},
		"inverted_window": {"since": {"2026-06-30"}, "until": {"2026-06-01"}},
		"zero_limit":      {"limit": {"0"}},
		"negative_offset": {"offset": {"-1"}},
	} {
		if _, errMsg := parseArtifactListOpts(q); errMsg == "" {
			t.Errorf("%s: expected a validation error, got none", name)
		}
	}
}

// activityReq builds a GET for the bot-activity endpoints carrying the active
// org + caller claims (and the team_id path value when set), with an optional
// raw query string for the filter cases. Mirrors usageRig.req but lets the
// caller control the query (req hardcodes ?since=). The handler is invoked
// directly so the test exercises the governance gate + role gates + RLS without
// the cookie-session dance.
func (r *usageRig) activityReq(caller, teamID, rawQuery string) *http.Request {
	url := "/api/usage/activity"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if teamID != "" {
		req.SetPathValue("team_id", teamID)
	}
	ctx := httpx.WithOrgID(req.Context(), r.orgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
	return req.WithContext(ctx)
}

// seedArtifacts stages the bot-activity rows the feed reads: two on teamA (a PR
// + a comment) and one on teamB (a branch), so the org feed is cross-team and
// the team feed is teamA-only. run_id is left NULL — the feed reads artifacts
// directly, not through a run.
func (r *usageRig) seedArtifacts(t *testing.T) {
	t.Helper()
	seed := func(teamID, provider, kind, state, key string) {
		pgtest.MustExec(t, r.h.AdminDB, `
			INSERT INTO artifacts (org_id, team_id, provider, kind, target, state, dedup_key)
			VALUES ($1, $2, $3, $4, 'octo/repo', $5, $6)
		`, r.orgID, teamID, provider, kind, state, key)
	}
	seed(r.teamA, "github", "pull_request", "open", "tA-pr")
	seed(r.teamA, "github", "comment", "posted", "tA-comment")
	seed(r.teamB, "git", "branch", "pushed", "tB-branch")
}

// TestUsageActivityHandler_GatesAndScope_Postgres pins the bot-activity feed's
// FeatureGovernance gate (unlicensed → 404), the role gates — team feed is team
// admin OR org admin (an org admin reads ANY team's activity, unlike the
// team-admin-only spend feed), org feed is org-admin-only — the cross-team org
// read, the filter passthrough, and the org feed's resolved team names.
func TestUsageActivityHandler_GatesAndScope_Postgres(t *testing.T) {
	r := newUsageRig(t)
	r.seedArtifacts(t)
	// Restore the community default after the whole test so the process-global
	// entitlement doesn't leak into sibling tests.
	t.Cleanup(entitlements.Reset)

	// --- unlicensed: every activity route 404s under the community default ---
	t.Run("unlicensed_team_404", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActivity(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("team activity unlicensed = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unlicensed_org_404", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActivity(rec, r.activityReq(r.orgAdmin, "", ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("org activity unlicensed = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	// Everything below runs with governance licensed.
	license := func() { entitlements.Register(governanceGrant{}) }

	t.Run("team_plain_member_403", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActivity(rec, r.activityReq(r.member, r.teamA, ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("team activity as plain member = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin_200_team_scoped", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActivity(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("team activity as team admin = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var out []activityArtifactJSON
		mustDecode(t, rec, &out)
		if len(out) != 2 {
			t.Errorf("teamA feed = %d rows, want 2 (PR + comment)", len(out))
		}
		// The team feed omits team fields (already one team).
		for _, a := range out {
			if a.TeamID != "" || a.TeamName != "" {
				t.Errorf("team feed row carried team fields: %+v", a)
			}
		}
	})

	t.Run("team_org_admin_not_member_200", func(t *testing.T) {
		// The key contrast with the spend team feed: the org-governance lens (an
		// org admin) may inspect ANY team's bot activity, even a team they're not on.
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActivity(rec, r.activityReq(r.orgAdmin, r.teamA, ""))
		if rec.Code != http.StatusOK {
			t.Errorf("team activity as org admin (not on teamA) = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_member_403", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActivity(rec, r.activityReq(r.member, "", ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("org activity as plain member = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_team_admin_403", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActivity(rec, r.activityReq(r.teamAdmin, "", ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("org activity as team admin (org member) = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_admin_200_cross_team_with_names", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActivity(rec, r.activityReq(r.orgAdmin, "", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("org activity as org admin = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var out []activityArtifactJSON
		mustDecode(t, rec, &out)
		if len(out) != 3 {
			t.Fatalf("org feed = %d rows, want 3 (cross-team)", len(out))
		}
		// Every org-feed row carries its team id + resolved name; the teamB branch
		// row names "teamB" (proves cross-team name resolution via Teams.GetSystem).
		var sawTeamB bool
		for _, a := range out {
			if a.TeamID == "" || a.TeamName == "" {
				t.Errorf("org feed row missing team id/name: %+v", a)
			}
			if a.TeamID == r.teamB {
				sawTeamB = true
				if a.TeamName != "teamB" {
					t.Errorf("teamB row team_name = %q, want %q", a.TeamName, "teamB")
				}
			}
		}
		if !sawTeamB {
			t.Errorf("org feed did not include the teamB row — not cross-team")
		}
	})

	t.Run("filter_passthrough_kind", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActivity(rec, r.activityReq(r.orgAdmin, "", "kind=pull_request"))
		if rec.Code != http.StatusOK {
			t.Fatalf("org activity kind filter = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var out []activityArtifactJSON
		mustDecode(t, rec, &out)
		if len(out) != 1 || out[0].Kind != "pull_request" {
			t.Errorf("kind=pull_request filter returned %d rows (%+v), want exactly the one PR", len(out), out)
		}
	})
}
