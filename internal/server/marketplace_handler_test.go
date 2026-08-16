package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// marketplaceRig is the multi-mode fixture for the publish/republish/delist
// handler family (TFAC-536): a real Postgres-backed Server (RLS enforced)
// with one org carrying a founder (write role on the default team), a plain
// member on a second team, and a viewer on the founder's team.
type marketplaceRig struct {
	h      *pgtest.Harness
	mh     *marketplaceHandler
	orgID  string
	teamID string
	admin  string // founder — write role on teamID
	viewer string // viewer role on teamID
	teamB  string
	member string // write role on teamB, no role on teamID
}

func newMarketplaceRig(t *testing.T) *marketplaceRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, founder, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	viewer := pgtest.SeedUser(t, h, "viewer")
	pgtest.AddOrgMember(t, h, viewer, orgID, teamID, "member", "viewer")
	teamB := pgtest.SeedTeam(t, h, orgID, "teamB")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamB, "member", "member")

	return &marketplaceRig{
		h:      h,
		mh:     &marketplaceHandler{tx: s.tx, az: s.az},
		orgID:  orgID,
		teamID: teamID,
		admin:  founder,
		viewer: viewer,
		teamB:  teamB,
		member: member,
	}
}

// req builds a request to path as callerID with claims + active org injected,
// mirroring viewerRig.req.
func (r *marketplaceRig) req(method, path, callerID string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

// seedPrompt inserts a team prompt via the admin pool and returns its id.
func (r *marketplaceRig) seedPrompt(t *testing.T, teamID, name, body, model, allowedTools string) string {
	t.Helper()
	var id string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source, model, allowed_tools)
		VALUES ($1, $2, $3, $4, $5, 'user', $6, $7) RETURNING id
	`, r.orgID, r.admin, teamID, name, body, model, allowedTools).Scan(&id); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return id
}

// seedBlueprint inserts a team blueprint (no steps) via the admin pool.
func (r *marketplaceRig) seedBlueprint(t *testing.T, teamID, name string) string {
	t.Helper()
	var id string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO blueprints (org_id, creator_user_id, team_id, name, source)
		VALUES ($1, $2, $3, $4, 'user') RETURNING id
	`, r.orgID, r.admin, teamID, name).Scan(&id); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	return id
}

// seedBlueprintStep attaches a step to a blueprint at stepIndex.
func (r *marketplaceRig) seedBlueprintStep(t *testing.T, teamID, blueprintID string, stepIndex int, promptID, brief string) {
	t.Helper()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id, brief)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, r.orgID, teamID, blueprintID, stepIndex, promptID, brief)
}

// seedInstall inserts a marketplace_installs audit row directly (TFAC-538's
// install/copy endpoint hasn't landed yet — this fixture only needs the
// install count List.Sort=installs reads, not the real copy-to-team flow).
func (r *marketplaceRig) seedInstall(t *testing.T, listingID, teamID, userID string) {
	t.Helper()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO marketplace_installs (listing_id, org_id, version, team_id, user_id, created_at)
		VALUES ($1, $2, 1, $3, $4, now())
	`, listingID, r.orgID, teamID, userID)
}

// seedStats writes a marketplace_listing_stats row directly (bypassing
// RecomputeStatsSystem — that aggregation is pinned at the pgtest layer,
// internal/db/pgtest/marketplace_stats_test.go; here the handler only needs
// a row to exist so List/Get's join and sort=used have something to read).
func (r *marketplaceRig) seedStats(t *testing.T, listingID string, teamsUsing, totalRuns int) {
	t.Helper()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO marketplace_listing_stats (listing_id, org_id, teams_using, total_runs, computed_at)
		VALUES ($1, $2, $3, $4, now())
	`, listingID, r.orgID, teamsUsing, totalRuns)
}

// getListingDetail reads a listing back through the store as callerID — used
// by the happy-path tests to assert on the built snapshot without duplicating
// the store's own scan logic.
func (r *marketplaceRig) getListingDetail(t *testing.T, callerID, listingID string) domain.ListingDetail {
	t.Helper()
	var detail domain.ListingDetail
	if err := r.h.WithUser(t, callerID, r.orgID, func(tx *sql.Tx) error {
		var e error
		detail, e = pgstore.NewForTx(tx, pgtest.SecretKey).Marketplace.Get(t.Context(), r.orgID, listingID, callerID)
		return e
	}); err != nil {
		t.Fatalf("get listing %s: %v", listingID, err)
	}
	return detail
}

// eventTypeFixture is a real seeded events_catalog id — other suites (e.g.
// pgtest/marketplace_test.go) use the same id, so it's guaranteed present.
const eventTypeFixture = "github:pr:opened"

