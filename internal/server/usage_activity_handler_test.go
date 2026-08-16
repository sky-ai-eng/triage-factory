package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestResolveArtifactActivityFilters is a Docker-free unit test for the
// artifacts feed's body validation: the pass-through filters, the time window
// (RFC3339 / YYYY-MM-DD, unbounded by default, inverted rejected), and the
// closed vocabularies.
func TestResolveArtifactActivityFilters(t *testing.T) {
	// Empty body → no filters, unbounded window.
	var v httpx.Validation
	opts := resolveArtifactActivityFilters(&v, artifactActivityListRequest{})
	if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
		t.Fatal("empty body was rejected")
	}
	if opts.Provider != "" || opts.Kind != "" || opts.State != "" || !opts.Since.IsZero() || !opts.Until.IsZero() {
		t.Errorf("default opts not empty: %+v", opts)
	}

	// Filters + a bare-date window pass through.
	v = httpx.Validation{}
	opts = resolveArtifactActivityFilters(&v, artifactActivityListRequest{
		Provider: "github", Kind: "pull_request", State: "open",
		Since: "2026-06-01", Until: "2026-06-30",
	})
	if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
		t.Fatal("valid body was rejected")
	}
	if opts.Provider != "github" || opts.Kind != "pull_request" || opts.State != "open" {
		t.Errorf("filters not parsed: %+v", opts)
	}
	if !opts.Since.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) || !opts.Until.Equal(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("window not parsed: since=%v until=%v", opts.Since, opts.Until)
	}

	// Malformed values are rejected, including filter values outside their
	// closed vocabularies — which used to reach the store as literals and
	// answer 200 with nothing.
	for name, req := range map[string]artifactActivityListRequest{
		"bad_since":        {Since: "yesterday"},
		"inverted_window":  {Since: "2026-06-30", Until: "2026-06-01"},
		"unknown_provider": {Provider: "gitlab"},
		"unknown_kind":     {Kind: "pull_requests"},
		"unknown_state":    {State: "opened"},
	} {
		var v httpx.Validation
		resolveArtifactActivityFilters(&v, req)
		if !v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
			t.Errorf("%s: expected a validation error, got none", name)
		}
	}

	// The other side of a closed vocabulary: every value the writers can
	// actually store has to be accepted. A union assembled by hand omitted
	// 'posted', so kind=comment + state=posted — a combination the comment and
	// message writers produce — was rejected as a typo.
	for _, state := range domain.ArtifactStates() {
		var v httpx.Validation
		resolveArtifactActivityFilters(&v, artifactActivityListRequest{State: state})
		if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
			t.Errorf("state=%q rejected", state)
		}
	}
	for _, kind := range domain.ArtifactKinds() {
		var v httpx.Validation
		resolveArtifactActivityFilters(&v, artifactActivityListRequest{Kind: kind})
		if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
			t.Errorf("kind=%q rejected", kind)
		}
	}
}

// TestResolveActionActivityFilters mirrors the artifacts test for the actions
// feed: provider and the time window are identical; kind/state become
// action/actor.
func TestResolveActionActivityFilters(t *testing.T) {
	var v httpx.Validation
	opts := resolveActionActivityFilters(&v, actionActivityListRequest{})
	if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
		t.Fatal("empty body was rejected")
	}
	if opts.Provider != "" || opts.Action != "" || opts.ActorUserID != "" {
		t.Errorf("default opts not empty: %+v", opts)
	}

	actor := "11111111-1111-1111-1111-111111111111"
	v = httpx.Validation{}
	opts = resolveActionActivityFilters(&v, actionActivityListRequest{
		Provider: "github", Action: domain.ExternalActionTypes()[0], Actor: actor,
		Since: "2026-06-01", Until: "2026-06-30",
	})
	if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
		t.Fatal("valid body was rejected")
	}
	if opts.Provider != "github" || opts.ActorUserID != actor {
		t.Errorf("filters not parsed: %+v", opts)
	}

	for name, req := range map[string]actionActivityListRequest{
		"unknown_provider": {Provider: "gitlab"},
		"unknown_action":   {Action: "pr_openedd"},
		"bad_actor":        {Actor: "not-a-uuid"},
		"bad_since":        {Since: "yesterday"},
		"inverted_window":  {Since: "2026-06-30", Until: "2026-06-01"},
	} {
		var v httpx.Validation
		resolveActionActivityFilters(&v, req)
		if !v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
			t.Errorf("%s: expected a validation error, got none", name)
		}
	}
	// Every action type the writers store must be accepted.
	for _, action := range domain.ExternalActionTypes() {
		var v httpx.Validation
		resolveActionActivityFilters(&v, actionActivityListRequest{Action: action})
		if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
			t.Errorf("action=%q rejected", action)
		}
	}
}

