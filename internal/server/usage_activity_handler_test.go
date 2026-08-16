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

	// Malformed values are rejected — including an over-max limit, which used
	// to clamp: a silently shortened page reads as the end of the feed. So do
	// filter values outside their closed vocabularies, which used to reach the
	// store as literals and answer 200 with nothing.
	for name, q := range map[string]url.Values{
		"bad_since":        {"since": {"yesterday"}},
		"inverted_window":  {"since": {"2026-06-30"}, "until": {"2026-06-01"}},
		"zero_limit":       {"limit": {"0"}},
		"over_max_limit":   {"limit": {"100000"}},
		"negative_offset":  {"offset": {"-1"}},
		"unknown_provider": {"provider": {"gitlab"}},
		"unknown_kind":     {"kind": {"pull_requests"}},
		"unknown_state":    {"state": {"opened"}},
	} {
		if _, errMsg := parseArtifactListOpts(q); errMsg == "" {
			t.Errorf("%s: expected a validation error, got none", name)
		}
	}
}

// TestParseExternalActionListOpts is the Docker-free unit test for the Actions
// lens's query parser: defaults, the action/actor pass-through filters, the time
// window (bound on occurred_at), and the limit/offset clamps. Mirrors
// TestParseArtifactListOpts (provider/time/paging are identical; kind/state →
// action/actor).
func TestParseExternalActionListOpts(t *testing.T) {
	opts, errMsg := parseExternalActionListOpts(url.Values{})
	if errMsg != "" {
		t.Fatalf("empty query errored: %q", errMsg)
	}
	if opts.Limit != activityPageDefault || opts.Provider != "" || opts.Action != "" || opts.ActorUserID != "" {
		t.Errorf("default opts wrong: %+v", opts)
	}

	opts, errMsg = parseExternalActionListOpts(url.Values{
		"provider": {"github"}, "action": {"pr_marked_ready"},
		"actor": {"9f1d1c2e-0000-4000-8000-00000000abcd"},
		"since": {"2026-06-01"}, "until": {"2026-06-30"}, "limit": {"10"}, "offset": {"20"},
	})
	if errMsg != "" {
		t.Fatalf("valid query errored: %q", errMsg)
	}
	if opts.Provider != "github" || opts.Action != "pr_marked_ready" || opts.ActorUserID != "9f1d1c2e-0000-4000-8000-00000000abcd" {
		t.Errorf("filters not parsed: %+v", opts)
	}
	if !opts.Since.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) || opts.Limit != 10 || opts.Offset != 20 {
		t.Errorf("window/paging not parsed: %+v", opts)
	}

	for name, q := range map[string]url.Values{
		"over_max_limit":  {"limit": {"100000"}},
		"unknown_action":  {"action": {"pr_marked_readyy"}},
		"bad_actor":       {"actor": {"u-123"}},
		"bad_since":       {"since": {"never"}},
		"inverted_window": {"since": {"2026-06-30"}, "until": {"2026-06-01"}},
		"zero_limit":      {"limit": {"0"}},
		"negative_offset": {"offset": {"-1"}},
	} {
		if _, errMsg := parseExternalActionListOpts(q); errMsg == "" {
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
	license := func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance)) }

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