func doMarketplaceJSON(fn func(http.ResponseWriter, *http.Request), req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

// TestMarketplace_LocalIs404: every endpoint in the family 404s in local
// mode, regardless of caller identity.
func TestMarketplace_LocalIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "local-gate", "mission", "", "")
	runmode.SetForTest(t, runmode.ModeLocal)

	for _, tc := range []struct {
		name string
		req  *http.Request
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"publish", r.req(http.MethodPost, "/api/marketplace/listings", r.admin,
			map[string]any{"kind": "prompt", "source_id": promptID, "name": "n"}), r.mh.handleMarketplacePublish},
		{"versions", r.req(http.MethodPost, "/api/marketplace/listings/x/versions", r.admin,
			map[string]any{"name": "n"}), r.mh.handleMarketplaceListingVersionCreate},
		{"delist", r.req(http.MethodPost, "/api/marketplace/listings/x/delist", r.admin, nil), r.mh.handleMarketplaceListingDelist},
		{"relist", r.req(http.MethodPost, "/api/marketplace/listings/x/relist", r.admin, nil), r.mh.handleMarketplaceListingRelist},
		{"by-source", r.req(http.MethodGet, "/api/marketplace/listings/by-source/"+promptID, r.admin, nil), r.mh.handleMarketplaceListingBySource},
		{"list", r.req(http.MethodGet, "/api/marketplace/listings", r.admin, nil), r.mh.handleMarketplaceList},
		{"get", r.req(http.MethodGet, "/api/marketplace/listings/x", r.admin, nil), r.mh.handleMarketplaceGet},
		{"vote", r.req(http.MethodPut, "/api/marketplace/listings/x/vote", r.admin, nil), r.mh.handleMarketplaceVote},
		{"unvote", r.req(http.MethodDelete, "/api/marketplace/listings/x/vote", r.admin, nil), r.mh.handleMarketplaceUnvote},
		{"install", r.req(http.MethodPost, "/api/marketplace/listings/x/install", r.admin,
			map[string]any{"team_id": ""}), r.mh.handleMarketplaceInstall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.SetPathValue("id", "x")
			tc.req.SetPathValue("source_id", promptID)
			rec := doMarketplaceJSON(tc.fn, tc.req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: status = %d, want 404 in local mode; body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestMarketplace_NoToggleGate: the within-org marketplace has no admin
// toggle — org_settings.marketplace_enabled defaults false (TFAC-535's
// ship-dark column, reserved for the future cross-org gate) and every
// endpoint in the family still works normally in multi mode regardless.
func TestMarketplace_NoToggleGate(t *testing.T) {
	r := newMarketplaceRig(t)
	// newMarketplaceRig seeds the org via the raw pgtest fixture helpers,
	// which (unlike real signup provisioning) don't materialize an
	// org_settings row — insert one here so the DEFAULT false clause on
	// marketplace_enabled actually gets exercised.
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO org_settings (org_id) VALUES ($1)`, r.orgID)
	var enabled bool
	if err := r.h.AdminDB.QueryRow(`SELECT marketplace_enabled FROM org_settings WHERE org_id = $1`, r.orgID).Scan(&enabled); err != nil {
		t.Fatalf("read marketplace_enabled: %v", err)
	}
	if enabled {
		t.Fatalf("marketplace_enabled = true by default, want false (ship-dark default, unrelated to this surface)")
	}

	promptID := r.seedPrompt(t, r.teamID, "no-gate", "mission", "", "")
	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin,
			map[string]any{"kind": "prompt", "source_id": promptID, "name": "n"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish with marketplace_enabled=false: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	listReq := r.req(http.MethodPost, "/api/marketplace/listings/list", r.admin, map[string]any{})
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceList, listReq); rec.Code != http.StatusOK {
		t.Fatalf("list with marketplace_enabled=false: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplacePublish_Prompt_HappyPath: publishing a standalone prompt
// builds the snapshot server-side from the prompt row (body/model/allowed_tools,
// no ids, no brief) and returns 201 with a fresh listing id.
func TestMarketplacePublish_Prompt_HappyPath(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "Great Prompt", "do the thing", "opus", "Bash,Read")

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "Great Prompt Listing",
			"description": "does the thing", "event_types": []string{eventTypeFixture},
		}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish prompt: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "" {
		t.Fatalf("publish prompt: empty listing id")
	}

	detail := r.getListingDetail(t, r.admin, resp.ID)
	if detail.Kind != domain.ListingKindPrompt {
		t.Errorf("kind = %q, want prompt", detail.Kind)
	}
	if detail.Name != "Great Prompt Listing" || detail.Description != "does the thing" {
		t.Errorf("name/description = %q/%q, want the request's values", detail.Name, detail.Description)
	}
	if detail.PublisherTeamID != r.teamID {
		t.Errorf("publisher_team_id = %q, want %q (the prompt's own team)", detail.PublisherTeamID, r.teamID)
	}
	if len(detail.CurrentSnapshot.Steps) != 1 {
		t.Fatalf("snapshot steps = %d, want 1", len(detail.CurrentSnapshot.Steps))
	}
	step := detail.CurrentSnapshot.Steps[0]
	if step.Name != "Great Prompt" || step.Body != "do the thing" || step.Model != "opus" || step.AllowedTools != "Bash,Read" {
		t.Errorf("snapshot step = %+v, want the source prompt's content verbatim", step)
	}
	if step.Brief != "" {
		t.Errorf("prompt-kind snapshot step brief = %q, want empty", step.Brief)
	}
}

// TestMarketplacePublish_Blueprint_HappyPath: publishing a blueprint inlines
// every step's prompt content (body/model/allowed_tools/brief) in step_index
// order — no ids anywhere in the snapshot.
func TestMarketplacePublish_Blueprint_HappyPath(t *testing.T) {
	r := newMarketplaceRig(t)
	p1 := r.seedPrompt(t, r.teamID, "Step One", "first mission", "sonnet", "")
	p2 := r.seedPrompt(t, r.teamID, "Step Two", "second mission", "opus", "Grep")
	bp := r.seedBlueprint(t, r.teamID, "Two-Step Chain")
	r.seedBlueprintStep(t, r.teamID, bp, 0, p1, "do step one first")
	r.seedBlueprintStep(t, r.teamID, bp, 1, p2, "")

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "blueprint", "source_id": bp, "name": "Chain Listing",
			"description": "a two-step chain", "event_types": []string{eventTypeFixture},
		}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish blueprint: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	detail := r.getListingDetail(t, r.admin, resp.ID)
	if detail.Kind != domain.ListingKindBlueprint {
		t.Errorf("kind = %q, want blueprint", detail.Kind)
	}
	if len(detail.CurrentSnapshot.Steps) != 2 {
		t.Fatalf("snapshot steps = %d, want 2", len(detail.CurrentSnapshot.Steps))
	}
	s0, s1 := detail.CurrentSnapshot.Steps[0], detail.CurrentSnapshot.Steps[1]
	if s0.StepIndex != 0 || s0.Name != "Step One" || s0.Body != "first mission" || s0.Model != "sonnet" || s0.Brief != "do step one first" {
		t.Errorf("step 0 = %+v, want source step 0's content verbatim", s0)
	}
	if s1.StepIndex != 1 || s1.Name != "Step Two" || s1.Body != "second mission" || s1.Model != "opus" || s1.AllowedTools != "Grep" {
		t.Errorf("step 1 = %+v, want source step 1's content verbatim", s1)
	}
	if len(detail.EventTypes) != 1 || detail.EventTypes[0] != eventTypeFixture {
		t.Errorf("event_types = %+v, want [%s]", detail.EventTypes, eventTypeFixture)
	}
}

// TestMarketplacePublish_NonWriterIs403: a viewer on the source object's team
// cannot publish it — 403 view-only, and nothing is created.
func TestMarketplacePublish_NonWriterIs403(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "guarded", "mission", "", "")

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.viewer, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer publish: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	var count int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_listings WHERE source_id = $1`, promptID).Scan(&count); err != nil {
		t.Fatalf("count listings: %v", err)
	}
	if count != 0 {
		t.Errorf("listing count = %d after blocked viewer publish, want 0", count)
	}
}

// TestMarketplacePublish_CrossTeamIs404: a write-role member of a *different*
// team can't publish someone else's team object. Unlike the same-team-viewer
// case (403 — visible but not writable), a member of teamB has no
// prompts_select visibility into teamID's prompt at all (RLS: creator-or-
// same-team), so buildListingSnapshot's Get returns nil and the response is
// 404 — the same "invisible → 404, visible-but-blocked → 403" convention
// authz.RequireTaskWrite documents elsewhere, so a prober on another team
// never learns the prompt exists.
func TestMarketplacePublish_CrossTeamIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "not-yours", "mission", "", "")

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.member, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team publish: status = %d, want 404 (invisible under RLS, not merely unwritable); body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplacePublish_DuplicateActiveSourceIs409: a source that already
// has an active listing can't be published again — the client should have
// offered republish.
func TestMarketplacePublish_DuplicateActiveSourceIs409(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "dup", "mission", "", "")

	first := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	if first.Code != http.StatusCreated {
		t.Fatalf("first publish: status = %d, want 201; body=%s", first.Code, first.Body.String())
	}

	second := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n again",
		}))
	if second.Code != http.StatusConflict {
		t.Fatalf("second publish: status = %d, want 409; body=%s", second.Code, second.Body.String())
	}
}

// TestMarketplacePublish_UnknownEventTypeIs400: an event_type not present in
// events_catalog is rejected before anything is written.
func TestMarketplacePublish_UnknownEventTypeIs400(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "bad-event", "mission", "", "")

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
			"event_types": []string{"not:a:real:event"},
		}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown event_type: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplacePublish_UnknownKindIs400 and missing-field validation.