// activityReq builds a POST for one of the four bot-activity list routes,
// carrying the active org + caller claims (and the team_id path value when
// set), with body as the JSON list request — "" means an empty body object.
// Mirrors usageRig.req but speaks the list contract. The handler is invoked
// directly so the test exercises the governance gate + role gates + RLS without
// the cookie-session dance.
func (r *usageRig) activityReq(caller, teamID, body string) *http.Request {
	if body == "" {
		body = "{}"
	}
	req := httptest.NewRequest(http.MethodPost, "/api/usage/activity/list", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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

// TestUsageArtifactsActivityHandler_Postgres pins the artifacts feed's
// FeatureGovernance gate (unlicensed → 404), the role gates — team feed is team
// admin OR org admin (an org admin reads ANY team's activity, unlike the
// team-admin-only spend feed), org feed is org-admin-only — the cross-team org
// read, the filter passthrough, and the org feed's resolved team names.
func TestUsageArtifactsActivityHandler_Postgres(t *testing.T) {
	r := newUsageRig(t)
	r.seedArtifacts(t)
	// Restore the community default after the whole test so the process-global
	// entitlement doesn't leak into sibling tests.
	t.Cleanup(entitlements.Reset)

	// --- unlicensed: every activity route 404s under the community default ---
	t.Run("unlicensed_team_404", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamArtifacts(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("team artifacts unlicensed = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unlicensed_org_404", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("org artifacts unlicensed = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	// Everything below runs with governance licensed.
	license := func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance)) }

	t.Run("team_plain_member_403", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamArtifacts(rec, r.activityReq(r.member, r.teamA, ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("team artifacts as plain member = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin_200_team_scoped", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamArtifacts(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		out := decodeList[activityArtifactJSON](t, rec)
		if len(out.Items) != 2 || out.Total() != 2 {
			t.Errorf("teamA feed = %d rows / total %d, want 2/2 (PR + comment)", len(out.Items), out.Total())
		}
		// The team feed omits team fields (already one team).
		for _, a := range out.Items {
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
		r.uh.handleUsageTeamArtifacts(rec, r.activityReq(r.orgAdmin, r.teamA, ""))
		if rec.Code != http.StatusOK {
			t.Errorf("team artifacts as org admin (not on teamA) = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_member_403", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.member, "", ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("org artifacts as plain member = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_team_admin_403", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.teamAdmin, "", ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("org artifacts as team admin (org member) = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_admin_200_cross_team_with_names", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", ""))
		out := decodeList[activityArtifactJSON](t, rec)
		if len(out.Items) != 3 || out.Total() != 3 {
			t.Fatalf("org feed = %d rows / total %d, want 3/3 (cross-team)", len(out.Items), out.Total())
		}
		// Every org-feed row carries its team id + resolved name; the teamB branch
		// row names "teamB" (proves cross-team name resolution via Teams.GetSystem).
		var sawTeamB bool
		for _, a := range out.Items {
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
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"kind":"pull_request"}`))
		out := decodeList[activityArtifactJSON](t, rec)
		if len(out.Items) != 1 || out.Items[0].Kind != "pull_request" {
			t.Errorf("kind=pull_request filter returned %d rows (%+v), want exactly the one PR", len(out.Items), out.Items)
		}
		// The filtered total counts the filtered set, not the feed — a page and a
		// total that disagree is how a UI shows "1 of 3".
		if out.Total() != 1 {
			t.Errorf("filtered total = %d, want 1", out.Total())
		}
	})

	t.Run("paging_walks_the_feed", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"page_size":2}`))
		first := decodeList[activityArtifactJSON](t, rec)
		if len(first.Items) != 2 || first.Total() != 3 || first.NextPageToken == "" {
			t.Fatalf("page 1 = %d rows / total %d / next %q, want 2/3/non-empty", len(first.Items), first.Total(), first.NextPageToken)
		}
		rec = httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"page_size":2,"page_token":"`+first.NextPageToken+`"}`))
		second := decodeList[activityArtifactJSON](t, rec)
		if len(second.Items) != 1 || second.NextPageToken != "" {
			t.Fatalf("page 2 = %d rows / next %q, want 1/empty", len(second.Items), second.NextPageToken)
		}
		// No row is served twice across the two pages — the ORDER BY's id
		// tiebreaker is what makes that true for rows sharing a created_at.
		seen := map[string]bool{}
		for _, a := range append(append([]activityArtifactJSON{}, first.Items...), second.Items...) {
			if seen[a.ID] {
				t.Errorf("artifact %s appeared on both pages", a.ID)
			}
			seen[a.ID] = true
		}
	})

	t.Run("over_max_page_size_400", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"page_size":100000}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("page_size=100000 = %d, want 400 (never a clamp); body=%s", rec.Code, rec.Body.String())
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

// TestUsageActionsActivityHandler_Postgres pins the actions feed — its own pair
// of routes since the ?view= multiplex split: the same FeatureGovernance gate
// (unlicensed → 404), the cross-team org read with resolved team + actor names
// (a human row names its authorizer, a system row carries none), the
// team-scoped read, and the action filter passthrough.
func TestUsageActionsActivityHandler_Postgres(t *testing.T) {
	r := newUsageRig(t)
	r.seedExternalActions(t)
	t.Cleanup(entitlements.Reset)

	// Unlicensed → 404, exactly like the artifacts feed (the gate is the shared
	// caller resolver, so it runs identically on both resources).
	t.Run("unlicensed_404", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("org actions unlicensed = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	license := func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance)) }

	t.Run("team_admin_200_scoped", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActions(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		out := decodeList[actionJSON](t, rec)
		if len(out.Items) != 2 || out.Total() != 2 {
			t.Fatalf("teamA actions = %d rows / total %d, want 2/2 (created + marked-ready)", len(out.Items), out.Total())
		}
		// The team feed omits the team chip (already one team) but DOES resolve the
		// authorizing actor's name — a team admin shouldn't see raw UUIDs.
		var sawHumanName bool
		for _, a := range out.Items {
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
		r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", ""))
		out := decodeList[actionJSON](t, rec)
		if len(out.Items) != 3 || out.Total() != 3 {
			t.Fatalf("org actions = %d rows / total %d, want 3/3 (cross-team)", len(out.Items), out.Total())
		}
		var sawTeamB, sawHumanActor, sawSystem bool
		for _, a := range out.Items {
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
		r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", `{"action":"pr_created"}`))
		out := decodeList[actionJSON](t, rec)
		if len(out.Items) != 1 || out.Items[0].Action != "pr_created" {
			t.Errorf("action=pr_created returned %d rows (%+v), want exactly the one", len(out.Items), out.Items)
		}
		if out.Total() != 1 {
			t.Errorf("filtered total = %d, want 1", out.Total())
		}
	})

	t.Run("actions_body_rejects_artifact_filters", func(t *testing.T) {
		// The two resources have different filter vocabularies, which is the whole
		// reason they are two routes: `state=` used to be accepted here and
		// silently ignored. Strict decoding makes it a 400.
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", `{"state":"open"}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("actions list with an artifacts filter = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})
}
