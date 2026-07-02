package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// handler family (TFAC-536): a real Postgres-backed Server (RLS + the
// marketplace_enabled toggle live) with one org carrying a founder (write
// role on the default team), a plain member on a second team, and a viewer
// on the founder's team.
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
	s := New(h.AdminDB, stores, 3000)

	orgID, founder, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	viewer := pgtest.SeedUser(t, h, "viewer")
	pgtest.AddOrgMember(t, h, viewer, orgID, teamID, "member", "viewer")
	teamB := pgtest.SeedTeam(t, h, orgID, "teamB")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamB, "member", "member")

	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_settings (org_id, marketplace_enabled) VALUES ($1, true)`, orgID)

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
// mode, regardless of caller identity or org toggle state.
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

// TestMarketplace_ToggleOffIs404: multi-mode but org_settings.marketplace_enabled
// is off (the default, ship-dark) — the whole surface 404s.
func TestMarketplace_ToggleOffIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "toggle-gate", "mission", "", "")
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE org_settings SET marketplace_enabled = false WHERE org_id = $1`, r.orgID)

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin,
			map[string]any{"kind": "prompt", "source_id": promptID, "name": "n"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("publish with toggle off: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	getReq := r.req(http.MethodGet, "/api/marketplace/listings/by-source/"+promptID, r.admin, nil)
	getReq.SetPathValue("source_id", promptID)
	rec = doMarketplaceJSON(r.mh.handleMarketplaceListingBySource, getReq)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("by-source with toggle off: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
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

// TestMarketplacePublish_CrossTeamIs403: a write-role member of a *different*
// team cannot publish someone else's team object — the gate keys on the
// source object's own team, not just "any write role somewhere".
func TestMarketplacePublish_CrossTeamIs403(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "not-yours", "mission", "", "")

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.member, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-team publish: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
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

// TestMarketplaceListingBySource_RoundTrip: the by-source lookup is null
// before publish and resolves the active listing after.
func TestMarketplaceListingBySource_RoundTrip(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "lookup-me", "mission", "", "")

	beforeReq := r.req(http.MethodGet, "/api/marketplace/listings/by-source/"+promptID, r.admin, nil)
	beforeReq.SetPathValue("source_id", promptID)
	beforeRec := doMarketplaceJSON(r.mh.handleMarketplaceListingBySource, beforeReq)
	if beforeRec.Code != http.StatusOK {
		t.Fatalf("by-source before publish: status = %d, want 200; body=%s", beforeRec.Code, beforeRec.Body.String())
	}
	if body := strings.TrimSpace(beforeRec.Body.String()); body != "null" {
		t.Fatalf("by-source before publish = %q, want null", body)
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