func TestMarketplacePublish_ValidationErrors(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "validate-me", "mission", "", "")

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"bad kind", map[string]any{"kind": "widget", "source_id": promptID, "name": "n"}},
		{"missing source_id", map[string]any{"kind": "prompt", "name": "n"}},
		{"missing name", map[string]any{"kind": "prompt", "source_id": promptID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
				r.req(http.MethodPost, "/api/marketplace/listings", r.admin, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400; body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestMarketplacePublish_SourceNotFoundIs404: a source_id that doesn't
// resolve to a live object in the caller's org 404s rather than 500ing or
// silently publishing an empty snapshot.
func TestMarketplacePublish_SourceNotFoundIs404(t *testing.T) {
	r := newMarketplaceRig(t)

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": "00000000-0000-0000-0000-000000000000", "name": "n",
		}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nonexistent source: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceRepublish_BumpsVersionPreservesV1: republishing rebuilds the
// snapshot from the *current* state of the source object, appends it as
// version 2, bumps current_version, and leaves version 1's frozen snapshot
// untouched.
func TestMarketplaceRepublish_BumpsVersionPreservesV1(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "v1 name", "v1 body", "sonnet", "")

	publishRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "Listing v1", "description": "d1",
		}))
	if publishRec.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d, want 201; body=%s", publishRec.Code, publishRec.Body.String())
	}
	var pubResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(publishRec.Body.Bytes(), &pubResp)

	// Edit the live prompt — the republish must pick up this new content, not
	// what was frozen into v1.
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE prompts SET name = $1, body = $2 WHERE id = $3`,
		"v2 name", "v2 body", promptID)

	repubReq := r.req(http.MethodPost, "/api/marketplace/listings/"+pubResp.ID+"/versions", r.admin, map[string]any{
		"name": "Listing v2", "description": "d2", "event_types": []string{eventTypeFixture},
	})
	repubReq.SetPathValue("id", pubResp.ID)
	repubRec := doMarketplaceJSON(r.mh.handleMarketplaceListingVersionCreate, repubReq)
	if repubRec.Code != http.StatusOK {
		t.Fatalf("republish: status = %d, want 200; body=%s", repubRec.Code, repubRec.Body.String())
	}
	var verResp struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(repubRec.Body.Bytes(), &verResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if verResp.Version != 2 {
		t.Fatalf("republish version = %d, want 2", verResp.Version)
	}

	detail := r.getListingDetail(t, r.admin, pubResp.ID)
	if detail.CurrentVersion != 2 {
		t.Errorf("current_version = %d, want 2", detail.CurrentVersion)
	}
	if detail.Name != "Listing v2" || detail.Description != "d2" {
		t.Errorf("listing name/description = %q/%q after republish, want the v2 values", detail.Name, detail.Description)
	}
	if len(detail.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(detail.Versions))
	}
	v1 := detail.Versions[0]
	if v1.Version != 1 || v1.Snapshot.Name != "Listing v1" || v1.Snapshot.Steps[0].Name != "v1 name" || v1.Snapshot.Steps[0].Body != "v1 body" {
		t.Errorf("v1 snapshot = %+v, want the original frozen content (untouched by the republish)", v1)
	}
	v2 := detail.Versions[1]
	if v2.Version != 2 || v2.Snapshot.Steps[0].Name != "v2 name" || v2.Snapshot.Steps[0].Body != "v2 body" {
		t.Errorf("v2 snapshot = %+v, want the edited prompt's current content", v2)
	}
	if detail.CurrentSnapshot.Steps[0].Name != "v2 name" {
		t.Errorf("current_snapshot = %+v, want the v2 content", detail.CurrentSnapshot)
	}
}

// TestMarketplaceRepublish_NonWriterIs403: only a writer on the *publisher*
// team may republish.
func TestMarketplaceRepublish_NonWriterIs403(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "guarded", "mission", "", "")
	publishRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	var pubResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(publishRec.Body.Bytes(), &pubResp)

	repubReq := r.req(http.MethodPost, "/api/marketplace/listings/"+pubResp.ID+"/versions", r.viewer, map[string]any{"name": "n2"})
	repubReq.SetPathValue("id", pubResp.ID)
	rec := doMarketplaceJSON(r.mh.handleMarketplaceListingVersionCreate, repubReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer republish: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceDelistRelist_RoundTrip: the publisher-team writer can delist
// (hides from browse, listing survives) and relist (restores it); a
// non-writer on the publisher team is blocked from both.
func TestMarketplaceDelistRelist_RoundTrip(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "delist-me", "mission", "", "")
	publishRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	var pubResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(publishRec.Body.Bytes(), &pubResp)

	// Viewer cannot delist.
	blockedReq := r.req(http.MethodPost, "/api/marketplace/listings/"+pubResp.ID+"/delist", r.viewer, nil)
	blockedReq.SetPathValue("id", pubResp.ID)
	blockedRec := doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, blockedReq)
	if blockedRec.Code != http.StatusForbidden {
		t.Fatalf("viewer delist: status = %d, want 403; body=%s", blockedRec.Code, blockedRec.Body.String())
	}

	// Founder delists.
	delistReq := r.req(http.MethodPost, "/api/marketplace/listings/"+pubResp.ID+"/delist", r.admin, nil)
	delistReq.SetPathValue("id", pubResp.ID)
	delistRec := doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, delistReq)
	if delistRec.Code != http.StatusNoContent {
		t.Fatalf("delist: status = %d, want 204; body=%s", delistRec.Code, delistRec.Body.String())
	}
	var status string
	if err := r.h.AdminDB.QueryRow(`SELECT status FROM marketplace_listings WHERE id = $1`, pubResp.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != domain.ListingStatusDelisted {
		t.Fatalf("status after delist = %q, want delisted", status)
	}

	// Founder relists.
	relistReq := r.req(http.MethodPost, "/api/marketplace/listings/"+pubResp.ID+"/relist", r.admin, nil)
	relistReq.SetPathValue("id", pubResp.ID)
	relistRec := doMarketplaceJSON(r.mh.handleMarketplaceListingRelist, relistReq)
	if relistRec.Code != http.StatusNoContent {
		t.Fatalf("relist: status = %d, want 204; body=%s", relistRec.Code, relistRec.Body.String())
	}
	if err := r.h.AdminDB.QueryRow(`SELECT status FROM marketplace_listings WHERE id = $1`, pubResp.ID).Scan(&status); err != nil {
		t.Fatalf("read status after relist: %v", err)
	}
	if status != domain.ListingStatusPublished {
		t.Fatalf("status after relist = %q, want published", status)
	}
}

// TestMarketplacePublish_DuplicateDelistedSourceIs409: publishing a source
// whose prior listing was delisted must not mint a second listing for the
// same source_id (GetActiveBySource's published-only filter would miss
// this — see GetBySource) — it 409s and steers the caller to relist.
func TestMarketplacePublish_DuplicateDelistedSourceIs409(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "dup-delisted", "mission", "", "")

	firstRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first publish: status = %d, want 201; body=%s", firstRec.Code, firstRec.Body.String())
	}
	var firstResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(firstRec.Body.Bytes(), &firstResp)

	delistReq := r.req(http.MethodPost, "/api/marketplace/listings/"+firstResp.ID+"/delist", r.admin, nil)
	delistReq.SetPathValue("id", firstResp.ID)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, delistReq); rec.Code != http.StatusNoContent {
		t.Fatalf("delist: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	secondRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n again",
		}))
	if secondRec.Code != http.StatusConflict {
		t.Fatalf("publish over a delisted source: status = %d, want 409; body=%s", secondRec.Code, secondRec.Body.String())
	}

	var count int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_listings WHERE source_id = $1`, promptID).Scan(&count); err != nil {
		t.Fatalf("count listings: %v", err)
	}
	if count != 1 {
		t.Fatalf("listing count for source = %d after blocked second publish, want 1 (no duplicate)", count)
	}
}