// seedExternalActions stages the Actions-lens rows: two on teamA (a bot
// pr_created with no actor + a human-authorized pr_marked_ready by orgAdmin) and
// one system row on teamB (a Jira transition, no actor). orgAdmin's display name
// is pinned so the org feed's resolved actor_name is deterministic.
func (r *usageRig) seedExternalActions(t *testing.T) {
	t.Helper()
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE users SET display_name = 'Org Admin' WHERE id = $1`, r.orgAdmin)
	seed := func(teamID, provider, action, target, cred, key, actor, from, to string) {
		var actorArg any
		if actor != "" {
			actorArg = actor
		}
		pgtest.MustExec(t, r.h.AdminDB, `
			INSERT INTO external_actions (org_id, team_id, provider, action, target, credential, dedup_key, actor_user_id, from_state, to_state)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9,''), NULLIF($10,''))
		`, r.orgID, teamID, provider, action, target, cred, key, actorArg, from, to)
	}
	seed(r.teamA, "github", "pr_created", "octo/repo#1", "github_app", "tA-created", "", "", "draft")
	seed(r.teamA, "github", "pr_marked_ready", "octo/repo#1", "github_app", "tA-ready", r.orgAdmin, "draft", "open")
	seed(r.teamB, "jira", "issue_transitioned", "SKY-1", "jira_org", "tB-trans", "", "To Do", "Done")
}

// TestUsageActionsActivityHandler_Postgres pins the Actions lens (?view=actions):
// the same FeatureGovernance gate (unlicensed → 404), the cross-team org read
// with resolved team + actor names (a human row names its authorizer, a system
// row carries none), the team-scoped read, and the action filter passthrough.
func TestUsageActionsActivityHandler_Postgres(t *testing.T) {
	r := newUsageRig(t)
	r.seedExternalActions(t)
	t.Cleanup(entitlements.Reset)

	// Unlicensed → 404, exactly like the Objects lens (the gate runs before the
	// view branch).
	t.Run("unlicensed_404", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActivity(rec, r.activityReq(r.orgAdmin, "", "view=actions"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("org actions unlicensed = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	license := func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance)) }

	t.Run("team_admin_200_scoped", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActivity(rec, r.activityReq(r.teamAdmin, r.teamA, "view=actions"))
		if rec.Code != http.StatusOK {
			t.Fatalf("team actions as team admin = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var out []actionJSON
		mustDecode(t, rec, &out)
		if len(out) != 2 {
			t.Fatalf("teamA actions = %d rows, want 2 (created + marked-ready)", len(out))
		}
		// The team feed omits the team chip (already one team) but DOES resolve the
		// authorizing actor's name — a team admin shouldn't see raw UUIDs.
		var sawHumanName bool
		for _, a := range out {
			if a.TeamID != "" || a.TeamName != "" {
				t.Errorf("team feed row carried a team chip: %+v", a)
			}
			switch a.Action {
			case "pr_marked_ready":
				if a.ActorUserID != r.orgAdmin || a.ActorName != "Org Admin" {
					t.Errorf("team feed human row actor = %q/%q, want orgAdmin/'Org Admin' (resolved)", a.ActorUserID, a.ActorName)
				}
				sawHumanName = true
			case "pr_created":
				if a.ActorName != "" {
					t.Errorf("bot row carried an actor name: %+v", a)
				}
			}
		}
		if !sawHumanName {
			t.Error("team feed did not resolve the human-authorized row's actor name")
		}
	})

	t.Run("org_admin_200_cross_team_with_names", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActivity(rec, r.activityReq(r.orgAdmin, "", "view=actions"))
		if rec.Code != http.StatusOK {
			t.Fatalf("org actions as org admin = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var out []actionJSON
		mustDecode(t, rec, &out)
		if len(out) != 3 {
			t.Fatalf("org actions = %d rows, want 3 (cross-team)", len(out))
		}
		var sawTeamB, sawHumanActor, sawSystem bool
		for _, a := range out {
			if a.TeamID == "" || a.TeamName == "" {
				t.Errorf("org feed action missing team id/name: %+v", a)
			}
			if a.TeamID == r.teamB {
				sawTeamB = true
			}
			switch a.Action {
			case "pr_marked_ready":
				// The human-authorized row names its authorizer + carries the transition.
				if a.ActorUserID != r.orgAdmin || a.ActorName != "Org Admin" {
					t.Errorf("human row actor = %q/%q, want orgAdmin/'Org Admin'", a.ActorUserID, a.ActorName)
				}
				if a.FromState != "draft" || a.ToState != "open" {
					t.Errorf("human row transition = %q→%q, want draft→open", a.FromState, a.ToState)
				}
				sawHumanActor = true
			case "pr_created", "issue_transitioned":
				// System/bot rows have no actor.
				if a.ActorUserID != "" || a.ActorName != "" {
					t.Errorf("system row carried an actor: %+v", a)
				}
				sawSystem = true
			}
		}
		if !sawTeamB || !sawHumanActor || !sawSystem {
			t.Errorf("missing coverage: teamB=%v human=%v system=%v", sawTeamB, sawHumanActor, sawSystem)
		}
	})

	t.Run("filter_passthrough_action", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActivity(rec, r.activityReq(r.orgAdmin, "", "view=actions&action=pr_created"))
		if rec.Code != http.StatusOK {
			t.Fatalf("action filter = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var out []actionJSON
		mustDecode(t, rec, &out)
		if len(out) != 1 || out[0].Action != "pr_created" {
			t.Errorf("action=pr_created returned %d rows (%+v), want exactly the one", len(out), out)
		}
	})
}