// TestMarketplaceListingBySource_ShowsDelisted: the by-source lookup must
// keep resolving a delisted listing (not revert to null) so the editor can
// show "Delisted" + Relist instead of "never published" — a null response
// here would let the frontend offer Publish again and mint a duplicate.
func TestMarketplaceListingBySource_ShowsDelisted(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "delisted-lookup", "mission", "", "")

	publishRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	var pubResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(publishRec.Body.Bytes(), &pubResp)

	delistReq := r.req(http.MethodPost, "/api/marketplace/listings/"+pubResp.ID+"/delist", r.admin, nil)
	delistReq.SetPathValue("id", pubResp.ID)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, delistReq); rec.Code != http.StatusNoContent {
		t.Fatalf("delist: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	afterReq := r.req(http.MethodGet, "/api/marketplace/listings/by-source/"+promptID, r.admin, nil)
	afterReq.SetPathValue("source_id", promptID)
	afterRec := doMarketplaceJSON(r.mh.handleMarketplaceListingBySource, afterReq)
	if afterRec.Code != http.StatusOK {
		t.Fatalf("by-source after delist: status = %d, want 200; body=%s", afterRec.Code, afterRec.Body.String())
	}
	var listing domain.MarketplaceListing
	if err := json.Unmarshal(afterRec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listing.ID != pubResp.ID {
		t.Fatalf("by-source id after delist = %q, want %q (not null)", listing.ID, pubResp.ID)
	}
	if listing.Status != domain.ListingStatusDelisted {
		t.Errorf("by-source status after delist = %q, want delisted", listing.Status)
	}
}

// TestMarketplaceListingBySource_RoundTrip: the by-source lookup 404s before
// publish — it used to answer 200 with a literal null, the only single read in
// the surface that reported absence in the body — and resolves the active
// listing after.
func TestMarketplaceListingBySource_RoundTrip(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "lookup-me", "mission", "", "")

	beforeReq := r.req(http.MethodGet, "/api/marketplace/listings/by-source/"+promptID, r.admin, nil)
	beforeReq.SetPathValue("source_id", promptID)
	beforeRec := doMarketplaceJSON(r.mh.handleMarketplaceListingBySource, beforeReq)
	if beforeRec.Code != http.StatusNotFound {
		t.Fatalf("by-source before publish: status = %d, want 404; body=%s", beforeRec.Code, beforeRec.Body.String())
	}

	publishRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	var pubResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(publishRec.Body.Bytes(), &pubResp)

	afterReq := r.req(http.MethodGet, "/api/marketplace/listings/by-source/"+promptID, r.admin, nil)
	afterReq.SetPathValue("source_id", promptID)
	afterRec := doMarketplaceJSON(r.mh.handleMarketplaceListingBySource, afterReq)
	if afterRec.Code != http.StatusOK {
		t.Fatalf("by-source after publish: status = %d, want 200; body=%s", afterRec.Code, afterRec.Body.String())
	}
	var listing domain.MarketplaceListing
	if err := json.Unmarshal(afterRec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listing.ID != pubResp.ID {
		t.Errorf("by-source id = %q, want %q", listing.ID, pubResp.ID)
	}
}

// TestMarketplaceListingBySource_IncludesEventTypes pins that the by-source
// response carries the listing's full event-type facet set, not just the
// header — the editor's republish dialog seeds its multi-select from this
// response, and PublishVersion replaces the whole facet set from whatever
// the client submits. Publishing with two event types and getting back
// only one (or none) here would silently narrow the listing's browse
// facets on the very next "Publish update" a user clicks.
func TestMarketplaceListingBySource_IncludesEventTypes(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "multi-facet", "mission", "", "")
	const secondEventType = "github:pr:ci_check_failed"

	publishRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
			"event_types": []string{eventTypeFixture, secondEventType},
		}))
	if publishRec.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d, want 201; body=%s", publishRec.Code, publishRec.Body.String())
	}
	var pubResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(publishRec.Body.Bytes(), &pubResp)

	afterReq := r.req(http.MethodGet, "/api/marketplace/listings/by-source/"+promptID, r.admin, nil)
	afterReq.SetPathValue("source_id", promptID)
	afterRec := doMarketplaceJSON(r.mh.handleMarketplaceListingBySource, afterReq)
	if afterRec.Code != http.StatusOK {
		t.Fatalf("by-source: status = %d, want 200; body=%s", afterRec.Code, afterRec.Body.String())
	}
	var summary domain.ListingSummary
	if err := json.Unmarshal(afterRec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := append([]string(nil), summary.EventTypes...)
	sort.Strings(got)
	want := []string{eventTypeFixture, secondEventType}
	sort.Strings(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("by-source event_types = %v, want %v (client seeds the republish dialog from exactly this)", got, want)
	}
}

// publishListing is a small helper wrapping handleMarketplacePublish for the
// browse-page tests below, which publish several listings and mostly care
// about the resulting id.
func (r *marketplaceRig) publishListing(t *testing.T, callerID string, body map[string]any) string {
	t.Helper()
	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish, r.req(http.MethodPost, "/api/marketplace/listings", callerID, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish %v: status = %d, want 201; body=%s", body, rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	return resp.ID
}

// list calls the browse list as callerID. query is the old query string
// ("kind=prompt", "sort=votes", ""), parsed here into the body the route now
// takes so the call sites stay readable.
func (r *marketplaceRig) list(t *testing.T, callerID, query string) []domain.ListingSummary {
	t.Helper()
	body := map[string]any{}
	for _, kv := range strings.Split(query, "&") {
		if kv == "" {
			continue
		}
		k, val, _ := strings.Cut(kv, "=")
		decoded, err := url.QueryUnescape(val)
		if err != nil {
			t.Fatalf("unescape %q: %v", val, err)
		}
		body[k] = decoded
	}
	rec := doMarketplaceJSON(r.mh.handleMarketplaceList, r.req(http.MethodPost, "/api/marketplace/listings/list", callerID, body))
	return decodeList[domain.ListingSummary](t, rec).Items
}

func listingIDs(summaries []domain.ListingSummary) []string {
	ids := make([]string, len(summaries))
	for i, s := range summaries {
		ids[i] = s.ID
	}
	return ids
}

// TestMarketplaceList_QueryFilter narrows to name/description substring
// matches, case-insensitively (ILIKE).
func TestMarketplaceList_QueryFilter(t *testing.T) {
	r := newMarketplaceRig(t)
	p1 := r.seedPrompt(t, r.teamID, "Triage Helper", "m", "", "")
	p2 := r.seedPrompt(t, r.teamID, "Deploy Bot", "m", "", "")
	id1 := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": p1, "name": "Triage Helper", "description": "helps triage"})
	r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": p2, "name": "Deploy Bot", "description": "deploys things"})

	got := r.list(t, r.admin, "query=triage")
	if ids := listingIDs(got); len(ids) != 1 || ids[0] != id1 {
		t.Fatalf("query=triage: ids = %v, want [%s]", ids, id1)
	}
}

// TestMarketplaceList_IncludesPublisherTeamName: the browse card needs a
// human-readable publisher label, not just publisher_team_id — resolved via
// a join into teams (any org member can read any team's name under RLS,
// unlike the caller-scoped /api/teams list).
func TestMarketplaceList_IncludesPublisherTeamName(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "named", "m", "", "")
	id := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": promptID, "name": "named"})

	got := r.list(t, r.member, "")
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("list = %+v, want the one listing", got)
	}
	if got[0].PublisherTeamName != "default" {
		t.Errorf("publisher_team_name = %q, want %q (the seeded team's name)", got[0].PublisherTeamName, "default")
	}
}

// TestMarketplaceList_EventTypeFilter narrows to listings faceted on the
// given event type — a listing with a different (or no) facet is excluded.
func TestMarketplaceList_EventTypeFilter(t *testing.T) {
	r := newMarketplaceRig(t)
	const otherEventType = "github:pr:ci_check_failed"
	p1 := r.seedPrompt(t, r.teamID, "Faceted", "m", "", "")
	p2 := r.seedPrompt(t, r.teamID, "Other Facet", "m", "", "")
	id1 := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": p1, "name": "Faceted", "event_types": []string{eventTypeFixture}})
	r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": p2, "name": "Other Facet", "event_types": []string{otherEventType}})

	got := r.list(t, r.admin, "event_type="+eventTypeFixture)
	if ids := listingIDs(got); len(ids) != 1 || ids[0] != id1 {
		t.Fatalf("event_type filter: ids = %v, want [%s]", ids, id1)
	}
}

// TestMarketplaceList_KindFilter narrows to prompt-only or blueprint-only
// listings.
func TestMarketplaceList_KindFilter(t *testing.T) {
	r := newMarketplaceRig(t)
	p1 := r.seedPrompt(t, r.teamID, "Standalone", "m", "", "")
	promptListing := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": p1, "name": "Standalone"})
	bp := r.seedBlueprint(t, r.teamID, "Chain")
	step := r.seedPrompt(t, r.teamID, "Chain Step", "m", "", "")
	r.seedBlueprintStep(t, r.teamID, bp, 0, step, "")
	blueprintListing := r.publishListing(t, r.admin, map[string]any{"kind": "blueprint", "source_id": bp, "name": "Chain"})

	if ids := listingIDs(r.list(t, r.admin, "kind=prompt")); len(ids) != 1 || ids[0] != promptListing {
		t.Fatalf("kind=prompt: ids = %v, want [%s]", ids, promptListing)
	}
	if ids := listingIDs(r.list(t, r.admin, "kind=blueprint")); len(ids) != 1 || ids[0] != blueprintListing {
		t.Fatalf("kind=blueprint: ids = %v, want [%s]", ids, blueprintListing)
	}
}

// TestMarketplaceList_InvalidKindOrSortIs400 rejects an unrecognized kind or
// sort value before ever reaching the store.
func TestMarketplaceList_InvalidKindOrSortIs400(t *testing.T) {
	r := newMarketplaceRig(t)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceList, r.req(http.MethodPost, "/api/marketplace/listings/list", r.admin, map[string]any{"kind": "widget"})); rec.Code != http.StatusBadRequest {
		t.Errorf("bad kind: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceList, r.req(http.MethodPost, "/api/marketplace/listings/list", r.admin, map[string]any{"sort": "popularity"})); rec.Code != http.StatusBadRequest {
		t.Errorf("bad sort: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceList_SortOrders pins each of the three accepted sort orders
// against listings with distinguishable install counts, vote counts, and
// publish recency.
func TestMarketplaceList_SortOrders(t *testing.T) {
	r := newMarketplaceRig(t)
	pOld := r.seedPrompt(t, r.teamID, "Old One", "m", "", "")
	pMid := r.seedPrompt(t, r.teamID, "Mid One", "m", "", "")
	pNew := r.seedPrompt(t, r.teamID, "New One", "m", "", "")
	old := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": pOld, "name": "Old One"})
	mid := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": pMid, "name": "Mid One"})
	fresh := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": pNew, "name": "New One"})

	// Recency: bump updated_at on each in publish order so fresh > mid > old.
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE marketplace_listings SET updated_at = now() - interval '2 hours' WHERE id = $1`, old)
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE marketplace_listings SET updated_at = now() - interval '1 hour' WHERE id = $1`, mid)
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE marketplace_listings SET updated_at = now() WHERE id = $1`, fresh)

	// Installs: old has the most, fresh has none.
	r.seedInstall(t, old, r.teamB, r.member)
	r.seedInstall(t, old, r.teamID, r.admin)
	r.seedInstall(t, mid, r.teamB, r.member)

	// Votes: mid has the most, old has none.
	voteReq := func(listingID, userID string) *http.Request {
		req := r.req(http.MethodPut, "/api/marketplace/listings/"+listingID+"/vote", userID, nil)
		req.SetPathValue("id", listingID)
		return req
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq(mid, r.admin)); rec.Code != http.StatusNoContent {
		t.Fatalf("vote mid/admin: status = %d, want 204", rec.Code)
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq(mid, r.viewer)); rec.Code != http.StatusNoContent {
		t.Fatalf("vote mid/viewer: status = %d, want 204", rec.Code)
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq(fresh, r.admin)); rec.Code != http.StatusNoContent {
		t.Fatalf("vote fresh/admin: status = %d, want 204", rec.Code)
	}

	if ids := listingIDs(r.list(t, r.admin, "sort=installs")); len(ids) != 3 || ids[0] != old || ids[1] != mid || ids[2] != fresh {
		t.Fatalf("sort=installs: ids = %v, want [%s %s %s] (2, 1, 0 installs)", ids, old, mid, fresh)
	}
	if ids := listingIDs(r.list(t, r.admin, "sort=votes")); len(ids) != 3 || ids[0] != mid || ids[1] != fresh || ids[2] != old {
		t.Fatalf("sort=votes: ids = %v, want [%s %s %s] (2, 1, 0 votes)", ids, mid, fresh, old)
	}
	if ids := listingIDs(r.list(t, r.admin, "sort=recent")); len(ids) != 3 || ids[0] != fresh || ids[1] != mid || ids[2] != old {
		t.Fatalf("sort=recent: ids = %v, want [%s %s %s] (newest updated_at first)", ids, fresh, mid, old)
	}

	// Run-derived usage (TFAC-540): fresh has the most total_runs despite
	// having zero installs/votes, old has none — sort=used must order purely
	// on total_runs, independent of the other two signals.
	r.seedStats(t, old, 2, 0)
	r.seedStats(t, mid, 1, 3)
	r.seedStats(t, fresh, 1, 7)
	if ids := listingIDs(r.list(t, r.admin, "sort=used")); len(ids) != 3 || ids[0] != fresh || ids[1] != mid || ids[2] != old {
		t.Fatalf("sort=used: ids = %v, want [%s %s %s] (7, 3, 0 total_runs)", ids, fresh, mid, old)
	}
	// Default (sort omitted) matches "installs" — the handler pins this
	// explicitly rather than falling through to the store's own internal
	// default (recent), so the HTTP contract doesn't depend on the store.
	if ids := listingIDs(r.list(t, r.admin, "")); len(ids) != 3 || ids[0] != old || ids[1] != mid || ids[2] != fresh {
		t.Fatalf("default sort: ids = %v, want [%s %s %s] (installs)", ids, old, mid, fresh)
	}
}

// TestMarketplaceList_ExcludesDelisted: a delisted listing drops out of
// browse for every member — including the publisher team's own member —
// since browse is "what's currently on offer", distinct from the by-source
// manage lookup which deliberately keeps resolving it.
func TestMarketplaceList_ExcludesDelisted(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "soon-gone", "m", "", "")
	id := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": promptID, "name": "soon-gone"})

	if got := r.list(t, r.member, ""); len(got) != 1 {
		t.Fatalf("list before delist = %+v, want 1 listing visible", got)
	}

	delistReq := r.req(http.MethodPost, "/api/marketplace/listings/"+id+"/delist", r.admin, nil)
	delistReq.SetPathValue("id", id)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, delistReq); rec.Code != http.StatusNoContent {
		t.Fatalf("delist: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	if got := r.list(t, r.member, ""); len(got) != 0 {
		t.Fatalf("cross-team member's list after delist = %+v, want empty", got)
	}
	if got := r.list(t, r.admin, ""); len(got) != 0 {
		t.Fatalf("publisher's own list after delist = %+v, want empty (browse ≠ manage)", got)
	}
}

// TestMarketplaceGet_DelistedVisibleToPublisherOnly: the detail endpoint
// (unlike List) still resolves a delisted listing for the publisher team —
// via RLS, not a status filter — so the manage/relist flow keeps working;
// a non-publisher member gets 404, same as any other invisible row.
func TestMarketplaceGet_DelistedVisibleToPublisherOnly(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "manage-me", "m", "", "")
	id := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": promptID, "name": "manage-me"})

	delistReq := r.req(http.MethodPost, "/api/marketplace/listings/"+id+"/delist", r.admin, nil)
	delistReq.SetPathValue("id", id)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, delistReq); rec.Code != http.StatusNoContent {
		t.Fatalf("delist: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	getReq := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.admin, nil)
	getReq.SetPathValue("id", id)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq); rec.Code != http.StatusOK {
		t.Fatalf("publisher's get(delisted): status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	memberReq := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.member, nil)
	memberReq.SetPathValue("id", id)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceGet, memberReq); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team member's get(delisted): status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceGet_UnknownIdIs404: a well-formed but nonexistent listing id
// 404s rather than 500ing.
func TestMarketplaceGet_UnknownIdIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	getReq := r.req(http.MethodGet, "/api/marketplace/listings/00000000-0000-0000-0000-000000000000", r.admin, nil)
	getReq.SetPathValue("id", "00000000-0000-0000-0000-000000000000")
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq); rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown id: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceGet_MalformedIdIs404: id is a uuid column on Postgres — a
// non-UUID path value must 404 cleanly rather than 500 on the store's
// SQLSTATE 22P02 cast error.
func TestMarketplaceGet_MalformedIdIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	getReq := r.req(http.MethodGet, "/api/marketplace/listings/not-a-uuid", r.admin, nil)
	getReq.SetPathValue("id", "not-a-uuid")
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq); rec.Code != http.StatusNotFound {
		t.Fatalf("get malformed id: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceGet_IncludesSnapshotAndVersions: the detail response carries
// the full current snapshot (for the read-only step preview) and the
// version history, not just the header — republishing must show up as a
// second version entry alongside the untouched v1.
func TestMarketplaceGet_IncludesSnapshotAndVersions(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "v1", "v1 body", "sonnet", "Bash")
	id := r.publishListing(t, r.admin, map[string]any{
		"kind": "prompt", "source_id": promptID, "name": "Detail Listing", "description": "d1",
		"event_types": []string{eventTypeFixture},
	})

	repubReq := r.req(http.MethodPost, "/api/marketplace/listings/"+id+"/versions", r.admin, map[string]any{
		"name": "Detail Listing v2", "description": "d2", "event_types": []string{eventTypeFixture},
	})
	repubReq.SetPathValue("id", id)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceListingVersionCreate, repubReq); rec.Code != http.StatusOK {
		t.Fatalf("republish: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	getReq := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.member, nil)
	getReq.SetPathValue("id", id)
	rec := doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var detail domain.ListingDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Name != "Detail Listing v2" {
		t.Errorf("name = %q, want the republished v2 name", detail.Name)
	}
	if len(detail.CurrentSnapshot.Steps) != 1 || detail.CurrentSnapshot.Steps[0].Body != "v1 body" {
		t.Errorf("current_snapshot = %+v, want the (unchanged) source prompt body", detail.CurrentSnapshot)
	}
	if len(detail.Versions) != 2 || detail.Versions[0].Version != 1 || detail.Versions[1].Version != 2 {
		t.Fatalf("versions = %+v, want v1 and v2", detail.Versions)
	}
}

// TestMarketplaceGet_Stats pins the TFAC-540 HTTP contract: Stats is nil
// (omitted from the JSON body) until the aggregation job has computed a row
// for this listing, and populated once it has — including the no-wrong-
// fallbacks case where success_rate is absent because total_runs is 0.
func TestMarketplaceGet_Stats(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "stats-listing", "m", "", "")
	id := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": promptID, "name": "stats-listing"})

	getReq := func() *http.Request {
		req := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.member, nil)
		req.SetPathValue("id", id)
		return req
	}

	rec := doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("get before stats computed: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var before domain.ListingDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if before.Stats != nil {
		t.Errorf("Stats before the job has run = %+v, want nil", before.Stats)
	}

	r.seedStats(t, id, 2, 0)
	rec = doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq())
	var zeroRuns domain.ListingDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &zeroRuns); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if zeroRuns.Stats == nil || zeroRuns.Stats.TeamsUsing != 2 || zeroRuns.Stats.TotalRuns != 0 {
		t.Fatalf("Stats with zero runs = %+v, want teams_using=2 total_runs=0", zeroRuns.Stats)
	}
	if zeroRuns.Stats.SuccessRate != nil {
		t.Errorf("SuccessRate with zero runs = %v, want nil (no wrong fallbacks)", *zeroRuns.Stats.SuccessRate)
	}

	pgtest.MustExec(t, r.h.AdminDB, `UPDATE marketplace_listing_stats SET total_runs = 4, success_rate = 0.5 WHERE listing_id = $1`, id)
	rec = doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq())
	var withRuns domain.ListingDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &withRuns); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if withRuns.Stats == nil || withRuns.Stats.TotalRuns != 4 || withRuns.Stats.SuccessRate == nil || *withRuns.Stats.SuccessRate != 0.5 {
		t.Fatalf("Stats with runs = %+v, want total_runs=4 success_rate=0.5", withRuns.Stats)
	}
}

// TestMarketplaceVote_Idempotent: voting twice from the same user leaves
// vote_count at 1 and viewer_voted true; a third-party's own viewer_voted
// stays false.
func TestMarketplaceVote_Idempotent(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "votable", "m", "", "")
	id := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": promptID, "name": "votable"})

	voteReq := func() *http.Request {
		req := r.req(http.MethodPut, "/api/marketplace/listings/"+id+"/vote", r.member, nil)
		req.SetPathValue("id", id)
		return req
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq()); rec.Code != http.StatusNoContent {
		t.Fatalf("first vote: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq()); rec.Code != http.StatusNoContent {
		t.Fatalf("second (repeat) vote: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	memberGetReq := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.member, nil)
	memberGetReq.SetPathValue("id", id)
	memberDetail := decodeListingDetail(t, doMarketplaceJSON(r.mh.handleMarketplaceGet, memberGetReq))
	if memberDetail.VoteCount != 1 {
		t.Errorf("vote_count after repeat vote = %d, want 1", memberDetail.VoteCount)
	}
	if !memberDetail.ViewerVoted {
		t.Errorf("member's viewer_voted = false, want true")
	}

	adminGetReq := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.admin, nil)
	adminGetReq.SetPathValue("id", id)
	adminDetail := decodeListingDetail(t, doMarketplaceJSON(r.mh.handleMarketplaceGet, adminGetReq))
	if adminDetail.ViewerVoted {
		t.Errorf("admin's viewer_voted = true, want false (admin never voted)")
	}
	if adminDetail.VoteCount != 1 {
		t.Errorf("admin's view of vote_count = %d, want 1 (shared count)", adminDetail.VoteCount)
	}
}

// TestMarketplaceUnvote_Idempotent: unvoting when no vote exists is a no-op
// (still 204), and unvoting an existing vote drops the count and clears
// viewer_voted.
func TestMarketplaceUnvote_Idempotent(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "unvotable", "m", "", "")
	id := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": promptID, "name": "unvotable"})

	unvoteReq := func() *http.Request {
		req := r.req(http.MethodDelete, "/api/marketplace/listings/"+id+"/vote", r.member, nil)
		req.SetPathValue("id", id)
		return req
	}
	// No prior vote — still a no-op 204.
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceUnvote, unvoteReq()); rec.Code != http.StatusNoContent {
		t.Fatalf("unvote with no prior vote: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	voteReq := r.req(http.MethodPut, "/api/marketplace/listings/"+id+"/vote", r.member, nil)
	voteReq.SetPathValue("id", id)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq); rec.Code != http.StatusNoContent {
		t.Fatalf("vote: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceUnvote, unvoteReq()); rec.Code != http.StatusNoContent {
		t.Fatalf("unvote: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	getReq := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.member, nil)
	getReq.SetPathValue("id", id)
	detail := decodeListingDetail(t, doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq))
	if detail.VoteCount != 0 {
		t.Errorf("vote_count after unvote = %d, want 0", detail.VoteCount)
	}
	if detail.ViewerVoted {
		t.Errorf("viewer_voted after unvote = true, want false")
	}
}

// TestMarketplaceVote_MalformedIdIs404: a non-UUID path value 404s cleanly —
// same reasoning as TestMarketplaceGet_MalformedIdIs404 — rather than 500ing
// on the store's cast error.
func TestMarketplaceVote_MalformedIdIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	req := r.req(http.MethodPut, "/api/marketplace/listings/not-a-uuid/vote", r.admin, nil)
	req.SetPathValue("id", "not-a-uuid")
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, req); rec.Code != http.StatusNotFound {
		t.Fatalf("vote malformed id: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceUnvote_MalformedIdIs404 mirrors the vote-side case.
func TestMarketplaceUnvote_MalformedIdIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	req := r.req(http.MethodDelete, "/api/marketplace/listings/not-a-uuid/vote", r.admin, nil)
	req.SetPathValue("id", "not-a-uuid")
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceUnvote, req); rec.Code != http.StatusNotFound {
		t.Fatalf("unvote malformed id: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceVote_UnknownIdIs404: a well-formed but nonexistent listing
// id 404s rather than 500ing on marketplace_votes' FK-to-marketplace_listings
// violation — the pre-check in handleMarketplaceVote must catch this before
// the INSERT ever runs.
func TestMarketplaceVote_UnknownIdIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	const unknownID = "00000000-0000-0000-0000-000000000000"
	req := r.req(http.MethodPut, "/api/marketplace/listings/"+unknownID+"/vote", r.admin, nil)
	req.SetPathValue("id", unknownID)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, req); rec.Code != http.StatusNotFound {
		t.Fatalf("vote unknown id: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var count int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_votes WHERE listing_id = $1`, unknownID).Scan(&count); err != nil {
		t.Fatalf("count votes: %v", err)
	}
	if count != 0 {
		t.Errorf("vote count for unknown listing = %d, want 0 (nothing written)", count)
	}
}

// TestMarketplaceUnvote_UnknownIdIsNoOp: unlike Vote, Unvote's DELETE carries
// no FK — a well-formed but nonexistent listing id is just a zero-row no-op,
// consistent with "removing a vote that doesn't exist" already being a no-op.
func TestMarketplaceUnvote_UnknownIdIsNoOp(t *testing.T) {
	r := newMarketplaceRig(t)
	const unknownID = "00000000-0000-0000-0000-000000000000"
	req := r.req(http.MethodDelete, "/api/marketplace/listings/"+unknownID+"/vote", r.admin, nil)
	req.SetPathValue("id", unknownID)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceUnvote, req); rec.Code != http.StatusNoContent {
		t.Fatalf("unvote unknown id: status = %d, want 204 (idempotent no-op); body=%s", rec.Code, rec.Body.String())
	}
}

func decodeListingDetail(t *testing.T, rec *httptest.ResponseRecorder) domain.ListingDetail {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var detail domain.ListingDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return detail
}

// installResponse mirrors installListingResponse — the wire shape the
// install tests below decode.
type installResponse struct {
	Kind      string   `json:"kind"`
	ID        string   `json:"id"`
	PromptIDs []string `json:"prompt_ids"`
}

// publishPrompt publishes name/body/model/tools as a prompt-kind listing
// (via the real handler, admin as publisher) and returns the listing id —
// shared setup for the install tests below.
func (r *marketplaceRig) publishPrompt(t *testing.T, name, body, model, tools, listingName string) string {
	t.Helper()
	promptID := r.seedPrompt(t, r.teamID, name, body, model, tools)
	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": listingName,
		}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish setup: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	return resp.ID
}

// installAs POSTs .../install as callerID with the given team_id and
// returns the recorder.
func (r *marketplaceRig) installAs(callerID, listingID, teamID string) *httptest.ResponseRecorder {
	req := r.req(http.MethodPost, "/api/marketplace/listings/"+listingID+"/install", callerID, map[string]any{
		"team_id": teamID,
	})
	req.SetPathValue("id", listingID)
	return doMarketplaceJSON(r.mh.handleMarketplaceInstall, req)
}

// TestMarketplaceInstall_Prompt_HappyPath: installing a prompt-kind listing
// creates one team prompt (source='marketplace', content copied verbatim)
// in the target team and records the install with the right root_object_id.
func TestMarketplaceInstall_Prompt_HappyPath(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "Great Prompt", "do the thing", "opus", "Bash,Read", "Great Prompt Listing")

	rec := r.installAs(r.member, listingID, r.teamB)
	if rec.Code != http.StatusCreated {
		t.Fatalf("install: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp installResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Kind != domain.ListingKindPrompt || resp.ID == "" || len(resp.PromptIDs) != 1 || resp.PromptIDs[0] != resp.ID {
		t.Fatalf("install response = %+v, want kind=prompt with a single prompt id equal to the root id", resp)
	}

	var name, body, model, tools, source, teamID string
	if err := r.h.AdminDB.QueryRow(`SELECT name, body, model, allowed_tools, source, team_id FROM prompts WHERE id = $1`, resp.ID).
		Scan(&name, &body, &model, &tools, &source, &teamID); err != nil {
		t.Fatalf("read installed prompt: %v", err)
	}
	if name != "Great Prompt" || body != "do the thing" || model != "opus" || tools != "Bash,Read" || source != "marketplace" || teamID != r.teamB {
		t.Errorf("installed prompt = (%q,%q,%q,%q,%q,%q), want the source content copied verbatim into teamB with source=marketplace",
			name, body, model, tools, source, teamID)
	}

	var installCount int
	var rootObjectID string
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(root_object_id::text), '') FROM marketplace_installs WHERE listing_id = $1`, listingID).
		Scan(&installCount, &rootObjectID); err != nil {
		t.Fatalf("count installs: %v", err)
	}
	if installCount != 1 || rootObjectID != resp.ID {
		t.Errorf("install record = count=%d root=%q, want count=1 root=%q", installCount, rootObjectID, resp.ID)
	}
}

// TestMarketplaceInstall_Blueprint_HappyPath: installing a blueprint-kind
// listing creates one fresh prompt per step (never the source ids — every
// step gets its own row) plus the blueprint, step order + briefs preserved,
// everything source='marketplace' and owned by the target team.
func TestMarketplaceInstall_Blueprint_HappyPath(t *testing.T) {
	r := newMarketplaceRig(t)
	p1 := r.seedPrompt(t, r.teamID, "Step One", "first mission", "sonnet", "")
	p2 := r.seedPrompt(t, r.teamID, "Step Two", "second mission", "opus", "Grep")
	bp := r.seedBlueprint(t, r.teamID, "Two-Step Chain")
	r.seedBlueprintStep(t, r.teamID, bp, 0, p1, "do step one first")
	r.seedBlueprintStep(t, r.teamID, bp, 1, p2, "")

	publishRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "blueprint", "source_id": bp, "name": "Chain Listing",
		}))
	if publishRec.Code != http.StatusCreated {
		t.Fatalf("publish setup: status = %d, want 201; body=%s", publishRec.Code, publishRec.Body.String())
	}
	var pubResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(publishRec.Body.Bytes(), &pubResp); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}

	rec := r.installAs(r.member, pubResp.ID, r.teamB)
	if rec.Code != http.StatusCreated {
		t.Fatalf("install: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp installResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Kind != domain.ListingKindBlueprint || resp.ID == "" || len(resp.PromptIDs) != 2 {
		t.Fatalf("install response = %+v, want kind=blueprint with 2 fresh prompt ids", resp)
	}
	if resp.PromptIDs[0] == p1 || resp.PromptIDs[1] == p2 {
		t.Errorf("install response reused the source prompt ids %v — every step must get a fresh prompt row", resp.PromptIDs)
	}

	var bpName, bpSource, bpTeam string
	if err := r.h.AdminDB.QueryRow(`SELECT name, source, team_id FROM blueprints WHERE id = $1`, resp.ID).Scan(&bpName, &bpSource, &bpTeam); err != nil {
		t.Fatalf("read installed blueprint: %v", err)
	}
	if bpName != "Chain Listing" || bpSource != "marketplace" || bpTeam != r.teamB {
		t.Errorf("installed blueprint = (%q,%q,%q), want (Chain Listing, marketplace, teamB)", bpName, bpSource, bpTeam)
	}

	rows, err := r.h.AdminDB.Query(`
		SELECT bs.step_index, bs.brief, p.name, p.body, p.model, p.allowed_tools
		FROM blueprint_steps bs JOIN prompts p ON p.id = bs.step_prompt_id
		WHERE bs.blueprint_id = $1 ORDER BY bs.step_index
	`, resp.ID)
	if err != nil {
		t.Fatalf("query steps: %v", err)
	}
	defer rows.Close()
	type stepRow struct {
		idx                             int
		brief, name, body, model, tools string
	}
	var steps []stepRow
	for rows.Next() {
		var s stepRow
		if err := rows.Scan(&s.idx, &s.brief, &s.name, &s.body, &s.model, &s.tools); err != nil {
			t.Fatalf("scan: %v", err)
		}
		steps = append(steps, s)
	}
	if len(steps) != 2 {
		t.Fatalf("step count = %d, want 2", len(steps))
	}
	if steps[0].name != "Step One" || steps[0].body != "first mission" || steps[0].model != "sonnet" || steps[0].brief != "do step one first" {
		t.Errorf("step 0 = %+v, want the source step's content verbatim", steps[0])
	}
	if steps[1].name != "Step Two" || steps[1].body != "second mission" || steps[1].tools != "Grep" {
		t.Errorf("step 1 = %+v, want the source step's content verbatim", steps[1])
	}
}

// TestMarketplaceInstall_NonWriterOnTargetTeamIs403: write role is checked
// on the *installing* team, not the publisher's — a viewer on the target
// team is blocked even though they can freely browse/read the listing.
func TestMarketplaceInstall_NonWriterOnTargetTeamIs403(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "guarded", "mission", "", "", "n")

	viewerB := pgtest.SeedUser(t, r.h, "viewerB")
	pgtest.AddOrgMember(t, r.h, viewerB, r.orgID, r.teamB, "member", "viewer")

	rec := r.installAs(viewerB, listingID, r.teamB)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer install: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	var count int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_installs WHERE listing_id = $1`, listingID).Scan(&count); err != nil {
		t.Fatalf("count installs: %v", err)
	}
	if count != 0 {
		t.Errorf("install count = %d after blocked viewer install, want 0", count)
	}
}

// TestMarketplaceInstall_UnknownListingIs404: a well-formed but nonexistent
// listing id 404s rather than 500ing.
func TestMarketplaceInstall_UnknownListingIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	const unknownID = "00000000-0000-0000-0000-000000000000"
	rec := r.installAs(r.admin, unknownID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown listing install: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceInstall_DelistedByPublisherIsGone: the publisher can still
// see (Get) their own delisted listing (RLS: published OR own-team-write),
// but installing it is refused with 410 — delisting must actually block new
// forks, not just hide from browse.
func TestMarketplaceInstall_DelistedByPublisherIsGone(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "delist-me", "mission", "", "", "n")

	delistReq := r.req(http.MethodPost, "/api/marketplace/listings/"+listingID+"/delist", r.admin, nil)
	delistReq.SetPathValue("id", listingID)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, delistReq); rec.Code != http.StatusNoContent {
		t.Fatalf("delist setup: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	rec := r.installAs(r.admin, listingID, "")
	if rec.Code != http.StatusGone {
		t.Fatalf("delisted install (publisher, visible): status = %d, want 410; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceInstall_DelistedInvisibleToNonPublisherIs404: a
// non-publisher can't see a delisted listing at all (RLS), so their install
// attempt 404s the same as any other unknown id — it never learns the
// listing existed.
func TestMarketplaceInstall_DelistedInvisibleToNonPublisherIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "delist-me-2", "mission", "", "", "n")

	delistReq := r.req(http.MethodPost, "/api/marketplace/listings/"+listingID+"/delist", r.admin, nil)
	delistReq.SetPathValue("id", listingID)
	doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, delistReq)

	rec := r.installAs(r.member, listingID, r.teamB)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delisted install (non-publisher, invisible): status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceInstall_RepeatInstallCreatesSecondCopy: forks are cheap —
// the same team installing the same listing twice gets two independent
// copies and two marketplace_installs rows, not a dedupe/no-op.
func TestMarketplaceInstall_RepeatInstallCreatesSecondCopy(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "repeatable", "mission", "", "", "n")

	install := func() string {
		rec := r.installAs(r.member, listingID, r.teamB)
		if rec.Code != http.StatusCreated {
			t.Fatalf("install: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
		}
		var resp installResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.ID
	}

	first := install()
	second := install()
	if first == second {
		t.Fatalf("repeat install produced the same root object id %q twice, want two distinct copies", first)
	}

	var count int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_installs WHERE listing_id = $1`, listingID).Scan(&count); err != nil {
		t.Fatalf("count installs: %v", err)
	}
	if count != 2 {
		t.Errorf("install count = %d after two installs by the same team, want 2", count)
	}
	var promptCount int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM prompts WHERE id IN ($1, $2)`, first, second).Scan(&promptCount); err != nil {
		t.Fatalf("count prompts: %v", err)
	}
	if promptCount != 2 {
		t.Errorf("prompt count = %d, want 2 distinct copies", promptCount)
	}
}

// TestMarketplaceInstall_SucceedsAfterSourceDeleted: install materializes
// the frozen listing snapshot, never the publisher team's live source
// object — so a hard-deleted source prompt does not break a later install
// (TFAC-535's cross-org insurance: the installer consumes a snapshot
// document, never source rows).
func TestMarketplaceInstall_SucceedsAfterSourceDeleted(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "vanishing", "mission before deletion", "", "")
	publishRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "Vanishing Prompt",
		}))
	if publishRec.Code != http.StatusCreated {
		t.Fatalf("publish setup: status = %d, want 201; body=%s", publishRec.Code, publishRec.Body.String())
	}
	var pubResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(publishRec.Body.Bytes(), &pubResp); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}

	pgtest.MustExec(t, r.h.AdminDB, `DELETE FROM prompts WHERE id = $1`, promptID)

	rec := r.installAs(r.member, pubResp.ID, r.teamB)
	if rec.Code != http.StatusCreated {
		t.Fatalf("install after source deleted: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp installResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var body string
	if err := r.h.AdminDB.QueryRow(`SELECT body FROM prompts WHERE id = $1`, resp.ID).Scan(&body); err != nil {
		t.Fatalf("read installed prompt: %v", err)
	}
	if body != "mission before deletion" {
		t.Errorf("installed prompt body = %q, want the frozen snapshot content", body)
	}
}
